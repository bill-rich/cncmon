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
		replayFile = flag.String("file", defaultReplayFile, "Replay file to monitor")
		pollDelay  = flag.Duration("delay", 100*time.Millisecond, "Delay between memory polls (unused - now polls every 50ms)")
		timeout    = flag.Duration("timeout", 2*time.Minute, "Timeout for file inactivity before returning to waiting mode")
		apiURL     = flag.String("api", "https://cncstats.herokuapp.com", "API endpoint URL for sending money data")
		testMode   = flag.Bool("test", false, "Test mode: process existing file immediately without waiting for file activity")
		help       = flag.Bool("help", false, "Show help information")
	)
	flag.Parse()

	// Show help if requested
	if *help {
		showHelp()
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
		// Wait for generals.exe process to be available
		memReader, err := waitForGeneralsProcess(sigChan)
		if err != nil {
			fmt.Printf("Process monitoring interrupted: %v\n", err)
			return
		}

		if *testMode {
			// Test mode: process file immediately
			fmt.Printf("Test mode: Processing existing replay file immediately...\n")
			eventCount := processReplayFile(*replayFile, memReader, *pollDelay, *timeout, *apiURL)
			fmt.Printf("Replay processing completed. Processed %d events.\n", eventCount)
			memReader.Close()
			fmt.Println("Test mode complete. Exiting.")
			return
		} else {
			// Production mode: wait for file activity
			if !waitForFileActivity(*replayFile, sigChan) {
				memReader.Close()
				fmt.Println("File monitoring interrupted. Exiting.")
				return
			}

			fmt.Printf("Replay file activity detected. Starting to monitor events...\n")

			// Process the replay file until completion or timeout
			eventCount := processReplayFile(*replayFile, memReader, *pollDelay, *timeout, *apiURL)

			fmt.Printf("Replay processing completed. Processed %d events.\n", eventCount)
			memReader.Close()
			fmt.Println("Returning to waiting mode for next replay...")
			fmt.Println()
		}
	}
}

// waitForGeneralsProcess waits for the generals.exe process to be available
func waitForGeneralsProcess(sigChan <-chan os.Signal) (*zhreader.Reader, error) {
	fmt.Println("Waiting for generals.exe process...")

	for {
		// Check for interrupt signals
		select {
		case sig := <-sigChan:
			return nil, fmt.Errorf("received signal %v", sig)
		default:
			// Continue with process checking
		}

		// Try to initialize memory reader
		memReader, err := zhreader.Init()
		if err == nil {
			fmt.Println("Generals.exe process found and memory reader initialized successfully")
			return memReader, nil
		}

		// Process not found, wait a bit before trying again
		time.Sleep(2 * time.Second)
	}
}

