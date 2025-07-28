// This file is compiled for raspberry pi 3b using the command:
// GOOS=linux GOARCH=arm GOARM=6 go build main.go

// the file is built for linux, using -c for ping, and parsing decimal rtt times in ms
// to insert into a sqlite database

package main

import (
	"bufio"
	"database/sql"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"encoding/json"
	"net/http"
)

// Log levels for filtering console output
type LogLevel int

const (
	LOG_SILENT LogLevel = 0 // No output
	LOG_ERROR  LogLevel = 1 // Only errors and critical events
	LOG_WARN   LogLevel = 2 // Warnings and above
	LOG_INFO   LogLevel = 3 // Important events (user actions, restarts)
	LOG_DEBUG  LogLevel = 4 // Detailed info (some API calls)
	LOG_TRACE  LogLevel = 5 // All output (everything)
)

var logLevelNames = map[LogLevel]string{
	LOG_SILENT: "SILENT",
	LOG_ERROR:  "ERROR",
	LOG_WARN:   "WARN",
	LOG_INFO:   "INFO",
	LOG_DEBUG:  "DEBUG",
	LOG_TRACE:  "TRACE",
}

// Terminal UI state
type TerminalUI struct {
	sync.RWMutex
	logLevel       LogLevel
	outputBuffer   []string
	maxLines       int
	currentInput   string
	cursorPos      int
	enabled        bool
	terminalMode   bool // Whether we're using the split terminal UI
	termHeight     int
	termWidth      int
	outputLines    int  // Number of lines for output area
	inputStartRow  int  // Row where input area starts
	needsRedraw    bool
}

var console = &TerminalUI{
	logLevel:     LOG_INFO, // Default to INFO level
	maxLines:     1000,     // Keep last 1000 lines
	enabled:      true,
	terminalMode: false, // Start in simple mode
}

type ISPState struct {
	ISPID                  int   `json:"ispid"`
	PowerState             bool  `json:"powerstate"`
	UxtimeWhenOffRequested int64 `json:"uxtimewhenoffrequested"`
	OffUntilUxtimesec      int64 `json:"offuntiluxtimesec"`
}

var ispPrimaryState bool = true
var ispSecondaryState bool = true

// ISP state cache for reducing database queries
type ISPStateCache struct {
	sync.RWMutex
	primaryState   ISPState
	secondaryState ISPState
	lastUpdated    time.Time
	initialized    bool
}

var ispStateCache = &ISPStateCache{}

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

// Channel for notifying GPIO logic of ISP state changes
var ispStateChangeNotifier = make(chan bool, 1)

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

var DOW = map[string]int{
	"monday":    1,
	"tuesday":   2,
	"wednesday": 3,
	"thursday":  4,
	"friday":    5,
	"saturday":  6,
	"sunday":    7,
}

type PingData struct {
	Untimesec  int64 `json:"untimesec"`
	Cloudflare int16 `json:"cloudflare"`
	Google     int16 `json:"google"`
	Facebook   int16 `json:"facebook"`
	X          int16 `json:"x"`
}

type Autorestart struct {
	ROWID       int `json:"rowid"`
	Uxtimesec   int `json:"uxtinesec"`
	Autorestart int `json:"autorestart"`
	Hour        int `json:"hour"`
	Min         int `json:"min"`
	Sec         int `json:"sec"`
	Daily       int `json:"daily"`
	Weekly      int `json:"weekly"`
	Monthly     int `json:"monthly"`
	Dayinweek   int `json:"dayinweek"`
	Weekinmonth int `json:"weekinmonth"`
}

type PingRequest struct {
	Rows int `json:"rows"`
}

type NetworkStatus struct {
	ActiveConnection string `json:"active_connection"`
	PublicIP         string `json:"public_ip"`
	Location         string `json:"location"`
	LastUpdated      int64  `json:"last_updated"`
}

type Log struct {
	Uxtimesec       int64  `json:"uxtimesec"`
	Reason          string `json:"reason"`
	IspID           *int   `json:"isp_id,omitempty"`
	IspName         *string `json:"isp_name,omitempty"`
	RestartType     *string `json:"restart_type,omitempty"`
	DurationMinutes *int   `json:"duration_minutes,omitempty"`
	ClientIP        *string `json:"client_ip,omitempty"`
}

type ServerResponse struct {
	Message string `json:"message"`
}

func main() {
	// Initialize console with startup info
	logInfo("ISP Monitor starting on %s/%s", runtime.GOOS, runtime.GOARCH)
	
	//connect to the sqlite db
	db, err := sql.Open("sqlite", "./isprestart.db")
	if err != nil {
		logError("Failed to open database: %v", err)
		log.Fatal(err)
	}
	defer db.Close()

	//configure database for concurrency
	err = configureDatabase(db)
	if err != nil {
		logError("Failed to configure database: %v", err)
		log.Fatal(err)
	}

	//create tables if they dont exist
	err = createTables(db)
	if err != nil {
		logError("Failed to create tables: %v", err)
		log.Fatal(err)
	}

	logInfo("Database initialized successfully")

	go startGPIOlogic(db)
	
	// Initialize terminal UI
	initTerminal()
	
	go startCommandInterface(db) // start command-line interface for testing

	pingChannel := make(chan map[string]int)
	go pingTest(pingChannel)             // start pinging
	go PrintPingResults(db, pingChannel) // save ping results to db
	go retriveSettings(db)               // start retrieving settings or save default values to db
	go periodicWALCheckpoint(db)         // periodic WAL checkpoint to prevent infinite growth

	http.HandleFunc("/api/ping-data", pingDataHandler(db))

	logInfo("Server listening on port 8081")
	logInfo("Command interface available - type 'help' for commands")
	logInfo("Default log level: %s (use 'loglevel <level>' to change)", logLevelNames[console.logLevel])
	http.ListenAndServe(":8081", nil)

	select {} // keep the program running
}

func startGPIOlogic(db *sql.DB) {
	// Initialize ISP states - check if table has data, if not create defaults
	err := initializeISPStates(db)
	if err != nil {
		logError("Error initializing ISP states: %v", err)
		log.Fatal(err)
	}

	// Load initial data into cache
	err = refreshISPStateCache(db)
	if err != nil {
		logError("Error loading initial ISP states into cache: %v", err)
		log.Fatal(err)
	}

	logInfo("GPIO logic started with cached ISP states")

	// Cache refresh interval (every 30 seconds for responsive updates)
	cacheRefreshInterval := 30 * time.Second

	// Create ticker for periodic checks
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Periodic check and refresh if stale
			if ispStateCache.isStale(cacheRefreshInterval) {
				err := refreshISPStateCache(db)
				if err != nil {
					fmt.Println("Error refreshing ISP state cache:", err)
				}
			}
			processGPIOLogic(db)
		}
	}
}

func processGPIOLogic(db *sql.DB) {
	// Get current states from cache (no database query)
	primary, secondary, initialized := ispStateCache.getStates()
	if !initialized {
		return
	}

	// Debug logging for ISP states (only when actions are needed)
	if (!primary.PowerState && primary.OffUntilUxtimesec == 0) || (!secondary.PowerState && secondary.OffUntilUxtimesec == 0) ||
	   (primary.PowerState && primary.UxtimeWhenOffRequested > 0) || (secondary.PowerState && secondary.UxtimeWhenOffRequested > 0) {
		fmt.Printf("[DEBUG] GPIO Processing - Primary: PowerState=%t, OffUntil=%d, OffRequested=%d, Secondary: PowerState=%t, OffUntil=%d, OffRequested=%d\n", 
			primary.PowerState, primary.OffUntilUxtimesec, primary.UxtimeWhenOffRequested, 
			secondary.PowerState, secondary.OffUntilUxtimesec, secondary.UxtimeWhenOffRequested)
	}

	// Reduced logging - only log when there are actual actions to take
	currentTime := time.Now().Unix()
	hasActions := false
	needsCacheRefresh := false

	// PRIMARY ISP LOGIC
	if !primary.PowerState { // Primary ISP is currently OFF

		if primary.OffUntilUxtimesec == 0 {
			// IMMEDIATE RESTART CASE - Turn on immediately
			logInfo("PRIMARY ISP: Immediate restart - turning ON")

			// GPIO: Turn primary ISP back ON (placeholder)
			// TODO: Replace with actual GPIO library calls
			// gpio.WritePin(18, gpio.High)  // Example GPIO pin 18
			logInfo("GPIO: Primary ISP pin set to HIGH (ON)")

			// Update database: ISP is back on, reset timers
			err := updateISPPowerState(db, 0, true, 0, 0)
			if err != nil {
				logError("Error updating primary ISP power state: %v", err)
			} else {
				logInfo("Primary ISP state updated in database: ON")
				logEvent(db, "[SYSTEM] Primary ISP restarted - immediate restart completed")
				// Update autorestart table with restart timestamp
				updateLastRestartTime(db, 0, time.Now().Unix())
				
				needsCacheRefresh = true
			}
			hasActions = true

		} else if currentTime >= primary.OffUntilUxtimesec {
			// TIMED RESTART CASE - Time has expired, turn back on
			logInfo("PRIMARY ISP: Timed restart complete - turning ON (was off until %d, now %d)", primary.OffUntilUxtimesec, currentTime)

			// GPIO: Turn primary ISP back ON (placeholder)
			// TODO: Replace with actual GPIO library calls
			// gpio.WritePin(18, gpio.High)  // Example GPIO pin 18
			logInfo("GPIO: Primary ISP pin set to HIGH (ON)")

			// Update database: ISP is back on, reset timers
			err := updateISPPowerState(db, 0, true, 0, 0)
			if err != nil {
				fmt.Printf("Error updating primary ISP power state: %v\n", err)
			} else {
				fmt.Printf("Primary ISP state updated in database: ON\n")
				logEvent(db, "[SYSTEM] Primary ISP powered on - scheduled restart completed")
				// Update autorestart table with restart timestamp
				updateLastRestartTime(db, 0, time.Now().Unix())
				
				needsCacheRefresh = true
			}
			hasActions = true

		} else {
			// TIMED RESTART CASE - Still waiting, keep ISP off
			// No action needed, just waiting for timer
		}

	} else if primary.PowerState && primary.UxtimeWhenOffRequested > 0 {
		// PRIMARY ISP WAS JUST REQUESTED TO BE TURNED OFF
		logInfo("PRIMARY ISP: Restart requested - turning OFF")

		// GPIO: Turn primary ISP OFF (placeholder)
		// TODO: Replace with actual GPIO library calls
		// gpio.WritePin(18, gpio.Low)  // Example GPIO pin 18
		logInfo("GPIO: Primary ISP pin set to LOW (OFF)")

		// Database update happens via cache refresh, no need to update here
		hasActions = true
		logEvent(db, "[SYSTEM] Primary ISP restart requested - turning off")
	}

	// If we need to refresh cache, do it now before processing secondary ISP
	if needsCacheRefresh {
		refreshISPStateCache(db)
		// Re-read the secondary state after cache refresh
		_, secondary, _ = ispStateCache.getStates()
		needsCacheRefresh = false
	}

	// SECONDARY ISP LOGIC (same pattern as primary)
	if !secondary.PowerState { // Secondary ISP is currently OFF

		if secondary.OffUntilUxtimesec == 0 {
			// IMMEDIATE RESTART CASE
			fmt.Printf("Secondary ISP immediate restart - turning ON (PowerState: %t, OffUntil: %d)\n", secondary.PowerState, secondary.OffUntilUxtimesec)

			// GPIO: Turn secondary ISP back ON (placeholder)
			// TODO: Replace with actual GPIO library calls
			// gpio.WritePin(19, gpio.High)  // Example GPIO pin 19
			fmt.Printf("GPIO: Secondary ISP pin set to HIGH (ON)\n")

			// Update database: ISP is back on, reset timers
			err := updateISPPowerState(db, 1, true, 0, 0)
			if err != nil {
				fmt.Printf("Error updating secondary ISP power state: %v\n", err)
			} else {
				fmt.Printf("Secondary ISP state updated in database: ON\n")
				logEvent(db, "[SYSTEM] Secondary ISP restarted - immediate restart completed")
				// Update autorestart table with restart timestamp
				updateLastRestartTime(db, 1, time.Now().Unix())
				
				needsCacheRefresh = true
			}
			hasActions = true

		} else if currentTime >= secondary.OffUntilUxtimesec {
			// TIMED RESTART CASE - Time has expired
			fmt.Printf("Secondary ISP timed restart - turning ON (off until %d, now %d)\n", secondary.OffUntilUxtimesec, currentTime)

			// GPIO: Turn secondary ISP back ON (placeholder)
			// TODO: Replace with actual GPIO library calls
			// gpio.WritePin(19, gpio.High)  // Example GPIO pin 19
			fmt.Printf("GPIO: Secondary ISP pin set to HIGH (ON)\n")

			// Update database: ISP is back on, reset timers
			err := updateISPPowerState(db, 1, true, 0, 0)
			if err != nil {
				fmt.Printf("Error updating secondary ISP power state: %v\n", err)
			} else {
				fmt.Printf("Secondary ISP state updated in database: ON\n")
				logEvent(db, "[SYSTEM] Secondary ISP powered on - scheduled restart completed")
				// Update autorestart table with restart timestamp
				updateLastRestartTime(db, 1, time.Now().Unix())
				
				needsCacheRefresh = true
			}
			hasActions = true
		}

	} else if secondary.PowerState && secondary.UxtimeWhenOffRequested > 0 {
		// SECONDARY ISP WAS JUST REQUESTED TO BE TURNED OFF
		fmt.Printf("Secondary ISP restart requested - turning OFF\n")

		// GPIO: Turn secondary ISP OFF (placeholder)
		// TODO: Replace with actual GPIO library calls
		// gpio.WritePin(19, gpio.Low)  // Example GPIO pin 19
		fmt.Printf("GPIO: Secondary ISP pin set to LOW (OFF)\n")

		hasActions = true
		logEvent(db, "[SYSTEM] Secondary ISP restart requested - turning off")
	}

	// Refresh cache at the end if needed
	if needsCacheRefresh {
		refreshISPStateCache(db)
	}

	// Only log states when there are actions
	if hasActions {
		logDebug("Primary ISP State: %+v", primary)
		logDebug("Secondary ISP State: %+v", secondary)
	}
}

