package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// IPInfoResponse represents the response from ipinfo.io
type IPInfoResponse struct {
	IP       string `json:"ip"`
	City     string `json:"city"`
	Region   string `json:"region"`
	Country  string `json:"country"`
	Org      string `json:"org"`
	Timezone string `json:"timezone"`
}

func startGPIOlogic(db *sql.DB) {
	// Initialize ISP states - check if table has data, if not create defaults
	err := initializeISPStates(db)
	if err != nil {
		logError("Error initializing ISP states: %v", err)
		panic(err)
	}

	// Load initial data into cache
	err = refreshISPStateCache(db)
	if err != nil {
		logError("Error loading initial ISP states into cache: %v", err)
		panic(err)
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

	// Check for scheduled auto-restarts
	checkAutoRestartSchedule(db)

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

				// Log the power-on action with specific context
				ispName := getISPName(0)
				logReason := fmt.Sprintf("[SYSTEM] %s auto-restart completed - ISP powered back online", ispName)
				logErr := logISPRestartAction(db, logReason, 0, ispName, "auto_restart_complete", 0, "system")
				if logErr != nil {
					logError("Failed to log auto-restart completion: %v", logErr)
				}

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

				// Log the power-on action with specific context
				ispName := getISPName(1)
				logReason := fmt.Sprintf("[SYSTEM] %s auto-restart completed - ISP powered back online", ispName)
				logErr := logISPRestartAction(db, logReason, 1, ispName, "auto_restart_complete", 0, "system")
				if logErr != nil {
					logError("Failed to log auto-restart completion: %v", logErr)
				}

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

func initializeISPStates(db *sql.DB) error {
	return retryDatabaseOperation(func() error {
		// Check if ISP states exist, if not create them
		for ispID := 0; ispID <= 1; ispID++ {
			var count int
			err := db.QueryRow("SELECT COUNT(*) FROM ispstates WHERE ispid = ?", ispID).Scan(&count)
			if err != nil {
				return fmt.Errorf("error checking for ISP %d state: %v", ispID, err)
			}

			if count == 0 {
				// Insert default state for this ISP (powerstate=1, all timers=0)
				_, err = db.Exec(`
					INSERT INTO ispstates 
					(ispid, powerstate, uxtimewhenoffrequested, offuntiluxtimesec) 
					VALUES (?, 1, 0, 0)`,
					ispID)
				if err != nil {
					return fmt.Errorf("error inserting default state for ISP %d: %v", ispID, err)
				}
				fmt.Printf("Initialized ISP %d state with default values (power ON)\n", ispID)
			}
		}
		return nil
	}, 3, 100*time.Millisecond)
}

func refreshISPStateCache(db *sql.DB) error {
	return retryDatabaseOperation(func() error {
		var primary, secondary ISPState

		// Get primary ISP state (ispid = 0)
		err := db.QueryRow("SELECT ispid, powerstate, uxtimewhenoffrequested, offuntiluxtimesec FROM ispstates WHERE ispid = 0").
			Scan(&primary.ISPID, &primary.PowerState, &primary.UxtimeWhenOffRequested, &primary.OffUntilUxtimesec)
		if err != nil {
			return fmt.Errorf("error querying primary ISP state: %v", err)
		}

		// Get secondary ISP state (ispid = 1)
		err = db.QueryRow("SELECT ispid, powerstate, uxtimewhenoffrequested, offuntiluxtimesec FROM ispstates WHERE ispid = 1").
			Scan(&secondary.ISPID, &secondary.PowerState, &secondary.UxtimeWhenOffRequested, &secondary.OffUntilUxtimesec)
		if err != nil {
			return fmt.Errorf("error querying secondary ISP state: %v", err)
		}

		// Update cache
		ispStateCache.updateStates(primary, secondary)

		return nil
	}, 3, 100*time.Millisecond)
}

func updateISPPowerState(db *sql.DB, ispID int, powerState bool, uxtimeWhenOffRequested int64, offUntilUxtimesec int64) error {
	return retryDatabaseOperation(func() error {
		_, err := db.Exec(`
			UPDATE ispstates 
			SET powerstate = ?, uxtimewhenoffrequested = ?, offuntiluxtimesec = ? 
			WHERE ispid = ?`,
			powerState, uxtimeWhenOffRequested, offUntilUxtimesec, ispID)
		return err
	}, 3, 100*time.Millisecond)
}

func showISPStatus(db *sql.DB) {
	var primaryState, secondaryState ISPState

	err := db.QueryRow("SELECT ispid, powerstate, uxtimewhenoffrequested, offuntiluxtimesec FROM ispstates WHERE ispid = 0").
		Scan(&primaryState.ISPID, &primaryState.PowerState, &primaryState.UxtimeWhenOffRequested, &primaryState.OffUntilUxtimesec)
	if err != nil {
		logError("Failed to query primary ISP state: %v", err)
		return
	}

	err = db.QueryRow("SELECT ispid, powerstate, uxtimewhenoffrequested, offuntiluxtimesec FROM ispstates WHERE ispid = 1").
		Scan(&secondaryState.ISPID, &secondaryState.PowerState, &secondaryState.UxtimeWhenOffRequested, &secondaryState.OffUntilUxtimesec)
	if err != nil {
		logError("Failed to query secondary ISP state: %v", err)
		return
	}

	logInfo("=== ISP Status ===")

	// Primary ISP status
	primaryStatus := "ON"
	if !primaryState.PowerState {
		primaryStatus = "OFF"
	}
	logInfo("Primary ISP:   %s", primaryStatus)

	if primaryState.UxtimeWhenOffRequested > 0 {
		logInfo("  - Off requested at: %s", time.Unix(primaryState.UxtimeWhenOffRequested, 0).Format("2006-01-02 15:04:05"))
	}
	if primaryState.OffUntilUxtimesec > 0 {
		logInfo("  - Off until: %s", time.Unix(primaryState.OffUntilUxtimesec, 0).Format("2006-01-02 15:04:05"))
	}

	// Secondary ISP status
	secondaryStatus := "ON"
	if !secondaryState.PowerState {
		secondaryStatus = "OFF"
	}
	logInfo("Secondary ISP: %s", secondaryStatus)

	if secondaryState.UxtimeWhenOffRequested > 0 {
		logInfo("  - Off requested at: %s", time.Unix(secondaryState.UxtimeWhenOffRequested, 0).Format("2006-01-02 15:04:05"))
	}
	if secondaryState.OffUntilUxtimesec > 0 {
		logInfo("  - Off until: %s", time.Unix(secondaryState.OffUntilUxtimesec, 0).Format("2006-01-02 15:04:05"))
	}

	logInfo("==================")
}

func setISPState(db *sql.DB, ispID int, powerState bool) {
	err := updateISPPowerState(db, ispID, powerState, 0, 0)
	if err != nil {
		logError("Failed to update ISP %d power state: %v", ispID, err)
		return
	}

	ispName := getISPName(ispID)
	state := "OFF"
	if powerState {
		state = "ON"
	}

	logInfo("ISP %d (%s) power state set to: %s", ispID, ispName, state)

	// Log the action
	logReason := fmt.Sprintf("[COMMAND] ISP %s power set to %s", ispName, state)
	logErr := logEvent(db, logReason)
	if logErr != nil {
		logError("Failed to log ISP state change: %v", logErr)
	}
}

func restartISP(db *sql.DB, ispID int, durationMinutes int) {
	currentTime := time.Now().Unix()
	offUntilTime := currentTime + (int64(durationMinutes) * 60)

	err := updateISPPowerState(db, ispID, false, currentTime, offUntilTime)
	if err != nil {
		logError("Failed to restart ISP %d: %v", ispID, err)
		return
	}

	ispName := getISPName(ispID)
	logInfo("ISP %d (%s) scheduled for restart: OFF for %d minutes", ispID, ispName, durationMinutes)

	// Log the action
	logReason := fmt.Sprintf("[COMMAND] ISP %s restart scheduled for %d minutes", ispName, durationMinutes)
	logErr := logISPRestartAction(db, logReason, ispID, ispName, "command_restart", durationMinutes, "console")
	if logErr != nil {
		logError("Failed to log ISP restart: %v", logErr)
	}
}

func setRestartTime(db *sql.DB, ispID int, restartTime int64) {
	err := updateLastRestartTime(db, ispID, restartTime)
	if err != nil {
		logError("Failed to set restart time for ISP %d: %v", ispID, err)
		return
	}

	ispName := getISPName(ispID)
	timeStr := time.Unix(restartTime, 0).Format("2006-01-02 15:04:05")
	logInfo("ISP %d (%s) last restart time set to: %s", ispID, ispName, timeStr)
}

// isAnyISPRestarting checks if any ISP is currently in an active restart state
func isAnyISPRestarting(db *sql.DB) bool {
	// Check both primary (0) and secondary (1) ISPs
	for ispID := 0; ispID <= 1; ispID++ {
		var powerState bool
		var uxtimeWhenOffRequested, offUntilUxtimesec int64

		err := db.QueryRow(`
			SELECT powerstate, uxtimewhenoffrequested, offuntiluxtimesec 
			FROM ispstates 
			WHERE ispid = ?`, ispID).
			Scan(&powerState, &uxtimeWhenOffRequested, &offUntilUxtimesec)

		if err != nil {
			continue // Skip if can't read state
		}

		currentTime := time.Now().Unix()

		// ISP is actively restarting if:
		// 1. It was recently requested to be turned off (within last 2 minutes) AND is currently off
		// 2. It's scheduled to turn back on soon (within next 2 minutes)
		isRecentlyTurnedOff := uxtimeWhenOffRequested > 0 && (currentTime-uxtimeWhenOffRequested) < 120 && !powerState
		isScheduledToTurnOnSoon := offUntilUxtimesec > 0 && (offUntilUxtimesec-currentTime) < 120 && offUntilUxtimesec > currentTime
		
		if isRecentlyTurnedOff || isScheduledToTurnOnSoon {
			return true
		}
	}
	return false
}

func monitorNetworkStatus(db *sql.DB) {
	// Initial update
	updateNetworkStatus(db)

	// Start with 5-minute interval (normal operation)
	currentInterval := 5 * time.Minute
	ticker := time.NewTicker(currentInterval)
	defer ticker.Stop()

	for range ticker.C {
		// Check if any ISP is currently being restarted
		isRestarting := isAnyISPRestarting(db)

		// Determine the appropriate interval
		var newInterval time.Duration
		if isRestarting {
			newInterval = 15 * time.Second // Fast polling during restarts
		} else {
			newInterval = 5 * time.Minute // Normal polling
		}

		// Update ticker if interval changed
		if newInterval != currentInterval {
			ticker.Stop()
			ticker = time.NewTicker(newInterval)
			currentInterval = newInterval
			logInfo("ISP info query interval changed to %v (restart mode: %t)", newInterval, isRestarting)
		}

		updateNetworkStatus(db)
	}
}

func updateNetworkStatus(db *sql.DB) {
	ipInfo, err := getIPInfo()
	if err != nil {
		fmt.Printf("Error getting IP info: %v\n", err)
		return
	}

	// Insert new network status record
	_, err = db.Exec(`
		INSERT INTO network_status (active_connection, public_ip, location, last_updated)
		VALUES (?, ?, ?, ?)`,
		ipInfo["connection"], ipInfo["ip"], ipInfo["location"], time.Now().Unix())

	if err != nil {
		fmt.Printf("Error inserting network status: %v\n", err)
		return
	}

	// Clean up old records - keep more during restart periods
	isRestarting := isAnyISPRestarting(db)
	var keepRecords int
	if isRestarting {
		keepRecords = 50 // Keep more records during restart periods for detailed monitoring
	} else {
		keepRecords = 10 // Normal amount for regular monitoring
	}

	_, err = db.Exec(`
		DELETE FROM network_status 
		WHERE id NOT IN (
			SELECT id FROM network_status 
			ORDER BY last_updated DESC 
			LIMIT ?
		)`, keepRecords)

	if err != nil {
		fmt.Printf("Error cleaning up old network status records: %v\n", err)
	} else {
		fmt.Printf("Network status updated: %s at %s (%s)\n",
			ipInfo["connection"], ipInfo["ip"], ipInfo["location"])
	}
}

func getIPInfo() (map[string]string, error) {
	// Create HTTP client with timeout to prevent hanging
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	// Make HTTP request to ipinfo.io
	resp, err := client.Get("https://ipinfo.io/json")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch network status: %v", err)
	}
	defer resp.Body.Close()

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read network status response: %v", err)
	}

	// Parse JSON response
	var ipInfo IPInfoResponse
	err = json.Unmarshal(body, &ipInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to parse network status JSON: %v", err)
	}

	// Format location as "City, Region (Country)"
	location := fmt.Sprintf("%s, %s (%s)", ipInfo.City, ipInfo.Region, ipInfo.Country)

	// Return info map with the required keys
	info := map[string]string{
		"connection": ipInfo.Org, // Using org as active connection
		"ip":         ipInfo.IP,  // Public IP
		"location":   location,   // Formatted location
	}

	return info, nil
}

