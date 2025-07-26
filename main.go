// This file is compiled for raspberry pi 3b using the command:
// GOOS=linux GOARCH=arm GOARM=6 go build main.go

// the file is built for linux, using -c for ping, and parsing decimal rtt times in ms
// to insert into a sqlite database

package main

import (
	"database/sql"
	"fmt"
	"log"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"encoding/json"
	"net/http"
)

type ISPState struct {
	ISPID                  int
	PowerState             bool
	UxtimeWhenOffRequested int64
	OffUntilUxtimesec      int64
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
	Untimesec  int64  `json:"untimesec"`
	Cloudflare uint16 `json:"cloudflare"`
	Google     uint16 `json:"google"`
	Facebook   uint16 `json:"facebook"`
	X          uint16 `json:"x"`
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

type ServerResponse struct {
	Message string `json:"message"`
}

func main() {
	//connect to the sqlite db
	db, err := sql.Open("sqlite", "./isprestart.db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	//configure database for concurrency
	err = configureDatabase(db)
	if err != nil {
		log.Fatal(err)
	}

	//create tables if they dont exist
	err = createTables(db)
	if err != nil {
		log.Fatal(err)
	}

	go startGPIOlogic(db)

	pingChannel := make(chan map[string]int)
	go pingTest(pingChannel)             // start pinging
	go PrintPingResults(db, pingChannel) // save ping results to db
	go retriveSettings(db)               // start retrieving settings or save default values to db

	http.HandleFunc("/api/ping-data", pingDataHandler(db))

	fmt.Println("Server listening on port 8081")
	http.ListenAndServe(":8081", nil)

	select {} // keep the program running
}

func startGPIOlogic(db *sql.DB) {
	// Initialize ISP states - check if table has data, if not create defaults
	err := initializeISPStates(db)
	if err != nil {
		fmt.Println("Error initializing ISP states:", err)
		log.Fatal(err)
	}

	// Load initial data into cache
	err = refreshISPStateCache(db)
	if err != nil {
		fmt.Println("Error loading initial ISP states into cache:", err)
		log.Fatal(err)
	}

	fmt.Println("GPIO logic started with cached ISP states")

	// Cache refresh interval (every 60 seconds instead of 5)
	cacheRefreshInterval := 60 * time.Second
	
	// Create ticker for periodic checks (as backup)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-ispStateChangeNotifier:
			// ISP state changed - process immediately
			fmt.Println("ISP state change notification received")
			err := refreshISPStateCache(db)
			if err != nil {
				fmt.Println("Error refreshing ISP state cache after notification:", err)
			} else {
				fmt.Println("ISP state cache refreshed from database after change notification")
			}
			processGPIOLogic()
			
		case <-ticker.C:
			// Periodic check (backup mechanism)
			if ispStateCache.isStale(cacheRefreshInterval) {
				err := refreshISPStateCache(db)
				if err != nil {
					fmt.Println("Error refreshing ISP state cache:", err)
				} else {
					fmt.Println("ISP state cache refreshed from database (periodic)")
				}
			}
			processGPIOLogic()
		}
	}
}

func processGPIOLogic() {
	// Get current states from cache (no database query)
	primary, secondary, initialized := ispStateCache.getStates()
	if !initialized {
		fmt.Println("ISP state cache not initialized, skipping GPIO processing")
		return
	}

	fmt.Println("Primary ISP State (cached):", primary)
	fmt.Println("Secondary ISP State (cached):", secondary)
	
	// TODO: Add actual GPIO logic here based on the states
	// Example GPIO logic:
	// - Check if primary.PowerState is false and if enough time has passed since off
	// - Control GPIO pins based on power states
	// - Handle restart timers and durations
	
	currentTime := time.Now().Unix()
	
	// Example: If ISP should be turned back on
	if !primary.PowerState && primary.OffUntilUxtimesec > 0 && currentTime >= primary.OffUntilUxtimesec {
		fmt.Printf("Primary ISP should be turned back on (off until %d, now %d)\n", primary.OffUntilUxtimesec, currentTime)
		// TODO: Add GPIO pin control here
	}
	
	if !secondary.PowerState && secondary.OffUntilUxtimesec > 0 && currentTime >= secondary.OffUntilUxtimesec {
		fmt.Printf("Secondary ISP should be turned back on (off until %d, now %d)\n", secondary.OffUntilUxtimesec, currentTime)
		// TODO: Add GPIO pin control here
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
				fmt.Printf("Database busy/locked, retrying... (error: %v)\n", lastErr)
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
		
		// Notify GPIO logic of state change (non-blocking)
		notifyISPStateChange()
		
		return nil
	}, 3, 100*time.Millisecond) // 3 retries with 100ms delay
}

