//go:build x86 || amd64 || arm || arm64

package memmon

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
)

type PollResult struct {
	Money                    [8]int32
	MoneyEarned              [8]int32
	UnitsBuilt               [8]int32
	UnitsLost                [8]int32
	PowerTotal               [8]int32
	PowerUsed                [8]int32
	RadarsBuilt              [8]int32
	SearchAndDestroy         [8]int32
	HoldTheLine              [8]int32
	Bombardment              [8]int32
	XP                       [8]int32
	XPLevel                  [8]int32
	GeneralsPointsUsed       [8]int32
	GeneralsPointsTotal      [8]int32
	TechBuildingsCaptured    [8]int32
	FactionBuildingsCaptured [8]int32
	UnitsKilled              [8][8]int32
	BuildingsBuilt           [8]int32
	BuildingsLost            [8]int32
	BuildingsKilled          [8][8]int32
}

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
func (r *Reader) Poll() PollResult {
	return PollResult{
		Money:       [8]int32{1000, 2000, 3000, 4000, 5000, 6000, 7000, 8000},
		MoneyEarned: [8]int32{1000, 2000, 3000, 4000, 5000, 6000, 7000, 8000},
		UnitsBuilt:  [8]int32{1000, 2000, 3000, 4000, 5000, 6000, 7000, 8000},
		UnitsLost:   [8]int32{1000, 2000, 3000, 4000, 5000, 6000, 7000, 8000},
		PowerTotal:  [8]int32{1000, 2000, 3000, 4000, 5000, 6000, 7000, 8000},
		PowerUsed:   [8]int32{1000, 2000, 3000, 4000, 5000, 6000, 7000, 8000},
	}
}

// FileSearchResult contains the result of a file pattern search
type FileSearchResult struct {
	Found     bool   // Whether the pattern was found
	Value     string // The hex value found at the location
	Location  uint64 // The file offset where the pattern was found
	HexOffset string // The location formatted as hex string
}

// FileSearchResults contains multiple search results
type FileSearchResults struct {
	Found   bool               // Whether any patterns were found
	Results []FileSearchResult // List of all matching results
}

// ParseAOBPattern parses an AOB pattern string and returns byte array with wildcards
func ParseAOBPattern(pattern string) ([]byte, []bool, error) {
	log.Printf("AOB Parse: Parsing pattern '%s'", pattern)

	// Remove spaces and convert to lowercase
	pattern = strings.ReplaceAll(strings.ToLower(pattern), " ", "")

	if len(pattern)%2 != 0 {
		return nil, nil, errors.New("pattern length must be even")
	}

	bytes := make([]byte, len(pattern)/2)
	wildcards := make([]bool, len(pattern)/2)
	wildcardCount := 0

	for i := 0; i < len(pattern); i += 2 {
		hexStr := pattern[i : i+2]
		if hexStr == "??" {
			bytes[i/2] = 0
			wildcards[i/2] = true
			wildcardCount++
		} else {
			var b byte
			_, err := fmt.Sscanf(hexStr, "%02x", &b)
			if err != nil {
				return nil, nil, fmt.Errorf("invalid hex byte: %s", hexStr)
			}
			bytes[i/2] = b
			wildcards[i/2] = false
		}
	}

	log.Printf("AOB Parse: Parsed %d bytes with %d wildcards", len(bytes), wildcardCount)
	return bytes, wildcards, nil
}

// ParseBinaryPattern parses a binary pattern string and returns byte array with wildcards
// ? signifies what we are looking for, * can match anything but we don't care about it
func ParseBinaryPattern(pattern string) ([]byte, []bool, error) {
	log.Printf("Binary Parse: Parsing pattern '%s'", pattern)

	// Remove spaces
	pattern = strings.ReplaceAll(pattern, " ", "")

	// Convert binary string to bytes
	if len(pattern)%8 != 0 {
		return nil, nil, errors.New("pattern length must be multiple of 8 bits")
	}

	bytes := make([]byte, len(pattern)/8)
	wildcards := make([]bool, len(pattern)/8)
	wildcardCount := 0

	for i := 0; i < len(pattern); i += 8 {
		bitGroup := pattern[i : i+8]

		// Check if this byte has any ? characters (what we're looking for)
		hasQuestion := strings.Contains(bitGroup, "?")
		// Check if this byte has any * characters (we don't care about these)
		hasAsterisk := strings.Contains(bitGroup, "*")

		if hasQuestion {
			// This byte contains ? characters - we need to match this pattern
			wildcards[i/8] = true
			wildcardCount++

			// Convert the pattern to a byte, treating ? as 0 for now
			// We'll handle the actual matching in the search function
			var b byte
			for j, bit := range bitGroup {
				if bit == '?' {
					// ? means we're looking for this bit, but we don't know what value yet
					// We'll set it to 0 for now and handle matching in searchBinaryPattern
					continue
				} else if bit == '*' {
					// * means we don't care about this bit
					continue
				} else if bit == '1' {
					b |= 1 << (7 - j)
				} else if bit == '0' {
					// bit is already 0
				} else {
					return nil, nil, fmt.Errorf("invalid character in bit pattern: %c", bit)
				}
			}
			bytes[i/8] = b
		} else if hasAsterisk {
			// This byte only has * characters - we don't care about this byte
			// But we still need to treat it as a wildcard for matching purposes
			bytes[i/8] = 0
			wildcards[i/8] = true
			wildcardCount++
		} else {
			// This byte has no wildcards - exact match required
			var b byte
			for j, bit := range bitGroup {
				if bit == '1' {
					b |= 1 << (7 - j)
				} else if bit != '0' {
					return nil, nil, fmt.Errorf("invalid character in bit pattern: %c", bit)
				}
			}
			bytes[i/8] = b
			wildcards[i/8] = false
		}
	}

	log.Printf("Binary Parse: Parsed %d bytes with %d wildcards", len(bytes), wildcardCount)
	return bytes, wildcards, nil
}

