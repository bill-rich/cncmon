package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	zhreader "github.com/bill-rich/cncmon/pkg/memmon"
	"github.com/bill-rich/cncmon/pkg/proto/player_money"
)

var debugEnabled bool

func debugLog(format string, args ...interface{}) {
	if debugEnabled {
		fmt.Printf("DEBUG: "+format, args...)
	}
}

// QueuedAPIRequest represents an API request that needs to be sent
type QueuedAPIRequest struct {
	Seed        string
	TimeCode    uint32
	Current     zhreader.PollResult
	Previous    zhreader.PollResult
	IsFirstPoll bool
	SuccessChan chan bool // Channel to signal success/failure
}

// GRPCClient wraps the gRPC client and stream
type GRPCClient struct {
	conn   *grpc.ClientConn
	client player_money.PlayerMoneyServiceClient
	stream grpc.BidiStreamingClient[player_money.MoneyDataRequest, player_money.MoneyDataResponse]
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
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
		replayFile   = flag.String("file", defaultReplayFile, "Replay file to monitor")
		pollDelay    = flag.Duration("delay", 50*time.Millisecond, "Delay between memory polls (unused - now polls every 50ms)")
		timeout      = flag.Duration("timeout", 2*time.Minute, "Timeout for file inactivity before returning to waiting mode")
		apiURL       = flag.String("api", "http://cncstats.computersrfun.org", "gRPC endpoint URL for sending money data")
		processName  = flag.String("process", "generals.exe", "Process name to monitor (default: generals.exe)")
		help         = flag.Bool("help", false, "Show help information")
		seed         = flag.String("seed", "", "Manual seed value to use instead of reading from replay file")
		apiQueueSize = flag.Int("api-queue-size", 1000, "Size of the API request queue buffer (default: 1000)")

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
			fmt.Println("Timecode monitoring interrupted. Returning to process monitoring...")
			continue
		}

		fmt.Printf("Timecode started increasing. Starting to monitor money values...\n")

		// Process money monitoring until completion or timeout
		eventCount := processMoneyMonitoring(memReader, *pollDelay, *timeout, *apiURL, *seed, *apiQueueSize, sigChan)

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
			return false
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

// apiRequestWorker processes API requests from the queue using gRPC streaming
// with retry logic to handle temporary failures
func apiRequestWorker(requestQueue <-chan QueuedAPIRequest, grpcClient *GRPCClient, wg *sync.WaitGroup, apiURL string) {
	defer wg.Done()
	const maxRetries = 3
	const baseRetryDelay = 100 * time.Millisecond

	for req := range requestQueue {
		var err error
		retryCount := 0

		// Retry loop with exponential backoff
		for retryCount <= maxRetries {
			err = sendMoneyDataGRPC(grpcClient, req.Seed, req.TimeCode, req.Current, req.Previous, req.IsFirstPoll)
			if err == nil {
				// Success - break out of retry loop
				if retryCount > 0 {
					fmt.Printf("Money data sent successfully via gRPC after %d retries\n", retryCount)
				} else {
					debugLog("Money data sent successfully via gRPC\n")
				}
				if req.SuccessChan != nil {
					req.SuccessChan <- true
				}
				break
			}

			// Check if error is recoverable (connection issue)
			if isConnectionError(err) && retryCount < maxRetries {
				retryCount++
				retryDelay := baseRetryDelay * time.Duration(1<<uint(retryCount-1)) // Exponential backoff
				fmt.Printf("Warning: Failed to send money data via gRPC (attempt %d/%d): %v. Retrying in %v...\n",
					retryCount, maxRetries+1, err, retryDelay)

				// Try to reconnect if connection is broken
				if retryCount == 1 {
					grpcAddr, parseErr := parseAPIURLForGRPC(apiURL)
					if parseErr == nil {
						newClient, reconnectErr := reconnectGRPCClient(grpcClient, grpcAddr)
						if reconnectErr == nil {
							grpcClient = newClient
							fmt.Println("gRPC connection reestablished")
						}
					}
				}

				time.Sleep(retryDelay)
				continue
			}

			// Non-recoverable error or max retries reached
			fmt.Printf("Warning: Failed to send money data via gRPC after %d attempts: %v\n", retryCount+1, err)
			if req.SuccessChan != nil {
				req.SuccessChan <- false
			}
			break
		}
	}
}

// isConnectionError checks if the error is a connection-related error that might be recoverable
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// Check for common connection error patterns
	return strings.Contains(errStr, "connection") || strings.Contains(errStr, "unavailable") ||
		strings.Contains(errStr, "deadline exceeded") || strings.Contains(errStr, "transport") ||
		strings.Contains(errStr, "EOF") || strings.Contains(errStr, "broken pipe")
}

