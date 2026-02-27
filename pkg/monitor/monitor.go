package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	zhreader "github.com/bill-rich/cncmon/pkg/memmon"
	"github.com/bill-rich/cncmon/pkg/proto/player_money"
)

// Config holds configuration for the monitor.
type Config struct {
	ProcessName  string
	APIURL       string
	APIQueueSize int
	PollDelay    time.Duration
	Timeout      time.Duration
	ManualSeed   string
	Debug        bool
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		ProcessName:  "generals.exe",
		APIURL:       "http://cncstats.computersrfun.org",
		APIQueueSize: 1000,
		PollDelay:    50 * time.Millisecond,
		Timeout:      2 * time.Minute,
		Debug:        false,
	}
}

// MonitorState represents the current state of the monitor.
type MonitorState int

const (
	StateWaitingForProcess MonitorState = iota
	StateWaitingForTimecode
	StateMonitoring
	StateStopped
)

func (s MonitorState) String() string {
	switch s {
	case StateWaitingForProcess:
		return "WaitingForProcess"
	case StateWaitingForTimecode:
		return "WaitingForTimecode"
	case StateMonitoring:
		return "Monitoring"
	case StateStopped:
		return "Stopped"
	default:
		return "Unknown"
	}
}

// Monitor handles game monitoring operations.
type Monitor struct {
	config    Config
	memReader *zhreader.Reader
	state     MonitorState
	stateMu   sync.RWMutex

	// Channels for coordination
	ctx        context.Context
	cancel     context.CancelFunc
	stateChan  chan MonitorState
	readyForNext chan struct{} // Signals when monitor is ready for next replay
}

// New creates a new Monitor instance.
func New(config Config) *Monitor {
	ctx, cancel := context.WithCancel(context.Background())
	return &Monitor{
		config:       config,
		state:        StateStopped,
		ctx:          ctx,
		cancel:       cancel,
		stateChan:    make(chan MonitorState, 10),
		readyForNext: make(chan struct{}, 1),
	}
}

// Start begins the monitoring loop in a goroutine.
// It continuously monitors for games and processes them.
func (m *Monitor) Start() {
	go m.runLoop()
}

// Stop gracefully stops the monitor.
func (m *Monitor) Stop() {
	m.cancel()
	m.setState(StateStopped)
	if m.memReader != nil {
		m.memReader.Close()
		m.memReader = nil
	}
}

// GetState returns the current monitor state.
func (m *Monitor) GetState() MonitorState {
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	return m.state
}

// StateChan returns a channel that receives state changes.
func (m *Monitor) StateChan() <-chan MonitorState {
	return m.stateChan
}

// ReadyForNext returns a channel that signals when the monitor
// is ready for the next replay (i.e., has returned to waiting for timecode).
func (m *Monitor) ReadyForNext() <-chan struct{} {
	return m.readyForNext
}

