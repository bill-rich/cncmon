package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bill-rich/cncmon/pkg/automation"
	zhreader "github.com/bill-rich/cncmon/pkg/memmon"
)

const (
	VK_F          = 0x46 // Virtual key code for 'F' key
	blacklistFile = "blacklisted_match_ids.txt"
)

// Config holds all configuration for the automation.
type Config struct {
	DirectoryA           string
	DirectoryB           string
	GeneralsExe          string
	ProcessName          string
	InitialWait          time.Duration
	ClicksBeforeStart    []Coordinate
	ClicksAfterStart     []Coordinate
	ClicksAfterStop      []Coordinate
	TimecodeStartTimeout time.Duration
	TimecodeStopTimeout  time.Duration
	ReplayAPIURL         string
	UseAPI               bool
	MaxReplays           int
}

// Coordinate represents an x,y screen position.
type Coordinate struct {
	X, Y int32
}

// ReplayInfo represents a replay from the API.
type ReplayInfo struct {
	MatchID      int    `json:"match_id"`
	URL          string `json:"url"`
	PresignedURL string `json:"presigned_url"`
}

// GeneralsProcess manages the generals.exe process lifecycle.
type GeneralsProcess struct {
	cmd     *exec.Cmd
	exitCh  chan struct{}
	exePath string
	mu      sync.Mutex
}

// NewGeneralsProcess creates a new process manager.
func NewGeneralsProcess(exePath string) *GeneralsProcess {
	return &GeneralsProcess{
		exePath: exePath,
		exitCh:  make(chan struct{}),
	}
}

// Start launches generals.exe and sets up exit monitoring.
func (gp *GeneralsProcess) Start() error {
	gp.mu.Lock()
	defer gp.mu.Unlock()

	cmd, err := automation.StartGenerals(gp.exePath)
	if err != nil {
		return fmt.Errorf("starting generals.exe: %w", err)
	}

	gp.cmd = cmd
	gp.exitCh = make(chan struct{})

	go func() {
		gp.cmd.Wait()
		close(gp.exitCh)
	}()

	return nil
}

// Initialize performs the initial setup: find window, wait, press escape.
func (gp *GeneralsProcess) Initialize(ctx context.Context, initialWait time.Duration) error {
	// Wait for window to appear
	if err := sleepWithContext(ctx, 2*time.Second); err != nil {
		return err
	}

	fmt.Println("Finding game window...")
	if err := automation.SetTargetWindow(uint32(gp.cmd.Process.Pid)); err != nil {
		fmt.Printf("Warning: Failed to find game window: %v (will try SendInput fallback)\n", err)
	} else {
		fmt.Println("Game window found - using PostMessage for input")
	}

	fmt.Printf("Waiting %v...\n", initialWait)
	if err := sleepWithContext(ctx, initialWait); err != nil {
		return err
	}

	fmt.Println("Pressing Escape key...")
	if err := automation.PressEscape(); err != nil {
		return fmt.Errorf("pressing escape: %w", err)
	}

	return sleepWithContext(ctx, 10*time.Second)
}

// IsRunning checks if the process is still running (non-blocking).
func (gp *GeneralsProcess) IsRunning() bool {
	select {
	case <-gp.exitCh:
		return false
	default:
		return true
	}
}

// ExitCh returns the channel that closes when the process exits.
func (gp *GeneralsProcess) ExitCh() <-chan struct{} {
	return gp.exitCh
}

// Kill terminates the process.
func (gp *GeneralsProcess) Kill() {
	gp.mu.Lock()
	defer gp.mu.Unlock()

	if gp.cmd != nil && gp.cmd.Process != nil {
		gp.cmd.Process.Kill()
	}
}

// Wait waits for the process to exit with context support.
func (gp *GeneralsProcess) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-gp.exitCh:
		return nil
	}
}

// Blacklist manages a set of blacklisted match IDs.
type Blacklist struct {
	matchIDs map[string]bool
	mu       sync.RWMutex
}

// NewBlacklist creates a new blacklist.
func NewBlacklist() *Blacklist {
	return &Blacklist{
		matchIDs: make(map[string]bool),
	}
}