// reconnectGRPCClient attempts to reconnect the gRPC client
func reconnectGRPCClient(oldClient *GRPCClient, addr string) (*GRPCClient, error) {
	// Close old connection
	oldClient.Close()

	// Create new connection
	return createGRPCClient(addr)
}

// processMoneyMonitoring monitors money values and timecode without replay file dependency
func processMoneyMonitoring(memReader *zhreader.Reader, pollDelay time.Duration, timeout time.Duration, apiURL string, manualSeed string, apiQueueSize int, sigChan <-chan os.Signal) int {
	debugLog("Starting processMoneyMonitoring...\n")

	// Initialize monitoring state
	var lastSentPollResult zhreader.PollResult
	var lastSentPollResultMutex sync.Mutex     // Protect lastSentPollResult from race conditions
	var lastSeenPollResult zhreader.PollResult // Track last poll result we saw (for change detection)
	var lastTimecode uint32
	eventCount := 0
	var lastEventTimeMutex sync.Mutex
	firstPoll := true // Track if this is the first poll to initialize lastSentPollResult

	// Parse API URL and create gRPC connection
	grpcAddr, err := parseAPIURLForGRPC(apiURL)
	if err != nil {
		fmt.Printf("Error parsing API URL for gRPC: %v\n", err)
		return eventCount
	}

	// Create gRPC client
	grpcClient, err := createGRPCClient(grpcAddr)
	if err != nil {
		fmt.Printf("Error creating gRPC client: %v\n", err)
		return eventCount
	}
	defer grpcClient.Close()

	// Create API request queue with buffer to prevent blocking
	requestQueue := make(chan QueuedAPIRequest, apiQueueSize)
	var wg sync.WaitGroup
	shutdownRequested := make(chan struct{})
	var shutdownOnce sync.Once

	// Start API request worker with API URL for reconnection
	wg.Add(1)
	go apiRequestWorker(requestQueue, grpcClient, &wg, apiURL)

	// Graceful shutdown function that drains the queue before closing
	gracefulShutdown := func() {
		shutdownOnce.Do(func() {
			fmt.Println("Initiating graceful shutdown - waiting for queue to drain...")
			close(shutdownRequested)

			// Wait for queue to drain with timeout
			drainTimeout := 30 * time.Second
			drainCtx, drainCancel := context.WithTimeout(context.Background(), drainTimeout)
			defer drainCancel()

			// Monitor queue depth
			ticker := time.NewTicker(1 * time.Second)
			defer ticker.Stop()

			drained := false
			for !drained {
				queueDepth := len(requestQueue)
				if queueDepth == 0 {
					fmt.Println("Queue drained successfully")
					drained = true
					break
				}

				select {
				case <-drainCtx.Done():
					fmt.Printf("Queue drain timeout after %v. %d items remaining in queue.\n", drainTimeout, queueDepth)
					drained = true
					break
				case <-ticker.C:
					fmt.Printf("Waiting for queue to drain... %d items remaining\n", queueDepth)
				}
			}

			// Close queue and wait for worker
			close(requestQueue)
			fmt.Println("Waiting for worker to finish processing...")
			wg.Wait()
			fmt.Println("Graceful shutdown complete")
		})
	}

	// Ensure worker goroutine is cleaned up on exit
	defer gracefulShutdown()

	// Start polling timer
	pollTicker := time.NewTicker(pollDelay)
	defer pollTicker.Stop()

	fmt.Printf("Starting money monitoring (polling every %v)...\n", pollDelay)
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

		// Use select to check for signals while waiting for ticker
		select {
		case <-sigChan:
			fmt.Printf("\nReceived interrupt signal. Shutting down gracefully...\n")
			panic("received interrupt signal")
		case <-pollTicker.C:
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
			fmt.Println("Stopping new event queuing and waiting for queue to drain...")
			// Trigger graceful shutdown - this will drain the queue before returning
			gracefulShutdown()
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
				fmt.Println("Stopping new event queuing and waiting for queue to drain...")
				// Trigger graceful shutdown - this will drain the queue before returning
				gracefulShutdown()
				return eventCount
			}
			lastTimecode = currentTimecode
			debugLog("Current timecode: %d\n", currentTimecode)
		}

		// Check if any values in PollResult have changed compared to the last poll we saw
		// This ensures we only queue changes even if the API queue is backed up
		// On first poll, always send to initialize the baseline
		valuesChanged := firstPoll || pollResultChanged(vals, lastSeenPollResult)

		// Update lastSeenPollResult after each poll (regardless of whether we queue)
		lastSeenPollResult = vals

		// Check if shutdown has been requested - don't queue new items if shutting down
		select {
		case <-shutdownRequested:
			// Shutdown requested, skip queuing new items
			continue
		default:
		}

		if valuesChanged {
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
				log.Printf("Using manual seed: %s", manualSeed)
				seedToUse = manualSeed
			} else {
				log.Printf("Using seed from memory: %s", seedToUse)
			}

			// Get lastSentPollResult for API request (to determine which fields changed)
			lastSentPollResultMutex.Lock()
			lastSentCopy := lastSentPollResult
			lastSentPollResultMutex.Unlock()

			// Create success channel to track API call result
			successChan := make(chan bool, 1)

			// Queue the API request with timeout to prevent indefinite blocking
			// but still wait for queue space to become available
			queueTimeout := 5 * time.Second
			queueCtx, queueCancel := context.WithTimeout(context.Background(), queueTimeout)

			select {
			case requestQueue <- QueuedAPIRequest{
				Seed:        seedToUse,
				TimeCode:    lastTimecode,
				Current:     vals,
				Previous:    lastSentCopy,
				IsFirstPoll: isFirstPoll,
				SuccessChan: successChan,
			}:
				// Request queued successfully
				queueCancel()
				// Handle success/failure asynchronously
				go func(currentVals zhreader.PollResult) {
					success := <-successChan
					if success {
						// Update last sent PollResult only after successful send
						lastSentPollResultMutex.Lock()
						lastSentPollResult = currentVals
						lastSentPollResultMutex.Unlock()
						lastEventTimeMutex.Lock()
						lastEventTimeMutex.Unlock()
					}
				}(vals)
			case <-queueCtx.Done():
				// Queue timeout - log warning but continue polling
				queueDepth := len(requestQueue)
				fmt.Printf("Warning: API request queue timeout after %v (queue depth: %d/%d, event %d). "+
					"API may be backed up. Continuing to poll...\n", queueTimeout, queueDepth, apiQueueSize, eventCount)
				queueCancel()
				// Signal failure to prevent goroutine leak
				select {
				case successChan <- false:
				default:
				}
			}

			// Log queue depth periodically for monitoring
			if eventCount%10 == 0 {
				queueDepth := len(requestQueue)
				if queueDepth > apiQueueSize/2 {
					fmt.Printf("Queue depth: %d/%d (%.1f%% full)\n", queueDepth, apiQueueSize,
						float64(queueDepth)/float64(apiQueueSize)*100)
				}
			}

			// Display memory values
			j, _ := json.Marshal(struct {
				P [8]int32 `json:"p"`
			}{vals.Money})
			fmt.Printf("Memory values: %s\n", string(j))
		}
	}
}

