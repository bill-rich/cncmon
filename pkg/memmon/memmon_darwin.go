//go:build darwin

package memmon

import (
	"fmt"
)

type Reader struct {
	// Mock implementation for macOS development
}

// Init creates a mock reader for development on macOS
func Init(processName string) (*Reader, error) {
	fmt.Printf("Warning: Running in development mode on macOS\n")
	fmt.Printf("Memory monitoring is only available on Windows\n")
	fmt.Printf("Process name: %s\n", processName)
	return &Reader{}, nil
}

func (r *Reader) Close() {
	// No-op for mock implementation
}

// Poll returns mock data for development
func (r *Reader) Poll() [8]int32 {
	// Return mock data for development
	return [8]int32{1000, 2000, 3000, 4000, 5000, 6000, 7000, 8000}
}