// LoadBlacklist loads the blacklist from a file.
func LoadBlacklist(filename string) (*Blacklist, error) {
	bl := NewBlacklist()

	file, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return bl, nil
		}
		return nil, fmt.Errorf("opening blacklist file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()

		// Strip comments
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}

		matchID := strings.TrimSpace(line)
		if matchID != "" {
			bl.matchIDs[matchID] = true
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading blacklist file: %w", err)
	}

	return bl, nil
}

// IsBlacklisted checks if a match ID is blacklisted.
func (bl *Blacklist) IsBlacklisted(matchID int) bool {
	bl.mu.RLock()
	defer bl.mu.RUnlock()
	return bl.matchIDs[strconv.Itoa(matchID)]
}

// Add adds a match ID to the blacklist (in-memory only).
func (bl *Blacklist) Add(matchID int) {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	bl.matchIDs[strconv.Itoa(matchID)] = true
}

// AddWithComment adds a match ID with a comment and persists to file.
func (bl *Blacklist) AddWithComment(matchID int, comment, filename string) error {
	bl.mu.Lock()
	defer bl.mu.Unlock()

	matchIDStr := strconv.Itoa(matchID)
	bl.matchIDs[matchIDStr] = true

	file, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("opening blacklist file: %w", err)
	}
	defer file.Close()

	line := matchIDStr
	if comment != "" {
		line += " // " + comment
	}
	line += "\n"

	if _, err := file.WriteString(line); err != nil {
		return fmt.Errorf("writing to blacklist file: %w", err)
	}

	return nil
}

// Count returns the number of blacklisted IDs.
func (bl *Blacklist) Count() int {
	bl.mu.RLock()
	defer bl.mu.RUnlock()
	return len(bl.matchIDs)
}

// Save persists the entire blacklist to file.
func (bl *Blacklist) Save(filename string) error {
	bl.mu.RLock()
	defer bl.mu.RUnlock()

	file, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("creating blacklist file: %w", err)
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	for matchID := range bl.matchIDs {
		if _, err := writer.WriteString(matchID + "\n"); err != nil {
			return fmt.Errorf("writing to blacklist file: %w", err)
		}
	}
	return writer.Flush()
}

// ReplayProcessor handles the core replay processing logic.
type ReplayProcessor struct {
	config    *Config
	blacklist *Blacklist
}

// NewReplayProcessor creates a new replay processor.
func NewReplayProcessor(config *Config, blacklist *Blacklist) *ReplayProcessor {
	return &ReplayProcessor{
		config:    config,
		blacklist: blacklist,
	}
}

// ProcessResult contains the result of processing a replay.
type ProcessResult struct {
	GameSeed string
	MatchID  int
	Success  bool
	Error    error
}

// Process handles the core replay processing steps after the file is in place.
func (rp *ReplayProcessor) Process(ctx context.Context, matchID int) ProcessResult {
	result := ProcessResult{MatchID: matchID}

	// Pre-start clicks
	if len(rp.config.ClicksBeforeStart) > 0 {
		fmt.Printf("Simulating %d mouse clicks before timecode start...\n", len(rp.config.ClicksBeforeStart))
		if err := automation.ClickAtMultiple(ctx, toAutomationCoords(rp.config.ClicksBeforeStart)); err != nil {
			result.Error = fmt.Errorf("clicking before start: %w", err)
			return result
		}
	}

	// Initialize memory reader
	fmt.Println("Waiting for process to be available...")
	memReader, err := rp.waitForMemoryReader(ctx)
	if err != nil {
		result.Error = fmt.Errorf("initializing memory reader: %w", err)
		return result
	}
	defer memReader.Close()

	getTimecode := func() (uint32, error) {
		return memReader.GetTimecode()
	}

	// Wait for timecode to start
	fmt.Println("Waiting for timecode to start...")
	if err := automation.WaitForTimecodeStart(ctx, getTimecode, rp.config.TimecodeStartTimeout); err != nil {
		result.Error = fmt.Errorf("waiting for timecode start: %w", err)
		return result
	}

	// Get game seed
	fmt.Println("Getting game seed from memory...")
	result.GameSeed = memReader.GetSeed()
	if result.GameSeed == "" {
		result.Error = fmt.Errorf("failed to get game seed")
		return result
	}
	fmt.Printf("Game seed: %s\n", result.GameSeed)

	// Press F key
	fmt.Println("Pressing 'F' key...")
	if err := sleepWithContext(ctx, 500*time.Millisecond); err != nil {
		result.Error = err
		return result
	}
	if err := automation.PressKey(VK_F); err != nil {
		result.Error = fmt.Errorf("pressing F key: %w", err)
		return result
	}

	// Post-start clicks
	if len(rp.config.ClicksAfterStart) > 0 {
		fmt.Printf("Simulating %d mouse clicks after timecode start...\n", len(rp.config.ClicksAfterStart))
		if err := automation.ClickAtMultiple(ctx, toAutomationCoords(rp.config.ClicksAfterStart)); err != nil {
			result.Error = fmt.Errorf("clicking after start: %w", err)
			return result
		}
	}

	// Wait for timecode to stop
	fmt.Println("Waiting for timecode to stop...")
	if _, err := automation.WaitForTimecodeStop(ctx, getTimecode, rp.config.TimecodeStopTimeout); err != nil {
		result.Error = fmt.Errorf("waiting for timecode stop: %w", err)
		return result
	}

	// Post-stop delay and clicks
	if err := sleepWithContext(ctx, 1*time.Second); err != nil {
		result.Error = err
		return result
	}

	if len(rp.config.ClicksAfterStop) > 0 {
		fmt.Printf("Simulating %d mouse clicks after timecode stops...\n", len(rp.config.ClicksAfterStop))
		if err := automation.ClickAtMultiple(ctx, toAutomationCoords(rp.config.ClicksAfterStop)); err != nil {
			// Log but don't fail
			fmt.Printf("Warning: Error clicking after stop: %v\n", err)
		}
	}

	result.Success = true
	return result
}