// checkAutoRestartSchedule checks if any ISP should be restarted based on scheduled times
func checkAutoRestartSchedule(db *sql.DB) {
	currentTime := time.Now()

	// Check both ISPs (0 = primary, 1 = secondary)
	for ispID := 0; ispID <= 1; ispID++ {
		var autorestart Autorestart

		// Query autorestart settings for this ISP
		err := db.QueryRow(`
			SELECT isp_id, uxtimesec, autorestart, hour, min, sec, daily, weekly, monthly, dayinweek, weekinmonth 
			FROM autorestart WHERE isp_id = ?`, ispID).
			Scan(&autorestart.ROWID, &autorestart.Uxtimesec, &autorestart.Autorestart,
				&autorestart.Hour, &autorestart.Min, &autorestart.Sec,
				&autorestart.Daily, &autorestart.Weekly, &autorestart.Monthly,
				&autorestart.Dayinweek, &autorestart.Weekinmonth)

		if err != nil {
			// Skip if no autorestart settings found
			continue
		}

		// Skip if autorestart is disabled
		if autorestart.Autorestart == 0 {
			continue
		}

		// Check if current time matches the schedule
		if shouldRestartNow(currentTime, autorestart) {
			// Check if ISP is currently powered on (only restart if it's on)
			var powerState bool
			err := db.QueryRow("SELECT powerstate FROM ispstates WHERE ispid = ?", ispID).Scan(&powerState)
			if err != nil || !powerState {
				continue // Skip if can't read state or already off
			}

			// Perform auto-restart (immediate restart, not timed)
			logInfo("AUTO-RESTART: ISP %d scheduled restart triggered", ispID)

			// Set ISP to OFF for immediate restart (0 means restart now)
			currentUnix := currentTime.Unix()

			// Update ISP state to OFF with offUntilUxtimesec = 0 for immediate restart
			err = updateISPPowerState(db, ispID, false, currentUnix, 0)
			if err != nil {
				logError("Failed to update ISP %d power state for auto-restart: %v", ispID, err)
				continue
			}

			// Log the auto-restart action with schedule details
			ispName := getISPName(ispID)

			// Generate human-readable schedule text
			var scheduleType string
			if autorestart.Daily == 1 {
				scheduleType = "daily"
			} else if autorestart.Weekly == 1 {
				scheduleType = "weekly"
			} else if autorestart.Monthly == 1 {
				scheduleType = "monthly"
			}

			scheduleText := generateScheduleText(scheduleType, autorestart.Dayinweek, autorestart.Hour, autorestart.Min, autorestart.Weekinmonth)
			logReason := fmt.Sprintf("[SYSTEM] %s auto-restart triggered - %s", ispName, scheduleText)
			logErr := logISPRestartAction(db, logReason, ispID, ispName, "auto_restart_immediate", 0, "system")
			if logErr != nil {
				logError("Failed to log auto-restart action: %v", logErr)
			}

			logInfo("AUTO-RESTART: %s triggered for immediate restart", ispName)
		}
	}
}

