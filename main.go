package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	zhreader "github.com/bill-rich/cncmon/pkg/memmon"
	"github.com/bill-rich/cncmon/pkg/monitor"
)

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
		pollDelay    = flag.Duration("delay", 50*time.Millisecond, "Delay between memory polls")
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

	// Create monitor config
	config := monitor.Config{
		ProcessName:  *processName,
		APIURL:       *apiURL,
		APIQueueSize: *apiQueueSize,
		PollDelay:    *pollDelay,
		Timeout:      *timeout,
		ManualSeed:   *seed,
		Debug:        *debugMode,
	}

	// Create and start the monitor
	mon := monitor.New(config)
	mon.Start()

	// Wait for interrupt signal
	<-sigChan
	fmt.Println("\nReceived interrupt signal. Shutting down...")
	mon.Stop()
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
		fmt.Printf("Found %d matching pattern(s)!\n", len(results.Results))
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
		fmt.Printf("Pattern not found in file.\n")
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
	fmt.Println("        Delay between memory polls (default: 50ms)")
	fmt.Println("  -timeout duration")
	fmt.Println("        Timeout for file inactivity before returning to waiting mode (default: 2m)")
	fmt.Println("  -api string")
	fmt.Println("        API endpoint URL for sending money data (default: http://cncstats.computersrfun.org)")
	fmt.Println("  -process string")
	fmt.Println("        Process name to monitor (default: generals.exe)")
	fmt.Println("  -seed string")
	fmt.Println("        Manual seed value to use instead of reading from replay file")
	fmt.Println("  -api-queue-size int")
	fmt.Println("        Size of the API request queue buffer (default: 1000)")
	fmt.Println("  -debug")
	fmt.Println("        Enable debug logging for troubleshooting")
	fmt.Println("  -help")
	fmt.Println("        Show this help information")
	fmt.Println()
	fmt.Println("File Search Mode:")
	fmt.Println("  -search-file string")
	fmt.Println("        Search for patterns in a static executable file")
	fmt.Println("  -search-pattern string")
	fmt.Println("        AOB pattern to search for (e.g., 'a1 ?? ?? ?? ?? 8b 40 0c 85 c0 74 78')")
	fmt.Println("  -search-binary string")
	fmt.Println("        Binary pattern to search for")
	fmt.Println("  -search-mode string")
	fmt.Println("        Search mode: 'aob' for AOB patterns, 'binary' for binary patterns (default: aob)")
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
}