// Helper function to notify GPIO logic of state changes
func notifyISPStateChange() {
	select {
	case ispStateChangeNotifier <- true:
		// Notification sent
	default:
		// Channel is full, skip (prevents blocking)
	}
}

// Database operation retry helper
func retryDatabaseOperation(operation func() error, maxRetries int, retryDelay time.Duration) error {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			fmt.Printf("Retrying database operation (attempt %d/%d)\n", attempt, maxRetries)
			time.Sleep(retryDelay)
		}

		lastErr = operation()
		if lastErr == nil {
			return nil // Success
		}

		// Check if this is a retryable error (busy/locked)
		if strings.Contains(lastErr.Error(), "database is locked") ||
			strings.Contains(lastErr.Error(), "database is busy") {
			if attempt < maxRetries {
				// Reduced logging to prevent spam
				if attempt == 1 {
					fmt.Printf("Database busy/locked, retrying... (error: %v)\n", lastErr)
				}
				continue
			}
		} else {
			// Non-retryable error, fail immediately
			return lastErr
		}
	}
	return fmt.Errorf("database operation failed after %d retries: %v", maxRetries, lastErr)
}

// Transaction helper with retry logic
func executeTransaction(db *sql.DB, operation func(*sql.Tx) error) error {
	return retryDatabaseOperation(func() error {
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("failed to begin transaction: %v", err)
		}
		defer tx.Rollback() // Rollback if not committed

		err = operation(tx)
		if err != nil {
			return err
		}

		err = tx.Commit()
		if err != nil {
			return fmt.Errorf("failed to commit transaction: %v", err)
		}

		return nil
	}, 3, 100*time.Millisecond)
}

func initializeISPStates(db *sql.DB) error {
	return retryDatabaseOperation(func() error {
		var count int
		err := db.QueryRow("SELECT COUNT(*) FROM ispstates").Scan(&count)
		if err != nil {
			return fmt.Errorf("error counting rows in ispstates table: %v", err)
		}

		if count == 0 {
			// Use transaction for inserting default values
			return executeTransaction(db, func(tx *sql.Tx) error {
				for isp := range 2 {
					fmt.Printf("Table is empty, inserting default values for isp %d\n", isp)
					stmt, err := tx.Prepare("INSERT INTO ispstates (ispid, powerstate, uxtimewhenoffrequested, offuntiluxtimesec) VALUES (?, ?, ?, ?)")
					if err != nil {
						return fmt.Errorf("error preparing statement for isp %d: %v", isp, err)
					}
					defer stmt.Close()

					_, err = stmt.Exec(isp, 1, 0, 0) // isp 0 is primary, 1 is secondary, power state is always on by default
					if err != nil {
						return fmt.Errorf("error inserting default values for isp %d: %v", isp, err)
					}
				}
				fmt.Println("Initialized ISP states with default values")
				return nil
			})
		}
		return nil
	}, 3, 200*time.Millisecond) // 3 retries with 200ms delay for initialization
}

func refreshISPStateCache(db *sql.DB) error {
	return retryDatabaseOperation(func() error {
		primaryISPState := ISPState{}
		secondaryISPState := ISPState{}

		// Query primary ISP state
		err := db.QueryRow("SELECT ispid, powerstate, uxtimewhenoffrequested, offuntiluxtimesec FROM ispstates WHERE ispid = 0").
			Scan(&primaryISPState.ISPID, &primaryISPState.PowerState, &primaryISPState.UxtimeWhenOffRequested, &primaryISPState.OffUntilUxtimesec)
		if err != nil {
			return fmt.Errorf("error retrieving primary ISP state: %v", err)
		}

		// Query secondary ISP state
		err = db.QueryRow("SELECT ispid, powerstate, uxtimewhenoffrequested, offuntiluxtimesec FROM ispstates WHERE ispid = 1").
			Scan(&secondaryISPState.ISPID, &secondaryISPState.PowerState, &secondaryISPState.UxtimeWhenOffRequested, &secondaryISPState.OffUntilUxtimesec)
		if err != nil {
			return fmt.Errorf("error retrieving secondary ISP state: %v", err)
		}

		// Update cache with retrieved values
		ispStateCache.updateStates(primaryISPState, secondaryISPState)

		return nil
	}, 3, 100*time.Millisecond) // 3 retries with 100ms delay
}

// Helper function to update ISP power state in database after GPIO actions
func updateISPPowerState(db *sql.DB, ispID int, powerState bool, uxtimeWhenOffRequested int64, offUntilUxtimesec int64) error {
	powerStateInt := 0
	if powerState {
		powerStateInt = 1
	}

	return retryDatabaseOperation(func() error {
		_, err := db.Exec(`
			UPDATE ispstates 
			SET powerstate = ?, 
			    uxtimewhenoffrequested = ?, 
			    offuntiluxtimesec = ? 
			WHERE ispid = ?`,
			powerStateInt, uxtimeWhenOffRequested, offUntilUxtimesec, ispID)
		return err
	}, 3, 100*time.Millisecond)
}

// Helper function to send error responses with CORS headers
func corsError(w http.ResponseWriter, message string, code int) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "text/plain")
	http.Error(w, message, code)
}