// WaitForReadyOrContext waits until the monitor is ready for the next replay
// or the context is cancelled.
func (m *Monitor) WaitForReadyOrContext(ctx context.Context) error {
	select {
	case <-m.readyForNext:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Monitor) setState(state MonitorState) {
	m.stateMu.Lock()
	oldState := m.state
	m.state = state
	m.stateMu.Unlock()

	if oldState != state {
		select {
		case m.stateChan <- state:
		default:
			// Channel full, skip notification
		}

		// Signal ready for next when we go back to waiting for timecode
		if state == StateWaitingForTimecode {
			select {
			case m.readyForNext <- struct{}{}:
			default:
				// Already signaled
			}
		}
	}
}

func (m *Monitor) debugLog(format string, args ...interface{}) {
	if m.config.Debug {
		fmt.Printf("DEBUG: "+format, args...)
	}
}

func (m *Monitor) runLoop() {
	for {
		select {
		case <-m.ctx.Done():
			return
		default:
		}

		// Wait for process
		m.setState(StateWaitingForProcess)
		memReader, err := m.waitForProcess()
		if err != nil {
			if m.ctx.Err() != nil {
				return
			}
			fmt.Printf("Process monitoring error: %v\n", err)
			time.Sleep(2 * time.Second)
			continue
		}
		m.memReader = memReader

		// Wait for timecode to start
		m.setState(StateWaitingForTimecode)
		if !m.waitForTimecodeStart() {
			m.memReader.Close()
			m.memReader = nil
			if m.ctx.Err() != nil {
				return
			}
			fmt.Println("Timecode monitoring interrupted. Returning to process monitoring...")
			continue
		}

		fmt.Println("Timecode started increasing. Starting to monitor money values...")

		// Process monitoring
		m.setState(StateMonitoring)
		eventCount := m.processMoneyMonitoring()

		fmt.Printf("Money monitoring completed. Processed %d events.\n", eventCount)
		m.memReader.Close()
		m.memReader = nil
		fmt.Println("Returning to waiting mode for next game...")
		fmt.Println()
	}
}

func (m *Monitor) waitForProcess() (*zhreader.Reader, error) {
	fmt.Printf("Waiting for %s process...\n", m.config.ProcessName)

	for {
		select {
		case <-m.ctx.Done():
			return nil, m.ctx.Err()
		default:
		}

		memReader, err := zhreader.Init(m.config.ProcessName)
		if err == nil {
			fmt.Printf("%s process found and memory reader initialized successfully\n", m.config.ProcessName)
			return memReader, nil
		}

		time.Sleep(2 * time.Second)
	}
}

func (m *Monitor) waitForTimecodeStart() bool {
	fmt.Println("Waiting for timecode to start increasing (game start)...")

	var lastTimecode uint32 = 0
	var increaseCount int = 0
	const requiredIncreases = 3

	for {
		select {
		case <-m.ctx.Done():
			fmt.Println("\nMonitor stopping...")
			return false
		default:
		}

		currentTimecode, err := m.memReader.GetTimecode()
		if err != nil {
			fmt.Printf("Warning: Failed to read timecode: %v\n", err)
			time.Sleep(500 * time.Millisecond)
			return false
		}

		if lastTimecode != 0 {
			if currentTimecode > lastTimecode {
				increaseCount++
				fmt.Printf("Timecode increased from %d to %d (%d/%d consecutive increases)\n",
					lastTimecode, currentTimecode, increaseCount, requiredIncreases)
				if increaseCount >= requiredIncreases {
					// Check money values - don't start if they match the default/initial state
					pollResult := m.memReader.Poll()
					expectedMoney := [8]int32{10000, 10000, 10000, 10000, 10000, 10000, 10000, -1}
					if pollResult.Money == expectedMoney {
						fmt.Printf("Money values match default state [10000, 10000, 10000, 10000, 10000, 10000, 10000, -1] - not starting monitoring, resetting detection\n")
						increaseCount = 0
						lastTimecode = 0
						continue
					}
					fmt.Printf("Timecode has increased %d times consecutively - game started!\n", requiredIncreases)
					return true
				}
			} else if currentTimecode < lastTimecode {
				fmt.Printf("Timecode decreased from %d to %d - resetting start detection and waiting for new game...\n",
					lastTimecode, currentTimecode)
				increaseCount = 0
			}
		}

		lastTimecode = currentTimecode
		fmt.Printf("Current timecode: %d (waiting for %d consecutive increases...)\n", currentTimecode, requiredIncreases)
		time.Sleep(500 * time.Millisecond)
	}
}

func (m *Monitor) processMoneyMonitoring() int {
	m.debugLog("Starting processMoneyMonitoring...\n")

	var lastSentPollResult zhreader.PollResult
	var lastSentPollResultMutex sync.Mutex
	var lastSeenPollResult zhreader.PollResult
	var lastTimecode uint32
	eventCount := 0
	var lastEventTimeMutex sync.Mutex
	firstPoll := true

	// Parse API URL and create gRPC connection
	grpcAddr, err := parseAPIURLForGRPC(m.config.APIURL)
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

	// Create API request queue
	requestQueue := make(chan QueuedAPIRequest, m.config.APIQueueSize)
	var wg sync.WaitGroup
	shutdownRequested := make(chan struct{})
	var shutdownOnce sync.Once

	// Start API request worker
	wg.Add(1)
	go apiRequestWorker(requestQueue, grpcClient, &wg, m.config.APIURL)

	// Graceful shutdown function
	gracefulShutdown := func() {
		shutdownOnce.Do(func() {
			fmt.Println("Initiating graceful shutdown - waiting for queue to drain...")
			close(shutdownRequested)

			// Close the queue channel immediately so the worker knows no more
			// items are coming and will exit once it finishes processing.
			close(requestQueue)

			drainTimeout := 30 * time.Second

			// Wait for the worker to finish processing all remaining items.
			// This is more reliable than polling len(requestQueue) because it
			// also accounts for the item currently being sent.
			workerDone := make(chan struct{})
			go func() {
				wg.Wait()
				close(workerDone)
			}()

			ticker := time.NewTicker(1 * time.Second)
			defer ticker.Stop()

			timer := time.NewTimer(drainTimeout)
			defer timer.Stop()

			for {
				select {
				case <-workerDone:
					fmt.Println("Queue drained successfully")
					// Signal server we're done sending.
					if err := grpcClient.stream.CloseSend(); err != nil {
						fmt.Printf("Warning: CloseSend failed: %v\n", err)
					}
					fmt.Println("Graceful shutdown complete")
					return
				case <-timer.C:
					fmt.Printf("Queue drain timeout after %v.\n", drainTimeout)
					fmt.Println("Graceful shutdown complete (with timeout)")
					return
				case <-ticker.C:
					fmt.Printf("Waiting for queue to drain... %d items remaining\n", len(requestQueue))
				}
			}
		})
	}

	defer gracefulShutdown()

	pollTicker := time.NewTicker(m.config.PollDelay)
	defer pollTicker.Stop()

	fmt.Printf("Starting money monitoring (polling every %v)...\n", m.config.PollDelay)
	loopCount := 0
	startTime := time.Now()

	for {
		loopCount++
		if loopCount%20 == 0 {
			fmt.Printf("Money monitoring: iteration %d, events: %d\n", loopCount, eventCount)
		}

		if time.Since(startTime) > 30*time.Second && eventCount == 0 {
			fmt.Printf("No money changes detected after 30 seconds, this might indicate an issue\n")
		}

		select {
		case <-m.ctx.Done():
			fmt.Printf("\nMonitor stopping...\n")
			return eventCount
		case <-pollTicker.C:
		}

		vals := m.memReader.Poll()

		// Check if all values are -1
		allInvalid := true
		for _, val := range vals.Money {
			if val != -1 {
				allInvalid = false
				break
			}
		}
		if allInvalid {
			fmt.Println("Warning: All memory values are invalid. Process may have gone away.")
			fmt.Println("Stopping new event queuing and waiting for queue to drain...")
			gracefulShutdown()
			return eventCount
		}

		// Get current timecode
		currentTimecode, err := m.memReader.GetTimecode()
		if err != nil {
			fmt.Printf("Warning: Failed to get timecode: %v\n", err)
		} else {
			if lastTimecode != 0 && currentTimecode < lastTimecode {
				fmt.Printf("Timecode decreased from %d to %d - ending monitoring and returning to waiting mode...\n",
					lastTimecode, currentTimecode)
				fmt.Println("Stopping new event queuing and waiting for queue to drain...")
				gracefulShutdown()
				return eventCount
			}
			lastTimecode = currentTimecode
			m.debugLog("Current timecode: %d\n", currentTimecode)
		}

		valuesChanged := firstPoll || pollResultChanged(vals, lastSeenPollResult)
		lastSeenPollResult = vals

		select {
		case <-shutdownRequested:
			continue
		default:
		}

		if valuesChanged {
			eventCount++
			isFirstPoll := firstPoll
			if firstPoll {
				fmt.Printf("Initial poll (event %d) - queuing data (timecode: %d)...\n", eventCount, lastTimecode)
				firstPoll = false
			} else {
				fmt.Printf("PollResult changed (event %d) - queuing data (timecode: %d)...\n", eventCount, lastTimecode)
			}

			seedToUse := m.memReader.GetSeed()
			if m.config.ManualSeed != "" {
				log.Printf("Using manual seed: %s", m.config.ManualSeed)
				seedToUse = m.config.ManualSeed
			} else {
				log.Printf("Using seed from memory: %s", seedToUse)
			}

			lastSentPollResultMutex.Lock()
			lastSentCopy := lastSentPollResult
			lastSentPollResultMutex.Unlock()

			successChan := make(chan bool, 1)

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
				queueCancel()
				go func(currentVals zhreader.PollResult) {
					success := <-successChan
					if success {
						lastSentPollResultMutex.Lock()
						lastSentPollResult = currentVals
						lastSentPollResultMutex.Unlock()
						lastEventTimeMutex.Lock()
						lastEventTimeMutex.Unlock()
					}
				}(vals)
			case <-queueCtx.Done():
				queueDepth := len(requestQueue)
				fmt.Printf("Warning: API request queue timeout after %v (queue depth: %d/%d, event %d). "+
					"API may be backed up. Continuing to poll...\n", queueTimeout, queueDepth, m.config.APIQueueSize, eventCount)
				queueCancel()
				select {
				case successChan <- false:
				default:
				}
			}

			if eventCount%10 == 0 {
				queueDepth := len(requestQueue)
				if queueDepth > m.config.APIQueueSize/2 {
					fmt.Printf("Queue depth: %d/%d (%.1f%% full)\n", queueDepth, m.config.APIQueueSize,
						float64(queueDepth)/float64(m.config.APIQueueSize)*100)
				}
			}

			j, _ := json.Marshal(struct {
				P [8]int32 `json:"p"`
			}{vals.Money})
			fmt.Printf("Memory values: %s\n", string(j))
		}
	}
}

