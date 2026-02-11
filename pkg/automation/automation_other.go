//go:build !windows

package automation

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// Stub implementations for non-Windows platforms

func PressKey(vkCode uint16) error {
	return fmt.Errorf("keyboard simulation not supported on this platform")
}

func PressEscape() error {
	return fmt.Errorf("keyboard simulation not supported on this platform")
}

func ClickAt(x, y int32) error {
	return fmt.Errorf("mouse simulation not supported on this platform")
}

func ClickAtMultiple(ctx context.Context, coords []struct{ X, Y int32 }) error {
	return fmt.Errorf("mouse simulation not supported on this platform")
}

func DeleteAllRepFiles(dir string) error {
	return fmt.Errorf("file operations not supported on this platform")
}

func MoveOneRepFile(sourceDir, destDir string) (string, error) {
	return "", fmt.Errorf("file operations not supported on this platform")
}

func StartGenerals(exePath string) (*exec.Cmd, error) {
	return nil, fmt.Errorf("process execution not supported on this platform")
}

func WaitForTimecodeStart(ctx context.Context, getTimecode func() (uint32, error), timeout time.Duration) error {
	return fmt.Errorf("timecode monitoring not supported on this platform")
}

func WaitForTimecodeStop(ctx context.Context, getTimecode func() (uint32, error), timeout time.Duration) (uint32, error) {
	return 0, fmt.Errorf("timecode monitoring not supported on this platform")
}

func CountRepFiles(dir string) (int, error) {
	return 0, fmt.Errorf("file operations not supported on this platform")
}

func SetTargetWindow(pid uint32) error {
	return fmt.Errorf("window management not supported on this platform")
}

// ValidateReplayFile uploads the replay file to the API and checks if BuildDate matches
// Returns true if BuildDate matches "Mar 10 2005 13:47:03", false otherwise
func ValidateReplayFile(filePath string, apiURL string) (bool, string, error) {
	return false, "", fmt.Errorf("replay validation not supported on this platform")
}

// MoveFileBackWithOldExtension moves a file from DirectoryA back to DirectoryB with .old extension
func MoveFileBackWithOldExtension(filePath, directoryB string) error {
	return fmt.Errorf("file operations not supported on this platform")
}
