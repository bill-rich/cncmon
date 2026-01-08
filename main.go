package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	zhreader "github.com/bill-rich/cncmon/pkg/memmon"
	"github.com/bill-rich/cncstats/pkg/zhreplay"
)

var debugEnabled bool

func debugLog(format string, args ...interface{}) {
	if debugEnabled {
		fmt.Printf("DEBUG: "+format, args...)
	}
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
		testMode    = flag.Bool("test", false, "Test mode: process existing file immediately without waiting for file activity")
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

		if *testMode {
			// Test mode: process file immediately
			fmt.Printf("Test mode: Processing existing replay file immediately...\n")
			eventCount := processReplayFile(*replayFile, memReader, *pollDelay, *timeout, *apiURL, *seed)
			fmt.Printf("Replay processing completed. Processed %d events.\n", eventCount)
			memReader.Close()
			fmt.Println("Test mode complete. Exiting.")
			return
		} else {
			// Production mode: wait for timecode to start increasing
			if !waitForTimecodeStart(memReader, sigChan) {
				memReader.Close()
				fmt.Println("Timecode monitoring interrupted. Exiting.")
				return
			}

			fmt.Printf("Timecode started increasing. Starting to monitor money values...\n")

			// Process money monitoring until completion or timeout
			eventCount := processMoneyMonitoring(memReader, *pollDelay, *timeout, *apiURL, *seed)

			fmt.Printf("Money monitoring completed. Processed %d events.\n", eventCount)
			memReader.Close()
			fmt.Println("Returning to waiting mode for next game...")
			fmt.Println()
		}
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
	var stableCount int = 0
	const requiredStableReads = 3 // Need 3 consecutive reads of the same timecode to consider it stable

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

		// Check if timecode is increasing
		if currentTimecode > lastTimecode {
			fmt.Printf("Timecode increased from %d to %d - game started!\n", lastTimecode, currentTimecode)
			return true
		}

		// Check if timecode is stable (not changing)
		if currentTimecode == lastTimecode && currentTimecode > 0 {
			stableCount++
			if stableCount >= requiredStableReads {
				fmt.Printf("Timecode stable at %d for %d reads - game may have started\n", currentTimecode, stableCount)
				return true
			}
		} else {
			stableCount = 0
		}

		lastTimecode = currentTimecode
		fmt.Printf("Current timecode: %d (waiting for increase...)\n", currentTimecode)
		time.Sleep(500 * time.Millisecond)
	}
}

