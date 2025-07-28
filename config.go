package main

import (
	"bufio"
	"database/sql"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// ServerConfig holds server configuration
type ServerConfig struct {
	ServerIP   string
	ServerPort int
}

// getServerConfig retrieves or prompts for server configuration
func getServerConfig(db *sql.DB) (*ServerConfig, error) {
	config := &ServerConfig{}
	
	// Try to get server IP from database
	var serverIP string
	err := db.QueryRow("SELECT value FROM server_settings WHERE key = 'server_ip'").Scan(&serverIP)
	if err != nil {
		if err == sql.ErrNoRows {
			// No server IP configured, prompt user
			serverIP, err = promptForServerIP()
			if err != nil {
				return nil, fmt.Errorf("failed to get server IP: %v", err)
			}
			
			// Save to database
			err = saveServerSetting(db, "server_ip", serverIP)
			if err != nil {
				return nil, fmt.Errorf("failed to save server IP: %v", err)
			}
			
			logInfo("Server IP saved to database: %s", serverIP)
		} else {
			return nil, fmt.Errorf("failed to query server IP: %v", err)
		}
	}
	config.ServerIP = serverIP
	
	// Try to get server port from database
	var portStr string
	err = db.QueryRow("SELECT value FROM server_settings WHERE key = 'server_port'").Scan(&portStr)
	if err != nil {
		if err == sql.ErrNoRows {
			// No server port configured, use default and save
			config.ServerPort = 8081
			err = saveServerSetting(db, "server_port", "8081")
			if err != nil {
				return nil, fmt.Errorf("failed to save server port: %v", err)
			}
			logInfo("Server port set to default: %d", config.ServerPort)
		} else {
			return nil, fmt.Errorf("failed to query server port: %v", err)
		}
	} else {
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, fmt.Errorf("invalid port in database: %s", portStr)
		}
		config.ServerPort = port
	}
	
	return config, nil
}

// promptForServerIP prompts the user to enter the server IP address
func promptForServerIP() (string, error) {
	// First, try to auto-detect local IP addresses
	localIPs := getLocalIPAddresses()
	
	fmt.Println("\n=== Server IP Configuration ===")
	fmt.Println("No server IP address is configured.")
	
	if len(localIPs) > 0 {
		fmt.Println("Detected local IP addresses:")
		for i, ip := range localIPs {
			fmt.Printf("  %d) %s\n", i+1, ip)
		}
		fmt.Println("  0) Enter custom IP address")
		fmt.Print("Select an option (1-" + fmt.Sprintf("%d", len(localIPs)) + " or 0): ")
		
		reader := bufio.NewReader(os.Stdin)
		input, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		
		choice := strings.TrimSpace(input)
		if choice == "0" {
			// Custom IP
			fmt.Print("Enter server IP address: ")
			customIP, err := reader.ReadString('\n')
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(customIP), nil
		} else {
			// Selected from list
			choiceNum, err := strconv.Atoi(choice)
			if err != nil || choiceNum < 1 || choiceNum > len(localIPs) {
				return "", fmt.Errorf("invalid selection")
			}
			return localIPs[choiceNum-1], nil
		}
	} else {
		// No local IPs detected, ask for manual entry
		fmt.Print("Enter server IP address: ")
		reader := bufio.NewReader(os.Stdin)
		input, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(input), nil
	}
}

// getLocalIPAddresses returns a list of local IP addresses
func getLocalIPAddresses() []string {
	var ips []string
	
	interfaces, err := net.Interfaces()
	if err != nil {
		return ips
	}
	
	for _, iface := range interfaces {
		// Skip loopback and down interfaces
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			
			// Only include IPv4 addresses
			if ip != nil && ip.To4() != nil {
				ips = append(ips, ip.String())
			}
		}
	}
	
	return ips
}

// saveServerSetting saves a server setting to the database
func saveServerSetting(db *sql.DB, key, value string) error {
	currentTime := time.Now().Unix()
	
	_, err := db.Exec(`
		INSERT OR REPLACE INTO server_settings (key, value, updated_at) 
		VALUES (?, ?, ?)`,
		key, value, currentTime)
	
	return err
}

// getServerSetting retrieves a server setting from the database
func getServerSetting(db *sql.DB, key string) (string, error) {
	var value string
	err := db.QueryRow("SELECT value FROM server_settings WHERE key = ?", key).Scan(&value)
	return value, err
}

// updateServerIP allows updating the server IP address
func updateServerIP(db *sql.DB, newIP string) error {
	err := saveServerSetting(db, "server_ip", newIP)
	if err != nil {
		return err
	}
	logInfo("Server IP updated to: %s", newIP)
	return nil
}