func pingDataHandler(db *sql.DB) func(http.ResponseWriter, *http.Request) { //im not sure if this is a bad idea
	return func(w http.ResponseWriter, r *http.Request) {
		// Log API requests at different levels based on type
		if r.URL.RawQuery != "" {
			// Frequent polling requests (ispstates, logs) at TRACE level
			if strings.Contains(r.URL.RawQuery, "ispstates") {
				logTrace("API: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			} else if strings.Contains(r.URL.RawQuery, "logs") || strings.Contains(r.URL.RawQuery, "networkstatus") {
				logDebug("API: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			} else {
				logDebug("API: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			}
		} else {
			logTrace("API: %s %s", r.Method, r.URL.Path)
		}
		var payload map[string]interface{}

		if r.Method == "OPTIONS" {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, OPTIONS") // Allow GET, HEAD, POST, and OPTIONS methods
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			return
		}

		if r.Method == "POST" {
			//fmt.Println("Received POST request with payload:", r.Body)
			err := json.NewDecoder(r.Body).Decode(&payload)
			if err != nil {
				corsError(w, err.Error(), http.StatusBadRequest)
				fmt.Println("Error decoding JSON payload:", err)
				fmt.Println("Payload:", payload)
				return
			}

			//fmt.Println("Received POST request with payload:", payload)

			_, autorestartFound := payload["autorestart"]
			_, rowsRequested := payload["Rows"]
			_, dailyRestart := payload["daily"]         // payload will be like {"hour": 0, "min": 0, "sec": 0 }
			_, weeklyRestart := payload["weekly"]       // payload will be like {"dayinweek": 1, "hour": 0, "min": 0, "sec": 0 }
			_, monthlyRestart := payload["monthly"]     // payload will be like {"weekinmonth": 1, "dayinweek": 1, "hour": 0, "min": 0, "sec": 0 }
			_, restartNowFound := payload["restartnow"] // payload will be like {"restartnow": 1, "isp_id": 0}
			_, restartForFound := payload["restartfor"] // payload will be like {"restartfor": 150, "isp_id": 0}

			if autorestartFound {
				// handle autorestart request here where the toggle button is turned off/on
				logInfo("USER ACTION: Autorestart toggle changed - payload: %v", payload)
				
				// Extract ISP ID from payload (default to primary ISP if not specified)
				ispID := 0
				if ispIDVal, ok := payload["isp_id"]; ok {
					if ispIDFloat, ok := ispIDVal.(float64); ok {
						ispID = int(ispIDFloat)
					}
				}
				
				// Extract autorestart value
				autorestartValue := 0
				if autorestartVal := payload["autorestart"]; autorestartVal != nil {
					switch v := autorestartVal.(type) {
					case bool:
						if v {
							autorestartValue = 1
						}
					case float64:
						autorestartValue = int(v)
					}
				}
				
				logInfo("Updating autorestart for ISP %d to %d", ispID, autorestartValue)
				
				// Update autorestart settings for the specified ISP
				err := retryDatabaseOperation(func() error {
					// First ensure the row exists for this ISP
					var count int
					err := db.QueryRow("SELECT COUNT(*) FROM autorestart WHERE isp_id = ?", ispID).Scan(&count)
					if err != nil {
						return err
					}
					
					var existingSettings Autorestart
					
					if count == 0 {
						// Row doesn't exist, create it first
						_, err := db.Exec(`
							INSERT INTO autorestart 
							(isp_id, uxtimesec, autorestart, hour, min, sec, daily, weekly, monthly, dayinweek, weekinmonth) 
							VALUES (?, ?, 0, 0, 0, 0, 0, 0, 0, 0, 0)`,
							ispID, time.Now().Unix())
						if err != nil {
							return fmt.Errorf("failed to create autorestart row for ISP %d: %v", ispID, err)
						}
						logInfo("Created autorestart row for ISP %d", ispID)
						
						// Set default values since we just created the row
						existingSettings.Hour = 0
						existingSettings.Min = 0
						existingSettings.Sec = 0
						existingSettings.Daily = 0
						existingSettings.Weekly = 0
						existingSettings.Monthly = 0
						existingSettings.Dayinweek = 0
						existingSettings.Weekinmonth = 0
					} else {
						// Row exists, read current settings
						err := db.QueryRow("SELECT hour, min, sec, daily, weekly, monthly, dayinweek, weekinmonth FROM autorestart WHERE isp_id = ?", ispID).
							Scan(&existingSettings.Hour, &existingSettings.Min, &existingSettings.Sec, &existingSettings.Daily, &existingSettings.Weekly, &existingSettings.Monthly, &existingSettings.Dayinweek, &existingSettings.Weekinmonth)
						if err != nil {
							return fmt.Errorf("failed to read existing settings for ISP %d: %v", ispID, err)
						}
					}
					
					if autorestartValue == 0 {
						// When turning OFF autorestart, clear all schedule settings
						_, err := db.Exec(`
							UPDATE autorestart 
							SET uxtimesec = ?, autorestart = 0, hour = 0, min = 0, sec = 0, 
							    daily = 0, weekly = 0, monthly = 0, dayinweek = 0, weekinmonth = 0
							WHERE isp_id = ?`,
							time.Now().Unix(), ispID)
						return err
					} else {
						// When turning ON autorestart, preserve existing schedule settings
						// If no schedule is configured, set default to daily at midnight
						if existingSettings.Daily == 0 && existingSettings.Weekly == 0 && existingSettings.Monthly == 0 {
							existingSettings.Daily = 1
							existingSettings.Hour = 0
							existingSettings.Min = 0
							existingSettings.Sec = 0
							logInfo("No schedule configured for ISP %d, setting default daily at midnight", ispID)
						}
						
						// Update existing row, preserve/set schedule settings
						_, err = db.Exec(`
							UPDATE autorestart 
							SET uxtimesec = ?, autorestart = 1, hour = ?, min = ?, sec = ?, 
							    daily = ?, weekly = ?, monthly = ?, dayinweek = ?, weekinmonth = ?
							WHERE isp_id = ?`,
							time.Now().Unix(), existingSettings.Hour, existingSettings.Min, existingSettings.Sec, 
							existingSettings.Daily, existingSettings.Weekly, existingSettings.Monthly, 
							existingSettings.Dayinweek, existingSettings.Weekinmonth, ispID)
						return err
					}
					
					// Log the enable/disable action after successful database update
					if err == nil {
						clientIP := getClientIP(r)
						ispName := getISPName(ispID)
						var logReason string
						
						if autorestartValue == 0 {
							logReason = "[USER ACTION] Auto-restart disabled"
						} else {
							// Generate human-readable schedule text for current settings
							var scheduleType string
							if existingSettings.Daily == 1 {
								scheduleType = "daily"
							} else if existingSettings.Weekly == 1 {
								scheduleType = "weekly"
							} else if existingSettings.Monthly == 1 {
								scheduleType = "monthly"
							}
							scheduleText := generateScheduleText(scheduleType, existingSettings.Dayinweek, existingSettings.Hour, existingSettings.Min, existingSettings.Weekinmonth)
							logReason = fmt.Sprintf("[USER ACTION] Auto-restart enabled - %s", scheduleText)
						}
						
						logErr := logISPRestartAction(db, logReason, ispID, ispName, "autorestart_toggle", 0, clientIP)
						if logErr != nil {
							fmt.Printf("[LOG ERROR] Failed to log autorestart toggle: %v\n", logErr)
						}
					}
					return err
				}, 3, 100*time.Millisecond)
				
				if err != nil {
					logError("Failed to update autorestart settings for ISP %d: %v", ispID, err)
					http.Error(w, fmt.Sprintf("Database error: %v", err), http.StatusInternalServerError)
					return
				}
				
				logInfo("Successfully updated autorestart for ISP %d to %d", ispID, autorestartValue)
				
				// Verify the update was successful by querying the database
				var verifyAutorestart int
				err = db.QueryRow("SELECT autorestart FROM autorestart WHERE isp_id = ?", ispID).Scan(&verifyAutorestart)
				if err != nil {
					logError("Failed to verify autorestart update for ISP %d: %v", ispID, err)
				} else {
					logInfo("Database verification: ISP %d autorestart is now %d", ispID, verifyAutorestart)
				}
				
				w.Header().Set("Access-Control-Allow-Origin", "*") // Include this header for all responses
				w.Header().Set("Content-Type", "application/json") // Include this header for JSON responses
				w.WriteHeader(http.StatusOK)

				fmt.Fprint(w, "Autorestart updated successfully")
			} else if dailyRestart {
				// handle daily restart request here
				logInfo("USER ACTION: Daily restart schedule updated - payload: %v", payload)
				values := payload["daily"].([]interface{})

				// Extract ISP ID from payload (default to primary ISP if not specified)
				ispID := 0
				if ispIDVal, ok := payload["isp_id"]; ok {
					if ispIDFloat, ok := ispIDVal.(float64); ok {
						ispID = int(ispIDFloat)
					}
				}

				// Ensure you handle the conversion from interface{} to appropriate types.
				hour, _ := values[0].(float64)
				minute, _ := values[1].(float64)
				second, _ := values[2].(float64)

				err := retryDatabaseOperation(func() error {
					// First ensure the row exists for this ISP
					var count int
					err := db.QueryRow("SELECT COUNT(*) FROM autorestart WHERE isp_id = ?", ispID).Scan(&count)
					if err != nil {
						return err
					}
					
					if count == 0 {
						// Row doesn't exist, create it first
						_, err := db.Exec(`
							INSERT INTO autorestart 
							(isp_id, uxtimesec, autorestart, hour, min, sec, daily, weekly, monthly, dayinweek, weekinmonth) 
							VALUES (?, ?, 1, ?, ?, ?, 1, 0, 0, 0, 0)`,
							ispID, time.Now().Unix(), int(hour), int(minute), int(second))
						if err != nil {
							return fmt.Errorf("failed to create autorestart row for ISP %d: %v", ispID, err)
						}
						logInfo("Created daily autorestart schedule for ISP %d at %02d:%02d:%02d", ispID, int(hour), int(minute), int(second))
					} else {
						// Row exists, update it
						_, err := db.Exec(`
							UPDATE autorestart 
							SET uxtimesec = ?, autorestart = 1, hour = ?, min = ?, sec = ?, 
							    daily = 1, weekly = 0, monthly = 0, dayinweek = 0, weekinmonth = 0
							WHERE isp_id = ?`,
							time.Now().Unix(), int(hour), int(minute), int(second), ispID)
						if err != nil {
							return fmt.Errorf("failed to update daily schedule for ISP %d: %v", ispID, err)
						}
						logInfo("Updated daily autorestart schedule for ISP %d at %02d:%02d:%02d", ispID, int(hour), int(minute), int(second))
					}
					return nil
				}, 3, 100*time.Millisecond)

				if err != nil {
					logError("Failed to update daily restart schedule for ISP %d: %v", ispID, err)
					http.Error(w, fmt.Sprintf("Database error: %v", err), http.StatusInternalServerError)
					return
				}

				// Log the custom schedule action
				clientIP := getClientIP(r)
				ispName := getISPName(ispID)
				scheduleText := generateScheduleText("daily", 0, int(hour), int(minute), 0)
				logReason := fmt.Sprintf("[USER ACTION] Auto-restart schedule set for %s - %s", ispName, scheduleText)
				logErr := logISPRestartAction(db, logReason, ispID, ispName, "schedule_set", 0, clientIP)
				if logErr != nil {
					fmt.Printf("[LOG ERROR] Failed to log schedule action: %v\n", logErr)
				}

				w.Header().Set("Access-Control-Allow-Origin", "*") // Include this header for all responses
				w.Header().Set("Content-Type", "application/json") // Include this header for JSON responses
				w.WriteHeader(http.StatusOK)

				fmt.Fprint(w, "Daily restart updated successfully")

			} else if weeklyRestart {
				// handle weekly restart request here
				logInfo("USER ACTION: Weekly restart schedule updated - payload: %v", payload)
				values := payload["weekly"].([]interface{})

				// Extract ISP ID from payload (default to primary ISP if not specified)
				ispID := 0
				if ispIDVal, ok := payload["isp_id"]; ok {
					if ispIDFloat, ok := ispIDVal.(float64); ok {
						ispID = int(ispIDFloat)
					}
				}

				// Ensure you handle the conversion from interface{} to appropriate types.
				dayinweek, _ := values[0].(float64)
				hour, _ := values[1].(float64)
				minute, _ := values[2].(float64)
				second, _ := values[3].(float64)

				err := retryDatabaseOperation(func() error {
					// First ensure the row exists for this ISP
					var count int
					err := db.QueryRow("SELECT COUNT(*) FROM autorestart WHERE isp_id = ?", ispID).Scan(&count)
					if err != nil {
						return err
					}
					
					if count == 0 {
						// Row doesn't exist, create it first
						_, err := db.Exec(`
							INSERT INTO autorestart 
							(isp_id, uxtimesec, autorestart, hour, min, sec, daily, weekly, monthly, dayinweek, weekinmonth) 
							VALUES (?, ?, 1, ?, ?, ?, 0, 1, 0, ?, 0)`,
							ispID, time.Now().Unix(), int(hour), int(minute), int(second), int(dayinweek))
						if err != nil {
							return fmt.Errorf("failed to create autorestart row for ISP %d: %v", ispID, err)
						}
						logInfo("Created weekly autorestart schedule for ISP %d: day %d at %02d:%02d:%02d", ispID, int(dayinweek), int(hour), int(minute), int(second))
					} else {
						// Row exists, update it
						_, err := db.Exec(`
							UPDATE autorestart 
							SET uxtimesec = ?, autorestart = 1, hour = ?, min = ?, sec = ?, 
							    daily = 0, weekly = 1, monthly = 0, dayinweek = ?, weekinmonth = 0
							WHERE isp_id = ?`,
							time.Now().Unix(), int(hour), int(minute), int(second), int(dayinweek), ispID)
						if err != nil {
							return fmt.Errorf("failed to update weekly schedule for ISP %d: %v", ispID, err)
						}
						logInfo("Updated weekly autorestart schedule for ISP %d: day %d at %02d:%02d:%02d", ispID, int(dayinweek), int(hour), int(minute), int(second))
					}
					return nil
				}, 3, 100*time.Millisecond)

				if err != nil {
					logError("Failed to update weekly restart schedule for ISP %d: %v", ispID, err)
					http.Error(w, fmt.Sprintf("Database error: %v", err), http.StatusInternalServerError)
					return
				}

				// Log the custom schedule action
				clientIP := getClientIP(r)
				ispName := getISPName(ispID)
				scheduleText := generateScheduleText("weekly", int(dayinweek), int(hour), int(minute), 0)
				logReason := fmt.Sprintf("[USER ACTION] Auto-restart schedule set for %s - %s", ispName, scheduleText)
				logErr := logISPRestartAction(db, logReason, ispID, ispName, "schedule_set", 0, clientIP)
				if logErr != nil {
					fmt.Printf("[LOG ERROR] Failed to log schedule action: %v\n", logErr)
				}

				w.Header().Set("Access-Control-Allow-Origin", "*") // Include this header for all responses
				w.Header().Set("Content-Type", "application/json") // Include this header for JSON responses
				w.WriteHeader(http.StatusOK)

				fmt.Fprint(w, "Weekly restart updated successfully")

			} else if monthlyRestart {
				// handle monthly restart request here
				logInfo("USER ACTION: Monthly restart schedule updated - payload: %v", payload)
				values := payload["monthly"].([]interface{})

				// Extract ISP ID from payload (default to primary ISP if not specified)
				ispID := 0
				if ispIDVal, ok := payload["isp_id"]; ok {
					if ispIDFloat, ok := ispIDVal.(float64); ok {
						ispID = int(ispIDFloat)
					}
				}

				// Ensure you handle the conversion from interface{} to appropriate types.
				weekinmonth, _ := values[0].(float64)
				dayinweek, _ := values[1].(float64)
				hour, _ := values[2].(float64)
				minute, _ := values[3].(float64)
				second, _ := values[4].(float64)

				err := retryDatabaseOperation(func() error {
					// First ensure the row exists for this ISP
					var count int
					err := db.QueryRow("SELECT COUNT(*) FROM autorestart WHERE isp_id = ?", ispID).Scan(&count)
					if err != nil {
						return err
					}
					
					if count == 0 {
						// Row doesn't exist, create it first
						_, err := db.Exec(`
							INSERT INTO autorestart 
							(isp_id, uxtimesec, autorestart, hour, min, sec, daily, weekly, monthly, dayinweek, weekinmonth) 
							VALUES (?, ?, 1, ?, ?, ?, 0, 0, 1, ?, ?)`,
							ispID, time.Now().Unix(), int(hour), int(minute), int(second), int(dayinweek), int(weekinmonth))
						if err != nil {
							return fmt.Errorf("failed to create autorestart row for ISP %d: %v", ispID, err)
						}
						logInfo("Created monthly autorestart schedule for ISP %d: week %d, day %d at %02d:%02d:%02d", ispID, int(weekinmonth), int(dayinweek), int(hour), int(minute), int(second))
					} else {
						// Row exists, update it
						_, err := db.Exec(`
							UPDATE autorestart 
							SET uxtimesec = ?, autorestart = 1, hour = ?, min = ?, sec = ?, 
							    daily = 0, weekly = 0, monthly = 1, dayinweek = ?, weekinmonth = ?
							WHERE isp_id = ?`,
							time.Now().Unix(), int(hour), int(minute), int(second), int(dayinweek), int(weekinmonth), ispID)
						if err != nil {
							return fmt.Errorf("failed to update monthly schedule for ISP %d: %v", ispID, err)
						}
						logInfo("Updated monthly autorestart schedule for ISP %d: week %d, day %d at %02d:%02d:%02d", ispID, int(weekinmonth), int(dayinweek), int(hour), int(minute), int(second))
					}
					return nil
				}, 3, 100*time.Millisecond)

				if err != nil {
					logError("Failed to update monthly restart schedule for ISP %d: %v", ispID, err)
					http.Error(w, fmt.Sprintf("Database error: %v", err), http.StatusInternalServerError)
					return
				}

				// Log the custom schedule action
				clientIP := getClientIP(r)
				ispName := getISPName(ispID)
				scheduleText := generateScheduleText("monthly", int(dayinweek), int(hour), int(minute), int(weekinmonth))
				logReason := fmt.Sprintf("[USER ACTION] Auto-restart schedule set for %s - %s", ispName, scheduleText)
				logErr := logISPRestartAction(db, logReason, ispID, ispName, "schedule_set", 0, clientIP)
				if logErr != nil {
					fmt.Printf("[LOG ERROR] Failed to log schedule action: %v\n", logErr)
				}

				w.Header().Set("Access-Control-Allow-Origin", "*") // Include this header for all responses
				w.Header().Set("Content-Type", "application/json") // Include this header for JSON responses
				w.WriteHeader(http.StatusOK)

				fmt.Fprint(w, "Monthly restart updated successfully")

			} else if rowsRequested {
				///////////////////////////////////////////
				requestedRows := int(payload["Rows"].(float64))
				logDebug("Querying ping data - requesting %d records", requestedRows)
				
				rows, err := db.Query("SELECT uxtimesec, cloudflare, google, facebook, x FROM pings ORDER BY uxtimesec DESC LIMIT ?", payload["Rows"])
				if err != nil {
					logError("Failed to query ping data: %v", err)
					corsError(w, err.Error(), http.StatusInternalServerError)
					return
				}
				defer rows.Close()

				pingResults := make([]PingData, 0)

				for rows.Next() {
					var data PingData
					err = rows.Scan(&data.Untimesec, &data.Cloudflare, &data.Google, &data.Facebook, &data.X)
					if err != nil {
						logError("Failed to scan ping row: %v", err)
						corsError(w, err.Error(), http.StatusInternalServerError)
						return
					}
					pingResults = append(pingResults, data)
				}
				
				logTrace("Retrieved %d ping records (latest: %d, oldest: %d)", len(pingResults), 
					func() int64 { if len(pingResults) > 0 { return pingResults[0].Untimesec } else { return 0 } }(),
					func() int64 { if len(pingResults) > 0 { return pingResults[len(pingResults)-1].Untimesec } else { return 0 } }())

				// Query restart times for both ISPs
				logTrace("Querying ISP restart times")
				var primaryLastRestart, secondaryLastRestart int64

				err = db.QueryRow("SELECT last_restart_uxtimesec FROM isp_restart_times WHERE isp_id = 0").Scan(&primaryLastRestart)
				if err != nil {
					logDebug("Failed to query primary ISP last restart time: %v", err)
					primaryLastRestart = 0
				} else {
					logTrace("Primary ISP last restart time: %d", primaryLastRestart)
				}

				err = db.QueryRow("SELECT last_restart_uxtimesec FROM isp_restart_times WHERE isp_id = 1").Scan(&secondaryLastRestart)
				if err != nil {
					logDebug("Failed to query secondary ISP last restart time: %v", err)
					secondaryLastRestart = 0
				} else {
					logTrace("Secondary ISP last restart time: %d", secondaryLastRestart)
				}

				// Create response with ping data and restart times
				response := map[string]interface{}{
					"ping_data": pingResults,
					"restart_times": map[string]int64{
						"primary": primaryLastRestart,
						"secondary": secondaryLastRestart,
					},
				}
				/////////////////////////////////

				w.Header().Set("Access-Control-Allow-Origin", "*") // Include this header for all responses
				w.Header().Set("Content-Type", "application/json") // Include this header for JSON responses

				json.NewEncoder(w).Encode(response)
			} else if restartNowFound {
				// Handle immediate ISP restart request  
				logInfo("USER ACTION: Immediate ISP restart requested - payload: %v", payload)

				// Extract ISP ID from payload
				ispID, ok := payload["isp_id"].(float64)
				if !ok {
					http.Error(w, "Invalid isp_id format", http.StatusBadRequest)
					return
				}

				// Get client IP address and ISP name for logging
				clientIP := getClientIP(r)
				ispName := getISPName(int(ispID))

				currentTime := time.Now().Unix()
				logInfo("Updating ISP %d for immediate restart - setting powerstate=0, uxtimewhenoffrequested=%d", int(ispID), currentTime)

				// Update database: set powerstate=0, uxtimewhenoffrequested=now, offuntiluxtimesec=0
				err := retryDatabaseOperation(func() error {
					_, err := db.Exec(`
						UPDATE ispstates 
						SET powerstate = 0, 
						    uxtimewhenoffrequested = ?, 
						    offuntiluxtimesec = 0 
						WHERE ispid = ?`,
						currentTime, int(ispID))
					return err
				}, 3, 100*time.Millisecond)

				if err != nil {
					fmt.Printf("[DATABASE ERROR] Failed to update ISP %d for restart: %v\n", int(ispID), err)
					http.Error(w, fmt.Sprintf("Database error: %v", err), http.StatusInternalServerError)
					return
				}

				// Log the custom restart action
				logReason := fmt.Sprintf("[USER ACTION] ISP restart requested by user from %s", clientIP)
				logErr := logISPRestartAction(db, logReason, int(ispID), ispName, "restart_now", 0, clientIP)
				if logErr != nil {
					fmt.Printf("[LOG ERROR] Failed to log ISP restart action: %v\n", logErr)
				}

				fmt.Printf("[DATABASE] Successfully updated ISP %d (%s) for immediate restart by %s\n", int(ispID), ispName, clientIP)

				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, "ISP restart now request processed successfully")

			} else if restartForFound {
				// Handle timed ISP restart request
				fmt.Println("Received restart for request with payload:", payload)

				// Extract ISP ID and duration from payload
				ispID, ok := payload["isp_id"].(float64)
				if !ok {
					http.Error(w, "Invalid isp_id format", http.StatusBadRequest)
					return
				}

				durationMinutes, ok := payload["restartfor"].(float64)
				if !ok {
					http.Error(w, "Invalid restartfor format", http.StatusBadRequest)
					return
				}

				// Get client IP address and ISP name for logging
				clientIP := getClientIP(r)
				ispName := getISPName(int(ispID))

				currentTime := time.Now().Unix()
				offUntilTime := currentTime + (int64(durationMinutes) * 60) // Convert minutes to seconds
				
				fmt.Printf("[DATABASE] Updating ISP %d for timed restart - off for %.0f minutes (until timestamp %d)\n", int(ispID), durationMinutes, offUntilTime)

				// Update database: set powerstate=0, times accordingly
				err := retryDatabaseOperation(func() error {
					_, err := db.Exec(`
						UPDATE ispstates 
						SET powerstate = 0, 
						    uxtimewhenoffrequested = ?, 
						    offuntiluxtimesec = ? 
						WHERE ispid = ?`,
						currentTime, offUntilTime, int(ispID))
					return err
				}, 3, 100*time.Millisecond)

				if err != nil {
					fmt.Printf("[DATABASE ERROR] Failed to update ISP %d for timed restart: %v\n", int(ispID), err)
					http.Error(w, fmt.Sprintf("Database error: %v", err), http.StatusInternalServerError)
					return
				}

				// Log the custom restart action
				logReason := fmt.Sprintf("[USER ACTION] ISP timed restart (%.0f minutes) requested by user from %s", durationMinutes, clientIP)
				logErr := logISPRestartAction(db, logReason, int(ispID), ispName, "restart_for_duration", int(durationMinutes), clientIP)
				if logErr != nil {
					fmt.Printf("[LOG ERROR] Failed to log ISP restart action: %v\n", logErr)
				}

				fmt.Printf("[DATABASE] Successfully updated ISP %d (%s) for timed restart (%.0f minutes) by %s\n", int(ispID), ispName, durationMinutes, clientIP)

				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusOK)
				fmt.Fprintf(w, "ISP restart for %.0f minutes request processed successfully", durationMinutes)
			}
		}

		if r.Method == "GET" {

			//fmt.Println("Received GET request with payload:", payload)

			params := r.URL.Query()
			//fmt.Println("Received autorestart query:", params)

			autorestartQuery := params.Get("pagestate")
			ispStatesQuery := params.Get("ispstates")
			networkStatusQuery := params.Get("networkstatus")
			logsQuery := params.Get("logs")

			if autorestartQuery != "" {
				// Handle get request for button state request data for both ISPs
				fmt.Printf("[DATABASE] Querying autorestart settings from autorestart table for both ISPs\n")
				
				var primaryAutorestart, secondaryAutorestart Autorestart
				
				// Query primary ISP (isp_id 0)
				err := db.QueryRow("SELECT isp_id, uxtimesec, autorestart, hour, min, sec, daily, weekly, monthly, dayinweek, weekinmonth FROM autorestart WHERE isp_id = 0").
					Scan(&primaryAutorestart.ROWID, &primaryAutorestart.Uxtimesec, &primaryAutorestart.Autorestart, &primaryAutorestart.Hour, &primaryAutorestart.Min, &primaryAutorestart.Sec, &primaryAutorestart.Daily, &primaryAutorestart.Weekly, &primaryAutorestart.Monthly, &primaryAutorestart.Dayinweek, &primaryAutorestart.Weekinmonth)
				if err != nil {
					if err == sql.ErrNoRows {
						fmt.Printf("[DATABASE WARNING] Primary ISP autorestart settings (isp_id=0) not found, creating defaults\n")
						primaryAutorestart = Autorestart{ROWID: 0, Autorestart: 0}
					} else {
						fmt.Printf("[DATABASE ERROR] Failed to query primary autorestart settings: %v\n", err)
						http.Error(w, fmt.Sprintf("Error fetching primary autorestart: %v", err), http.StatusInternalServerError)
						return
					}
				}
				
				// Query secondary ISP (isp_id 1)
				err = db.QueryRow("SELECT isp_id, uxtimesec, autorestart, hour, min, sec, daily, weekly, monthly, dayinweek, weekinmonth FROM autorestart WHERE isp_id = 1").
					Scan(&secondaryAutorestart.ROWID, &secondaryAutorestart.Uxtimesec, &secondaryAutorestart.Autorestart, &secondaryAutorestart.Hour, &secondaryAutorestart.Min, &secondaryAutorestart.Sec, &secondaryAutorestart.Daily, &secondaryAutorestart.Weekly, &secondaryAutorestart.Monthly, &secondaryAutorestart.Dayinweek, &secondaryAutorestart.Weekinmonth)
				if err != nil {
					if err == sql.ErrNoRows {
						fmt.Printf("[DATABASE WARNING] Secondary ISP autorestart settings (isp_id=1) not found, creating defaults\n")
						secondaryAutorestart = Autorestart{ROWID: 1, Autorestart: 0}
					} else {
						fmt.Printf("[DATABASE ERROR] Failed to query secondary autorestart settings: %v\n", err)
						http.Error(w, fmt.Sprintf("Error fetching secondary autorestart: %v", err), http.StatusInternalServerError)
						return
					}
				}
				
				fmt.Printf("[DATABASE] Successfully retrieved autorestart settings - Primary: enabled=%d, Secondary: enabled=%d\n", primaryAutorestart.Autorestart, secondaryAutorestart.Autorestart)
				
				// Return both ISP settings
				response := map[string]interface{}{
					"primary": primaryAutorestart,
					"secondary": secondaryAutorestart,
				}
				
				w.Header().Set("Access-Control-Allow-Origin", "*") // Include this header for all responses
				w.Header().Set("Content-Type", "application/json") // Include this header for JSON responses
				json.NewEncoder(w).Encode(response)
			} else if ispStatesQuery != "" {
				// Handle ISP states request (most frequent - only log at TRACE level)
				logTrace("Querying ISP states")
				
				var primaryState, secondaryState ISPState
				
				err := db.QueryRow("SELECT ispid, powerstate, uxtimewhenoffrequested, offuntiluxtimesec FROM ispstates WHERE ispid = 0").
					Scan(&primaryState.ISPID, &primaryState.PowerState, &primaryState.UxtimeWhenOffRequested, &primaryState.OffUntilUxtimesec)
				if err != nil {
					if err == sql.ErrNoRows {
						fmt.Printf("[DATABASE WARNING] Primary ISP (id=0) not found in database\n")
						// Create default primary state
						primaryState = ISPState{
							ISPID: 0,
							PowerState: true,
							UxtimeWhenOffRequested: 0,
							OffUntilUxtimesec: 0,
						}
					} else {
						fmt.Printf("[DATABASE ERROR] Failed to query primary ISP state: %v\n", err)
						http.Error(w, fmt.Sprintf("Error fetching primary ISP state: %v", err), http.StatusInternalServerError)
						return
					}
				}
				
				err = db.QueryRow("SELECT ispid, powerstate, uxtimewhenoffrequested, offuntiluxtimesec FROM ispstates WHERE ispid = 1").
					Scan(&secondaryState.ISPID, &secondaryState.PowerState, &secondaryState.UxtimeWhenOffRequested, &secondaryState.OffUntilUxtimesec)
				if err != nil {
					if err == sql.ErrNoRows {
						fmt.Printf("[DATABASE WARNING] Secondary ISP (id=1) not found in database\n")
						// Create default secondary state
						secondaryState = ISPState{
							ISPID: 1,
							PowerState: true,
							UxtimeWhenOffRequested: 0,
							OffUntilUxtimesec: 0,
						}
					} else {
						fmt.Printf("[DATABASE ERROR] Failed to query secondary ISP state: %v\n", err)
						http.Error(w, fmt.Sprintf("Error fetching secondary ISP state: %v", err), http.StatusInternalServerError)
						return
					}
				}
				
				logTrace("Retrieved ISP states - Primary: power=%t, Secondary: power=%t", primaryState.PowerState, secondaryState.PowerState)
				
				response := map[string]interface{}{
					"primary": primaryState,
					"secondary": secondaryState,
				}
				
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(response)
			} else if networkStatusQuery != "" {
				// Handle network status request
				fmt.Printf("[DATABASE] Querying network status from network_status table\n")
				
				var status NetworkStatus
				
				err := db.QueryRow("SELECT active_connection, public_ip, location, last_updated FROM network_status ORDER BY id DESC LIMIT 1").
					Scan(&status.ActiveConnection, &status.PublicIP, &status.Location, &status.LastUpdated)
				if err != nil {
					if err == sql.ErrNoRows {
						fmt.Printf("[DATABASE] No network status data found, using defaults\n")
						// Return default values if no data
						status = NetworkStatus{
							ActiveConnection: "Unknown",
							PublicIP:         "Unknown",
							Location:         "Unknown",
							LastUpdated:      time.Now().Unix(),
						}
					} else {
						fmt.Printf("[DATABASE ERROR] Failed to query network status: %v\n", err)
						http.Error(w, fmt.Sprintf("Error fetching network status: %v", err), http.StatusInternalServerError)
						return
					}
				} else {
					fmt.Printf("[DATABASE] Successfully retrieved network status: %s at %s\n", status.ActiveConnection, status.PublicIP)
				}
				
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(status)
			} else if logsQuery != "" {
				// Handle logs request
				limit, err := strconv.Atoi(logsQuery)
				if err != nil || limit <= 0 {
					limit = 10 // Default to 10 logs
				}
				
				fmt.Printf("[DATABASE] Querying activity logs from logs table - Requesting %d most recent entries\n", limit)
				
				rows, err := db.Query("SELECT uxtimesec, reason, isp_id, isp_name, restart_type, duration_minutes, client_ip FROM logs ORDER BY uxtimesec DESC LIMIT ?", limit)
				if err != nil {
					fmt.Printf("[DATABASE ERROR] Failed to query logs: %v\n", err)
					http.Error(w, fmt.Sprintf("Error fetching logs: %v", err), http.StatusInternalServerError)
					return
				}
				defer rows.Close()
				
				logs := make([]Log, 0)
				for rows.Next() {
					var log Log
					var ispID sql.NullInt64
					var ispName sql.NullString
					var restartType sql.NullString
					var durationMinutes sql.NullInt64
					var clientIP sql.NullString
					
					err = rows.Scan(&log.Uxtimesec, &log.Reason, &ispID, &ispName, &restartType, &durationMinutes, &clientIP)
					if err != nil {
						fmt.Printf("[DATABASE ERROR] Failed to scan log row: %v\n", err)
						continue
					}
					
					// Convert nullable fields to pointers
					if ispID.Valid {
						val := int(ispID.Int64)
						log.IspID = &val
					}
					if ispName.Valid {
						log.IspName = &ispName.String
					}
					if restartType.Valid {
						log.RestartType = &restartType.String
					}
					if durationMinutes.Valid {
						val := int(durationMinutes.Int64)
						log.DurationMinutes = &val
					}
					if clientIP.Valid {
						log.ClientIP = &clientIP.String
					}
					
					logs = append(logs, log)
				}
				
				fmt.Printf("[DATABASE] Successfully retrieved %d activity log entries\n", len(logs))
				
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(logs)
			}

		}
	}
}

func configureDatabase(db *sql.DB) error {
	// Enable WAL mode for better concurrency (allows concurrent readers)
	_, err := db.Exec("PRAGMA journal_mode=WAL")
	if err != nil {
		return fmt.Errorf("failed to enable WAL mode: %v", err)
	}

	// Set busy timeout to handle lock contention (reduced from 5s to 1s)
	_, err = db.Exec("PRAGMA busy_timeout=1000")
	if err != nil {
		return fmt.Errorf("failed to set busy timeout: %v", err)
	}

	// Optimize WAL mode settings
	_, err = db.Exec("PRAGMA synchronous=NORMAL") // Faster writes
	if err != nil {
		return fmt.Errorf("failed to set synchronous mode: %v", err)
	}

	// Set WAL auto-checkpoint to prevent infinite growth
	_, err = db.Exec("PRAGMA wal_autocheckpoint=1000") // Checkpoint every 1000 pages
	if err != nil {
		return fmt.Errorf("failed to set WAL autocheckpoint: %v", err)
	}

	// Configure connection pooling for optimal concurrency (reduced for less overhead)
	db.SetMaxOpenConns(5)                   // Reduced from 10 to 5
	db.SetMaxIdleConns(2)                   // Reduced from 5 to 2
	db.SetConnMaxLifetime(30 * time.Minute) // Reduced from 1 hour

	fmt.Println("Database configured for concurrency with optimized WAL mode")
	return nil
}

// periodicWALCheckpoint runs a WAL checkpoint every 5 minutes to prevent infinite WAL growth
func periodicWALCheckpoint(db *sql.DB) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	
	for range ticker.C {
		// Perform manual WAL checkpoint
		_, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
		if err != nil {
			logError("Failed to perform WAL checkpoint: %v", err)
		} else {
			logTrace("WAL checkpoint completed successfully")
		}
	}
}

func createTables(db *sql.DB) error {
	_, err := db.Exec(`
        CREATE TABLE IF NOT EXISTS pings (
            uxtimesec INTEGER PRIMARY KEY,
            cloudflare SMALLINT NOT NULL,
            google SMALLINT NOT NULL,
	    	facebook SMALLINT NOT NULL,
	    	x SMALLINT NOT NULL
        );
    `)

	_, err1 := db.Exec(`
        CREATE TABLE IF NOT EXISTS autorestart (
            isp_id TINYINT PRIMARY KEY,
            uxtimesec INTEGER NOT NULL,
			autorestart TINYINT NOT NULL,
			hour TINYINT NOT NULL,
			min TINYINT NOT NULL,
			sec TINYINT NOT NULL,
			daily TINYINT NOT NULL,
            weekly TINYINT NOT NULL,
	    	monthly TINYINT NOT NULL,
	    	dayinweek TINYINT NOT NULL,
			weekinmonth TINYINT NOT NULL
        );
    `)

	_, err2 := db.Exec(`
        CREATE TABLE IF NOT EXISTS logs (
            uxtimesec INTEGER PRIMARY KEY,
            reason TEXT NOT NULL,
            isp_id INTEGER,
            isp_name TEXT,
            restart_type TEXT,
            duration_minutes INTEGER,
            client_ip TEXT
        );
    `)

	//isp 0 is primary, isp 1 is secondary
	_, err3 := db.Exec(`
		CREATE TABLE IF NOT EXISTS ispstates (
		    ispid TINYINT PRIMARY KEY,
		    powerstate TINYINT NOT NULL,
		    uxtimewhenoffrequested INTEGER NOT NULL,
		    offuntiluxtimesec INTEGER NOT NULL		    
		);
	`)

	_, err4 := db.Exec(`
		CREATE TABLE IF NOT EXISTS network_status (
		    id INTEGER PRIMARY KEY,
		    active_connection TEXT NOT NULL,
		    public_ip TEXT NOT NULL,
		    location TEXT NOT NULL,
		    last_updated INTEGER NOT NULL
		);
	`)

	_, err5 := db.Exec(`
		CREATE TABLE IF NOT EXISTS isp_restart_times (
		    isp_id TINYINT PRIMARY KEY,
		    last_restart_uxtimesec INTEGER NOT NULL DEFAULT 0
		);
	`)

	for _, err := range []error{err, err1, err2, err3, err4, err5} {
		if err != nil {
			return fmt.Errorf("error creating table: %v", err)
		}
	}

	return nil
}

func PrintPingResults(db *sql.DB, pingResults chan map[string]int) {

	for result := range pingResults {
		now := time.Now()
		unixTimestamp := now.Unix()
		cloudflareMS := result["cloudflare"]
		googleMS := result["google"]
		facebookMS := result["facebook"]
		xMS := result["x"]

		logTrace("Inserting ping results: CF=%dms, Google=%dms, FB=%dms, X=%dms at %d", 
			cloudflareMS, googleMS, facebookMS, xMS, unixTimestamp)

		// Simple insert without retry for high-frequency ping data
		query := `
			INSERT INTO pings (uxtimesec, cloudflare, google, facebook, x)
			VALUES (?, ?, ?, ?, ?);
		`
		_, err := db.Exec(query, unixTimestamp, cloudflareMS, googleMS, facebookMS, xMS)
		if err != nil {
			logError("Failed to insert ping results: %v", err)
		} else {
			logTrace("Successfully inserted ping results")
		}
	}
}

func pingTest(pingResults chan map[string]int) {
	// -1 is some error
	// -2 is when latency is larger than 32000 ms

	pingThese := map[string]string{
		"google":     "8.8.8.8",
		"cloudflare": "1.1.1.1",
		"facebook":   "facebook.com",
		"x":          "x.com",
	}

	ticker := time.NewTicker(15 * time.Second)
	
	// Initial ping test immediately on startup
	fmt.Printf("[PING] Starting initial ping test...\n")
	
	for {
		select {
		case <-ticker.C:
			fmt.Printf("[PING] Running ping test cycle...\n")
			results := make(map[string]int)

			for website, ip := range pingThese {
				fmt.Printf("[PING] Testing %s (%s)...\n", website, ip)
				var cmd *exec.Cmd
				
				// Detect OS and use appropriate ping command
				if runtime.GOOS == "windows" {
					cmd = exec.Command("ping", "-n", "1", ip) // Windows uses -n for count
				} else {
					cmd = exec.Command("ping", "-c", "1", ip) // Linux/Mac use -c for count
				}

				output, err := cmd.CombinedOutput()
				if err != nil {
					fmt.Printf("[PING] Error pinging %s: %v\n", website, err)
					results[website] = -1
					continue
				}
				//log.Println(string(output))
				
				// Parse ping output based on OS
				var latency float64
				var found bool
				
				if runtime.GOOS == "windows" {
					// Windows format: "Reply from x.x.x.x: bytes=32 time=10ms TTL=64"
					lines := strings.Split(string(output), "\n")
					for _, line := range lines {
						if strings.Contains(line, "time=") && strings.Contains(line, "ms") {
							// Extract time value
							parts := strings.Split(line, "time=")
							if len(parts) > 1 {
								timePart := strings.Split(parts[1], "ms")[0]
								timePart = strings.TrimSpace(timePart)
								// Handle "<1" case for very low latencies
								if strings.Contains(timePart, "<") {
									latency = 1.0
									found = true
							} else {
								if val, err := strconv.ParseFloat(timePart, 64); err == nil {
									latency = val
									found = true
								}
							}
							break
						}
					}
				}
			} else {
				// Linux format: "64 bytes from x.x.x.x: icmp_seq=1 ttl=64 time=10.5 ms"
				for _, word := range strings.Split(string(output), " ") {
					if strings.Contains(word, "time") {
						digits := ""
						decimal := ""
						dotFound := false
						for _, char := range word {
							if char == '.' {
								dotFound = true
							} else if char >= '0' && char <= '9' {
								if !dotFound {
									digits += string(char)
								} else {
									decimal += string(char)
								}
							}
						}
						if timeStr := digits + "." + decimal; timeStr != "." {
							if val, err := strconv.ParseFloat(timeStr, 64); err == nil {
								latency = val
								found = true
								break
							}
						}
					}
				}
			}
			
			if found {
				roundedTime := int(math.Round(latency))
				if roundedTime < 32000 {
					results[website] = roundedTime
				} else {
					results[website] = -2
				}
			} else {
				results[website] = -1
			}
		}

			fmt.Printf("[PING] Completed ping cycle, sending results\n")
			pingResults <- results
		}
	}
}

func retriveSettings(db *sql.DB) {
	// Initialize autorestart table with default rows for both ISPs if they don't exist
	err := initializeAutorestartTable(db)
	if err != nil {
		fmt.Printf("Error initializing autorestart table: %v\n", err)
		return
	}
}

func initializeAutorestartTable(db *sql.DB) error {
	return retryDatabaseOperation(func() error {
		// Check if both required rows exist (isp_id 0 for primary, isp_id 1 for secondary)
		for ispID := 0; ispID <= 1; ispID++ {
			var count int
			err := db.QueryRow("SELECT COUNT(*) FROM autorestart WHERE isp_id = ?", ispID).Scan(&count)
			if err != nil {
				return fmt.Errorf("error checking for ISP %d autorestart row: %v", ispID, err)
			}
			
			if count == 0 {
				// Insert default row for this ISP (autorestart disabled by default)
				_, err = db.Exec(`
					INSERT INTO autorestart 
					(isp_id, uxtimesec, autorestart, hour, min, sec, daily, weekly, monthly, dayinweek, weekinmonth) 
					VALUES (?, ?, 0, 0, 0, 0, 0, 0, 0, 0, 0)`,
					ispID, time.Now().Unix())
				if err != nil {
					return fmt.Errorf("error inserting default autorestart row for ISP %d: %v", ispID, err)
				}
				fmt.Printf("Initialized autorestart table with default values for ISP %d (disabled)\n", ispID)
			} else {
				fmt.Printf("Autorestart row for ISP %d already exists\n", ispID)
			}
		}
		return nil
	}, 3, 200*time.Millisecond)
}

// Monitor network status and update database
func monitorNetworkStatus(db *sql.DB) {
	// Initial update
	updateNetworkStatus(db)
	
	// Update every minute
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	
	for range ticker.C {
		updateNetworkStatus(db)
	}
}

func updateNetworkStatus(db *sql.DB) {
	// Get public IP and location
	ipInfo, err := getIPInfo()
	if err != nil {
		fmt.Printf("Error getting IP info: %v\n", err)
		ipInfo = map[string]string{
			"ip": "Unknown",
			"city": "Unknown",
			"region": "Unknown",
			"country": "Unknown",
		}
	}
	
	// Determine active connection (simplified - you may want to enhance this)
	var activeConnection string
	if ispStateCache.primaryState.PowerState {
		activeConnection = "Primary ISP"
	} else if ispStateCache.secondaryState.PowerState {
		activeConnection = "Secondary ISP"
	} else {
		activeConnection = "No Active Connection"
	}
	
	location := fmt.Sprintf("%s, %s, %s", ipInfo["city"], ipInfo["region"], ipInfo["country"])
	currentTime := time.Now().Unix()
	
	// Insert or update network status
	err = retryDatabaseOperation(func() error {
		// First, try to update existing record
		result, err := db.Exec(`
			UPDATE network_status 
			SET active_connection = ?, public_ip = ?, location = ?, last_updated = ?
			WHERE id = 1`,
			activeConnection, ipInfo["ip"], location, currentTime)
		if err != nil {
			return err
		}
		
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		
		// If no rows were updated, insert a new record
		if rowsAffected == 0 {
			_, err = db.Exec(`
				INSERT INTO network_status (id, active_connection, public_ip, location, last_updated)
				VALUES (1, ?, ?, ?, ?)`,
				activeConnection, ipInfo["ip"], location, currentTime)
			return err
		}
		
		return nil
	}, 3, 100*time.Millisecond)
	
	if err != nil {
		fmt.Printf("Error updating network status: %v\n", err)
	}
}

// Get public IP info using a free IP geolocation service
func getIPInfo() (map[string]string, error) {
	resp, err := http.Get("http://ip-api.com/json/")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	
	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	
	// Extract relevant fields
	ipInfo := make(map[string]string)
	if ip, ok := result["query"].(string); ok {
		ipInfo["ip"] = ip
	} else {
		ipInfo["ip"] = "Unknown"
	}
	
	if city, ok := result["city"].(string); ok {
		ipInfo["city"] = city
	} else {
		ipInfo["city"] = "Unknown"
	}
	
	if region, ok := result["regionName"].(string); ok {
		ipInfo["region"] = region
	} else {
		ipInfo["region"] = "Unknown"
	}
	
	if country, ok := result["country"].(string); ok {
		ipInfo["country"] = country
	} else {
		ipInfo["country"] = "Unknown"
	}
	
	return ipInfo, nil
}

// Log an event to the logs table
func logEvent(db *sql.DB, reason string) error {
	currentTime := time.Now().Unix()
	
	return retryDatabaseOperation(func() error {
		_, err := db.Exec("INSERT INTO logs (uxtimesec, reason) VALUES (?, ?)", currentTime, reason)
		return err
	}, 3, 100*time.Millisecond)
}

// Log an ISP restart action with detailed information
func logISPRestartAction(db *sql.DB, reason string, ispID int, ispName string, restartType string, durationMinutes int, clientIP string) error {
	currentTime := time.Now().Unix()
	
	return retryDatabaseOperation(func() error {
		_, err := db.Exec("INSERT INTO logs (uxtimesec, reason, isp_id, isp_name, restart_type, duration_minutes, client_ip) VALUES (?, ?, ?, ?, ?, ?, ?)", 
			currentTime, reason, ispID, ispName, restartType, durationMinutes, clientIP)
		return err
	}, 3, 100*time.Millisecond)
}

// Generate human-readable schedule text for logging
func generateScheduleText(scheduleType string, dayinweek int, hour int, minute int, weekinmonth int) string {
	dayNames := map[int]string{
		1: "Sunday", 2: "Monday", 3: "Tuesday", 4: "Wednesday",
		5: "Thursday", 6: "Friday", 7: "Saturday",
	}
	
	weekNames := map[int]string{
		1: "first", 2: "second", 3: "third", 4: "fourth",
	}
	
	timeStr := fmt.Sprintf("%02d:%02d", hour, minute)
	
	switch scheduleType {
	case "daily":
		return fmt.Sprintf("daily at %s", timeStr)
	case "weekly":
		dayName := dayNames[dayinweek]
		if dayName == "" {
			dayName = fmt.Sprintf("day %d", dayinweek)
		}
		return fmt.Sprintf("weekly every %s at %s", dayName, timeStr)
	case "monthly":
		dayName := dayNames[dayinweek]
		if dayName == "" {
			dayName = fmt.Sprintf("day %d", dayinweek)
		}
		weekName := weekNames[weekinmonth]
		if weekName == "" {
			weekName = fmt.Sprintf("week %d", weekinmonth)
		}
		return fmt.Sprintf("monthly on the %s %s at %s", weekName, dayName, timeStr)
	default:
		return fmt.Sprintf("at %s", timeStr)
	}
}

// Get client IP address from HTTP request
func getClientIP(r *http.Request) string {
	// Check for forwarded header first (if behind proxy)
	forwarded := r.Header.Get("X-Forwarded-For")
	if forwarded != "" {
		// X-Forwarded-For can contain multiple IPs, get the first one
		parts := strings.Split(forwarded, ",")
		return strings.TrimSpace(parts[0])
	}
	
	// Check for real IP header (Cloudflare, etc.)
	realIP := r.Header.Get("X-Real-IP")
	if realIP != "" {
		return realIP
	}
	
	// Fall back to remote address
	ip := r.RemoteAddr
	// Remove port if present
	if idx := strings.LastIndex(ip, ":"); idx != -1 {
		ip = ip[:idx]
	}
	return ip
}

// Get ISP name from ISP ID
func getISPName(ispID int) string {
	if ispID == 0 {
		return "Primary (T-Mobile)"
	} else if ispID == 1 {
		return "Secondary (Mercury)"
	}
	return "Unknown"
}

// Interactive command-line interface for testing ISP state changes
func startCommandInterface(db *sql.DB) {
	if console.terminalMode {
		// Use raw terminal input mode
		startTerminalInput(db)
	} else {
		// Use simple line-based input mode
		startSimpleInput(db)
	}
}

func startSimpleInput(db *sql.DB) {
	reader := bufio.NewReader(os.Stdin)
	eofCount := 0
	
	for {
		fmt.Print("\n[CMD] Enter command (help for list): ")
		input, err := reader.ReadString('\n')
		if err != nil {
			eofCount++
			fmt.Printf("[CMD ERROR] Error reading input: %v\n", err)
			
			// If we get multiple EOF errors in a row, stdin is not available
			// This happens when running as a service or without a terminal
			if eofCount >= 3 {
				fmt.Println("[CMD] No interactive terminal detected, disabling command input")
				return
			}
			continue
		}
		
		// Reset EOF count on successful read
		eofCount = 0
		
		command := strings.TrimSpace(input)
		if command != "" {
			processCommand(db, command)
		}
	}
}

func startTerminalInput(db *sql.DB) {
	// Set stdin to raw mode for character-by-character input
	if runtime.GOOS == "windows" {
		startSimpleInput(db) // Fallback for Windows
		return
	}
	
	// Disable line buffering and echo
	exec.Command("stty", "-echo", "cbreak").Run()
	defer exec.Command("stty", "echo", "-cbreak").Run()
	
	reader := bufio.NewReader(os.Stdin)
	
	for {
		char, err := reader.ReadByte()
		if err != nil {
			logError("Error reading terminal input: %v", err)
			break
		}
		
		console.Lock()
		
		switch char {
		case 13, 10: // Enter key
			if len(console.currentInput) > 0 {
				command := strings.TrimSpace(console.currentInput)
				console.currentInput = ""
				console.cursorPos = 0
				console.Unlock()
				
				if command == "exit" || command == "quit" {
					cleanupTerminal()
					os.Exit(0)
				}
				
				processCommand(db, command)
			} else {
				console.Unlock()
			}
			
		case 127, 8: // Backspace
			if console.cursorPos > 0 {
				console.currentInput = console.currentInput[:console.cursorPos-1] + console.currentInput[console.cursorPos:]
				console.cursorPos--
			}
			console.Unlock()
			
		case 3: // Ctrl+C
			console.Unlock()
			cleanupTerminal()
			os.Exit(0)
			
		case 27: // Escape sequence (arrow keys, etc.)
			console.Unlock()
			handleEscapeSequence(reader)
			
		default:
			if char >= 32 && char <= 126 { // Printable characters
				console.currentInput = console.currentInput[:console.cursorPos] + string(char) + console.currentInput[console.cursorPos:]
				console.cursorPos++
			}
			console.Unlock()
		}
		
		if console.terminalMode {
			drawInputArea()
		}
	}
}

func handleEscapeSequence(reader *bufio.Reader) {
	// Read the rest of the escape sequence
	next, _ := reader.ReadByte()
	if next == '[' {
		final, _ := reader.ReadByte()
		console.Lock()
		defer console.Unlock()
		
		switch final {
		case 'D': // Left arrow
			if console.cursorPos > 0 {
				console.cursorPos--
			}
		case 'C': // Right arrow  
			if console.cursorPos < len(console.currentInput) {
				console.cursorPos++
			}
		case 'A': // Up arrow - could implement command history
		case 'B': // Down arrow - could implement command history
		}
	}
}

func processCommand(db *sql.DB, command string) {
	parts := strings.Fields(command)
	
	if len(parts) == 0 {
		return
	}
	
	switch strings.ToLower(parts[0]) {
	case "help":
		printHelp()
		
	case "status":
		showISPStatus(db)
	
	case "loglevel":
		if len(parts) != 2 {
			logInfo("Current log level: %s", logLevelNames[console.logLevel])
			logInfo("Usage: loglevel <level>")
			logInfo("Available levels: SILENT, ERROR, WARN, INFO, DEBUG, TRACE")
			return
		}
		setLogLevel(strings.ToUpper(parts[1]))
	
	case "logs":
		showRecentLogs()
	
	case "flush":
		// Force flush buffered logs in simple mode
		if !console.terminalMode {
			console.RLock()
			logs := make([]string, len(console.outputBuffer))
			copy(logs, console.outputBuffer)
			console.RUnlock()
			
			fmt.Printf("\n[FLUSH] Showing %d buffered log entries:\n", len(logs))
			start := 0
			if len(logs) > 50 {  // Show last 50 entries
				start = len(logs) - 50
			}
			for i := start; i < len(logs); i++ {
				fmt.Println(logs[i])
			}
			fmt.Println("[FLUSH] End of buffered logs\n")
		} else {
			logInfo("Flush command only works in simple mode")
		}
	
	case "clear":
		if console.terminalMode {
			console.Lock()
			console.outputBuffer = []string{}
			console.Unlock()
			redrawTerminal()
		} else {
			fmt.Print("\033[2J\033[H") // Clear screen in simple mode
		}
	
	case "ui":
		if len(parts) > 1 && strings.ToLower(parts[1]) == "simple" {
			// Switch to simple mode
			if console.terminalMode {
				cleanupTerminal()
				console.Lock()
				console.terminalMode = false
				console.Unlock()
				logInfo("Switched to simple UI mode")
			}
		} else {
			logInfo("Current UI mode: %s", func() string {
				if console.terminalMode { return "terminal" } else { return "simple" }
			}())
			logInfo("Usage: ui simple - switch to simple mode")
		}
		
	case "set":
		if len(parts) != 3 {
			logInfo("Usage: set <isp_id> <on|off>")
			logInfo("Example: set 0 off")
			return
		}
		
		ispID, err := strconv.Atoi(parts[1])
		if err != nil || (ispID != 0 && ispID != 1) {
			logInfo("Error: ISP ID must be 0 (primary) or 1 (secondary)")
			return
		}
		
		var powerState bool
		switch strings.ToLower(parts[2]) {
		case "on", "true", "1":
			powerState = true
		case "off", "false", "0":
			powerState = false
		default:
			logInfo("Error: State must be 'on' or 'off'")
			return
		}
		
		setISPState(db, ispID, powerState)
		
	case "restart":
		if len(parts) < 2 {
			logInfo("Usage: restart <isp_id> [duration_minutes]")
			logInfo("Example: restart 0 5")
			return
		}
		
		ispID, err := strconv.Atoi(parts[1])
		if err != nil || (ispID != 0 && ispID != 1) {
			logInfo("Error: ISP ID must be 0 (primary) or 1 (secondary)")
			return
		}
		
		var duration int
		if len(parts) >= 3 {
			duration, err = strconv.Atoi(parts[2])
			if err != nil {
				logInfo("Error: Duration must be a number (minutes)")
				return
			}
		}
		
		restartISP(db, ispID, duration)
		
	case "setrestart":
		if len(parts) != 3 {
			logInfo("Usage: setrestart <isp_id> <unix_timestamp>")
			logInfo("Example: setrestart 0 1640995200")
			logInfo("Use 0 for timestamp to clear restart time")
			return
		}
		
		ispID, err := strconv.Atoi(parts[1])
		if err != nil || (ispID != 0 && ispID != 1) {
			logInfo("Error: ISP ID must be 0 (primary) or 1 (secondary)")
			return
		}
		
		restartTime, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			logInfo("Error: Timestamp must be a valid Unix timestamp")
			return
		}
		
		setRestartTime(db, ispID, restartTime)
			
	case "exit", "quit":
		if console.terminalMode {
			cleanupTerminal()
		}
		logInfo("Command interface disabled")
		os.Exit(0)
		
	default:
		logInfo("Unknown command: %s", parts[0])
		logInfo("Type 'help' for available commands")
	}
}