func (rp *ReplayProcessor) waitForMemoryReader(ctx context.Context) (*zhreader.Reader, error) {
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		memReader, err := zhreader.Init(rp.config.ProcessName)
		if err == nil {
			return memReader, nil
		}

		if err := sleepWithContext(ctx, 1*time.Second); err != nil {
			return nil, err
		}
	}
}

// fetchReplaysFromAPI fetches replays from the API, filtering out blacklisted ones.
func fetchReplaysFromAPI(ctx context.Context, maxReplays int, blacklist *Blacklist) ([]ReplayInfo, error) {
	url := fmt.Sprintf("https://www.radarvan.com/api/replays_without_playerstats/?max_to_return=%d", maxReplays)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status: %s", resp.Status)
	}

	var replays []ReplayInfo
	if err := json.NewDecoder(resp.Body).Decode(&replays); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	// Filter out blacklisted
	var filtered []ReplayInfo
	skipped := 0
	for _, replay := range replays {
		if blacklist.IsBlacklisted(replay.MatchID) {
			skipped++
			continue
		}
		filtered = append(filtered, replay)
	}

	if skipped > 0 {
		fmt.Printf("Skipped %d blacklisted replay(s)\n", skipped)
	}

	return filtered, nil
}

// downloadReplayFile downloads a replay file from a URL.
func downloadReplayFile(ctx context.Context, url, destDir string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("404_NOT_FOUND: download returned 404")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download returned status: %s", resp.Status)
	}

	// Extract filename, stripping query parameters
	urlPath := url
	if idx := strings.Index(url, "?"); idx != -1 {
		urlPath = url[:idx]
	}
	filename := filepath.Base(urlPath)
	if filename == "" || filename == "." || filename == "/" {
		filename = "replay.rep"
	}
	if !strings.HasSuffix(strings.ToLower(filename), ".rep") {
		filename += ".rep"
	}

	destPath := filepath.Join(destDir, filename)

	out, err := os.Create(destPath)
	if err != nil {
		return "", fmt.Errorf("creating file: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, resp.Body); err != nil {
		return "", fmt.Errorf("writing file: %w", err)
	}

	return destPath, nil
}

// downloadReplayWithFallback tries primary URL, then presigned URL if available.
func downloadReplayWithFallback(ctx context.Context, replay ReplayInfo, destDir string) (string, error) {
	if replay.URL != "" {
		filePath, err := downloadReplayFile(ctx, replay.URL, destDir)
		if err == nil {
			return filePath, nil
		}

		if replay.PresignedURL != "" {
			fmt.Printf("Primary URL failed (%v), trying presigned URL...\n", err)
			return downloadReplayFile(ctx, replay.PresignedURL, destDir)
		}
		return "", err
	}

	if replay.PresignedURL != "" {
		return downloadReplayFile(ctx, replay.PresignedURL, destDir)
	}

	return "", fmt.Errorf("no URL available for download")
}

