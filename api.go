package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CORS middleware to handle cross-origin requests
func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set CORS headers for all requests
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
		w.Header().Set("Access-Control-Max-Age", "86400") // 24 hours
		
		// Handle preflight OPTIONS request
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		
		// Call the next handler
		next.ServeHTTP(w, r)
	})
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
				
				fmt.Printf("[DATABASE] Successfully retrieved %d log entries\n", len(logs))
				
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(logs)
			}
		}
	}
}

func generateScheduleText(scheduleType string, dayinweek int, hour int, minute int, weekinmonth int) string {
	weekdays := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	weekNumbers := []string{"", "First", "Second", "Third", "Fourth", "Last"}
	
	timeStr := fmt.Sprintf("%02d:%02d", hour, minute)
	
	switch scheduleType {
	case "daily":
		return fmt.Sprintf("Daily at %s", timeStr)
	case "weekly":
		if dayinweek >= 0 && dayinweek < len(weekdays) {
			return fmt.Sprintf("Weekly on %s at %s", weekdays[dayinweek], timeStr)
		}
		return fmt.Sprintf("Weekly on day %d at %s", dayinweek, timeStr)
	case "monthly":
		weekStr := ""
		if weekinmonth >= 1 && weekinmonth < len(weekNumbers) {
			weekStr = weekNumbers[weekinmonth]
		} else {
			weekStr = fmt.Sprintf("Week %d", weekinmonth)
		}
		
		dayStr := ""
		if dayinweek >= 0 && dayinweek < len(weekdays) {
			dayStr = weekdays[dayinweek]
		} else {
			dayStr = fmt.Sprintf("Day %d", dayinweek)
		}
		
		return fmt.Sprintf("Monthly on %s %s at %s", weekStr, dayStr, timeStr)
	default:
		return "Custom schedule"
	}
}

func getClientIP(r *http.Request) string {
	// Check common headers for real IP when behind a proxy
	headers := []string{
		"X-Real-IP",
		"X-Forwarded-For",
		"CF-Connecting-IP", // Cloudflare
		"True-Client-IP",   // Akamai and Cloudflare
		"X-Client-IP",
	}
	
	for _, header := range headers {
		ip := r.Header.Get(header)
		if ip != "" {
			// X-Forwarded-For may contain multiple IPs, get the first one
			if header == "X-Forwarded-For" {
				ips := strings.Split(ip, ",")
				if len(ips) > 0 {
					return strings.TrimSpace(ips[0])
				}
			}
			return ip
		}
	}
	
	// Fall back to RemoteAddr
	ip := r.RemoteAddr
	// Remove port if present
	if colonIndex := strings.LastIndex(ip, ":"); colonIndex != -1 {
		return ip[:colonIndex]
	}
	return ip
}

func getISPName(ispID int) string {
	switch ispID {
	case 0:
		return "Primary ISP"
	case 1:
		return "Secondary ISP"
	default:
		return fmt.Sprintf("ISP %d", ispID)
	}
}