// QueuedAPIRequest represents an API request to be sent.
type QueuedAPIRequest struct {
	Seed        string
	TimeCode    uint32
	Current     zhreader.PollResult
	Previous    zhreader.PollResult
	IsFirstPoll bool
	SuccessChan chan bool
}

// GRPCClient wraps the gRPC client and stream.
type GRPCClient struct {
	conn   *grpc.ClientConn
	client player_money.PlayerMoneyServiceClient
	stream grpc.BidiStreamingClient[player_money.MoneyDataRequest, player_money.MoneyDataResponse]
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
}

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

	// Drain server responses in background. The server sends a response for
	// each request. If nobody reads them, gRPC's internal buffer fills up
	// and stream.Send() blocks on the server side, which prevents the server
	// from calling Recv(), which in turn blocks the client's Send().
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				return
			}
		}
	}()

	return &GRPCClient{
		conn:   conn,
		client: client,
		stream: stream,
		ctx:    ctx,
		cancel: cancel,
	}, nil
}

func reconnectGRPCClient(oldClient *GRPCClient, addr string) (*GRPCClient, error) {
	oldClient.Close()
	return createGRPCClient(addr)
}

func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "connection") || strings.Contains(errStr, "unavailable") ||
		strings.Contains(errStr, "deadline exceeded") || strings.Contains(errStr, "transport") ||
		strings.Contains(errStr, "EOF") || strings.Contains(errStr, "broken pipe")
}

