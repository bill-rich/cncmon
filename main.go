package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	zhreader "github.com/bill-rich/cncmon/pkg/memmon"
)

var debugEnabled bool

func debugLog(format string, args ...interface{}) {
	if debugEnabled {
		fmt.Printf("DEBUG: "+format, args...)
	}
}

// QueuedAPIRequest represents an API request that needs to be sent
type QueuedAPIRequest struct {
	APIURL      string
	Seed        string
	TimeCode    uint32
	Current     zhreader.PollResult
	Previous    zhreader.PollResult
	IsFirstPoll bool
	SuccessChan chan bool // Channel to signal success/failure
}

// EventData stores event information with money values for API transmission
type EventData struct {
	TimeCode    uint32   `json:"timeCode"`
	OrderCode   uint32   `json:"orderCode"`
	OrderName   string   `json:"orderName"`
	PlayerID    uint32   `json:"playerId"`
	PlayerName  string   `json:"playerName,omitempty"`
	PlayerMoney [8]int32 `json:"playerMoney"`
	Timestamp   string   `json:"timestamp"`
}

// ReplaySession stores complete replay session data
type ReplaySession struct {
	Seed       string      `json:"seed"`
	Events     []EventData `json:"events"`
	EventCount int         `json:"eventCount"`
}

func main() {
	// Get user's home directory for default replay file path
	homeDir, err := os.UserHomeDir()
	if err != nil {
		homeDir = "C:\\Users\\User" // Fallback for Windows
	}

	// Default replay file path for Windows
	defaultReplayFile := filepath.Join(homeDir, "Documents", "Command and Conquer Generals Zero Hour Data", "Replays", "00000000.rep")

	// Parse command line arguments
	var (
		replayFile  = flag.String("file", defaultReplayFile, "Replay file to monitor")
		pollDelay   = flag.Duration("delay", 50*time.Millisecond, "Delay between memory polls (unused - now polls every 50ms)")
		timeout     = flag.Duration("timeout", 2*time.Minute, "Timeout for file inactivity before returning to waiting mode")
		apiURL      = flag.String("api", "https://cncstats.herokuapp.com", "API endpoint URL for sending money data")
		processName = flag.String("process", "generals.exe", "Process name to monitor (default: generals.exe)")
		help        = flag.Bool("help", false, "Show help information")
		seed        = flag.String("seed", "", "Manual seed value to use instead of reading from replay file")

		// File search flags
		searchFile    = flag.String("search-file", "", "Search for patterns in a static executable file")
		searchPattern = flag.String("search-pattern", "", "AOB pattern to search for (e.g., 'a1 ?? ?? ?? ?? 8b 40 0c 85 c0 74 78')")
		searchBinary  = flag.String("search-binary", "", "Binary pattern to search for (e.g., '10100001 ???????? ???????? ???????? ???????? 10001011 01***000 00001100 10000101 11****** 01110100 01111000')")
		searchMode    = flag.String("search-mode", "aob", "Search mode: 'aob' for AOB patterns, 'binary' for binary patterns")
		debugMode     = flag.Bool("debug", false, "Enable debug logging for troubleshooting")
	)
	flag.Parse()

	// Set debug mode globally
	debugEnabled = *debugMode

	// Show help if requested
	if *help {
		showHelp()
		return
	}

	// Handle file search mode
	if *searchFile != "" {
		handleFileSearch(*searchFile, *searchPattern, *searchBinary, *searchMode)
		return
	}

	// Validate required arguments
	if *replayFile == "" {
		fmt.Println("Error: replay file is required. Use -file flag to specify a replay file.")
		fmt.Println("Use -help for more information.")
		os.Exit(1)
	}

	fmt.Printf("Starting continuous monitoring of replay file: %s\n", *replayFile)
	fmt.Printf("Timeout: %v, Poll delay: %v\n", *timeout, *pollDelay)
	fmt.Println("Waiting for generals.exe process and replay file...")

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Continuous monitoring loop
	for {
		// Wait for the specified process to be available
		memReader, err := waitForGeneralsProcess(sigChan, *processName)
		if err != nil {
			fmt.Printf("Process monitoring interrupted: %v\n", err)
			return
		}

		// Production mode: wait for timecode to start increasing
		if !waitForTimecodeStart(memReader, sigChan) {
			memReader.Close()
			fmt.Println("Timecode monitoring interrupted. Exiting.")
			return
		}

		fmt.Printf("Timecode started increasing. Starting to monitor money values...\n")

		// Process money monitoring until completion or timeout
		eventCount := processMoneyMonitoring(memReader, *pollDelay, *timeout, *apiURL, *seed, sigChan)

		fmt.Printf("Money monitoring completed. Processed %d events.\n", eventCount)
		memReader.Close()
		fmt.Println("Returning to waiting mode for next game...")
		fmt.Println()
	}
}

