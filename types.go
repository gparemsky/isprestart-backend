package main

import (
	"sync"
	"time"
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

// Channel for notifying GPIO logic of ISP state changes
var ispStateChangeNotifier = make(chan bool, 1)

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
	Uxtimesec       int64   `json:"uxtimesec"`
	Reason          string  `json:"reason"`
	IspID           *int    `json:"isp_id,omitempty"`
	IspName         *string `json:"isp_name,omitempty"`
	RestartType     *string `json:"restart_type,omitempty"`
	DurationMinutes *int    `json:"duration_minutes,omitempty"`
	ClientIP        *string `json:"client_ip,omitempty"`
}

type ServerResponse struct {
	ISPStates []ISPState `json:"isp_states"`
}