// triggerReparse triggers a reparse on the API.
func triggerReparse(ctx context.Context, gameSeed string, matchID int, blacklist *Blacklist) (bool, error) {
	fmt.Println("Waiting 2 seconds before triggering reparse...")
	if err := sleepWithContext(ctx, 2*time.Second); err != nil {
		return false, err
	}

	fmt.Printf("Triggering reparse for seed: %s (match_id: %d)\n", gameSeed, matchID)

	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("https://www.radarvan.com/api/reprase/%s", gameSeed), nil)
	if err != nil {
		return false, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("reparse failed: %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Errorf("reading response: %w", err)
	}

	bodyStr := strings.TrimSpace(string(body))
	isNull := bodyStr == "" || bodyStr == "null" || bodyStr == "{}" || bodyStr == "[]"

	if isNull {
		if matchID > 0 {
			fmt.Printf("Reparse returned null for match_id: %d\n", matchID)
			blacklist.Add(matchID)
			if err := blacklist.Save(blacklistFile); err != nil {
				fmt.Printf("Warning: Failed to save blacklist: %v\n", err)
			} else {
				fmt.Printf("Added match_id %d to blacklist\n", matchID)
			}
		} else {
			fmt.Println("Reparse returned null (non-API mode, not blacklisting)")
		}
		return true, nil
	}

	fmt.Println("Reparse triggered successfully")
	return false, nil
}

// runAPIMode runs the automation in API mode.
func runAPIMode(ctx context.Context, config *Config, blacklist *Blacklist) error {
	gp := NewGeneralsProcess(config.GeneralsExe)
	processor := NewReplayProcessor(config, blacklist)

	fmt.Println("Starting generals.exe for API mode...")
	if err := gp.Start(); err != nil {
		return err
	}
	defer gp.Kill()

	if err := gp.Initialize(ctx, config.InitialWait); err != nil {
		return err
	}

	var lastMatchID int
	var lastSuccessfulMatchID int
	fileCount := 0

	for {
		if ctx.Err() != nil {
			return nil
		}

		// Check if process crashed
		if !gp.IsRunning() {
			fmt.Println("Process exited unexpectedly!")
			if lastMatchID > 0 {
				fmt.Printf("Adding match_id %d to blacklist (process crashed)\n", lastMatchID)
				if err := blacklist.AddWithComment(lastMatchID, "process crashed during replay", blacklistFile); err != nil {
					fmt.Printf("Warning: Failed to add to blacklist: %v\n", err)
				}
			}

			fmt.Println("Restarting generals.exe...")
			if err := gp.Start(); err != nil {
				return err
			}
			if err := gp.Initialize(ctx, config.InitialWait); err != nil {
				return err
			}
			lastMatchID = 0
			continue
		}

		// Fetch replays
		fmt.Println("\nFetching replays from API...")
		replays, err := fetchReplaysFromAPI(ctx, config.MaxReplays, blacklist)
		if err != nil {
			return fmt.Errorf("fetching replays: %w", err)
		}

		if len(replays) == 0 {
			fmt.Println("No more replays available. Automation complete!")
			break
		}

		fileCount++
		currentMatchID := replays[0].MatchID
		lastMatchID = currentMatchID

		// Check if this is the same match_id we just successfully processed
		// This indicates radarvan isn't reparsing it properly
		if lastSuccessfulMatchID > 0 && currentMatchID == lastSuccessfulMatchID {
			fmt.Printf("match_id %d returned again after successful processing - radarvan not reparsing properly\n", currentMatchID)
			if err := blacklist.AddWithComment(currentMatchID, "radarvan not reparsing properly", blacklistFile); err != nil {
				fmt.Printf("Warning: Failed to add to blacklist: %v\n", err)
			}
			lastSuccessfulMatchID = 0 // Reset to avoid repeated blacklisting
			continue
		}

		fmt.Printf("\n=== Processing file %d (match_id: %d) ===\n", fileCount, currentMatchID)

		// Clear destination and download replay
		fmt.Println("Clearing destination directory...")
		if err := automation.DeleteAllRepFiles(config.DirectoryA); err != nil {
			return fmt.Errorf("deleting files: %w", err)
		}

		fmt.Println("Downloading replay file...")
		replayPath, err := downloadReplayWithFallback(ctx, replays[0], config.DirectoryA)
		if err != nil {
			if strings.Contains(err.Error(), "404_NOT_FOUND") {
				fmt.Printf("Replay returned 404, blacklisting match_id %d\n", currentMatchID)
				blacklist.AddWithComment(currentMatchID, "404 error downloading replay", blacklistFile)
				continue
			}
			return fmt.Errorf("downloading replay: %w", err)
		}
		fmt.Printf("Downloaded: %s\n", replayPath)

		// Validate replay
		fmt.Println("Validating replay file...")
		isValid, actualBuildDate, err := automation.ValidateReplayFile(replayPath, config.ReplayAPIURL)
		if err != nil {
			return fmt.Errorf("validating replay: %w", err)
		}
		if !isValid {
			fmt.Printf("Warning: BuildDate mismatch. Expected: Mar 10 2005 13:47:03, Got: %s\n", actualBuildDate)
		}

		// Process the replay
		result := processor.Process(ctx, currentMatchID)
		if result.Error != nil {
			if ctx.Err() != nil {
				return nil
			}
			fmt.Printf("Error processing replay: %v\n", result.Error)
			continue
		}

		// Trigger reparse
		if _, err := triggerReparse(ctx, result.GameSeed, currentMatchID, blacklist); err != nil {
			fmt.Printf("Error triggering reparse: %v\n", err)
		}

		// Track this as the last successfully completed match_id
		lastSuccessfulMatchID = currentMatchID
		fmt.Printf("Completed processing file %d\n", fileCount)
	}

	return nil
}