// SearchAOBPatternInFile searches for an AOB pattern in a file and returns all results
func SearchAOBPatternInFile(filePath, pattern string) (*FileSearchResults, error) {
	log.Printf("File Search: Searching for AOB pattern '%s' in file '%s'", pattern, filePath)

	// Parse the AOB pattern
	patternBytes, wildcards, err := ParseAOBPattern(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to parse AOB pattern: %w", err)
	}

	// Open the file
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// Get file size
	fileInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to get file info: %w", err)
	}
	fileSize := fileInfo.Size()

	log.Printf("File Search: File size: %d bytes", fileSize)

	// Read file in chunks to search for the pattern
	chunkSize := int64(0x10000) // 64KB chunks
	buffer := make([]byte, chunkSize)
	bytesSearched := int64(0)
	chunksSearched := 0
	results := make([]FileSearchResult, 0)

	for offset := int64(0); offset < fileSize; offset += chunkSize {
		// Adjust chunk size for the last chunk
		remaining := fileSize - offset
		if remaining < chunkSize {
			chunkSize = remaining
			buffer = make([]byte, chunkSize)
		}

		// Read file chunk
		_, err := file.ReadAt(buffer, offset)
		if err != nil && err != io.EOF {
			log.Printf("File Search: Failed to read file at offset 0x%X (chunk %d)", offset, chunksSearched)
			continue
		}

		chunksSearched++
		bytesSearched += int64(len(buffer))

		// Search for pattern in this chunk
		for i := 0; i <= len(buffer)-len(patternBytes); i++ {
			match := true
			for j := 0; j < len(patternBytes); j++ {
				if !wildcards[j] && buffer[i+j] != patternBytes[j] {
					match = false
					break
				}
			}

			if match {
				foundOffset := offset + int64(i)
				// The pattern is "a1 ?? ?? ?? ?? 8b 40 0c 85 c0 74 78"
				// We want the address of the wildcard bytes (?? ?? ?? ??), not the "a1"
				// The wildcards start at offset 1 from the beginning of the pattern
				wildcardOffset := foundOffset + 1

				// Read the 4-byte value at the wildcard location
				valueBytes := make([]byte, 4)
				_, err := file.ReadAt(valueBytes, wildcardOffset)
				if err != nil {
					log.Printf("File Search: Failed to read value at offset 0x%X", wildcardOffset)
					continue
				}

				// Convert to hex string
				valueHex := fmt.Sprintf("%02X %02X %02X %02X", valueBytes[0], valueBytes[1], valueBytes[2], valueBytes[3])

				log.Printf("File Search: Pattern found at offset 0x%X, wildcard section at 0x%X, value: %s",
					foundOffset, wildcardOffset, valueHex)

				// Add to results instead of returning immediately
				results = append(results, FileSearchResult{
					Found:     true,
					Value:     valueHex,
					Location:  uint64(wildcardOffset),
					HexOffset: fmt.Sprintf("0x%X", wildcardOffset),
				})
			}
		}

		// Log progress every 100 chunks
		if chunksSearched%100 == 0 {
			log.Printf("File Search: Progress - searched %d chunks (%d bytes)", chunksSearched, bytesSearched)
		}
	}

	log.Printf("File Search: Found %d matches after searching %d chunks (%d bytes)", len(results), chunksSearched, bytesSearched)
	return &FileSearchResults{
		Found:   len(results) > 0,
		Results: results,
	}, nil
}