func printHelp() {
	// Use direct printing to avoid potential deadlocks with the logging system
	helpText := `
Available commands:
  help                        - Show this help message
  status                      - Show current ISP states
  loglevel [level]           - Show/set log level (SILENT/ERROR/WARN/INFO/DEBUG/TRACE)
  logs                        - Show recent log entries
  flush                       - Show buffered logs (simple mode only)
  clear                       - Clear output buffer
  ui [simple]                - Show UI mode or switch to simple mode
  set <id> <on|off>          - Set ISP power state (id: 0=primary, 1=secondary)
  restart <id> [minutes]     - Restart ISP for specified duration (0=immediate)
  setrestart <id> <timestamp> - Set last restart time for testing uptime clocks
  exit                        - Exit program

Log Levels:
  SILENT - No output
  ERROR  - Only errors and critical events
  WARN   - Warnings and above
  INFO   - Important events (user actions, restarts) [DEFAULT]
  DEBUG  - Detailed info (some API calls)
  TRACE  - All output (everything)

Examples:
  loglevel INFO              - Reset to default level (recommended for Windows)
  loglevel WARN              - Reduce noise, only warnings and errors
  flush                      - Show buffered DEBUG/TRACE messages
  set 0 off                  - Turn off primary ISP
  restart 0 5                - Restart primary ISP for 5 minutes
`
	fmt.Print(helpText)
}

