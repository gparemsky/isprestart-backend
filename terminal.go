package main

import (
	"bufio"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Cache methods for ISP states
func (cache *ISPStateCache) updateStates(primary, secondary ISPState) {
	cache.Lock()
	defer cache.Unlock()
	cache.primaryState = primary
	cache.secondaryState = secondary
	cache.lastUpdated = time.Now()
	cache.initialized = true
}

func (cache *ISPStateCache) getStates() (ISPState, ISPState, bool) {
	cache.RLock()
	defer cache.RUnlock()
	return cache.primaryState, cache.secondaryState, cache.initialized
}

func (cache *ISPStateCache) isStale(maxAge time.Duration) bool {
	cache.RLock()
	defer cache.RUnlock()
	return !cache.initialized || time.Since(cache.lastUpdated) > maxAge
}

// Terminal control functions
func initTerminal() {
	if runtime.GOOS == "windows" {
		// On Windows, try to enable ANSI escape sequences
		// This may not work on older Windows versions
		console.terminalMode = false
		return
	}
	
	console.Lock()
	defer console.Unlock()
	
	// Get terminal size
	console.termHeight, console.termWidth = getTerminalSize()
	if console.termHeight < 10 {
		console.terminalMode = false
		return
	}
	
	// Reserve bottom 3 lines for input area
	console.outputLines = console.termHeight - 3
	console.inputStartRow = console.termHeight - 2
	console.terminalMode = true
	console.needsRedraw = true
	
	// Enable alternative screen buffer and hide cursor
	fmt.Print("\033[?1049h\033[?25l")
	
	// Set up signal handler for cleanup
	setupSignalHandler()
	
	redrawTerminal()
}

func getTerminalSize() (height, width int) {
	// Try to get terminal size using stty (Unix/Linux/Mac)
	if runtime.GOOS != "windows" {
		cmd := exec.Command("stty", "size")
		cmd.Stdin = os.Stdin
		output, err := cmd.Output()
		if err == nil {
			fmt.Sscanf(string(output), "%d %d", &height, &width)
			if height > 0 && width > 0 {
				return height, width
			}
		}
	}
	
	// Default fallback
	return 24, 80
}

func setupSignalHandler() {
	c := make(chan os.Signal, 1)
	if runtime.GOOS != "windows" {
		// SIGWINCH is not available on Windows
		signal.Notify(c, os.Interrupt, syscall.SIGTERM, syscall.Signal(28)) // SIGWINCH = 28 on Unix
	} else {
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	}
	
	go func() {
		for sig := range c {
			switch sig {
			case syscall.Signal(28): // SIGWINCH on Unix
				// Terminal resized
				if console.terminalMode && runtime.GOOS != "windows" {
					console.Lock()
					console.termHeight, console.termWidth = getTerminalSize()
					if console.termHeight >= 10 {
						console.outputLines = console.termHeight - 3
						console.inputStartRow = console.termHeight - 2
						console.needsRedraw = true
					}
					console.Unlock()
					redrawTerminal()
				}
			case os.Interrupt, syscall.SIGTERM:
				cleanupTerminal()
				os.Exit(0)
			}
		}
	}()
}

func cleanupTerminal() {
	if console.terminalMode {
		// Show cursor and restore normal screen buffer
		fmt.Print("\033[?25h\033[?1049l")
	}
}

func redrawTerminal() {
	if !console.terminalMode {
		return
	}
	
	// Clear screen
	fmt.Print("\033[2J\033[H")
	
	// Draw output area
	drawOutputArea()
	
	// Draw separator line
	drawSeparator()
	
	// Draw input area
	drawInputArea()
	
	console.needsRedraw = false
}

func drawOutputArea() {
	console.RLock()
	buffer := console.outputBuffer
	console.RUnlock()
	
	// Calculate which lines to show
	start := 0
	if len(buffer) > console.outputLines {
		start = len(buffer) - console.outputLines
	}
	
	// Position cursor at top and draw output lines
	fmt.Print("\033[H")
	for i := 0; i < console.outputLines; i++ {
		if start+i < len(buffer) {
			line := buffer[start+i]
			if len(line) > console.termWidth {
				line = line[:console.termWidth-3] + "..."
			}
			fmt.Printf("%-*s\n", console.termWidth, line)
		} else {
			fmt.Printf("%-*s\n", console.termWidth, "")
		}
	}
}

func drawSeparator() {
	// Move to separator line and draw it
	fmt.Printf("\033[%d;1H", console.inputStartRow-1)
	for i := 0; i < console.termWidth; i++ {
		fmt.Print("─")
	}
}

func drawInputArea() {
	// Move to input line
	fmt.Printf("\033[%d;1H", console.inputStartRow)
	prompt := "[CMD] > "
	fmt.Printf("%-*s", console.termWidth, prompt+console.currentInput)
	
	// Position cursor correctly
	cursorCol := len(prompt) + console.cursorPos + 1
	fmt.Printf("\033[%d;%dH", console.inputStartRow, cursorCol)
}

// Logging functions with levels
func logAtLevel(level LogLevel, format string, args ...interface{}) {
	if !console.enabled || level > console.logLevel {
		return
	}
	
	message := fmt.Sprintf(format, args...)
	timestamp := time.Now().Format("15:04:05")
	logLine := fmt.Sprintf("[%s] %s", timestamp, message)
	
	console.Lock()
	console.outputBuffer = append(console.outputBuffer, logLine)
	if len(console.outputBuffer) > console.maxLines {
		console.outputBuffer = console.outputBuffer[1:]
	}
	console.Unlock()
	
	if console.terminalMode {
		// In terminal mode, redraw only the output area
		drawOutputArea()
		drawInputArea() // Restore input area
	} else {
		// In simple mode, buffer output and only print periodically to reduce interruption
		// This helps on Windows where we can't use the terminal UI
		if level <= LOG_INFO || len(console.outputBuffer) % 10 == 0 {
			// Print immediately for important messages or every 10th message
			fmt.Println(logLine)
		}
		// Less important messages (DEBUG/TRACE) are still stored in buffer but not printed immediately
	}
}

func logError(format string, args ...interface{}) {
	logAtLevel(LOG_ERROR, "[ERROR] "+format, args...)
}

func logWarn(format string, args ...interface{}) {
	logAtLevel(LOG_WARN, "[WARN] "+format, args...)
}

func logInfo(format string, args ...interface{}) {
	logAtLevel(LOG_INFO, "[INFO] "+format, args...)
}

func logDebug(format string, args ...interface{}) {
	logAtLevel(LOG_DEBUG, "[DEBUG] "+format, args...)
}

func logTrace(format string, args ...interface{}) {
	logAtLevel(LOG_TRACE, "[TRACE] "+format, args...)
}

// Command interface functions
func startCommandInterface(db *sql.DB) {
	if console.terminalMode {
		startTerminalInput(db)
	} else {
		startSimpleInput(db)
	}
}

func startSimpleInput(db *sql.DB) {
	reader := bufio.NewReader(os.Stdin)
	for {
		// In simple mode, just wait for input and process commands
		command, err := reader.ReadString('\n')
		if err != nil {
			continue
		}
		
		command = strings.TrimSpace(command)
		if command == "" {
			continue
		}
		
		// Process the command
		processCommand(db, command)
		
		// After processing, flush any buffered output in simple mode
		if !console.terminalMode {
			console.RLock()
			for _, line := range console.outputBuffer {
				fmt.Println(line)
			}
			console.outputBuffer = nil
			console.RUnlock()
		}
	}
}

func startTerminalInput(db *sql.DB) {
	reader := bufio.NewReader(os.Stdin)
	
	// Enable raw mode for character-by-character input
	// Note: This is platform-specific and may need adjustment
	
	for {
		char, _, err := reader.ReadRune()
		if err != nil {
			continue
		}
		
		console.Lock()
		
		switch char {
		case '\n', '\r': // Enter
			// Process the command
			command := console.currentInput
			console.currentInput = ""
			console.cursorPos = 0
			console.Unlock()
			
			if command != "" {
				processCommand(db, command)
			}
			
			redrawTerminal()
			
		case '\b', 127: // Backspace/Delete
			if console.cursorPos > 0 {
				console.currentInput = console.currentInput[:console.cursorPos-1] + console.currentInput[console.cursorPos:]
				console.cursorPos--
			}
			console.Unlock()
			drawInputArea()
			
		case '\x1b': // Escape sequence
			console.Unlock()
			handleEscapeSequence(reader)
			
		default:
			// Regular character
			if char >= 32 && char < 127 { // Printable ASCII
				console.currentInput = console.currentInput[:console.cursorPos] + string(char) + console.currentInput[console.cursorPos:]
				console.cursorPos++
			}
			console.Unlock()
			drawInputArea()
		}
	}
}

func handleEscapeSequence(reader *bufio.Reader) {
	// Read the next character
	char, _, err := reader.ReadRune()
	if err != nil || char != '[' {
		return
	}
	
	// Read the sequence
	seqChar, _, err := reader.ReadRune()
	if err != nil {
		return
	}
	
	console.Lock()
	defer console.Unlock()
	
	switch seqChar {
	case 'D': // Left arrow
		if console.cursorPos > 0 {
			console.cursorPos--
		}
	case 'C': // Right arrow
		if console.cursorPos < len(console.currentInput) {
			console.cursorPos++
		}
	case 'A', 'B': // Up/Down arrows - could implement command history here
		// For now, ignore
	}
	
	drawInputArea()
}

func processCommand(db *sql.DB, command string) {
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return
	}
	
	cmd := strings.ToLower(parts[0])
	
	switch cmd {
	case "help":
		printHelp()
		
	case "status":
		showISPStatus(db)
		
	case "isp1", "isp2":
		if len(parts) < 2 {
			logError("Usage: %s <on|off>", cmd)
			return
		}
		ispID := 1
		if cmd == "isp2" {
			ispID = 2
		}
		powerState := strings.ToLower(parts[1]) == "on"
		setISPState(db, ispID, powerState)
		
	case "restart":
		if len(parts) < 2 {
			logError("Usage: restart <isp1|isp2> [duration_minutes]")
			return
		}
		
		ispID := 0
		if strings.ToLower(parts[1]) == "isp1" {
			ispID = 1
		} else if strings.ToLower(parts[1]) == "isp2" {
			ispID = 2
		} else {
			logError("Invalid ISP. Use 'isp1' or 'isp2'")
			return
		}
		
		duration := 5 // default 5 minutes
		if len(parts) >= 3 {
			if d, err := strconv.Atoi(parts[2]); err == nil {
				duration = d
			}
		}
		
		restartISP(db, ispID, duration)
		
	case "loglevel":
		if len(parts) < 2 {
			logInfo("Current log level: %s", logLevelNames[console.logLevel])
			logInfo("Usage: loglevel <silent|error|warn|info|debug|trace>")
			return
		}
		setLogLevel(parts[1])
		
	case "logs":
		showRecentLogs()
		
	case "clear":
		console.Lock()
		console.outputBuffer = nil
		console.Unlock()
		if console.terminalMode {
			redrawTerminal()
		}
		
	case "exit", "quit":
		logInfo("Shutting down...")
		cleanupTerminal()
		os.Exit(0)
		
	default:
		logError("Unknown command: %s. Type 'help' for available commands", cmd)
	}
}