// shouldRestartNow checks if the current time matches the autorestart schedule
func shouldRestartNow(currentTime time.Time, schedule Autorestart) bool {
	currentHour := currentTime.Hour()
	currentMin := currentTime.Minute()
	currentSec := currentTime.Second()

	// Check if time matches (within 15-second window since we check every 15 seconds)
	timeMatches := currentHour == schedule.Hour &&
		currentMin == schedule.Min &&
		currentSec >= schedule.Sec &&
		currentSec < (schedule.Sec+15)

	if !timeMatches {
		return false
	}

	// Check schedule type
	if schedule.Daily == 1 {
		return true // Daily restart at specified time
	}

	if schedule.Weekly == 1 {
		currentWeekday := int(currentTime.Weekday()) // Sunday = 0, Monday = 1, etc.
		return currentWeekday == schedule.Dayinweek
	}

	if schedule.Monthly == 1 {
		// Calculate which week of the month and day of week
		currentWeekday := int(currentTime.Weekday())
		weekOfMonth := getWeekOfMonth(currentTime)

		return currentWeekday == schedule.Dayinweek && weekOfMonth == schedule.Weekinmonth
	}

	return false
}

// getWeekOfMonth calculates which week of the month (1-based) the given date falls in
func getWeekOfMonth(t time.Time) int {
	firstOfMonth := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
	firstWeekday := int(firstOfMonth.Weekday())

	// Calculate week number (1-based)
	dayOfMonth := t.Day()
	week := (dayOfMonth+firstWeekday-1)/7 + 1

	return week
}
