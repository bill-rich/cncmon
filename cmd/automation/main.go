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

const VK_F = 0x46 // Virtual key code for 'F' key

const blacklistFile = "blacklisted_match_ids.txt"

// ReplayInfo represents a replay from the API
type ReplayInfo struct {
	MatchID int    `json:"match_id"`
	URL     string `json:"url"`
}

// Blacklist manages a set of blacklisted match IDs
type Blacklist struct {
	matchIDs map[string]bool
	mu       sync.RWMutex
}

// NewBlacklist creates a new blacklist
func NewBlacklist() *Blacklist {
	return &Blacklist{
		matchIDs: make(map[string]bool),
	}
}

// LoadBlacklist loads the blacklist from a file
func LoadBlacklist(filename string) (*Blacklist, error) {
	bl := NewBlacklist()

	file, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			// File doesn't exist yet, return empty blacklist
			return bl, nil
		}
		return nil, fmt.Errorf("error opening blacklist file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()

		// Strip comments (everything after //)
		if commentIdx := strings.Index(line, "//"); commentIdx >= 0 {
			line = line[:commentIdx]
		}

		// Trim whitespace
		matchID := strings.TrimSpace(line)

		// Only process non-empty match_ids
		if matchID != "" {
			bl.matchIDs[matchID] = true
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading blacklist file: %w", err)
	}

	return bl, nil
}

// SaveBlacklist saves the blacklist to a file
func (bl *Blacklist) SaveBlacklist(filename string) error {
	bl.mu.RLock()
	defer bl.mu.RUnlock()

	file, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("error creating blacklist file: %w", err)
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	for matchID := range bl.matchIDs {
		if _, err := writer.WriteString(matchID + "\n"); err != nil {
			return fmt.Errorf("error writing to blacklist file: %w", err)
		}
	}
	return writer.Flush()
}

// AddWithComment adds a match ID to the blacklist with a comment and appends it to the file
func (bl *Blacklist) AddWithComment(matchID int, comment string, filename string) error {
	bl.mu.Lock()
	defer bl.mu.Unlock()

	matchIDStr := strconv.Itoa(matchID)
	bl.matchIDs[matchIDStr] = true

	// Append to file with comment
	file, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("error opening blacklist file: %w", err)
	}
	defer file.Close()

	line := matchIDStr
	if comment != "" {
		line += " // " + comment
	}
	line += "\n"

	if _, err := file.WriteString(line); err != nil {
		return fmt.Errorf("error writing to blacklist file: %w", err)
	}

	return nil
}

// IsBlacklisted checks if a match ID is blacklisted
func (bl *Blacklist) IsBlacklisted(matchID int) bool {
	bl.mu.RLock()
	defer bl.mu.RUnlock()
	return bl.matchIDs[strconv.Itoa(matchID)]
}

// Add adds a match ID to the blacklist
func (bl *Blacklist) Add(matchID int) error {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	bl.matchIDs[strconv.Itoa(matchID)] = true
	return nil
}

// fetchReplaysFromAPI fetches a list of replays from the radarvan API and filters out blacklisted seeds
func fetchReplaysFromAPI(ctx context.Context, maxReplays int, blacklist *Blacklist) ([]ReplayInfo, error) {
	url := fmt.Sprintf("https://www.radarvan.com/api/replays_without_playerstats/?max_to_return=%d", maxReplays)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %w", err)
	}
	req.Header.Set("accept", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status: %s", resp.Status)
	}

	var replays []ReplayInfo
	if err := json.NewDecoder(resp.Body).Decode(&replays); err != nil {
		return nil, fmt.Errorf("error decoding response: %w", err)
	}

	// Filter out replays with blacklisted match IDs
	var filteredReplays []ReplayInfo
	skippedCount := 0
	for _, replay := range replays {
		if blacklist.IsBlacklisted(replay.MatchID) {
			skippedCount++
			continue
		}
		filteredReplays = append(filteredReplays, replay)
	}

	if skippedCount > 0 {
		fmt.Printf("Skipped %d blacklisted replay(s)\n", skippedCount)
	}

	return filteredReplays, nil
}