// waitForGeneralsProcess waits for the specified process to be available
func waitForGeneralsProcess(sigChan <-chan os.Signal, processName string) (*zhreader.Reader, error) {
	fmt.Printf("Waiting for %s process...\n", processName)

	for {
		// Check for interrupt signals
		select {
		case sig := <-sigChan:
			return nil, fmt.Errorf("received signal %v", sig)
		default:
			// Continue with process checking
		}

		// Try to initialize memory reader
		memReader, err := zhreader.Init(processName)
		if err == nil {
			fmt.Printf("%s process found and memory reader initialized successfully\n", processName)
			return memReader, nil
		}

		// Process not found, wait a bit before trying again
		time.Sleep(2 * time.Second)
	}
}

// waitForTimecodeStart waits for the timecode to start increasing, indicating game start
func waitForTimecodeStart(memReader *zhreader.Reader, sigChan <-chan os.Signal) bool {
	fmt.Println("Waiting for timecode to start increasing (game start)...")

	var lastTimecode uint32 = 0
	var increaseCount int = 0
	const requiredIncreases = 3 // Need 3 consecutive increases to consider the game started

	for {
		// Check for interrupt signals
		select {
		case sig := <-sigChan:
			fmt.Printf("\nReceived signal %v. Shutting down gracefully...\n", sig)
			return false
		default:
			// Continue with timecode checking
		}

		// Get current timecode from memory
		currentTimecode, err := memReader.GetTimecode()
		if err != nil {
			fmt.Printf("Warning: Failed to read timecode: %v\n", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}

		// Only start once we've seen requiredIncreases consecutive increases in timecode.
		if lastTimecode != 0 {
			if currentTimecode > lastTimecode {
				increaseCount++
				fmt.Printf("Timecode increased from %d to %d (%d/%d consecutive increases)\n",
					lastTimecode, currentTimecode, increaseCount, requiredIncreases)
				if increaseCount >= requiredIncreases {
					// Check money values - don't start if they match the default/initial state
					pollResult := memReader.Poll()
					expectedMoney := [8]int32{10000, 10000, 10000, 10000, 10000, 10000, 10000, -1}
					if pollResult.Money == expectedMoney {
						fmt.Printf("Money values match default state [10000, 10000, 10000, 10000, 10000, 10000, 10000, -1] - not starting monitoring, resetting detection\n")
						increaseCount = 0
						lastTimecode = 0 // Reset to start fresh
						continue
					}
					fmt.Printf("Timecode has increased %d times consecutively - game started!\n", requiredIncreases)
					return true
				}
			} else if currentTimecode < lastTimecode {
				// Timecode went backwards – reset detection and wait for a fresh sequence
				fmt.Printf("Timecode decreased from %d to %d - resetting start detection and waiting for new game...\n",
					lastTimecode, currentTimecode)
				increaseCount = 0
			}
			// If equal, do nothing special – just keep waiting
		}

		lastTimecode = currentTimecode
		fmt.Printf("Current timecode: %d (waiting for %d consecutive increases...)\n", currentTimecode, requiredIncreases)
		time.Sleep(500 * time.Millisecond)
	}
}

// pollResultChanged checks if any field in the PollResult has changed compared to the previous one
func pollResultChanged(current, previous zhreader.PollResult) bool {
	// Compare all array fields
	if current.Money != previous.Money {
		return true
	}
	if current.MoneyEarned != previous.MoneyEarned {
		return true
	}
	if current.UnitsBuilt != previous.UnitsBuilt {
		return true
	}
	if current.UnitsLost != previous.UnitsLost {
		return true
	}
	if current.BuildingsBuilt != previous.BuildingsBuilt {
		return true
	}
	if current.BuildingsLost != previous.BuildingsLost {
		return true
	}
	if current.PowerTotal != previous.PowerTotal {
		return true
	}
	if current.PowerUsed != previous.PowerUsed {
		return true
	}
	if current.RadarsBuilt != previous.RadarsBuilt {
		return true
	}
	if current.SearchAndDestroy != previous.SearchAndDestroy {
		return true
	}
	if current.HoldTheLine != previous.HoldTheLine {
		return true
	}
	if current.Bombardment != previous.Bombardment {
		return true
	}
	if current.XP != previous.XP {
		return true
	}
	if current.XPLevel != previous.XPLevel {
		return true
	}
	if current.GeneralsPointsUsed != previous.GeneralsPointsUsed {
		return true
	}
	if current.GeneralsPointsTotal != previous.GeneralsPointsTotal {
		return true
	}
	if current.TechBuildingsCaptured != previous.TechBuildingsCaptured {
		return true
	}
	if current.FactionBuildingsCaptured != previous.FactionBuildingsCaptured {
		return true
	}
	// Compare 2D arrays
	if current.UnitsKilled != previous.UnitsKilled {
		return true
	}
	if current.BuildingsKilled != previous.BuildingsKilled {
		return true
	}
	return false
}

// apiRequestWorker processes API requests from the queue
func apiRequestWorker(requestQueue <-chan QueuedAPIRequest, wg *sync.WaitGroup) {
	defer wg.Done()
	for req := range requestQueue {
		err := sendMoneyData(req.APIURL, req.Seed, req.TimeCode, req.Current, req.Previous, req.IsFirstPoll)
		if err != nil {
			fmt.Printf("Warning: Failed to send money data via API: %v\n", err)
			if req.SuccessChan != nil {
				req.SuccessChan <- false
			}
		} else {
			fmt.Println("Money data sent successfully")
			if req.SuccessChan != nil {
				req.SuccessChan <- true
			}
		}
	}
}

// processMoneyMonitoring monitors money values and timecode without replay file dependency
func processMoneyMonitoring(memReader *zhreader.Reader, pollDelay time.Duration, timeout time.Duration, apiURL string, manualSeed string, sigChan <-chan os.Signal) int {
	debugLog("Starting processMoneyMonitoring...\n")

	// Initialize monitoring state
	var lastSentPollResult zhreader.PollResult
	var lastSentPollResultMutex sync.Mutex // Protect lastSentPollResult from race conditions
	var lastTimecode uint32
	eventCount := 0
	var lastEventTimeMutex sync.Mutex
	lastEventTime := time.Now()
	firstPoll := true // Track if this is the first poll to initialize lastSentPollResult

	// Create API request queue with buffer to prevent blocking
	// Buffer size of 100 should be sufficient for most cases
	requestQueue := make(chan QueuedAPIRequest, 100)
	var wg sync.WaitGroup

	// Start API request worker
	wg.Add(1)
	go apiRequestWorker(requestQueue, &wg)

	// Ensure worker goroutine is cleaned up on exit
	defer func() {
		close(requestQueue)
		wg.Wait()
	}()

	// Start polling timer
	pollTicker := time.NewTicker(pollDelay)
	defer pollTicker.Stop()

	fmt.Printf("Starting money monitoring (polling every %v)...\n", pollDelay)
	loopCount := 0
	startTime := time.Now()

	for {
		log.Printf("Money monitoring: iteration %d, events: %d", loopCount, eventCount)
		loopCount++
		if loopCount%20 == 0 { // Log every 10 seconds (20 * 500ms)
			fmt.Printf("Money monitoring: iteration %d, events: %d\n", loopCount, eventCount)
		}

		// Check if we've been waiting too long without any events
		if time.Since(startTime) > 30*time.Second && eventCount == 0 {
			fmt.Printf("No money changes detected after 30 seconds, this might indicate an issue\n")
		}

		log.Printf("Money monitoring: waiting for poll ticker")
		// Use select to check for signals while waiting for ticker
		select {
		case <-sigChan:
			fmt.Printf("\nReceived interrupt signal. Shutting down gracefully...\n")
			panic("received interrupt signal")
		case <-pollTicker.C:
			log.Printf("Money monitoring: poll ticker received")
		}
		// Poll memory for money values
		vals := memReader.Poll()

		// Check if all values are -1, which indicates the process may have gone away
		allInvalid := true
		for _, val := range vals.Money {
			if val != -1 {
				allInvalid = false
				break
			}
		}
		if allInvalid {
			fmt.Println("Warning: All memory values are invalid. Generals.exe process may have gone away.")
			fmt.Println("Returning to process monitoring...")
			return eventCount
		}

		// Get current timecode
		currentTimecode, err := memReader.GetTimecode()
		if err != nil {
			fmt.Printf("Warning: Failed to get timecode: %v\n", err)
			// Continue with money monitoring even if timecode fails
		} else {
			// If timecode ever decreases, assume this game session has ended
			// and return so the caller can wait for a new game.
			if lastTimecode != 0 && currentTimecode < lastTimecode {
				fmt.Printf("Timecode decreased from %d to %d - ending monitoring and returning to waiting mode...\n",
					lastTimecode, currentTimecode)
				return eventCount
			}
			lastTimecode = currentTimecode
			debugLog("Current timecode: %d\n", currentTimecode)
		}

		// Check if any values in PollResult have changed
		// On first poll, always send to initialize the baseline
		lastSentPollResultMutex.Lock()
		lastSentCopy := lastSentPollResult
		lastSentPollResultMutex.Unlock()
		valuesChanged := firstPoll || pollResultChanged(vals, lastSentCopy)

		if valuesChanged {
			// Convert PollResult to json and log only when values change
			valsJSON, _ := json.Marshal(vals)
			log.Printf("Memory poll completed, got values: %s", string(valsJSON))
			eventCount++
			isFirstPoll := firstPoll // Capture the flag before it's updated
			if firstPoll {
				fmt.Printf("Initial poll (event %d) - queuing data (timecode: %d)...\n", eventCount, lastTimecode)
				firstPoll = false
			} else {
				fmt.Printf("PollResult changed (event %d) - queuing data (timecode: %d)...\n", eventCount, lastTimecode)
			}

			// Determine seed to use for API calls
			seedToUse := memReader.GetSeed()
			if manualSeed != "" {
				seedToUse = manualSeed
			}

			// Create success channel to track API call result
			successChan := make(chan bool, 1)

			// Queue the API request (non-blocking if buffer has space)
			select {
			case requestQueue <- QueuedAPIRequest{
				APIURL:      apiURL,
				Seed:        seedToUse,
				TimeCode:    lastTimecode,
				Current:     vals,
				Previous:    lastSentCopy,
				IsFirstPoll: isFirstPoll,
				SuccessChan: successChan,
			}:
				// Request queued successfully
				// Handle success/failure asynchronously
				go func(currentVals zhreader.PollResult) {
					success := <-successChan
					if success {
						// Update last sent PollResult only after successful send
						lastSentPollResultMutex.Lock()
						lastSentPollResult = currentVals
						lastSentPollResultMutex.Unlock()
						lastEventTimeMutex.Lock()
						lastEventTime = time.Now()
						lastEventTimeMutex.Unlock()
					}
				}(vals)
			default:
				// Queue is full - log warning but continue polling
				fmt.Printf("Warning: API request queue is full, dropping request (event %d)\n", eventCount)
			}

			// Display memory values
			j, _ := json.Marshal(struct {
				P [8]int32 `json:"p"`
			}{vals.Money})
			fmt.Printf("Memory values: %s\n", string(j))
		}

		// Check for timeout due to inactivity
		lastEventTimeMutex.Lock()
		timeSinceLastEvent := time.Since(lastEventTime)
		lastEventTimeMutex.Unlock()
		if timeSinceLastEvent > timeout {
			fmt.Printf("\nTimeout reached (no events for %v). Processed %d events before timeout.\n", timeout, eventCount)
			return eventCount
		}
	}
}

// MoneyDataRequest represents the API request for player money data
// Only changed fields are included (except Seed and Timecode which are always included)
type MoneyDataRequest struct {
	Seed                     string       `json:"seed"`
	Timecode                 int64        `json:"timecode"`
	Money                    *[8]int32    `json:"money,omitempty"`
	MoneyEarned              *[8]int32    `json:"money_earned,omitempty"`
	UnitsBuilt               *[8]int32    `json:"units_built,omitempty"`
	UnitsLost                *[8]int32    `json:"units_lost,omitempty"`
	BuildingsBuilt           *[8]int32    `json:"buildings_built,omitempty"`
	BuildingsLost            *[8]int32    `json:"buildings_lost,omitempty"`
	BuildingsKilled          *[8][8]int32 `json:"buildings_killed,omitempty"`
	UnitsKilled              *[8][8]int32 `json:"units_killed,omitempty"`
	GeneralsPointsTotal      *[8]int32    `json:"generals_points_total,omitempty"`
	GeneralsPointsUsed       *[8]int32    `json:"generals_points_used,omitempty"`
	RadarsBuilt              *[8]int32    `json:"radars_built,omitempty"`
	SearchAndDestroy         *[8]int32    `json:"search_and_destroy,omitempty"`
	HoldTheLine              *[8]int32    `json:"hold_the_line,omitempty"`
	Bombardment              *[8]int32    `json:"bombardment,omitempty"`
	XP                       *[8]int32    `json:"xp,omitempty"`
	XPLevel                  *[8]int32    `json:"xp_level,omitempty"`
	TechBuildingsCaptured    *[8]int32    `json:"tech_buildings_captured,omitempty"`
	FactionBuildingsCaptured *[8]int32    `json:"faction_buildings_captured,omitempty"`
	PowerTotal               *[8]int32    `json:"power_total,omitempty"`
	PowerUsed                *[8]int32    `json:"power_used,omitempty"`
}

// sendMoneyData sends player money data to the API endpoint
// Only changed fields are included (except Seed and Timecode which are always included)
func sendMoneyData(apiURL string, seed string, timeCode uint32, current zhreader.PollResult, previous zhreader.PollResult, isFirstPoll bool) error {
	// Create the request payload with only changed fields
	request := MoneyDataRequest{
		Seed:     seed,
		Timecode: int64(timeCode),
	}

	// Only include fields that have changed (or on first poll, include all)
	if isFirstPoll || current.Money != previous.Money {
		request.Money = &current.Money
	}
	if isFirstPoll || current.MoneyEarned != previous.MoneyEarned {
		request.MoneyEarned = &current.MoneyEarned
	}
	if isFirstPoll || current.UnitsBuilt != previous.UnitsBuilt {
		request.UnitsBuilt = &current.UnitsBuilt
	}
	if isFirstPoll || current.UnitsLost != previous.UnitsLost {
		request.UnitsLost = &current.UnitsLost
	}
	if isFirstPoll || current.BuildingsBuilt != previous.BuildingsBuilt {
		request.BuildingsBuilt = &current.BuildingsBuilt
	}
	if isFirstPoll || current.BuildingsLost != previous.BuildingsLost {
		request.BuildingsLost = &current.BuildingsLost
	}
	if isFirstPoll || current.BuildingsKilled != previous.BuildingsKilled {
		request.BuildingsKilled = &current.BuildingsKilled
	}
	if isFirstPoll || current.UnitsKilled != previous.UnitsKilled {
		request.UnitsKilled = &current.UnitsKilled
	}
	if isFirstPoll || current.GeneralsPointsTotal != previous.GeneralsPointsTotal {
		request.GeneralsPointsTotal = &current.GeneralsPointsTotal
	}
	if isFirstPoll || current.GeneralsPointsUsed != previous.GeneralsPointsUsed {
		request.GeneralsPointsUsed = &current.GeneralsPointsUsed
	}
	if isFirstPoll || current.RadarsBuilt != previous.RadarsBuilt {
		request.RadarsBuilt = &current.RadarsBuilt
	}
	if isFirstPoll || current.SearchAndDestroy != previous.SearchAndDestroy {
		request.SearchAndDestroy = &current.SearchAndDestroy
	}
	if isFirstPoll || current.HoldTheLine != previous.HoldTheLine {
		request.HoldTheLine = &current.HoldTheLine
	}
	if isFirstPoll || current.Bombardment != previous.Bombardment {
		request.Bombardment = &current.Bombardment
	}
	if isFirstPoll || current.XP != previous.XP {
		request.XP = &current.XP
	}
	if isFirstPoll || current.XPLevel != previous.XPLevel {
		request.XPLevel = &current.XPLevel
	}
	if isFirstPoll || current.TechBuildingsCaptured != previous.TechBuildingsCaptured {
		request.TechBuildingsCaptured = &current.TechBuildingsCaptured
	}
	if isFirstPoll || current.FactionBuildingsCaptured != previous.FactionBuildingsCaptured {
		request.FactionBuildingsCaptured = &current.FactionBuildingsCaptured
	}
	if isFirstPoll || current.PowerTotal != previous.PowerTotal {
		request.PowerTotal = &current.PowerTotal
	}
	if isFirstPoll || current.PowerUsed != previous.PowerUsed {
		request.PowerUsed = &current.PowerUsed
	}

	// Marshal to JSON
	jsonData, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("failed to marshal money data: %w", err)
	}

	// Create HTTP request
	req, err := http.NewRequest("POST", apiURL+"/player-money", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	// Send the request
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	// Check response status
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("API request failed with status: %d", resp.StatusCode)
	}

	return nil
}