// parseAPIURLForGRPC extracts the hostname from the API URL and returns gRPC address with port 9090
func parseAPIURLForGRPC(apiURL string) (string, error) {
	u, err := url.Parse(apiURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse API URL: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("invalid API URL: no hostname found")
	}
	return fmt.Sprintf("%s:9090", host), nil
}

// createGRPCClient creates a gRPC client connection and stream
func createGRPCClient(addr string) (*GRPCClient, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to gRPC server: %w", err)
	}

	client := player_money.NewPlayerMoneyServiceClient(conn)
	ctx, cancel := context.WithCancel(context.Background())

	stream, err := client.StreamCreatePlayerMoneyData(ctx)
	if err != nil {
		cancel()
		conn.Close()
		return nil, fmt.Errorf("failed to create stream: %w", err)
	}

	return &GRPCClient{
		conn:   conn,
		client: client,
		stream: stream,
		ctx:    ctx,
		cancel: cancel,
	}, nil
}

// Close closes the gRPC connection and stream
func (c *GRPCClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cancel != nil {
		c.cancel()
	}
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// convertArray8ToSlice converts [8]int32 to []int32
func convertArray8ToSlice(arr [8]int32) []int32 {
	return arr[:]
}

// convertArray8x8ToProto converts [8][8]int32 to []*Int32Array8X8
func convertArray8x8ToProto(arr [8][8]int32) []*player_money.Int32Array8X8 {
	result := make([]*player_money.Int32Array8X8, 8)
	for i := 0; i < 8; i++ {
		result[i] = &player_money.Int32Array8X8{
			Values: arr[i][:],
		}
	}
	return result
}

// sendMoneyDataGRPC sends player money data via gRPC streaming
// Only changed fields are included (except Seed and Timecode which are always included)
func sendMoneyDataGRPC(grpcClient *GRPCClient, seed string, timeCode uint32, current zhreader.PollResult, previous zhreader.PollResult, isFirstPoll bool) error {
	// Early return for invalid data
	switch {
	case current.Money == [8]int32{0, 0, 0, 0, 0, 0, 0, 0}:
		return nil
	case seed == "0":
		return nil
	}

	grpcClient.mu.Lock()
	defer grpcClient.mu.Unlock()

	// Create the request payload with only changed fields
	request := &player_money.MoneyDataRequest{
		Seed:     seed,
		Timecode: int64(timeCode),
	}

	// Only include fields that have changed (or on first poll, include all)
	if isFirstPoll || current.Money != previous.Money {
		request.Money = convertArray8ToSlice(current.Money)
	}
	if isFirstPoll || current.MoneyEarned != previous.MoneyEarned {
		request.MoneyEarned = convertArray8ToSlice(current.MoneyEarned)
	}
	if isFirstPoll || current.UnitsBuilt != previous.UnitsBuilt {
		request.UnitsBuilt = convertArray8ToSlice(current.UnitsBuilt)
	}
	if isFirstPoll || current.UnitsLost != previous.UnitsLost {
		request.UnitsLost = convertArray8ToSlice(current.UnitsLost)
	}
	if isFirstPoll || current.BuildingsBuilt != previous.BuildingsBuilt {
		request.BuildingsBuilt = convertArray8ToSlice(current.BuildingsBuilt)
	}
	if isFirstPoll || current.BuildingsLost != previous.BuildingsLost {
		request.BuildingsLost = convertArray8ToSlice(current.BuildingsLost)
	}
	if isFirstPoll || current.BuildingsKilled != previous.BuildingsKilled {
		request.BuildingsKilled = convertArray8x8ToProto(current.BuildingsKilled)
	}
	if isFirstPoll || current.UnitsKilled != previous.UnitsKilled {
		request.UnitsKilled = convertArray8x8ToProto(current.UnitsKilled)
	}
	if isFirstPoll || current.GeneralsPointsTotal != previous.GeneralsPointsTotal {
		request.GeneralsPointsTotal = convertArray8ToSlice(current.GeneralsPointsTotal)
	}
	if isFirstPoll || current.GeneralsPointsUsed != previous.GeneralsPointsUsed {
		request.GeneralsPointsUsed = convertArray8ToSlice(current.GeneralsPointsUsed)
	}
	if isFirstPoll || current.RadarsBuilt != previous.RadarsBuilt {
		request.RadarsBuilt = convertArray8ToSlice(current.RadarsBuilt)
	}
	if isFirstPoll || current.SearchAndDestroy != previous.SearchAndDestroy {
		request.SearchAndDestroy = convertArray8ToSlice(current.SearchAndDestroy)
	}
	if isFirstPoll || current.HoldTheLine != previous.HoldTheLine {
		request.HoldTheLine = convertArray8ToSlice(current.HoldTheLine)
	}
	if isFirstPoll || current.Bombardment != previous.Bombardment {
		request.Bombardment = convertArray8ToSlice(current.Bombardment)
	}
	if isFirstPoll || current.XP != previous.XP {
		request.Xp = convertArray8ToSlice(current.XP)
	}
	if isFirstPoll || current.XPLevel != previous.XPLevel {
		request.XpLevel = convertArray8ToSlice(current.XPLevel)
	}
	if isFirstPoll || current.TechBuildingsCaptured != previous.TechBuildingsCaptured {
		request.TechBuildingsCaptured = convertArray8ToSlice(current.TechBuildingsCaptured)
	}
	if isFirstPoll || current.FactionBuildingsCaptured != previous.FactionBuildingsCaptured {
		request.FactionBuildingsCaptured = convertArray8ToSlice(current.FactionBuildingsCaptured)
	}
	if isFirstPoll || current.PowerTotal != previous.PowerTotal {
		request.PowerTotal = convertArray8ToSlice(current.PowerTotal)
	}
	if isFirstPoll || current.PowerUsed != previous.PowerUsed {
		request.PowerUsed = convertArray8ToSlice(current.PowerUsed)
	}

	// Log the request (for debugging)
	jsonData, _ := json.Marshal(request)
	log.Printf("Sending gRPC request with the following values: %s", string(jsonData))

	// Send via gRPC stream
	if err := grpcClient.stream.Send(request); err != nil {
		return fmt.Errorf("failed to send gRPC message: %w", err)
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
	fmt.Println("  -api-queue-size int")
	fmt.Println("        Size of the API request queue buffer (default: 1000)")
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