// runDirectoryMode runs the automation in directory mode.
func runDirectoryMode(ctx context.Context, config *Config, blacklist *Blacklist) error {
	processor := NewReplayProcessor(config, blacklist)
	fileCount := 0

	for {
		if ctx.Err() != nil {
			return nil
		}

		// Check for remaining files
		count, err := automation.CountRepFiles(config.DirectoryB)
		if err != nil {
			return fmt.Errorf("counting files: %w", err)
		}

		if count == 0 {
			fmt.Println("No more .rep files. Automation complete!")
			break
		}

		fileCount++
		fmt.Printf("\n=== Processing file %d ===\n", fileCount)

		// Start generals for this file
		gp := NewGeneralsProcess(config.GeneralsExe)
		fmt.Println("Starting generals.exe...")
		if err := gp.Start(); err != nil {
			fmt.Printf("Error starting generals: %v\n", err)
			continue
		}

		if err := gp.Initialize(ctx, config.InitialWait); err != nil {
			gp.Kill()
			if ctx.Err() != nil {
				return nil
			}
			fmt.Printf("Error initializing: %v\n", err)
			continue
		}

		// Clear destination and move replay
		fmt.Println("Clearing destination directory...")
		if err := automation.DeleteAllRepFiles(config.DirectoryA); err != nil {
			gp.Kill()
			fmt.Printf("Error deleting files: %v\n", err)
			continue
		}

		fmt.Println("Moving replay file...")
		movedFile, err := automation.MoveOneRepFile(config.DirectoryB, config.DirectoryA)
		if err != nil {
			gp.Kill()
			fmt.Printf("Error moving file: %v\n", err)
			continue
		}
		fmt.Printf("Moved: %s\n", movedFile)

		// Validate replay
		fmt.Println("Validating replay file...")
		isValid, actualBuildDate, err := automation.ValidateReplayFile(movedFile, config.ReplayAPIURL)
		if err != nil {
			fmt.Printf("Error validating: %v\n", err)
			automation.MoveFileBackWithOldExtension(movedFile, config.DirectoryB)
			gp.Kill()
			continue
		}
		if !isValid {
			fmt.Printf("Warning: BuildDate mismatch. Expected: Mar 10 2005 13:47:03, Got: %s\n", actualBuildDate)
		}

		// Process the replay
		result := processor.Process(ctx, 0)
		if result.Error != nil {
			gp.Kill()
			if ctx.Err() != nil {
				return nil
			}
			fmt.Printf("Error processing: %v\n", result.Error)
			continue
		}

		// Trigger reparse (matchID=0 for directory mode)
		if _, err := triggerReparse(ctx, result.GameSeed, 0, blacklist); err != nil {
			fmt.Printf("Error triggering reparse: %v\n", err)
		}

		// Cleanup
		fmt.Println("Terminating generals.exe...")
		gp.Kill()
		gp.Wait(ctx)

		fmt.Printf("Completed processing file %d\n", fileCount)
		sleepWithContext(ctx, 2*time.Second)
	}

	return nil
}