// handleFileSearch handles file pattern searching
func handleFileSearch(filePath, aobPattern, binaryPattern, searchMode string) {
	fmt.Printf("Searching for patterns in file: %s\n", filePath)
	fmt.Printf("Search mode: %s\n", searchMode)
	fmt.Println()

	var results *zhreader.FileSearchResults
	var err error

	if searchMode == "binary" {
		if binaryPattern == "" {
			fmt.Println("Error: Binary pattern is required when using binary search mode.")
			fmt.Println("Use -search-binary flag to specify the binary pattern.")
			os.Exit(1)
		}
		fmt.Printf("Binary pattern: %s\n", binaryPattern)
		results, err = zhreader.SearchBinaryPatternInFile(filePath, binaryPattern)
	} else {
		if aobPattern == "" {
			fmt.Println("Error: AOB pattern is required when using AOB search mode.")
			fmt.Println("Use -search-pattern flag to specify the AOB pattern.")
			os.Exit(1)
		}
		fmt.Printf("AOB pattern: %s\n", aobPattern)
		results, err = zhreader.SearchAOBPatternInFile(filePath, aobPattern)
	}

	if err != nil {
		fmt.Printf("Error searching file: %v\n", err)
		os.Exit(1)
	}

	if results.Found {
		fmt.Printf("✓ Found %d matching pattern(s)!\n", len(results.Results))
		fmt.Println()
		for i, result := range results.Results {
			fmt.Printf("Match %d:\n", i+1)
			fmt.Printf("  Location: %s\n", result.HexOffset)
			fmt.Printf("  Value: %s\n", result.Value)
			fmt.Printf("  Decimal offset: %d\n", result.Location)
			if i < len(results.Results)-1 {
				fmt.Println()
			}
		}
	} else {
		fmt.Printf("✗ Pattern not found in file.\n")
		os.Exit(1)
	}
}