// downloadReplayFile downloads a replay file from a URL and saves it to the destination directory
func downloadReplayFile(ctx context.Context, url string, destDir string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("error creating request: %w", err)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("error downloading file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("404_NOT_FOUND: download returned 404")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download returned status: %s", resp.Status)
	}

	// Extract filename from URL
	filename := filepath.Base(url)
	if filename == "" || filename == "." || filename == "/" {
		filename = "replay.rep"
	}

	// Ensure it has .rep extension
	if !strings.HasSuffix(strings.ToLower(filename), ".rep") {
		filename += ".rep"
	}

	destPath := filepath.Join(destDir, filename)

	// Create the file
	out, err := os.Create(destPath)
	if err != nil {
		return "", fmt.Errorf("error creating file: %w", err)
	}
	defer out.Close()

	// Write the body to file
	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return "", fmt.Errorf("error writing file: %w", err)
	}

	return destPath, nil
}

// triggerReparse triggers a reparse on the radarvan API for the given game seed
// Returns true if the response was null/empty (indicating the match_id should be blacklisted)
func triggerReparse(ctx context.Context, gameSeed string, matchID int, blacklist *Blacklist) (bool, error) {
	fmt.Printf("Waiting %d seconds before triggering reparse...\n", 2)
	if err := sleepWithContext(ctx, time.Duration(2)*time.Second); err != nil {
		return false, err
	}

	fmt.Printf("Triggering reparse on the radarvan api for seed: %s (match_id: %d)\n", gameSeed, matchID)
	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("https://www.radarvan.com/api/reprase/%s", gameSeed), nil)
	if err != nil {
		return false, fmt.Errorf("error creating request: %w", err)
	}
	req.Header.Set("accept", "application/json")
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("error sending request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("error triggering reparse: %s", resp.Status)
	}

	// Read the response body to check if it's null/empty
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Errorf("error reading response: %w", err)
	}

	bodyStr := strings.TrimSpace(string(body))
	isNull := bodyStr == "" || bodyStr == "null" || bodyStr == "{}" || bodyStr == "[]"

	if isNull {
		if matchID > 0 {
			// Only blacklist if we have a valid match_id (API mode)
			fmt.Printf("Reparse API returned null/empty response for match_id: %d\n", matchID)
			blacklist.Add(matchID)
			if err := blacklist.SaveBlacklist(blacklistFile); err != nil {
				fmt.Printf("Warning: Failed to save blacklist: %v\n", err)
			} else {
				fmt.Printf("Added match_id %d to blacklist\n", matchID)
			}
		} else {
			fmt.Printf("Reparse API returned null/empty response (non-API mode, not blacklisting)\n")
		}
		return true, nil
	}

	fmt.Printf("Reparse triggered successfully\n")
	return false, nil
}