// processReplayFile processes a replay file until completion or timeout
func processReplayFile(replayFile string, memReader *zhreader.Reader, pollDelay time.Duration, timeout time.Duration, apiURL string, manualSeed string) int {
	debugLog("Starting processReplayFile for: %s\n", replayFile)

	// Create context with a much longer timeout to allow for real-time streaming
	// The cncstats library will handle the actual timeout for new data
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Configure streaming options for better real-time monitoring
	options := &zhreplay.StreamReplayOptions{
		PollInterval:      pollDelay * time.Millisecond, // Check more frequently for new data
		MaxWaitTime:       30 * time.Second,             // Max wait for individual operations
		InactivityTimeout: timeout,                      // Use our timeout for inactivity (2 minutes default)
		BufferSize:        100,
	}

	fmt.Printf("DEBUG: Waiting 5 seconds for seed to be written...\n")
	// Give it some time to write the seed
	time.Sleep(5 * time.Second)

	// Start streaming replay events
	fmt.Println("DEBUG: Starting replay streaming with real-time monitoring...")

	// Try to get the streaming replay with retries
	var streamingReplay *zhreplay.StreamingReplay
	var err error
	maxRetries := 10
	retryCount := 0

	for retryCount < maxRetries {
		fmt.Printf("DEBUG: Attempt %d/%d to start streaming...\n", retryCount+1, maxRetries)

		_, streamingReplay, err = zhreplay.StreamReplay(ctx, replayFile, nil, nil, nil, options)
		if err != nil {
			fmt.Printf("DEBUG: Failed to start streaming (attempt %d): %v\n", retryCount+1, err)
			time.Sleep(2 * time.Second)
			retryCount++
			continue
		}

		if streamingReplay == nil {
			fmt.Printf("DEBUG: StreamingReplay is nil (attempt %d)\n", retryCount+1)
			time.Sleep(2 * time.Second)
			retryCount++
			continue
		}

		if streamingReplay.Header == nil {
			fmt.Printf("DEBUG: Header is nil (attempt %d)\n", retryCount+1)
			time.Sleep(2 * time.Second)
			retryCount++
			continue
		}

		if streamingReplay.Header.Metadata.Seed == "" {
			fmt.Printf("DEBUG: Replay seed not yet available (attempt %d). Waiting...\n", retryCount+1)
			time.Sleep(2 * time.Second)
			retryCount++
			continue
		}

		fmt.Printf("DEBUG: Successfully got streaming replay with seed: %s\n", streamingReplay.Header.Metadata.Seed)
		break
	}

	if retryCount >= maxRetries {
		fmt.Printf("ERROR: Failed to start streaming after %d attempts\n", maxRetries)
		return 0
	}

	fmt.Printf("DEBUG: Starting main streaming loop...\n")
	bodyChan, streamingReplay, err := zhreplay.StreamReplay(ctx, replayFile, nil, nil, nil, options)
	if err != nil {
		fmt.Printf("ERROR: Failed to start main streaming: %v\n", err)
		return 0
	}

	if bodyChan == nil {
		fmt.Printf("ERROR: bodyChan is nil\n")
		return 0
	}

	fmt.Printf("DEBUG: Successfully started streaming, bodyChan created\n")

	// Determine seed to use
	var seed string
	if manualSeed != "" {
		seed = manualSeed
		fmt.Printf("Using manual seed: %s\n", seed)
	} else {
		seed = streamingReplay.Header.Metadata.Seed
		fmt.Printf("Replay Seed: %s\n", seed)
	}

	// Initialize replay session data
	session := &ReplaySession{
		Seed:       seed,
		Events:     make([]EventData, 0),
		EventCount: 0,
	}

	// Print header information
	fmt.Printf("Replay Header:\n")
	fmt.Printf("  Map: %s\n", streamingReplay.Header.Metadata.MapFile)
	fmt.Printf("  Players: %d\n", len(streamingReplay.Header.Metadata.Players))
	for i, player := range streamingReplay.Header.Metadata.Players {
		fmt.Printf("    Player %d: %s (Team %s)\n", i+1, player.Name, player.Team)
	}
	fmt.Println()

	// Initialize polling state
	var lastMoneyValues [8]int32
	var lastTimeCode uint32
	var timeCodeIncrement uint32
	eventCount := 0
	lastEventTime := time.Now()

	// Start 50ms polling timer
	pollTicker := time.NewTicker(pollDelay * time.Millisecond)
	defer pollTicker.Stop()

	fmt.Printf("DEBUG: Starting main event loop...\n")
	loopCount := 0
	startTime := time.Now()
	maxWaitTime := 30 * time.Second // Maximum time to wait for events

	for {
		loopCount++
		if loopCount%100 == 0 {
			fmt.Printf("DEBUG: Main loop iteration %d, waiting for events...\n", loopCount)
		}

		// Check if we've been waiting too long without any events
		if time.Since(startTime) > maxWaitTime && eventCount == 0 {
			fmt.Printf("DEBUG: No events received after %v, this might indicate an issue\n", maxWaitTime)
			fmt.Printf("DEBUG: File size: %d bytes, last modified: %v\n",
				func() int64 {
					if info, err := os.Stat(replayFile); err == nil {
						return info.Size()
					}
					return -1
				}(),
				func() time.Time {
					if info, err := os.Stat(replayFile); err == nil {
						return info.ModTime()
					}
					return time.Time{}
				}())
		}

		select {
		case chunk, ok := <-bodyChan:
			fmt.Printf("DEBUG: Received chunk from bodyChan (ok=%v)\n", ok)
			if !ok {
				fmt.Printf("\nStreaming completed (channel closed). Processed %d events.\n", eventCount)
				fmt.Println("This could mean:")
				fmt.Println("  - EndReplay command was received")
				fmt.Println("  - File reached end and no new data for 2 minutes")
				fmt.Println("  - Context was cancelled")
				return eventCount
			}

			// Get timecode from memory using binary search instead of replay file
			memoryTimecode, err := memReader.GetTimecode()
			if err != nil {
				fmt.Printf("Warning: Failed to get timecode from memory: %v\n", err)
				// Fall back to replay file timecode if memory timecode fails
				lastTimeCode = uint32(chunk.TimeCode)
			} else {
				lastTimeCode = memoryTimecode
				fmt.Printf("Memory Timecode: %d (from binary search)\n", memoryTimecode)
			}
			timeCodeIncrement = 0 // Reset increment when we get a new replay event
			lastEventTime = time.Now()
			eventCount++

			// Print replay event information (for debugging)
			fmt.Printf("Replay Event: Time=%d, Order=%s, PlayerID=%d", chunk.TimeCode, chunk.OrderName, chunk.PlayerID)
			if chunk.PlayerName != "" {
				fmt.Printf(", Player=%s", chunk.PlayerName)
			}
			fmt.Println()

			// Check for EndReplay command
			if chunk.OrderCode == 27 {
				fmt.Println("EndReplay command detected - streaming will stop.")
				return eventCount
			}

		case <-pollTicker.C:
			fmt.Printf("DEBUG: Poll ticker fired, polling memory...\n")
			// Poll memory every 50ms
			vals := memReader.Poll()
			fmt.Printf("DEBUG: Memory poll completed, got values: %v\n", vals)

			// Check if all values are -1, which indicates the process may have gone away
			allInvalid := true
			for _, val := range vals.Money {
				if val != -1 {
					allInvalid = false
					break
				}
			}
			if allInvalid {
				fmt.Println("  Warning: All memory values are invalid. Generals.exe process may have gone away.")
				fmt.Println("  Returning to process monitoring...")
				return eventCount
			}

			// Check if money values have changed
			moneyChanged := false
			for i, val := range vals {
				if val != lastMoneyValues[i] {
					moneyChanged = true
					break
				}
			}

			if moneyChanged {
				// Get current timecode from memory using binary search
				currentTimecode, err := memReader.GetTimecode()
				if err != nil {
					fmt.Printf("  Warning: Failed to get timecode from memory: %v\n", err)
					// Use last known timecode with increment as fallback
					if timeCodeIncrement > 0 {
						timeCodeIncrement++
					} else {
						timeCodeIncrement = 1
					}
					currentTimecode = lastTimeCode + timeCodeIncrement
				} else {
					// Use the current timecode from memory
					lastTimeCode = currentTimecode
					timeCodeIncrement = 0 // Reset increment when we get a new timecode from memory
				}

				// Send money data via API with current timecode
				fmt.Printf("  Money changed - sending data (timecode: %d)...\n", currentTimecode)
				err = sendMoneyData(apiURL, session.Seed, currentTimecode, vals)
				if err != nil {
					fmt.Printf("  Warning: Failed to send money data via API: %v\n", err)
				} else {
					fmt.Println("  Money data sent successfully")
				}

				// Display memory values
				j, _ := json.Marshal(struct {
					P [8]int32 `json:"p"`
				}{vals})
				fmt.Printf("  Memory values: %s\n", string(j))

				// Update last known values
				lastMoneyValues = vals
			}

		case <-ctx.Done():
			fmt.Printf("DEBUG: Context done signal received\n")
			// Check if we timed out due to inactivity
			timeSinceLastEvent := time.Since(lastEventTime)
			fmt.Printf("DEBUG: Time since last event: %v, timeout: %v\n", timeSinceLastEvent, timeout)
			if timeSinceLastEvent > timeout {
				fmt.Printf("\nTimeout reached (no events for %v). Processed %d events before timeout.\n", timeout, eventCount)
			} else {
				fmt.Printf("\nContext cancelled. Processed %d events before timeout.\n", eventCount)
			}
			return eventCount
		}
	}
}