// waitForFileActivity waits for the replay file to exist and start being written to
func waitForFileActivity(replayFile string, sigChan <-chan os.Signal) bool {
	var lastSize int64 = -1
	var lastModTime time.Time
	initialWait := true

	fmt.Println("Waiting for replay file to be actively written to...")

	for {
		// Check for interrupt signals
		select {
		case sig := <-sigChan:
			fmt.Printf("\nReceived signal %v. Shutting down gracefully...\n", sig)
			return false
		default:
			// Continue with file checking
		}

		// Check if file exists
		info, err := os.Stat(replayFile)
		if err != nil {
			// File doesn't exist yet, wait a bit
			time.Sleep(100 * time.Millisecond)
			continue
		}

		currentSize := info.Size()
		currentModTime := info.ModTime()

		// If this is the first time we see the file, record its state and wait
		if initialWait {
			lastSize = currentSize
			lastModTime = currentModTime
			initialWait = false
			fmt.Printf("File found (size: %d bytes). Waiting to detect write activity...\n", currentSize)
			time.Sleep(1 * time.Second) // Wait longer to see if it's being written
			continue
		}

		// If file size or modification time changed, it's being written to
		if currentSize != lastSize || !currentModTime.Equal(lastModTime) {
			lastSize = currentSize
			lastModTime = currentModTime
			fmt.Printf("File activity detected (size: %d bytes). Waiting for stability...\n", currentSize)
			// Wait a bit more to ensure it's actively being written
			time.Sleep(500 * time.Millisecond)
			return true
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// processReplayFile processes a replay file until completion or timeout
func processReplayFile(replayFile string, memReader *zhreader.Reader, pollDelay time.Duration, timeout time.Duration, apiURL string) int {
	// Create context with a much longer timeout to allow for real-time streaming
	// The cncstats library will handle the actual timeout for new data
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Configure streaming options for better real-time monitoring
	options := &zhreplay.StreamReplayOptions{
		PollInterval:      50 * time.Millisecond, // Check more frequently for new data
		MaxWaitTime:       30 * time.Second,      // Max wait for individual operations
		InactivityTimeout: timeout,               // Use our timeout for inactivity (2 minutes default)
		BufferSize:        100,
	}

	// Give it some time to write the seed
	time.Sleep(5 * time.Second)

	// Start streaming replay events
	fmt.Println("Starting replay streaming with real-time monitoring...")
	bodyChan, streamingReplay, err := zhreplay.StreamReplay(ctx, replayFile, nil, nil, nil, options)
	if err != nil {
		fmt.Printf("Failed to start streaming: %v\n", err)
		return 0
	}

	// Capture Seed from replay header
	seed := streamingReplay.Header.Metadata.Seed
	fmt.Printf("Replay Seed: %s\n", seed)

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
	pollTicker := time.NewTicker(500 * time.Millisecond)
	defer pollTicker.Stop()

	for {
		select {
		case chunk, ok := <-bodyChan:
			if !ok {
				fmt.Printf("\nStreaming completed (channel closed). Processed %d events.\n", eventCount)
				fmt.Println("This could mean:")
				fmt.Println("  - EndReplay command was received")
				fmt.Println("  - File reached end and no new data for 2 minutes")
				fmt.Println("  - Context was cancelled")
				return eventCount
			}

			// Update last seen timecode from replay events (no filtering)
			lastTimeCode = uint32(chunk.TimeCode)
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
			// Poll memory every 50ms
			vals := memReader.Poll()

			// Check if all values are -1, which indicates the process may have gone away
			allInvalid := true
			for _, val := range vals {
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
				// Increment timecode if this is a subsequent change between replay events
				if timeCodeIncrement > 0 {
					timeCodeIncrement++
				} else {
					timeCodeIncrement = 1
				}

				// Send money data via API with incremented timecode
				fmt.Printf("  Money changed - sending data (timecode: %d + %d)...\n", lastTimeCode, timeCodeIncrement)
				err := sendMoneyData(apiURL, session.Seed, lastTimeCode+timeCodeIncrement, vals)
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
			// Check if we timed out due to inactivity
			if time.Since(lastEventTime) > timeout {
				fmt.Printf("\nTimeout reached (no events for %v). Processed %d events before timeout.\n", timeout, eventCount)
			} else {
				fmt.Printf("\nContext cancelled. Processed %d events before timeout.\n", eventCount)
			}
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
	fmt.Println("  -test")
	fmt.Println("        Test mode: process existing file immediately without waiting for file activity")
	fmt.Println("  -help")
	fmt.Println("        Show this help information")
	fmt.Println()
	fmt.Println("This tool continuously monitors a Command and Conquer replay file and polls memory values")
	fmt.Println("from the running generals.exe process every 50ms. When money values change, it sends")
	fmt.Println("events to the API using the last seen timecode from replay events. Multiple money changes")
	fmt.Println("between replay events increment the timecode. Replay events are still used to detect")
	fmt.Println("the end of the replay.")
	fmt.Println("It waits for the file to be written to, processes it until completion or timeout,")
	fmt.Println("then returns to waiting mode for the next replay.")
}