func main() {
	// Parse command line arguments
	var (
		directoryA           = flag.String("dir-a", "C:\\Users\\Bill\\Documents\\Command and Conquer Generals Zero Hour Data\\Replays", "Directory A (destination for replay files)")
		directoryB           = flag.String("dir-b", "C:\\Users\\Bill\\Desktop\\Replays", "Directory B (source for replay files)")
		generalsExe          = flag.String("generals-exe", "C:\\Program Files (x86)\\Origin Games\\Command and Conquer Generals Zero Hour\\Command and Conquer Generals Zero Hour\\generals.exe", "Path to generals.exe")
		processName          = flag.String("process", "generals.exe", "Process name to monitor (default: generals.exe)")
		initialWait          = flag.Duration("initial-wait", 15*time.Second, "Wait time after starting generals.exe")
		clicksBeforeStart    = flag.String("clicks-before-start", "100,100;2036,512;2036,412;778,388;2073,415;1100,795", "Mouse click coordinates before timecode start (format: x1,y1;x2,y2;...)")
		clicksAfterStart     = flag.String("clicks-after-start", "", "Mouse click coordinates after timecode starts (format: x1,y1;x2,y2;...)")
		clicksAfterStop      = flag.String("clicks-after-stop", "2181,1364;2096,996", "Mouse click coordinates after timecode stops (format: x1,y1;x2,y2;...)")
		timecodeStartTimeout = flag.Duration("timecode-start-timeout", 1*time.Minute, "Timeout for waiting for timecode to start")
		timecodeStopTimeout  = flag.Duration("timecode-stop-timeout", 10*time.Minute, "Timeout for waiting for timecode to stop")
		replayAPIURL         = flag.String("replay-api-url", "https://cncstats.herokuapp.com/replay", "API URL for replay validation")
		useAPI               = flag.Bool("use-api", false, "Use API mode: fetch replays from radarvan API instead of DirectoryB")
		maxReplays           = flag.Int("max-replays", 10, "Maximum number of replays to fetch from API (only used with -use-api)")
		help                 = flag.Bool("help", false, "Show help information")
	)
	flag.Parse()

	// Show help if requested
	if *help {
		showHelp()
		return
	}

	// Validate required parameters
	if *directoryA == "" {
		fmt.Println("Error: -dir-a is required")
		os.Exit(1)
	}
	if !*useAPI && *directoryB == "" {
		fmt.Println("Error: -dir-b is required when not using API mode")
		os.Exit(1)
	}
	if *generalsExe == "" {
		fmt.Println("Error: -generals-exe is required")
		os.Exit(1)
	}

	// Parse coordinates
	clicksBeforeStartCoords, err := parseCoordinates(*clicksBeforeStart)
	if err != nil {
		fmt.Printf("Error parsing -clicks-before-start: %v\n", err)
		os.Exit(1)
	}

	clicksAfterStartCoords, err := parseCoordinates(*clicksAfterStart)
	if err != nil {
		fmt.Printf("Error parsing -clicks-after-start: %v\n", err)
		os.Exit(1)
	}

	clicksAfterStopCoords, err := parseCoordinates(*clicksAfterStop)
	if err != nil {
		fmt.Printf("Error parsing -clicks-after-stop: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("=== Automated Replay Parsing Mode ===")
	fmt.Printf("Directory A (destination): %s\n", *directoryA)
	if *useAPI {
		fmt.Printf("Mode: API (fetching from radarvan API)\n")
		fmt.Printf("Max replays per fetch: %d\n", *maxReplays)
	} else {
		fmt.Printf("Directory B (source): %s\n", *directoryB)
	}
	fmt.Printf("Generals.exe: %s\n", *generalsExe)
	fmt.Printf("Initial wait: %v\n", *initialWait)
	fmt.Printf("Clicks before start: %d\n", len(clicksBeforeStartCoords))
	fmt.Printf("Key after start: F (0x%02X) [hardcoded]\n", VK_F)
	fmt.Printf("Clicks after start: %d\n", len(clicksAfterStartCoords))
	fmt.Printf("Clicks after stop: %d\n", len(clicksAfterStopCoords))
	fmt.Println()

	// Load blacklist
	blacklist, err := LoadBlacklist(blacklistFile)
	if err != nil {
		fmt.Printf("Warning: Failed to load blacklist: %v\n", err)
		blacklist = NewBlacklist()
	} else {
		blacklist.mu.RLock()
		count := len(blacklist.matchIDs)
		blacklist.mu.RUnlock()
		if count > 0 {
			fmt.Printf("Loaded %d blacklisted match_id(s) from %s\n", count, blacklistFile)
		}
	}

	// Set up context-based signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Cancel context when signal is received
	go func() {
		sig := <-sigChan
		fmt.Printf("\nReceived signal %v. Shutting down gracefully...\n", sig)
		cancel()
	}()

	fileCount := 0

	if *useAPI {
		// API Mode: Start generals.exe once, then loop fetching and processing replays
		fmt.Println("Starting generals.exe for API mode...")
		cmd, err := automation.StartGenerals(*generalsExe)
		if err != nil {
			fmt.Printf("Error starting generals.exe: %v\n", err)
			os.Exit(1)
		}

		// Wait a bit for window to appear, then set target window for PostMessage
		if err := sleepWithContext(ctx, 2*time.Second); err != nil {
			cmd.Process.Kill()
			return
		}
		fmt.Println("Finding game window...")
		if err := automation.SetTargetWindow(uint32(cmd.Process.Pid)); err != nil {
			fmt.Printf("Warning: Failed to find game window: %v (will try SendInput fallback)\n", err)
		} else {
			fmt.Println("Game window found - using PostMessage for input (works better with DirectInput games)")
		}

		// Wait for initial wait time
		fmt.Printf("Waiting %v...\n", *initialWait)
		if err := sleepWithContext(ctx, *initialWait); err != nil {
			cmd.Process.Kill()
			return
		}

		// Press Escape
		fmt.Println("Pressing Escape key...")
		if err := automation.PressEscape(); err != nil {
			fmt.Printf("Error pressing Escape: %v\n", err)
			cmd.Process.Kill()
			os.Exit(1)
		}
		if err := sleepWithContext(ctx, 10*time.Second); err != nil {
			cmd.Process.Kill()
			return
		}

		// Main loop: fetch replays and process them
		for {
			// Check if context is cancelled
			if ctx.Err() != nil {
				cmd.Process.Kill()
				return
			}

			// Step 2: Fetch replays from API and download the first one
			fmt.Println("\nStep 2: Fetching replays from API...")
			replays, err := fetchReplaysFromAPI(ctx, *maxReplays, blacklist)
			if err != nil {
				fmt.Printf("Error fetching replays: %v\n", err)
				cmd.Process.Kill()
				os.Exit(1)
			}

			if len(replays) == 0 {
				fmt.Println("No more replays available from API. Automation complete!")
				break
			}

			fileCount++
			currentMatchID := replays[0].MatchID
			fmt.Printf("\n=== Processing file %d (match_id: %d) ===\n", fileCount, currentMatchID)

			// Step 1: Delete all .rep files from DirectoryA
			fmt.Println("Step 1: Deleting all .rep files from DirectoryA...")
			if err := automation.DeleteAllRepFiles(*directoryA); err != nil {
				fmt.Printf("Error deleting files: %v\n", err)
				cmd.Process.Kill()
				os.Exit(1)
			}

			// Download the first replay
			fmt.Println("Step 2 (continued): Downloading replay file...")
			movedFile, err := downloadReplayFile(ctx, replays[0].URL, *directoryA)
			if err != nil {
				// Check if it's a 404 error
				if strings.Contains(err.Error(), "404_NOT_FOUND") {
					fmt.Printf("Replay file returned 404, adding match_id %d to blacklist\n", currentMatchID)
					if err := blacklist.AddWithComment(currentMatchID, "404 error downloading replay", blacklistFile); err != nil {
						fmt.Printf("Warning: Failed to add to blacklist: %v\n", err)
					} else {
						fmt.Printf("Added match_id %d to blacklist (404 error)\n", currentMatchID)
					}
					// Continue to next replay
					continue
				}
				fmt.Printf("Error downloading file: %v\n", err)
				cmd.Process.Kill()
				os.Exit(1)
			}
			fmt.Printf("Downloaded file: %s\n", movedFile)

			// Step 2.5: Validate replay file BuildDate
			fmt.Println("Step 2.5: Validating replay file BuildDate...")
			isValid, actualBuildDate, err := automation.ValidateReplayFile(movedFile, *replayAPIURL)
			if err != nil {
				fmt.Printf("Error validating replay file: %v\n", err)
				cmd.Process.Kill()
				os.Exit(1)
			}
			if !isValid {
				fmt.Printf("BuildDate does not match expected value. Expected: Mar 10 2005 13:47:03, Actual BuildDate: %s\n", actualBuildDate)
			}
			fmt.Println("Replay file BuildDate validated successfully.")

			// Step 6: Simulate mouse clicks before timecode start
			if len(clicksBeforeStartCoords) > 0 {
				fmt.Printf("Step 6: Simulating %d mouse clicks before timecode start...\n", len(clicksBeforeStartCoords))
				if err := automation.ClickAtMultiple(ctx, clicksBeforeStartCoords); err != nil {
					if ctx.Err() != nil {
						cmd.Process.Kill()
						return
					}
					fmt.Printf("Error clicking: %v\n", err)
					cmd.Process.Kill()
					continue
				}
			}

			// Step 7: Wait for generals.exe process to be available and initialize memory reader
			fmt.Println("Step 7: Waiting for generals.exe process to be available...")
			var memReader *zhreader.Reader
			for {
				if ctx.Err() != nil {
					cmd.Process.Kill()
					return
				}

				memReader, err = zhreader.Init(*processName)
				if err == nil {
					break
				}
				if err := sleepWithContext(ctx, 1*time.Second); err != nil {
					cmd.Process.Kill()
					return
				}
			}

			// Step 8: Wait for timecode to start increasing
			fmt.Println("Step 8: Waiting for timecode to start increasing...")
			getTimecode := func() (uint32, error) {
				return memReader.GetTimecode()
			}

			if err := automation.WaitForTimecodeStart(ctx, getTimecode, *timecodeStartTimeout); err != nil {
				if ctx.Err() != nil {
					memReader.Close()
					cmd.Process.Kill()
					return
				}
				fmt.Printf("Error waiting for timecode to start: %v\n", err)
				memReader.Close()
				cmd.Process.Kill()
				continue
			}

			// Step 8.5: Get game seed from memory
			fmt.Println("Step 8.5: Getting game seed from memory...")
			gameSeed := memReader.GetSeed()
			if gameSeed == "" {
				fmt.Printf("Error getting game seed: %v\n", err)
				memReader.Close()
				cmd.Process.Kill()
				continue
			}
			fmt.Printf("Game seed: %s\n", gameSeed)

			// Step 9: Press 'F' key after timecode starts (hardcoded)
			fmt.Println("Step 9: Pressing 'F' key...")
			if err := sleepWithContext(ctx, 500*time.Millisecond); err != nil {
				memReader.Close()
				cmd.Process.Kill()
				return
			}
			if err := automation.PressKey(VK_F); err != nil {
				fmt.Printf("Error pressing 'F' key: %v\n", err)
				memReader.Close()
				cmd.Process.Kill()
				continue
			}

			// Step 10: Wait for timecode to stop increasing
			fmt.Println("Step 11: Waiting for timecode to stop increasing...")
			_, err = automation.WaitForTimecodeStop(ctx, getTimecode, *timecodeStopTimeout)
			if err != nil {
				if ctx.Err() != nil {
					memReader.Close()
					cmd.Process.Kill()
					return
				}
				fmt.Printf("Error waiting for timecode to stop: %v\n", err)
				memReader.Close()
				cmd.Process.Kill()
				continue
			}

			// Step 11: Simulate mouse clicks after timecode stops
			if err := sleepWithContext(ctx, 1*time.Second); err != nil {
				memReader.Close()
				cmd.Process.Kill()
				return
			}
			if len(clicksAfterStopCoords) > 0 {
				fmt.Printf("Step 12: Simulating %d mouse clicks after timecode stops...\n", len(clicksAfterStopCoords))
				if err := automation.ClickAtMultiple(ctx, clicksAfterStopCoords); err != nil {
					if ctx.Err() != nil {
						memReader.Close()
						cmd.Process.Kill()
						return
					}
					fmt.Printf("Error clicking: %v\n", err)
				}
			}

			// Trigger reparse before killing generals.exe (API mode)
			_, err = triggerReparse(ctx, gameSeed, currentMatchID, blacklist)
			if err != nil {
				fmt.Printf("Error triggering reparse: %v\n", err)
				memReader.Close()
				cmd.Process.Kill()
				continue
			}

			// Close memory reader after processing
			memReader.Close()

			fmt.Printf("Completed processing file %d\n", fileCount)
			// Loop back to Step 2 to fetch next replay
		}

		// Clean up: kill generals.exe process
		fmt.Println("Cleaning up: Terminating generals.exe...")
		if err := cmd.Process.Kill(); err != nil {
			fmt.Printf("Warning: Error killing process: %v\n", err)
		}
		// Make cmd.Wait() interruptible
		waitDone := make(chan error, 1)
		go func() {
			waitDone <- cmd.Wait()
		}()
		select {
		case <-ctx.Done():
			return
		case <-waitDone:
		}
	} else {
		// Non-API Mode: Original behavior with files from DirectoryB
		for {
			// Check if context is cancelled
			if ctx.Err() != nil {
				return
			}

			// Check if there are any files left in DirectoryB
			count, err := automation.CountRepFiles(*directoryB)
			if err != nil {
				fmt.Printf("Error counting files in DirectoryB: %v\n", err)
				os.Exit(1)
			}

			if count == 0 {
				fmt.Println("No more .rep files in DirectoryB. Automation complete!")
				break
			}

			fileCount++
			fmt.Printf("\n=== Processing file %d ===\n", fileCount)

			// Step 3: Start generals.exe
			fmt.Println("Step 3: Starting generals.exe...")
			cmd, err := automation.StartGenerals(*generalsExe)
			if err != nil {
				fmt.Printf("Error starting generals.exe: %v\n", err)
				continue
			}

			// Wait a bit for window to appear, then set target window for PostMessage
			if err := sleepWithContext(ctx, 2*time.Second); err != nil {
				cmd.Process.Kill()
				return
			}
			fmt.Println("Step 3.5: Finding game window...")
			if err := automation.SetTargetWindow(uint32(cmd.Process.Pid)); err != nil {
				fmt.Printf("Warning: Failed to find game window: %v (will try SendInput fallback)\n", err)
			} else {
				fmt.Println("Game window found - using PostMessage for input (works better with DirectInput games)")
			}

			// Wait for initial wait time
			fmt.Printf("Step 4: Waiting %v...\n", *initialWait)
			if err := sleepWithContext(ctx, *initialWait); err != nil {
				cmd.Process.Kill()
				return
			}

			// Step 5: Press Escape
			fmt.Println("Step 5: Pressing Escape key...")
			if err := automation.PressEscape(); err != nil {
				fmt.Printf("Error pressing Escape: %v\n", err)
				cmd.Process.Kill()
				continue
			}
			if err := sleepWithContext(ctx, 10*time.Second); err != nil {
				cmd.Process.Kill()
				return
			}

			// Step 1: Delete all .rep files from DirectoryA
			fmt.Println("Step 1: Deleting all .rep files from DirectoryA...")
			if err := automation.DeleteAllRepFiles(*directoryA); err != nil {
				fmt.Printf("Error deleting files: %v\n", err)
				cmd.Process.Kill()
				continue
			}

			// Step 2: Move one file from DirectoryB to DirectoryA
			fmt.Println("Step 2: Moving one file from DirectoryB to DirectoryA...")
			movedFile, err := automation.MoveOneRepFile(*directoryB, *directoryA)
			if err != nil {
				fmt.Printf("Error moving file: %v\n", err)
				cmd.Process.Kill()
				continue
			}
			fmt.Printf("Moved file: %s\n", movedFile)

			// Step 2.5: Validate replay file BuildDate
			fmt.Println("Step 2.5: Validating replay file BuildDate...")
			isValid, actualBuildDate, err := automation.ValidateReplayFile(movedFile, *replayAPIURL)
			if err != nil {
				fmt.Printf("Error validating replay file: %v\n", err)
				// Move file back with .old extension on error
				if moveErr := automation.MoveFileBackWithOldExtension(movedFile, *directoryB); moveErr != nil {
					fmt.Printf("Error moving file back: %v\n", moveErr)
				}
				cmd.Process.Kill()
				continue
			}
			if !isValid {
				fmt.Printf("BuildDate does not match expected value. Expected: Mar 10 2005 13:47:03, Actual BuildDate: %s\n", actualBuildDate)
			}
			fmt.Println("Replay file BuildDate validated successfully.")

			// Step 6: Simulate mouse clicks before timecode start
			if len(clicksBeforeStartCoords) > 0 {
				fmt.Printf("Step 6: Simulating %d mouse clicks before timecode start...\n", len(clicksBeforeStartCoords))
				if err := automation.ClickAtMultiple(ctx, clicksBeforeStartCoords); err != nil {
					if ctx.Err() != nil {
						cmd.Process.Kill()
						return
					}
					fmt.Printf("Error clicking: %v\n", err)
					cmd.Process.Kill()
					continue
				}
			}

			// Step 7: Wait for generals.exe process to be available and initialize memory reader
			fmt.Println("Step 7: Waiting for generals.exe process to be available...")
			var memReader *zhreader.Reader
			for {
				if ctx.Err() != nil {
					cmd.Process.Kill()
					return
				}

				memReader, err = zhreader.Init(*processName)
				if err == nil {
					break
				}
				if err := sleepWithContext(ctx, 1*time.Second); err != nil {
					cmd.Process.Kill()
					return
				}
			}
			defer memReader.Close()

			// Step 8: Wait for timecode to start increasing
			fmt.Println("Step 8: Waiting for timecode to start increasing...")
			getTimecode := func() (uint32, error) {
				return memReader.GetTimecode()
			}

			if err := automation.WaitForTimecodeStart(ctx, getTimecode, *timecodeStartTimeout); err != nil {
				if ctx.Err() != nil {
					cmd.Process.Kill()
					return
				}
				fmt.Printf("Error waiting for timecode to start: %v\n", err)
				cmd.Process.Kill()
				continue
			}

			// Step 8.5: Get game seed from memory
			fmt.Println("Step 8.5: Getting game seed from memory...")
			gameSeed := memReader.GetSeed()
			if gameSeed == "" {
				fmt.Printf("Error getting game seed: %v\n", err)
				cmd.Process.Kill()
				continue
			}
			fmt.Printf("Game seed: %s\n", gameSeed)

			// Step 9: Press 'F' key after timecode starts (hardcoded)
			fmt.Println("Step 9: Pressing 'F' key...")
			if err := sleepWithContext(ctx, 500*time.Millisecond); err != nil {
				cmd.Process.Kill()
				return
			}
			if err := automation.PressKey(VK_F); err != nil {
				fmt.Printf("Error pressing 'F' key: %v\n", err)
				cmd.Process.Kill()
				continue
			}

			// Step 10: Wait for timecode to stop increasing
			fmt.Println("Step 11: Waiting for timecode to stop increasing...")
			_, err = automation.WaitForTimecodeStop(ctx, getTimecode, *timecodeStopTimeout)
			if err != nil {
				if ctx.Err() != nil {
					cmd.Process.Kill()
					return
				}
				fmt.Printf("Error waiting for timecode to stop: %v\n", err)
				cmd.Process.Kill()
				continue
			}

			// Step 11: Simulate mouse clicks after timecode stops
			if err := sleepWithContext(ctx, 1*time.Second); err != nil {
				cmd.Process.Kill()
				return
			}
			if len(clicksAfterStopCoords) > 0 {
				fmt.Printf("Step 12: Simulating %d mouse clicks after timecode stops...\n", len(clicksAfterStopCoords))
				if err := automation.ClickAtMultiple(ctx, clicksAfterStopCoords); err != nil {
					if ctx.Err() != nil {
						cmd.Process.Kill()
						return
					}
					fmt.Printf("Error clicking: %v\n", err)
				}
			}

			// Trigger reparse before killing generals.exe (non-API mode)
			// Note: match_id is 0 for non-API mode since we don't have match_id from API
			_, err = triggerReparse(ctx, gameSeed, 0, blacklist)
			if err != nil {
				fmt.Printf("Error triggering reparse: %v\n", err)
			}

			// Clean up: kill generals.exe process
			fmt.Println("Cleaning up: Terminating generals.exe...")
			if err := cmd.Process.Kill(); err != nil {
				fmt.Printf("Warning: Error killing process: %v\n", err)
			}
			// Make cmd.Wait() interruptible
			waitDone := make(chan error, 1)
			go func() {
				waitDone <- cmd.Wait()
			}()
			select {
			case <-ctx.Done():
				return
			case <-waitDone:
			}

			fmt.Printf("Completed processing file %d\n", fileCount)
			if err := sleepWithContext(ctx, 2*time.Second); err != nil {
				return
			}
		}
	}

	fmt.Printf("\n=== Automation Complete ===\n")
	fmt.Printf("Processed %d file(s)\n", fileCount)
}

// sleepWithContext sleeps for the specified duration, but returns early if context is cancelled
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

// parseCoordinates parses a coordinate string in the format "x1,y1;x2,y2;..."
func parseCoordinates(coordStr string) ([]struct{ X, Y int32 }, error) {
	if coordStr == "" {
		return []struct{ X, Y int32 }{}, nil
	}

	coords := []struct{ X, Y int32 }{}
	pairs := strings.Split(coordStr, ";")

	for _, pair := range pairs {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}

		parts := strings.Split(pair, ",")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid coordinate format: %s (expected x,y)", pair)
		}

		x, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid X coordinate: %s", parts[0])
		}

		y, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid Y coordinate: %s", parts[1])
		}

		coords = append(coords, struct{ X, Y int32 }{X: int32(x), Y: int32(y)})
	}

	return coords, nil
}