func showISPStatus(db *sql.DB) {
	err := retryDatabaseOperation(func() error {
		rows, err := db.Query("SELECT ispid, powerstate, uxtimewhenoffrequested, offuntiluxtimesec FROM ispstates ORDER BY ispid")
		if err != nil {
			return err
		}
		defer rows.Close()
		
		fmt.Println("\n[CMD] Current ISP States:")
		fmt.Println("ID | Name      | Power | Off Requested | Off Until")
		fmt.Println("---|-----------|-------|---------------|----------")
		
		for rows.Next() {
			var ispid int
			var powerstate int
			var offrequested int64
			var offuntil int64
			
			err := rows.Scan(&ispid, &powerstate, &offrequested, &offuntil)
			if err != nil {
				return err
			}
			
			name := "Primary"
			if ispid == 1 {
				name = "Secondary"
			}
			
			power := "OFF"
			if powerstate == 1 {
				power = "ON"
			}
			
			offReqStr := "None"
			if offrequested > 0 {
				offReqStr = time.Unix(offrequested, 0).Format("15:04:05")
			}
			
			offUntilStr := "None"
			if offuntil > 0 {
				offUntilStr = time.Unix(offuntil, 0).Format("15:04:05")
			}
			
			fmt.Printf("%d  | %-9s | %-5s | %-13s | %s\n", ispid, name, power, offReqStr, offUntilStr)
		}
		
		return rows.Err()
	}, 3, 100*time.Millisecond)
	
	if err != nil {
		fmt.Printf("[CMD ERROR] Failed to fetch ISP status: %v\n", err)
	}
}

