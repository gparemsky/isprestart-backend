package main

import (
	"database/sql"
	"fmt"
	"time"
)

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

	_, err6 := db.Exec(`
		CREATE TABLE IF NOT EXISTS server_settings (
		    key TEXT PRIMARY KEY,
		    value TEXT NOT NULL,
		    updated_at INTEGER NOT NULL
		);
	`)

	for _, err := range []error{err, err1, err2, err3, err4, err5, err6} {
		if err != nil {
			return fmt.Errorf("error creating table: %v", err)
		}
	}

	return nil
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
			}
		}
		return nil
	}, 3, 100*time.Millisecond)
}

func retryDatabaseOperation(operation func() error, maxRetries int, retryDelay time.Duration) error {
	var err error
	for i := 0; i < maxRetries; i++ {
		err = operation()
		if err == nil {
			return nil
		}
		
		// Check if it's a database locked error
		if err.Error() == "database is locked" || err.Error() == "database table is locked" {
			logWarn("Database locked (attempt %d/%d), retrying in %v...", i+1, maxRetries, retryDelay)
			time.Sleep(retryDelay)
			continue
		}
		
		// For other errors, don't retry
		return err
	}
	return fmt.Errorf("operation failed after %d retries: %v", maxRetries, err)
}

// executeTransaction wraps a function in a database transaction with automatic rollback on error
func executeTransaction(db *sql.DB, operation func(*sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %v", err)
	}
	
	// Ensure rollback on panic
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
			panic(r)
		}
	}()
	
	if err := operation(tx); err != nil {
		tx.Rollback()
		return err
	}
	
	return tx.Commit()
}

func logEvent(db *sql.DB, reason string) error {
	_, err := db.Exec(`
		INSERT INTO logs (uxtimesec, reason)
		VALUES (?, ?)`,
		time.Now().Unix(), reason)
	return err
}

func logISPRestartAction(db *sql.DB, reason string, ispID int, ispName string, restartType string, durationMinutes int, clientIP string) error {
	_, err := db.Exec(`
		INSERT INTO logs (uxtimesec, reason, isp_id, isp_name, restart_type, duration_minutes, client_ip)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		time.Now().Unix(), reason, ispID, ispName, restartType, durationMinutes, clientIP)
	return err
}

func updateLastRestartTime(db *sql.DB, ispID int, restartTime int64) error {
	_, err := db.Exec(`
		INSERT OR REPLACE INTO isp_restart_times (isp_id, last_restart_uxtimesec)
		VALUES (?, ?)`,
		ispID, restartTime)
	return err
}