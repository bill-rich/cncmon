package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bill-rich/cncmon/pkg/automation"
	zhreader "github.com/bill-rich/cncmon/pkg/memmon"
)

const VK_F = 0x46 // Virtual key code for 'F' key

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
		clicksAfterStop      = flag.String("clicks-after-stop", "2181,1364;2096,996;2096,803", "Mouse click coordinates after timecode stops (format: x1,y1;x2,y2;...)")
		timecodeStartTimeout = flag.Duration("timecode-start-timeout", 1*time.Minute, "Timeout for waiting for timecode to start")
		timecodeStopTimeout  = flag.Duration("timecode-stop-timeout", 10*time.Minute, "Timeout for waiting for timecode to stop")
		replayAPIURL         = flag.String("replay-api-url", "https://cncstats.herokuapp.com/replay", "API URL for replay validation")
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
	if *directoryB == "" {
		fmt.Println("Error: -dir-b is required")
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
	fmt.Printf("Directory B (source): %s\n", *directoryB)
	fmt.Printf("Generals.exe: %s\n", *generalsExe)
	fmt.Printf("Initial wait: %v\n", *initialWait)
	fmt.Printf("Clicks before start: %d\n", len(clicksBeforeStartCoords))
	fmt.Printf("Key after start: F (0x%02X) [hardcoded]\n", VK_F)
	fmt.Printf("Clicks after start: %d\n", len(clicksAfterStartCoords))
	fmt.Printf("Clicks after stop: %d\n", len(clicksAfterStopCoords))
	fmt.Println()

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
	lastTimecode := uint32(0)
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

		// Step 1: Delete all .rep files from DirectoryA
		fmt.Println("Step 1: Deleting all .rep files from DirectoryA...")
		if err := automation.DeleteAllRepFiles(*directoryA); err != nil {
			fmt.Printf("Error deleting files: %v\n", err)
			continue
		}

		// Step 2: Move one file from DirectoryB to DirectoryA
		fmt.Println("Step 2: Moving one file from DirectoryB to DirectoryA...")
		movedFile, err := automation.MoveOneRepFile(*directoryB, *directoryA)
		if err != nil {
			fmt.Printf("Error moving file: %v\n", err)
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
			continue
		}
		if !isValid {
			fmt.Println("BuildDate does not match expected value. Expected: Mar 10 2005 13:47:03, Actual BuildDate: %s", actualBuildDate)
			/*
				if err := automation.MoveFileBackWithOldExtension(movedFile, *directoryB); err != nil {
					fmt.Printf("Error moving file back: %v\n", err)
				}
				continue
			*/
		}
		fmt.Println("Replay file BuildDate validated successfully.")

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
		lastTimecode, err = automation.WaitForTimecodeStop(ctx, getTimecode, *timecodeStopTimeout)
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

		waitTime := 35
		fmt.Printf("Last timecode: %d\n", lastTimecode)
		if lastTimecode != 0 {
			fmt.Printf("Last timecode is not 0, adding 2 seconds to wait time\n")
			waitTime = int(lastTimecode/1000) + 2
		}

		fmt.Printf("Waiting %d seconds before triggering reparse...\n", waitTime)
		if err := sleepWithContext(ctx, time.Duration(waitTime)*time.Second); err != nil {
			return
		}

		// Trigger reparse on the radarvan api (curl -X 'POST' \ 'https://www.radarvan.com/api/reprase/123' \ -H 'accept: application/json' \ -d '')
		// Use http request to trigger reparse
		fmt.Printf("Triggering reparse on the radarvan api for seed: %s\n", gameSeed)
		req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("https://www.radarvan.com/api/reprase/%s", gameSeed), nil)
		if err != nil {
			fmt.Printf("Error creating request: %v\n", err)
			return
		}
		req.Header.Set("accept", "application/json")
		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			fmt.Printf("Error sending request: %v\n", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			fmt.Printf("Error triggering reparse: %s\n", resp.Status)
			return
		}
		fmt.Printf("Reparse triggered successfully\n")
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
	fmt.Println("  -help")
	fmt.Println("        Show this help information")
	fmt.Println()
	fmt.Println("Note: The 'F' key is automatically pressed after timecode starts (hardcoded).")
	fmt.Println()
	fmt.Println("Example:")
	fmt.Println("  automation -dir-a C:\\replays\\processed -dir-b C:\\replays\\queue -generals-exe C:\\game\\generals.exe -clicks-before-start \"100,200;300,400\" -clicks-after-start \"500,600\" -clicks-after-stop \"700,800\"")
}