func showHelp() {
	fmt.Println("CNC Monitor - Command and Conquer Replay Monitor with Memory Polling")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  cncmon [flags]")
	fmt.Println()
	fmt.Println("Flags:")
	fmt.Println("  -file string")
	fmt.Println("        Replay file to monitor (required)")
	fmt.Println("  -delay duration")
	fmt.Println("        Delay between memory polls (unused - now polls every 50ms)")
	fmt.Println("  -timeout duration")
	fmt.Println("        Timeout for file inactivity before returning to waiting mode (default: 2m)")
	fmt.Println("  -api string")
	fmt.Println("        API endpoint URL for sending money data (default: https://cncstats.herokuapp.com)")
	fmt.Println("  -process string")
	fmt.Println("        Process name to monitor (default: generals.exe)")
	fmt.Println("  -test")
	fmt.Println("        Test mode: process existing file immediately without waiting for file activity")
	fmt.Println("  -seed string")
	fmt.Println("        Manual seed value to use instead of reading from replay file")
	fmt.Println("  -help")
	fmt.Println("        Show this help information")
	fmt.Println()
	fmt.Println("File Search Mode:")
	fmt.Println("  -search-file string")
	fmt.Println("        Search for patterns in a static executable file")
	fmt.Println("  -search-pattern string")
	fmt.Println("        AOB pattern to search for (e.g., 'a1 ?? ?? ?? ?? 8b 40 0c 85 c0 74 78')")
	fmt.Println("  -search-binary string")
	fmt.Println("        Binary pattern to search for (e.g., '10100001 ???????? ???????? ???????? ???????? 10001011 01***000 00001100 10000101 11****** 01110100 01111000')")
	fmt.Println("  -search-mode string")
	fmt.Println("        Search mode: 'aob' for AOB patterns, 'binary' for binary patterns (default: aob)")
	fmt.Println("  -debug")
	fmt.Println("        Enable debug logging for troubleshooting")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  # Monitor replay file (default mode)")
	fmt.Println("  cncmon -file C:\\replay.rep")
	fmt.Println()
	fmt.Println("  # Monitor with manual seed")
	fmt.Println("  cncmon -seed \"my-custom-seed-123\"")
	fmt.Println()
	fmt.Println("  # Search for AOB pattern in executable")
	fmt.Println("  cncmon -search-file C:\\game\\generals.exe -search-pattern 'a1 ?? ?? ?? ?? 8b 40 0c 85 c0 74 78'")
	fmt.Println()
	fmt.Println("  # Search for binary pattern in executable")
	fmt.Println("  cncmon -search-file C:\\game\\generals.exe -search-binary '10100001 ???????? ???????? ???????? ???????? 10001011 01***000 00001100 10000101 11****** 01110100 01111000' -search-mode binary")
	fmt.Println()
	fmt.Println("This tool continuously monitors a Command and Conquer replay file and polls memory values")
	fmt.Println("from the running generals.exe process every 50ms. When money values change, it sends")
	fmt.Println("events to the API using the last seen timecode from replay events. Multiple money changes")
	fmt.Println("between replay events increment the timecode. Replay events are still used to detect")
	fmt.Println("the end of the replay.")
	fmt.Println("It waits for the file to be written to, processes it until completion or timeout,")
	fmt.Println("then returns to waiting mode for the next replay.")
	fmt.Println()
	fmt.Println("File search mode allows you to search for patterns in static executable files without")
	fmt.Println("requiring them to be running. This is useful for analyzing game executables to find")
	fmt.Println("memory patterns and offsets.")
}