func setISPState(db *sql.DB, ispID int, powerState bool) {
	ispName := "Primary"
	if ispID == 1 {
		ispName = "Secondary"
	}
	
	powerStateInt := 0
	if powerState {
		powerStateInt = 1
	}
	
	err := retryDatabaseOperation(func() error {
		_, err := db.Exec(`
			UPDATE ispstates 
			SET powerstate = ?, 
			    uxtimewhenoffrequested = 0, 
			    offuntiluxtimesec = 0 
			WHERE ispid = ?`,
			powerStateInt, ispID)
		return err
	}, 3, 100*time.Millisecond)
	
	if err != nil {
		fmt.Printf("[CMD ERROR] Failed to update %s ISP state: %v\n", ispName, err)
		return
	}
	
	stateStr := "OFF"
	if powerState {
		stateStr = "ON"
	}
	
	fmt.Printf("[CMD] %s ISP set to %s\n", ispName, stateStr)
	
	// Log the manual change
	reason := fmt.Sprintf("Manual command: %s ISP set to %s", ispName, stateStr)
	logEvent(db, reason)
}

func restartISP(db *sql.DB, ispID int, durationMinutes int) {
	ispName := "Primary"
	if ispID == 1 {
		ispName = "Secondary"
	}
	
	currentTime := time.Now().Unix()
	var offUntilTime int64
	
	if durationMinutes > 0 {
		offUntilTime = currentTime + int64(durationMinutes*60)
	}
	
	err := retryDatabaseOperation(func() error {
		_, err := db.Exec(`
			UPDATE ispstates 
			SET powerstate = 0, 
			    uxtimewhenoffrequested = ?, 
			    offuntiluxtimesec = ? 
			WHERE ispid = ?`,
			currentTime, offUntilTime, ispID)
		return err
	}, 3, 100*time.Millisecond)
	
	if err != nil {
		fmt.Printf("[CMD ERROR] Failed to restart %s ISP: %v\n", ispName, err)
		return
	}
	
	if durationMinutes > 0 {
		fmt.Printf("[CMD] %s ISP restarted for %d minutes (until %s)\n", 
			ispName, durationMinutes, time.Unix(offUntilTime, 0).Format("15:04:05"))
	} else {
		fmt.Printf("[CMD] %s ISP restarted immediately\n", ispName)
	}
	
	// Log the manual restart
	reason := fmt.Sprintf("Manual command: %s ISP restart", ispName)
	if durationMinutes > 0 {
		reason += fmt.Sprintf(" for %d minutes", durationMinutes)
	} else {
		reason += " immediately"
	}
	logEvent(db, reason)
}