// processMoneyMonitoring monitors money values and timecode without replay file dependency
func processMoneyMonitoring(memReader *zhreader.Reader, pollDelay time.Duration, timeout time.Duration, apiURL string, manualSeed string) int {
	debugLog("Starting processMoneyMonitoring...\n")

	// Initialize monitoring state
	var lastMoneyValues [8]int32
	var lastTimecode uint32
	eventCount := 0
	lastEventTime := time.Now()

	// Start polling timer
	pollTicker := time.NewTicker(pollDelay * time.Millisecond)
	defer pollTicker.Stop()

	fmt.Printf("Starting money monitoring (polling every 500ms)...\n")
	loopCount := 0
	startTime := time.Now()

	for {
		loopCount++
		if loopCount%20 == 0 { // Log every 10 seconds (20 * 500ms)
			fmt.Printf("Money monitoring: iteration %d, events: %d\n", loopCount, eventCount)
		}

		// Check if we've been waiting too long without any events
		if time.Since(startTime) > 30*time.Second && eventCount == 0 {
			fmt.Printf("No money changes detected after 30 seconds, this might indicate an issue\n")
		}

		<-pollTicker.C
		// Poll memory for money values
		vals := memReader.Poll()
		debugLog("Memory poll completed, got values: %v\n", vals)

		// Check if all values are -1, which indicates the process may have gone away
		allInvalid := true
		for _, val := range vals {
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
			lastTimecode = currentTimecode
			debugLog("Current timecode: %d\n", currentTimecode)
		}

		// Check if money values have changed
		moneyChanged := false
		for i, val := range vals {
			if val != lastMoneyValues[i] {
				moneyChanged = true
				break
			}
		}

		if moneyChanged {
			eventCount++
			fmt.Printf("Money changed (event %d) - sending data (timecode: %d)...\n", eventCount, lastTimecode)

			// Determine seed to use for API calls
			seedToUse := "direct-monitoring"
			if manualSeed != "" {
				seedToUse = manualSeed
			}

			// Send money data via API
			err := sendMoneyData(apiURL, seedToUse, lastTimecode, vals)
			if err != nil {
				fmt.Printf("Warning: Failed to send money data via API: %v\n", err)
			} else {
				fmt.Println("Money data sent successfully")
			}

			// Display memory values
			j, _ := json.Marshal(struct {
				P [8]int32 `json:"p"`
			}{vals})
			fmt.Printf("Memory values: %s\n", string(j))

			// Update last known values
			lastMoneyValues = vals
			lastEventTime = time.Now()
		}

		// Check for timeout due to inactivity
		timeSinceLastEvent := time.Since(lastEventTime)
		if timeSinceLastEvent > timeout {
			fmt.Printf("\nTimeout reached (no events for %v). Processed %d events before timeout.\n", timeout, eventCount)
			return eventCount
		}
	}
}

// MoneyDataRequest represents the API request for player money data
type MoneyDataRequest struct {
	Seed         string `json:"seed"`
	Timecode     int64  `json:"timecode"`
	Player1Money int64  `json:"player_1_money"`
	Player2Money int64  `json:"player_2_money"`
	Player3Money int64  `json:"player_3_money"`
	Player4Money int64  `json:"player_4_money"`
	Player5Money int64  `json:"player_5_money"`
	Player6Money int64  `json:"player_6_money"`
	Player7Money int64  `json:"player_7_money"`
	Player8Money int64  `json:"player_8_money"`
}

// sendMoneyData sends player money data to the API endpoint
func sendMoneyData(apiURL string, seed string, timeCode uint32, playerMoney [8]int32) error {
	// Create the request payload
	request := MoneyDataRequest{
		Seed:         seed,
		Timecode:     int64(timeCode),
		Player1Money: int64(playerMoney[0]),
		Player2Money: int64(playerMoney[1]),
		Player3Money: int64(playerMoney[2]),
		Player4Money: int64(playerMoney[3]),
		Player5Money: int64(playerMoney[4]),
		Player6Money: int64(playerMoney[5]),
		Player7Money: int64(playerMoney[6]),
		Player8Money: int64(playerMoney[7]),
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
