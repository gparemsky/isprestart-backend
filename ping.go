package main

import (
	"database/sql"
	"fmt"
	"math"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

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
	defer ticker.Stop()
	
	// Function to perform ping test
	performPingTest := func() {
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
	
	// Initial ping test immediately on startup
	fmt.Printf("[PING] Starting initial ping test...\n")
	performPingTest()
	
	// Continue with periodic pings
	for range ticker.C {
		performPingTest()
	}
}