// SearchBinaryPatternInFile searches for a binary pattern in a file and returns all results
func SearchBinaryPatternInFile(filePath, pattern string) (*FileSearchResults, error) {
	log.Printf("File Search: Searching for binary pattern '%s' in file '%s'", pattern, filePath)

	// Parse the binary pattern
	patternBytes, wildcards, err := ParseBinaryPattern(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to parse binary pattern: %w", err)
	}

	// Open the file
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// Get file size
	fileInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to get file info: %w", err)
	}
	fileSize := fileInfo.Size()

	log.Printf("File Search: File size: %d bytes", fileSize)

	// Read file in chunks to search for the pattern
	chunkSize := int64(0x10000) // 64KB chunks
	buffer := make([]byte, chunkSize)
	bytesSearched := int64(0)
	chunksSearched := 0
	results := make([]FileSearchResult, 0)

	for offset := int64(0); offset < fileSize; offset += chunkSize {
		// Adjust chunk size for the last chunk
		remaining := fileSize - offset
		if remaining < chunkSize {
			chunkSize = remaining
			buffer = make([]byte, chunkSize)
		}

		// Read file chunk
		_, err := file.ReadAt(buffer, offset)
		if err != nil && err != io.EOF {
			log.Printf("File Search: Failed to read file at offset 0x%X (chunk %d)", offset, chunksSearched)
			continue
		}

		chunksSearched++
		bytesSearched += int64(len(buffer))

		// Search for pattern in this chunk
		for i := 0; i <= len(buffer)-len(patternBytes); i++ {
			match := true
			for j := 0; j < len(patternBytes); j++ {
				if wildcards[j] {
					// This byte has wildcards - we need to check if the pattern matches
					if !matchBinaryByteInFile(buffer[i+j], j, pattern) {
						match = false
						break
					}
				} else {
					// Exact match required
					if buffer[i+j] != patternBytes[j] {
						match = false
						break
					}
				}
			}

			if match {
				foundOffset := offset + int64(i)
				// Find where the ? characters are in the pattern
				wildcardOffset := foundOffset + int64(findWildcardOffsetInFile(pattern))

				// Read the 4-byte value at the wildcard location
				valueBytes := make([]byte, 4)
				_, err := file.ReadAt(valueBytes, wildcardOffset)
				if err != nil {
					log.Printf("File Search: Failed to read value at offset 0x%X", wildcardOffset)
					continue
				}

				// Convert to hex string
				valueHex := fmt.Sprintf("%02X %02X %02X %02X", valueBytes[0], valueBytes[1], valueBytes[2], valueBytes[3])

				log.Printf("File Search: Binary pattern found at offset 0x%X, wildcard section at 0x%X, value: %s",
					foundOffset, wildcardOffset, valueHex)

				// Add to results instead of returning immediately
				results = append(results, FileSearchResult{
					Found:     true,
					Value:     valueHex,
					Location:  uint64(wildcardOffset),
					HexOffset: fmt.Sprintf("0x%X", wildcardOffset),
				})
			}
		}

		// Log progress every 100 chunks
		if chunksSearched%100 == 0 {
			log.Printf("File Search: Progress - searched %d chunks (%d bytes)", chunksSearched, bytesSearched)
		}
	}

	log.Printf("File Search: Found %d binary matches after searching %d chunks (%d bytes)", len(results), chunksSearched, bytesSearched)
	return &FileSearchResults{
		Found:   len(results) > 0,
		Results: results,
	}, nil
}

// matchBinaryByteInFile checks if a byte matches the binary pattern at a specific position (file version)
func matchBinaryByteInFile(actualByte byte, byteIndex int, pattern string) bool {
	// Remove spaces from pattern
	pattern = strings.ReplaceAll(pattern, " ", "")

	// Calculate the bit position in the pattern
	bitStart := byteIndex * 8
	if bitStart+8 > len(pattern) {
		return false
	}

	bitGroup := pattern[bitStart : bitStart+8]

	// Check each bit
	for j, bit := range bitGroup {
		if bit == '?' {
			// ? means we're looking for this bit - any value is acceptable
			continue
		} else if bit == '*' {
			// * means we don't care about this bit - any value is acceptable
			continue
		} else if bit == '1' {
			// Must be 1
			if (actualByte & (1 << (7 - j))) == 0 {
				return false
			}
		} else if bit == '0' {
			// Must be 0
			if (actualByte & (1 << (7 - j))) != 0 {
				return false
			}
		} else {
			return false
		}
	}

	return true
}

// findWildcardOffsetInFile finds the byte offset where the ? characters start in the pattern (file version)
func findWildcardOffsetInFile(pattern string) int {
	// Remove spaces from pattern
	pattern = strings.ReplaceAll(pattern, " ", "")

	// Find the first occurrence of ? in the pattern
	for i, char := range pattern {
		if char == '?' {
			// Convert bit position to byte offset
			return i / 8
		}
	}

	// If no ? found, return 0 (shouldn't happen for valid patterns)
	return 0
}

// GetTimecode reads the current timecode from memory using direct offset (mock implementation)
func (r *Reader) GetTimecode() (uint32, error) {
	// Mock implementation for development on macOS
	fmt.Printf("Warning: Timecode reading not available on macOS\n")
	fmt.Printf("Would read from offset: generals.exe+63ABE0\n")
	return 0, fmt.Errorf("timecode reading not available on macOS")
}

// GetSeed returns the seed value (mock implementation)
func (r *Reader) GetSeed() string {
	// Mock implementation for development on macOS
	return "mock-seed-macos"
}