func main() {
	config, err := parseFlags()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	if config == nil {
		return // Help was shown
	}

	printConfig(config)

	// Load blacklist
	blacklist, err := LoadBlacklist(blacklistFile)
	if err != nil {
		fmt.Printf("Warning: Failed to load blacklist: %v\n", err)
		blacklist = NewBlacklist()
	} else if count := blacklist.Count(); count > 0 {
		fmt.Printf("Loaded %d blacklisted match_id(s)\n", count)
	}

	// Setup context with signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		fmt.Printf("\nReceived signal %v. Shutting down...\n", sig)
		cancel()
	}()

	// Run appropriate mode
	if config.UseAPI {
		if err := runAPIMode(ctx, config, blacklist); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
	} else {
		if err := runDirectoryMode(ctx, config, blacklist); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Println("\n=== Automation Complete ===")
}

func parseFlags() (*Config, error) {
	var (
		directoryA           = flag.String("dir-a", "C:\\Users\\Bill\\Documents\\Command and Conquer Generals Zero Hour Data\\Replays", "Directory A (destination)")
		directoryB           = flag.String("dir-b", "C:\\Users\\Bill\\Desktop\\Replays", "Directory B (source)")
		generalsExe          = flag.String("generals-exe", "C:\\Program Files (x86)\\Origin Games\\Command and Conquer Generals Zero Hour\\Command and Conquer Generals Zero Hour\\generals.exe", "Path to generals.exe")
		processName          = flag.String("process", "generals.exe", "Process name to monitor")
		initialWait          = flag.Duration("initial-wait", 15*time.Second, "Wait time after starting")
		clicksBeforeStart    = flag.String("clicks-before-start", "100,100;2036,512;2036,412;778,388;2073,415;1100,795", "Mouse clicks before timecode start")
		clicksAfterStart     = flag.String("clicks-after-start", "", "Mouse clicks after timecode starts")
		clicksAfterStop      = flag.String("clicks-after-stop", "2181,1364;2096,996", "Mouse clicks after timecode stops")
		timecodeStartTimeout = flag.Duration("timecode-start-timeout", 1*time.Minute, "Timeout for timecode start")
		timecodeStopTimeout  = flag.Duration("timecode-stop-timeout", 10*time.Minute, "Timeout for timecode stop")
		replayAPIURL         = flag.String("replay-api-url", "https://cncstats.herokuapp.com/replay", "API URL for validation")
		useAPI               = flag.Bool("use-api", false, "Use API mode")
		maxReplays           = flag.Int("max-replays", 10, "Max replays to fetch from API")
		help                 = flag.Bool("help", false, "Show help")
	)
	flag.Parse()

	if *help {
		showHelp()
		return nil, nil
	}

	// Validate
	if *directoryA == "" {
		return nil, fmt.Errorf("-dir-a is required")
	}
	if !*useAPI && *directoryB == "" {
		return nil, fmt.Errorf("-dir-b is required when not using API mode")
	}
	if *generalsExe == "" {
		return nil, fmt.Errorf("-generals-exe is required")
	}

	// Parse coordinates
	beforeCoords, err := parseCoordinates(*clicksBeforeStart)
	if err != nil {
		return nil, fmt.Errorf("parsing -clicks-before-start: %w", err)
	}
	afterStartCoords, err := parseCoordinates(*clicksAfterStart)
	if err != nil {
		return nil, fmt.Errorf("parsing -clicks-after-start: %w", err)
	}
	afterStopCoords, err := parseCoordinates(*clicksAfterStop)
	if err != nil {
		return nil, fmt.Errorf("parsing -clicks-after-stop: %w", err)
	}

	return &Config{
		DirectoryA:           *directoryA,
		DirectoryB:           *directoryB,
		GeneralsExe:          *generalsExe,
		ProcessName:          *processName,
		InitialWait:          *initialWait,
		ClicksBeforeStart:    beforeCoords,
		ClicksAfterStart:     afterStartCoords,
		ClicksAfterStop:      afterStopCoords,
		TimecodeStartTimeout: *timecodeStartTimeout,
		TimecodeStopTimeout:  *timecodeStopTimeout,
		ReplayAPIURL:         *replayAPIURL,
		UseAPI:               *useAPI,
		MaxReplays:           *maxReplays,
	}, nil
}

