//go:build !windows

package automation

import (
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

func ClickAtMultiple(coords []struct{ X, Y int32 }) error {
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

func WaitForTimecodeStart(getTimecode func() (uint32, error), timeout time.Duration) error {
	return fmt.Errorf("timecode monitoring not supported on this platform")
}

func WaitForTimecodeStop(getTimecode func() (uint32, error), timeout time.Duration) error {
	return fmt.Errorf("timecode monitoring not supported on this platform")
}

func CountRepFiles(dir string) (int, error) {
	return 0, fmt.Errorf("file operations not supported on this platform")
}

func SetTargetWindow(pid uint32) error {
	return fmt.Errorf("window management not supported on this platform")
}