func showHelp() {
	fmt.Println("CNC Automation - Automated Replay Parsing Tool")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  automation [flags]")
	fmt.Println()
	fmt.Println("Flags:")
	fmt.Println("  -dir-a string")
	fmt.Println("        Directory A (destination for replay files) (required)")
	fmt.Println("  -dir-b string")
	fmt.Println("        Directory B (source for replay files) (required)")
	fmt.Println("  -generals-exe string")
	fmt.Println("        Path to generals.exe (required)")
	fmt.Println("  -process string")
	fmt.Println("        Process name to monitor (default: generals.exe)")
	fmt.Println("  -initial-wait duration")
	fmt.Println("        Wait time after starting generals.exe (default: 10s)")
	fmt.Println("  -clicks-before-start string")
	fmt.Println("        Mouse click coordinates before timecode start (format: x1,y1;x2,y2;...)")
	fmt.Println("  -clicks-after-start string")
	fmt.Println("        Mouse click coordinates after timecode starts (format: x1,y1;x2,y2;...)")
	fmt.Println("  -clicks-after-stop string")
	fmt.Println("        Mouse click coordinates after timecode stops (format: x1,y1;x2,y2;...)")
	fmt.Println("  -timecode-start-timeout duration")
	fmt.Println("        Timeout for waiting for timecode to start (default: 2m)")
	fmt.Println("  -timecode-stop-timeout duration")
	fmt.Println("        Timeout for waiting for timecode to stop (default: 10m)")
	fmt.Println("  -replay-api-url string")
	fmt.Println("        API URL for replay validation (default: https://cncstats.herokuapp.com/replay)")
	fmt.Println("  -use-api")
	fmt.Println("        Use API mode: fetch replays from radarvan API instead of DirectoryB")
	fmt.Println("  -max-replays int")
	fmt.Println("        Maximum number of replays to fetch from API per request (only used with -use-api) (default: 10)")
	fmt.Println("  -help")
	fmt.Println("        Show this help information")
	fmt.Println()
	fmt.Println("Note: The 'F' key is automatically pressed after timecode starts (hardcoded).")
	fmt.Println()
	fmt.Println("Example:")
	fmt.Println("  automation -dir-a C:\\replays\\processed -dir-b C:\\replays\\queue -generals-exe C:\\game\\generals.exe -clicks-before-start \"100,200;300,400\" -clicks-after-start \"500,600\" -clicks-after-stop \"700,800\"")
}
