package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	zhreader "github.com/bill-rich/cncmon/pkg/memmon"
	"github.com/bill-rich/cncstats/pkg/iniparse"
	"github.com/bill-rich/cncstats/pkg/zhreplay"
)

func main() {
	// Parse command line arguments
	var (
		replayFile = flag.String("file", "", "Replay file to monitor (required)")
		iniData    = flag.String("ini", "./inizh/Data/INI", "Path to CNC INI data directory")
		pollDelay  = flag.Duration("delay", 100*time.Millisecond, "Delay between memory polls when events are received")
		timeout    = flag.Duration("timeout", 2*time.Minute, "Timeout for file inactivity before returning to waiting mode")
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

	// Initialize memory reader
	memReader, err := zhreader.Init()
	if err != nil {
		fmt.Printf("Failed to initialize memory reader: %v\n", err)
		return
	}
	defer memReader.Close()

	// Initialize cncstats stores
	objectStore, err := iniparse.NewObjectStore(*iniData)
	if err != nil {
		log.Fatalf("Could not load object store: %v", err)
	}

	powerStore, err := iniparse.NewPowerStore(*iniData)
	if err != nil {
		log.Fatalf("Could not load power store: %v", err)
	}

	upgradeStore, err := iniparse.NewUpgradeStore(*iniData)
	if err != nil {
		log.Fatalf("Could not load upgrade store: %v", err)
	}

	fmt.Printf("Starting continuous monitoring of replay file: %s\n", *replayFile)
	fmt.Printf("Timeout: %v, Poll delay: %v\n", *timeout, *pollDelay)
	fmt.Println("Waiting for replay file to be written to...")

	// Continuous monitoring loop
	for {
		// Wait for file to exist and start being written to
		if !waitForFileActivity(*replayFile) {
			fmt.Println("File monitoring interrupted. Exiting.")
			return
		}

		fmt.Printf("Replay file activity detected. Starting to monitor events...\n")

		// Process the replay file until completion or timeout
		eventCount := processReplayFile(*replayFile, memReader, objectStore, powerStore, upgradeStore, *pollDelay, *timeout)

		fmt.Printf("Replay processing completed. Processed %d events.\n", eventCount)
		fmt.Println("Returning to waiting mode for next replay...")
		fmt.Println()
	}
}

// waitForFileActivity waits for the replay file to exist and start being written to
func waitForFileActivity(replayFile string) bool {
	var lastSize int64 = -1
	noChangeCount := 0

	for {
		// Check if file exists
		info, err := os.Stat(replayFile)
		if err != nil {
			// File doesn't exist yet, wait a bit
			time.Sleep(100 * time.Millisecond)
			continue
		}

		currentSize := info.Size()

		// If file size changed, it's being written to
		if currentSize != lastSize {
			lastSize = currentSize
			noChangeCount = 0
			// Wait a bit more to ensure it's actively being written
			time.Sleep(500 * time.Millisecond)
			continue
		}

		// File size hasn't changed
		noChangeCount++

		// If file has been stable for a while, it might be ready to read
		if noChangeCount > 10 { // 1 second of stability
			return true
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// processReplayFile processes a replay file until completion or timeout
func processReplayFile(replayFile string, memReader *zhreader.Reader, objectStore *iniparse.ObjectStore, powerStore *iniparse.PowerStore, upgradeStore *iniparse.UpgradeStore, pollDelay time.Duration, timeout time.Duration) int {
	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Configure streaming options
	options := &zhreplay.StreamReplayOptions{
		PollInterval: 100 * time.Millisecond,
		MaxWaitTime:  30 * time.Second,
		BufferSize:   100,
	}

	// Start streaming replay events
	bodyChan, streamingReplay, err := zhreplay.StreamReplay(ctx, replayFile, objectStore, powerStore, upgradeStore, options)
	if err != nil {
		fmt.Printf("Failed to start streaming: %v\n", err)
		return 0
	}

	// Print header information
	fmt.Printf("Replay Header:\n")
	fmt.Printf("  Map: %s\n", streamingReplay.Header.Metadata.MapFile)
	fmt.Printf("  Players: %d\n", len(streamingReplay.Header.Metadata.Players))
	for i, player := range streamingReplay.Header.Metadata.Players {
		fmt.Printf("    Player %d: %s (Team %s)\n", i+1, player.Name, player.Team)
	}
	fmt.Println()

	// Process body events and poll memory
	eventCount := 0
	lastEventTime := time.Now()

	for {
		select {
		case chunk, ok := <-bodyChan:
			if !ok {
				fmt.Printf("\nStreaming completed. Processed %d events.\n", eventCount)
				return eventCount
			}

			eventCount++
			lastEventTime = time.Now()

			// Print event information
			fmt.Printf("Event %d: Time=%d, Order=%s, PlayerID=%d",
				eventCount, chunk.TimeCode, chunk.OrderName, chunk.PlayerID)

			// Add player name if available
			if chunk.PlayerName != "" {
				fmt.Printf(", Player=%s", chunk.PlayerName)
			}

			// Add details for specific order types
			if chunk.Details != nil {
				switch chunk.OrderCode {
				case 1047: // CreateUnit
					fmt.Printf(", Unit=%s (Cost=%d)", chunk.Details.GetName(), chunk.Details.GetCost())
				case 1049: // BuildObject
					fmt.Printf(", Building=%s (Cost=%d)", chunk.Details.GetName(), chunk.Details.GetCost())
				case 1045: // BuildUpgrade
					fmt.Printf(", Upgrade=%s (Cost=%d)", chunk.Details.GetName(), chunk.Details.GetCost())
				case 1040, 1041, 1042: // SpecialPower variants
					fmt.Printf(", Power=%s", chunk.Details.GetName())
				}
			}

			fmt.Println()

			// Poll memory values when a new event is received
			fmt.Println("  Polling memory values...")
			vals := memReader.Poll()
			j, _ := json.Marshal(struct {
				P [8]int32 `json:"p"`
			}{vals})
			fmt.Printf("  Memory values: %s\n", string(j))

			// Add a small delay to avoid overwhelming the system
			time.Sleep(pollDelay)

			// Check for EndReplay command
			if chunk.OrderCode == 27 {
				fmt.Println("EndReplay command detected - streaming will stop.")
				return eventCount
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

func showHelp() {
	fmt.Println("CNC Monitor - Command and Conquer Replay Monitor with Memory Polling")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  cncmon [flags]")
	fmt.Println()
	fmt.Println("Flags:")
	fmt.Println("  -file string")
	fmt.Println("        Replay file to monitor (required)")
	fmt.Println("  -ini string")
	fmt.Println("        Path to CNC INI data directory (default: ./inizh/Data/INI)")
	fmt.Println("  -delay duration")
	fmt.Println("        Delay between memory polls when events are received (default: 100ms)")
	fmt.Println("  -timeout duration")
	fmt.Println("        Timeout for file inactivity before returning to waiting mode (default: 2m)")
	fmt.Println("  -help")
	fmt.Println("        Show this help information")
	fmt.Println()
	fmt.Println("This tool continuously monitors a Command and Conquer replay file and polls memory values")
	fmt.Println("from the running generals.exe process whenever new events are detected in the replay.")
	fmt.Println("It waits for the file to be written to, processes it until completion or timeout,")
	fmt.Println("then returns to waiting mode for the next replay.")
}