// Set restart time for testing purposes
func setRestartTime(db *sql.DB, ispID int, restartTime int64) {
	ispName := "Primary"
	if ispID == 1 {
		ispName = "Secondary"
	}
	
	err := retryDatabaseOperation(func() error {
		_, err := db.Exec(`
			INSERT OR REPLACE INTO isp_restart_times (isp_id, last_restart_uxtimesec) 
			VALUES (?, ?)`,
			ispID, restartTime)
		return err
	}, 3, 100*time.Millisecond)
	
	if err != nil {
		fmt.Printf("[CMD ERROR] Failed to set %s ISP restart time: %v\n", ispName, err)
		return
	}
	
	if restartTime == 0 {
		fmt.Printf("[CMD] %s ISP restart time cleared\n", ispName)
	} else {
		fmt.Printf("[CMD] %s ISP restart time set to %d (%s)\n", ispName, restartTime, 
			time.Unix(restartTime, 0).Format("2006-01-02 15:04:05"))
	}
	
	// Log the manual change
	reason := fmt.Sprintf("Manual command: %s ISP restart time set to %d", ispName, restartTime)
	logEvent(db, reason)
}

// Update last restart time for an ISP - using a separate table for tracking restart times
func updateLastRestartTime(db *sql.DB, ispID int, restartTime int64) error {
	return retryDatabaseOperation(func() error {
		// Insert or update the restart time record
		_, err := db.Exec(`
			INSERT OR REPLACE INTO isp_restart_times (isp_id, last_restart_uxtimesec) 
			VALUES (?, ?)`,
			ispID, restartTime)
		return err
	}, 3, 100*time.Millisecond)
}

// Set log level command
func setLogLevel(levelStr string) {
	var newLevel LogLevel
	var found bool
	
	for level, name := range logLevelNames {
		if name == levelStr {
			newLevel = level
			found = true
			break
		}
	}
	
	if !found {
		logInfo("Invalid log level: %s", levelStr)
		logInfo("Available levels: SILENT, ERROR, WARN, INFO, DEBUG, TRACE")
		return
	}
	
	console.Lock()
	oldLevel := console.logLevel
	console.logLevel = newLevel
	console.Unlock()
	
	logInfo("Log level changed from %s to %s", logLevelNames[oldLevel], logLevelNames[newLevel])
}

// Show recent logs command
func showRecentLogs() {
	console.RLock()
	logs := make([]string, len(console.outputBuffer))
	copy(logs, console.outputBuffer)
	console.RUnlock()
	
	logInfo("")
	logInfo("Recent logs (last %d entries, level: %s):", len(logs), logLevelNames[console.logLevel])
	logInfo("=====================================")
	
	// Show last 20 entries
	start := 0
	if len(logs) > 20 {
		start = len(logs) - 20
	}
	
	for i := start; i < len(logs); i++ {
		logInfo("%s", logs[i])
	}
	logInfo("=====================================")
}
