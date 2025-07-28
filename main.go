// This file is compiled for raspberry pi 3b using the command:
// GOOS=linux GOARCH=arm GOARM=6 go build main.go

// the file is built for linux, using -c for ping, and parsing decimal rtt times in ms
// to insert into a sqlite database

package main

import (
	"database/sql"
	"log"
	"net/http"
	"runtime"

	_ "modernc.org/sqlite"
)

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
	go monitorNetworkStatus(db)          // monitor network status every minute

	http.HandleFunc("/api/ping-data", pingDataHandler(db))

	logInfo("Server listening on port 8081")
	logInfo("Command interface available - type 'help' for commands")
	logInfo("Default log level: %s (use 'loglevel <level>' to change)", logLevelNames[console.logLevel])
	http.ListenAndServe(":8081", nil)

	select {} // keep the program running
}