func pingDataHandler(db *sql.DB) func(http.ResponseWriter, *http.Request) { //im not sure if this is a bad idea
	return func(w http.ResponseWriter, r *http.Request) {
		//fmt.Printf("Received %s request to %s\n", r.Method, r.URL.Path)
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
				http.Error(w, err.Error(), http.StatusBadRequest)
				fmt.Println("Error decoding JSON payload:", err)
				fmt.Println("Payload:", payload)
				return
			}

			fmt.Println("Received POST request with payload:", payload)

			_, autorestartFound := payload["autorestart"]
			_, rowsRequested := payload["Rows"]
			_, dailyRestart := payload["daily"]     // payload will be like {"hour": 0, "min": 0, "sec": 0 }
			_, weeklyRestart := payload["weekly"]   // payload will be like {"dayinweek": 1, "hour": 0, "min": 0, "sec": 0 }
			_, monthlyRestart := payload["monthly"] // payload will be like {"weekinmonth": 1, "dayinweek": 1, "hour": 0, "min": 0, "sec": 0 }

			if autorestartFound {
				// handle autorestart request here where the toggle button is turned off
				//fmt.Println("Autorestart found:", autorestartFound)
				//fmt.Println("Received autorestart request with payload:", payload["autorestart"])
				if payload["autorestart"] == false || payload["autorestart"] == 0 {
					_, err = db.Exec("UPDATE autorestart SET autorestart = ?, hour=?, min=?, sec=?, daily=?, weekly=?, monthly=?, dayinweek=?, weekinmonth=? WHERE ROWID = 0", payload["autorestart"], 0, 0, 0, 0, 0, 0, 0, 0)
					if err != nil {
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
					fmt.Println("Autorestart turned off")
				}

				w.Header().Set("Access-Control-Allow-Origin", "*") // Include this header for all responses
				w.Header().Set("Content-Type", "application/json") // Include this header for JSON responses
				w.WriteHeader(http.StatusOK)

				fmt.Fprint(w, "Autorestart updated successfully")
			} else if dailyRestart {
				// handle daily restart request here
				fmt.Println("Received daily restart request with payload:", payload["daily"])
				values := payload["daily"].([]interface{})

				// Ensure you handle the conversion from interface{} to appropriate types.
				hour, _ := values[0].(float64)
				minute, _ := values[1].(float64)
				second, _ := values[2].(float64)

				stmt, err := db.Prepare("UPDATE autorestart SET autorestart=?, hour=?, min=?, sec=?, daily=?, weekly=?, monthly=?, dayinweek=?, weekinmonth=? WHERE ROWID=0")
				if err != nil {
					log.Fatal(err)
				}
				defer stmt.Close()

				// Execute the statement with values for the specified columns
				_, err = stmt.Exec(1, int(hour), int(minute), int(second), 1, 0, 0, 0, 0)
				if err != nil {
					log.Fatal(err)
				}

				w.Header().Set("Access-Control-Allow-Origin", "*") // Include this header for all responses
				w.Header().Set("Content-Type", "application/json") // Include this header for JSON responses
				w.WriteHeader(http.StatusOK)

				fmt.Fprint(w, "Daily restart updated successfully")

			} else if weeklyRestart {
				// handle weekly restart request here
				fmt.Println("Received weekly restart request with payload:", payload["weekly"])
				values := payload["weekly"].([]interface{})

				// Ensure you handle the conversion from interface{} to appropriate types.
				dayinweek, _ := values[0].(float64)
				hour, _ := values[1].(float64)
				minute, _ := values[2].(float64)
				second, _ := values[3].(float64)

				stmt, err := db.Prepare("UPDATE autorestart SET autorestart=?, hour=?, min=?, sec=?, daily=?, weekly=?, monthly=?, dayinweek=?, weekinmonth=? WHERE ROWID=0")
				if err != nil {
					log.Fatal(err)
				}
				defer stmt.Close()

				// Execute the statement with values for the specified columns
				_, err = stmt.Exec(1, int(hour), int(minute), int(second), 0, 1, 0, int(dayinweek), 0)
				if err != nil {
					log.Fatal(err)
				}

				w.Header().Set("Access-Control-Allow-Origin", "*") // Include this header for all responses
				w.Header().Set("Content-Type", "application/json") // Include this header for JSON responses
				w.WriteHeader(http.StatusOK)

				fmt.Fprint(w, "Weekly restart updated successfully")

			} else if monthlyRestart {
				// handle monthly restart request here
				fmt.Println("Received monthly restart request with payload:", payload["monthly"])
				values := payload["monthly"].([]interface{})

				// Ensure you handle the conversion from interface{} to appropriate types.

				weekinmonth, _ := values[0].(float64)
				dayinweek, _ := values[1].(float64)
				hour, _ := values[2].(float64)
				minute, _ := values[3].(float64)
				second, _ := values[4].(float64)

				stmt, err := db.Prepare("UPDATE autorestart SET autorestart=?, hour=?, min=?, sec=?, daily=?, weekly=?, monthly=?, dayinweek=?, weekinmonth=? WHERE ROWID=0")
				if err != nil {
					log.Fatal(err)
				}
				defer stmt.Close()

				// Execute the statement with values for the specified columns
				_, err = stmt.Exec(1, int(hour), int(minute), int(second), 0, 0, 1, int(dayinweek), int(weekinmonth))
				if err != nil {
					log.Fatal(err)
				}

				w.Header().Set("Access-Control-Allow-Origin", "*") // Include this header for all responses
				w.Header().Set("Content-Type", "application/json") // Include this header for JSON responses
				w.WriteHeader(http.StatusOK)

				fmt.Fprint(w, "Monthly restart updated successfully")

			} else if rowsRequested {
				///////////////////////////////////////////
				rows, err := db.Query("SELECT uxtimesec, cloudflare, google, facebook, x FROM pings ORDER BY uxtimesec DESC LIMIT ?", payload["Rows"])
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				defer rows.Close()

				result := make([]PingData, 0)

				for rows.Next() {
					var data PingData
					err = rows.Scan(&data.Untimesec, &data.Cloudflare, &data.Google, &data.Facebook, &data.X)
					if err != nil {
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
					result = append(result, data)
				}
				/////////////////////////////////

				//fmt.Println("++++++++++++++++++++++++")

				w.Header().Set("Access-Control-Allow-Origin", "*") // Include this header for all responses
				w.Header().Set("Content-Type", "application/json") // Include this header for JSON responses

				json.NewEncoder(w).Encode(result)
			}
		}

		if r.Method == "GET" {

			fmt.Println("Received GET request with payload:", payload)

			params := r.URL.Query()
			fmt.Println("Received autorestart query:", params)

			autorestartQuery := params.Get("pagestate")

			if autorestartQuery != "" {
				// Handle get request for button state request data
				var autorestart Autorestart
				err := db.QueryRow("SELECT ROWID, uxtimesec, autorestart, hour, min, sec, daily, weekly, monthly, dayinweek, weekinmonth FROM autorestart WHERE ROWID = 0").
					Scan(&autorestart.ROWID, &autorestart.Uxtimesec, &autorestart.Autorestart, &autorestart.Hour, &autorestart.Min, &autorestart.Sec, &autorestart.Daily, &autorestart.Weekly, &autorestart.Monthly, &autorestart.Dayinweek, &autorestart.Weekinmonth)
				if err != nil {
					http.Error(w, fmt.Sprintf("Error fetching autorestart: %v", err), http.StatusInternalServerError)
					return
				}
				w.Header().Set("Access-Control-Allow-Origin", "*") // Include this header for all responses
				w.Header().Set("Content-Type", "application/json") // Include this header for JSON responses
				fmt.Println("Sending autorestart data:", autorestart)
				json.NewEncoder(w).Encode(autorestart)
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

	// Set busy timeout to handle lock contention (5 seconds)
	_, err = db.Exec("PRAGMA busy_timeout=5000")
	if err != nil {
		return fmt.Errorf("failed to set busy timeout: %v", err)
	}

	// Configure connection pooling for optimal concurrency
	db.SetMaxOpenConns(10)  // Maximum number of open connections
	db.SetMaxIdleConns(5)   // Maximum number of idle connections
	db.SetConnMaxLifetime(time.Hour) // Connection max lifetime

	fmt.Println("Database configured for concurrency with WAL mode and connection pooling")
	return nil
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
            uxtimesec INTEGER PRIMARY KEY,
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
            reason TEXT NOT NULL
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

	for _, err := range []error{err, err1, err2, err3} {
		if err != nil {
			return fmt.Errorf("error creating table: %v", err)
		}
	}

	return nil
}

func PrintPingResults(db *sql.DB, pingResults chan map[string]int) {

	for result := range pingResults {
		log.Println(result)
		now := time.Now()
		unixTimestamp := now.Unix()
		log.Println(unixTimestamp)
		cloudflareMS := result["cloudflare"]
		googleMS := result["google"]
		facebookMS := result["facebook"]
		xMS := result["x"]
		
		// Use retry logic for ping data insertion
		err := retryDatabaseOperation(func() error {
			query := `
				INSERT INTO pings (uxtimesec, cloudflare, google, facebook, x)
				VALUES (?, ?, ?, ?, ?);
			`
			_, err := db.Exec(query, unixTimestamp, cloudflareMS, googleMS, facebookMS, xMS)
			return err
		}, 3, 50*time.Millisecond) // 3 retries with 50ms delay for ping data
		
		if err != nil {
			log.Printf("error inserting ping entry after retries: %v", err)
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

	for range ticker.C {
		results := make(map[string]int)

		for website, ip := range pingThese {
			cmd := exec.Command("ping", "-c", "1", ip) //change to -c on linux, and -n on windows

			output, err := cmd.CombinedOutput()
			if err != nil {
				results[website] = -1
				continue
			}
			//log.Println(string(output))
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
					time, err := strconv.ParseFloat(digits+"."+decimal, 64)
					if err != nil {
						fmt.Println(err)
						continue
					}
					roundedTime := int(math.Round(time))
					if roundedTime < 32000 {
						results[website] = roundedTime
					} else {
						results[website] = -2
					}
					break
				}
			}
		}

		pingResults <- results
	}
}

func retriveSettings(db *sql.DB) {
	// query the database for the autorestart table
	// check the table for a any amount of rows, if it has more than 0 rows, then retrieve the values
	// if it has 0 rows, then insert the default values

	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM autorestart").Scan(&count)
	if err != nil {
		fmt.Println(err)
		return
	}

	if count > 0 {
		//"Table is not empty" - lets retrieve the values
		fmt.Println("Table is not empty, retrieving values")
		var uxtimesec, autorestart, hour, min, sec, daily, weekly, monthly, dayinweek, weekinmonth int

		err = db.QueryRow("SELECT uxtimesec, autorestart, hour, min, sec, daily, weekly, monthly, dayinweek, weekinmonth FROM autorestart").Scan(&uxtimesec, &autorestart, &hour, &min, &sec, &daily, &weekly, &monthly, &dayinweek, &weekinmonth)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Retrieved values: uxtimesec=%d, autorestart=%d, hour=%d, min=%d, sec=%d, daily=%d, weekly=%d, monthly=%d, dayinweek=%d, weekinmonth=%d\n", uxtimesec, autorestart, hour, min, sec, daily, weekly, monthly, dayinweek, weekinmonth)

	} else {
		//"Table is empty" - lets populate it with default values
		fmt.Println("Table is empty, inserting default values")
		stmt, err := db.Prepare("INSERT INTO autorestart (uxtimesec, autorestart, hour, min, sec, daily, weekly, monthly, dayinweek, weekinmonth) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		if err != nil {
			log.Fatal(err)
		}
		defer stmt.Close()

		_, err = stmt.Exec(0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
		if err != nil {
			log.Fatal(err)
		}
	}

}