func printConfig(config *Config) {
	fmt.Println("=== Automated Replay Parsing Mode ===")
	fmt.Printf("Directory A (destination): %s\n", config.DirectoryA)
	if config.UseAPI {
		fmt.Printf("Mode: API (fetching from radarvan API)\n")
		fmt.Printf("Max replays per fetch: %d\n", config.MaxReplays)
	} else {
		fmt.Printf("Directory B (source): %s\n", config.DirectoryB)
	}
	fmt.Printf("Generals.exe: %s\n", config.GeneralsExe)
	fmt.Printf("Initial wait: %v\n", config.InitialWait)
	fmt.Printf("Clicks before start: %d\n", len(config.ClicksBeforeStart))
	fmt.Printf("Key after start: F (0x%02X)\n", VK_F)
	fmt.Printf("Clicks after start: %d\n", len(config.ClicksAfterStart))
	fmt.Printf("Clicks after stop: %d\n", len(config.ClicksAfterStop))
	fmt.Println()
}

func parseCoordinates(coordStr string) ([]Coordinate, error) {
	if coordStr == "" {
		return nil, nil
	}

	var coords []Coordinate
	pairs := strings.Split(coordStr, ";")

	for _, pair := range pairs {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}

		parts := strings.Split(pair, ",")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid coordinate: %s (expected x,y)", pair)
		}

		x, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid X coordinate: %s", parts[0])
		}

		y, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid Y coordinate: %s", parts[1])
		}

		coords = append(coords, Coordinate{X: int32(x), Y: int32(y)})
	}

	return coords, nil
}

// toAutomationCoords converts our Coordinate type to the automation package format.
func toAutomationCoords(coords []Coordinate) []struct{ X, Y int32 } {
	result := make([]struct{ X, Y int32 }, len(coords))
	for i, c := range coords {
		result[i] = struct{ X, Y int32 }{X: c.X, Y: c.Y}
	}
	return result
}

func sleepWithContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func showHelp() {
	fmt.Println("CNC Automation - Automated Replay Parsing Tool")
	fmt.Println()
	fmt.Println("Usage: automation [flags]")
	fmt.Println()
	fmt.Println("Flags:")
	fmt.Println("  -dir-a string              Directory A - destination for replay files (required)")
	fmt.Println("  -dir-b string              Directory B - source for replay files (required without -use-api)")
	fmt.Println("  -generals-exe string       Path to generals.exe (required)")
	fmt.Println("  -process string            Process name to monitor (default: generals.exe)")
	fmt.Println("  -initial-wait duration     Wait time after starting (default: 15s)")
	fmt.Println("  -clicks-before-start       Mouse clicks before timecode start (format: x1,y1;x2,y2;...)")
	fmt.Println("  -clicks-after-start        Mouse clicks after timecode starts")
	fmt.Println("  -clicks-after-stop         Mouse clicks after timecode stops")
	fmt.Println("  -timecode-start-timeout    Timeout for timecode start (default: 1m)")
	fmt.Println("  -timecode-stop-timeout     Timeout for timecode stop (default: 10m)")
	fmt.Println("  -replay-api-url string     API URL for replay validation")
	fmt.Println("  -use-api                   Use API mode: fetch replays from radarvan API")
	fmt.Println("  -max-replays int           Max replays to fetch per API request (default: 10)")
	fmt.Println("  -help                      Show this help")
	fmt.Println()
	fmt.Println("Note: The 'F' key is automatically pressed after timecode starts.")
	fmt.Println()
	fmt.Println("Example:")
	fmt.Println("  automation -dir-a C:\\replays\\processed -dir-b C:\\replays\\queue \\")
	fmt.Println("    -generals-exe C:\\game\\generals.exe -clicks-before-start \"100,200;300,400\"")
}