func apiRequestWorker(requestQueue <-chan QueuedAPIRequest, grpcClient *GRPCClient, wg *sync.WaitGroup, apiURL string) {
	defer wg.Done()
	const maxRetries = 3
	const baseRetryDelay = 100 * time.Millisecond
	const sendTimeout = 10 * time.Second

	for req := range requestQueue {
		var err error
		retryCount := 0

		for retryCount <= maxRetries {
			err = sendMoneyDataGRPCWithTimeout(grpcClient, req, sendTimeout)
			if err == nil {
				if retryCount > 0 {
					fmt.Printf("Money data sent successfully via gRPC after %d retries\n", retryCount)
				}
				if req.SuccessChan != nil {
					req.SuccessChan <- true
				}
				break
			}

			if isConnectionError(err) && retryCount < maxRetries {
				retryCount++
				retryDelay := baseRetryDelay * time.Duration(1<<uint(retryCount-1))
				fmt.Printf("Warning: Failed to send money data via gRPC (attempt %d/%d): %v. Retrying in %v...\n",
					retryCount, maxRetries+1, err, retryDelay)

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

			fmt.Printf("Warning: Failed to send money data via gRPC after %d attempts: %v\n", retryCount+1, err)
			if req.SuccessChan != nil {
				req.SuccessChan <- false
			}
			break
		}
	}
}

// sendMoneyDataGRPCWithTimeout wraps sendMoneyDataGRPC with a timeout so that
// a blocked stream.Send() cannot stall the worker indefinitely.
func sendMoneyDataGRPCWithTimeout(grpcClient *GRPCClient, req QueuedAPIRequest, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() {
		done <- sendMoneyDataGRPC(grpcClient, req.Seed, req.TimeCode, req.Current, req.Previous, req.IsFirstPoll)
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("gRPC send timed out after %v: deadline exceeded", timeout)
	}
}

func convertArray8ToSlice(arr [8]int32) []int32 {
	return arr[:]
}

func convertArray8x8ToProto(arr [8][8]int32) []*player_money.Int32Array8X8 {
	result := make([]*player_money.Int32Array8X8, 8)
	for i := 0; i < 8; i++ {
		result[i] = &player_money.Int32Array8X8{
			Values: arr[i][:],
		}
	}
	return result
}

func sendMoneyDataGRPC(grpcClient *GRPCClient, seed string, timeCode uint32, current zhreader.PollResult, previous zhreader.PollResult, isFirstPoll bool) error {
	switch {
	case current.Money == [8]int32{0, 0, 0, 0, 0, 0, 0, 0}:
		return nil
	case seed == "0":
		return nil
	}

	grpcClient.mu.Lock()
	defer grpcClient.mu.Unlock()

	request := &player_money.MoneyDataRequest{
		Seed:      seed,
		Timecode:  int64(timeCode),
		ResetSeed: isFirstPoll,
	}

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

	jsonData, _ := json.Marshal(request)
	log.Printf("Sending gRPC request with the following values: %s", string(jsonData))

	if err := grpcClient.stream.Send(request); err != nil {
		return fmt.Errorf("failed to send gRPC message: %w", err)
	}

	return nil
}

func pollResultChanged(current, previous zhreader.PollResult) bool {
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
	if current.UnitsKilled != previous.UnitsKilled {
		return true
	}
	if current.BuildingsKilled != previous.BuildingsKilled {
		return true
	}
	return false
}
