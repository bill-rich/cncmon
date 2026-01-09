package main

import (
	"flag"
	"fmt"
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
		clicksBeforeStart    = flag.String("clicks-before-start", "100,100;2036,512;2036,412;778,388;2073,415", "Mouse click coordinates before timecode start (format: x1,y1;x2,y2;...)")
		clicksAfterStart     = flag.String("clicks-after-start", "", "Mouse click coordinates after timecode starts (format: x1,y1;x2,y2;...)")
		clicksAfterStop      = flag.String("clicks-after-stop", "2181,1364;2096,996;2096,803", "Mouse click coordinates after timecode stops (format: x1,y1;x2,y2;...)")
		timecodeStartTimeout = flag.Duration("timecode-start-timeout", 1*time.Minute, "Timeout for waiting for timecode to start")
		timecodeStopTimeout  = flag.Duration("timecode-stop-timeout", 10*time.Minute, "Timeout for waiting for timecode to stop")
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

	// Set up signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	fileCount := 0
	for {
		// Check for interrupt
		select {
		case sig := <-sigChan:
			fmt.Printf("\nReceived signal %v. Shutting down gracefully...\n", sig)
			return
		default:
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

		// Step 3: Start generals.exe
		fmt.Println("Step 3: Starting generals.exe...")
		cmd, err := automation.StartGenerals(*generalsExe)
		if err != nil {
			fmt.Printf("Error starting generals.exe: %v\n", err)
			continue
		}

		// Wait a bit for window to appear, then set target window for PostMessage
		time.Sleep(2 * time.Second)
		fmt.Println("Step 3.5: Finding game window...")
		if err := automation.SetTargetWindow(uint32(cmd.Process.Pid)); err != nil {
			fmt.Printf("Warning: Failed to find game window: %v (will try SendInput fallback)\n", err)
		} else {
			fmt.Println("Game window found - using PostMessage for input (works better with DirectInput games)")
		}

		// Wait for initial wait time
		fmt.Printf("Step 4: Waiting %v...\n", *initialWait)
		time.Sleep(*initialWait)

		// Step 5: Press Escape
		fmt.Println("Step 5: Pressing Escape key...")
		if err := automation.PressEscape(); err != nil {
			fmt.Printf("Error pressing Escape: %v\n", err)
			cmd.Process.Kill()
			continue
		}
		time.Sleep(10 * time.Second)

		// Step 6: Simulate mouse clicks before timecode start
		if len(clicksBeforeStartCoords) > 0 {
			fmt.Printf("Step 6: Simulating %d mouse clicks before timecode start...\n", len(clicksBeforeStartCoords))
			if err := automation.ClickAtMultiple(clicksBeforeStartCoords); err != nil {
				fmt.Printf("Error clicking: %v\n", err)
				cmd.Process.Kill()
				continue
			}
		}

		// Step 7: Wait for generals.exe process to be available and initialize memory reader
		fmt.Println("Step 7: Waiting for generals.exe process to be available...")
		var memReader *zhreader.Reader
		for {
			select {
			case sig := <-sigChan:
				fmt.Printf("\nReceived signal %v. Shutting down gracefully...\n", sig)
				cmd.Process.Kill()
				return
			default:
			}

			memReader, err = zhreader.Init(*processName)
			if err == nil {
				break
			}
			time.Sleep(1 * time.Second)
		}
		defer memReader.Close()

		// Step 8: Wait for timecode to start increasing
		fmt.Println("Step 8: Waiting for timecode to start increasing...")
		getTimecode := func() (uint32, error) {
			return memReader.GetTimecode()
		}

		if err := automation.WaitForTimecodeStart(getTimecode, *timecodeStartTimeout); err != nil {
			fmt.Printf("Error waiting for timecode to start: %v\n", err)
			cmd.Process.Kill()
			continue
		}

		// Step 9: Press 'F' key after timecode starts (hardcoded)
		fmt.Println("Step 9: Pressing 'F' key...")
		time.Sleep(500 * time.Millisecond)
		if err := automation.PressKey(VK_F); err != nil {
			fmt.Printf("Error pressing 'F' key: %v\n", err)
			cmd.Process.Kill()
			continue
		}

		// Step 10: Wait for timecode to stop increasing
		fmt.Println("Step 11: Waiting for timecode to stop increasing...")
		if err := automation.WaitForTimecodeStop(getTimecode, *timecodeStopTimeout); err != nil {
			fmt.Printf("Error waiting for timecode to stop: %v\n", err)
			cmd.Process.Kill()
			continue
		}

		// Step 11: Simulate mouse clicks after timecode stops
		time.Sleep(1 * time.Second)
		if len(clicksAfterStopCoords) > 0 {
			fmt.Printf("Step 12: Simulating %d mouse clicks after timecode stops...\n", len(clicksAfterStopCoords))
			if err := automation.ClickAtMultiple(clicksAfterStopCoords); err != nil {
				fmt.Printf("Error clicking: %v\n", err)
			}
		}

		// Clean up: kill generals.exe process
		fmt.Println("Cleaning up: Terminating generals.exe...")
		if err := cmd.Process.Kill(); err != nil {
			fmt.Printf("Warning: Error killing process: %v\n", err)
		}
		cmd.Wait()

		fmt.Printf("Completed processing file %d\n", fileCount)
		time.Sleep(2 * time.Second) // Brief pause between files
	}

	fmt.Printf("\n=== Automation Complete ===\n")
	fmt.Printf("Processed %d file(s)\n", fileCount)
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
	fmt.Println("  -help")
	fmt.Println("        Show this help information")
	fmt.Println()
	fmt.Println("Note: The 'F' key is automatically pressed after timecode starts (hardcoded).")
	fmt.Println()
	fmt.Println("Example:")
	fmt.Println("  automation -dir-a C:\\replays\\processed -dir-b C:\\replays\\queue -generals-exe C:\\game\\generals.exe -clicks-before-start \"100,200;300,400\" -clicks-after-start \"500,600\" -clicks-after-stop \"700,800\"")
}