func printHelp() {
	logInfo("Available commands:")
	logInfo("  help           - Show this help message")
	logInfo("  status         - Show ISP status")
	logInfo("  isp1 <on|off>  - Control ISP 1 power state")
	logInfo("  isp2 <on|off>  - Control ISP 2 power state")
	logInfo("  restart <isp1|isp2> [duration_minutes] - Restart ISP for specified duration (default 5 min)")
	logInfo("  loglevel <level> - Set log level (silent|error|warn|info|debug|trace)")
	logInfo("  logs           - Show recent log entries")
	logInfo("  clear          - Clear console output")
	logInfo("  exit/quit      - Exit the program")
}

func showRecentLogs() {
	console.RLock()
	defer console.RUnlock()
	
	// Show last 20 logs
	start := 0
	if len(console.outputBuffer) > 20 {
		start = len(console.outputBuffer) - 20
	}
	
	logInfo("=== Recent logs ===")
	for i := start; i < len(console.outputBuffer); i++ {
		fmt.Println(console.outputBuffer[i])
	}
	logInfo("=== End of logs ===")
}

func setLogLevel(levelStr string) {
	levelStr = strings.ToLower(levelStr)
	
	var newLevel LogLevel
	switch levelStr {
	case "silent":
		newLevel = LOG_SILENT
	case "error":
		newLevel = LOG_ERROR
	case "warn":
		newLevel = LOG_WARN
	case "info":
		newLevel = LOG_INFO
	case "debug":
		newLevel = LOG_DEBUG
	case "trace":
		newLevel = LOG_TRACE
	default:
		logError("Invalid log level: %s", levelStr)
		return
	}
	
	console.Lock()
	console.logLevel = newLevel
	console.Unlock()
	
	logInfo("Log level set to: %s", logLevelNames[newLevel])
}