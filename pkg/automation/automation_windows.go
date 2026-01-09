//go:build windows

package automation

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                       = windows.NewLazySystemDLL("user32.dll")
	procSendInput                = user32.NewProc("SendInput")
	procPostMessage              = user32.NewProc("PostMessageW")
	procSendMessage              = user32.NewProc("SendMessageW")
	procSetCursorPos             = user32.NewProc("SetCursorPos")
	procGetCursorPos             = user32.NewProc("GetCursorPos")
	procFindWindow               = user32.NewProc("FindWindowW")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procEnumChildWindows         = user32.NewProc("EnumChildWindows")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procMapVirtualKey            = user32.NewProc("MapVirtualKeyW")
	procVkKeyScan                = user32.NewProc("VkKeyScanW")
	kernel32                     = windows.NewLazySystemDLL("kernel32.dll")
	procGetAsyncKeyState         = user32.NewProc("GetAsyncKeyState")
	procGetLastError             = kernel32.NewProc("GetLastError")
)

var targetWindowHandle windows.Handle
var targetChildWindows []windows.Handle

const (
	INPUT_KEYBOARD = 1
	INPUT_MOUSE    = 0

	KEYEVENTF_KEYUP    = 0x0002
	KEYEVENTF_SCANCODE = 0x0008

	MOUSEEVENTF_LEFTDOWN   = 0x0002
	MOUSEEVENTF_LEFTUP     = 0x0004
	MOUSEEVENTF_RIGHTDOWN  = 0x0008
	MOUSEEVENTF_RIGHTUP    = 0x0010
	MOUSEEVENTF_MIDDLEDOWN = 0x0020
	MOUSEEVENTF_MIDDLEUP   = 0x0040
	MOUSEEVENTF_ABSOLUTE   = 0x8000
	MOUSEEVENTF_MOVE       = 0x0001

	VK_ESCAPE = 0x1B
)

type KEYBDINPUT struct {
	WVk         uint16
	WScan       uint16
	DwFlags     uint32
	Time        uint32
	DwExtraInfo uintptr
}

type MOUSEINPUT struct {
	Dx          int32
	Dy          int32
	MouseData   uint32
	DwFlags     uint32
	Time        uint32
	DwExtraInfo uintptr
}

type HARDWAREINPUT struct {
	UMsg    uint32
	WParamL uint16
	WParamH uint16
}

// INPUT structure - Windows uses a union for the input data
// On 64-bit Windows, INPUT is 40 bytes: Type (4) + padding (4) + union (24) + padding (8)
// We'll use KEYBDINPUT as the union member since it's the largest (24 bytes)
type INPUT struct {
	Type uint32
	_    [4]byte    // padding to align union to 8-byte boundary
	Ki   KEYBDINPUT // union member (24 bytes) - largest member
	_    [8]byte    // padding to make total 40 bytes
}

// For mouse input, we need to overlay MOUSEINPUT in the same union space
// We'll use unsafe to cast when needed

type POINT struct {
	X, Y int32
}

const (
	WM_KEYDOWN     = 0x0100
	WM_KEYUP       = 0x0101
	WM_CHAR        = 0x0102
	WM_LBUTTONDOWN = 0x0201
	WM_LBUTTONUP   = 0x0202
	WM_MOUSEMOVE   = 0x0200
)

// FindWindowByProcessID finds the main window for a given process ID
// Returns all windows belonging to the process, not just the first one
func FindWindowByProcessID(pid uint32) ([]windows.Handle, error) {
	var foundWindows []windows.Handle

	// Callback function for EnumWindows
	enumProc := syscall.NewCallback(func(h uintptr, l uintptr) uintptr {
		var processID uint32
		procGetWindowThreadProcessId.Call(h, uintptr(unsafe.Pointer(&processID)))
		if processID == pid {
			// Check if window is visible and has a title (likely the main window)
			// But we'll collect all windows
			foundWindows = append(foundWindows, windows.Handle(h))
		}
		return 1 // Continue enumeration to find all windows
	})

	// Try multiple times as window may not be ready immediately
	for i := 0; i < 10; i++ {
		procEnumWindows.Call(enumProc, 0)
		if len(foundWindows) > 0 {
			return foundWindows, nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	return nil, fmt.Errorf("no windows found for process ID %d", pid)
}

// MapVirtualKey maps a virtual key code to a scan code
func MapVirtualKey(uCode, uMapType uint32) uint32 {
	ret, _, _ := procMapVirtualKey.Call(uintptr(uCode), uintptr(uMapType))
	return uint32(ret)
}

// FindChildWindows finds all child windows of a parent window
func FindChildWindows(parentHwnd windows.Handle) []windows.Handle {
	var childWindows []windows.Handle

	// Callback function for EnumChildWindows
	enumProc := syscall.NewCallback(func(h uintptr, l uintptr) uintptr {
		childWindows = append(childWindows, windows.Handle(h))
		return 1 // Continue enumeration
	})

	procEnumChildWindows.Call(uintptr(parentHwnd), enumProc, 0)
	return childWindows
}

// SetTargetWindow sets the target window for PostMessage-based input
func SetTargetWindow(pid uint32) error {
	allWindows, err := FindWindowByProcessID(pid)
	if err != nil {
		return err
	}

	fmt.Printf("SetTargetWindow: Found %d windows for process %d\n", len(allWindows), pid)

	// Use the first window as the main window (usually the main application window)
	if len(allWindows) > 0 {
		targetWindowHandle = allWindows[0]
		fmt.Printf("SetTargetWindow: Main window handle: 0x%X\n", targetWindowHandle)

		// Find child windows of the main window
		targetChildWindows = FindChildWindows(targetWindowHandle)
		fmt.Printf("SetTargetWindow: Found %d child windows\n", len(targetChildWindows))

		// Also add all other top-level windows as potential targets
		// Games often have a separate render window that's a sibling, not a child
		if len(allWindows) > 1 {
			fmt.Printf("SetTargetWindow: Also found %d additional top-level windows\n", len(allWindows)-1)
			// Add other windows to child windows list for testing
			targetChildWindows = append(targetChildWindows, allWindows[1:]...)
		}
	}

	return nil
}

// sendKeyToWindow sends a key message to a specific window
func sendKeyToWindow(hwnd windows.Handle, vkCode uint16, isDown bool) bool {
	scanCode := MapVirtualKey(uint32(vkCode), 0)

	var message uint32
	var lParam uintptr

	if isDown {
		message = WM_KEYDOWN
		lParam = uintptr((scanCode << 16) | 0x00000001)
	} else {
		message = WM_KEYUP
		lParam = uintptr((scanCode << 16) | 0xC0000001)
	}

	// Try PostMessage first (asynchronous, less likely to block)
	ret, _, _ := procPostMessage.Call(uintptr(hwnd), uintptr(message), uintptr(vkCode), lParam)
	if ret != 0 {
		fmt.Printf("  PostMessage sent to window 0x%X: %s (0x%02X)\n", hwnd, map[bool]string{true: "KEYDOWN", false: "KEYUP"}[isDown], vkCode)
		return true
	}

	// Fallback to SendMessage
	ret, _, _ = procSendMessage.Call(uintptr(hwnd), uintptr(message), uintptr(vkCode), lParam)
	if ret != 0 {
		fmt.Printf("  SendMessage sent to window 0x%X: %s (0x%02X)\n", hwnd, map[bool]string{true: "KEYDOWN", false: "KEYUP"}[isDown], vkCode)
		return true
	}

	errCode, _, _ := procGetLastError.Call()
	fmt.Printf("  Failed to send to window 0x%X: error code %d\n", hwnd, errCode)
	return false
}

// PressKey simulates pressing a key
// Uses PostMessage/SendMessage for games that use DirectInput (like generals.exe)
// Tries sending to main window and all child windows
func PressKey(vkCode uint16) error {
	fmt.Printf("PressKey: Attempting to send key 0x%02X\n", vkCode)

	// If we have a target window, use PostMessage (works better with DirectInput games)
	if targetWindowHandle != 0 {
		fmt.Printf("PressKey: Target window handle: 0x%X, child windows: %d\n", targetWindowHandle, len(targetChildWindows))

		// Try sending to main window first
		fmt.Printf("PressKey: Trying main window...\n")
		mainSuccess := false
		if sendKeyToWindow(targetWindowHandle, vkCode, true) {
			time.Sleep(20 * time.Millisecond)
			if sendKeyToWindow(targetWindowHandle, vkCode, false) {
				mainSuccess = true
				fmt.Printf("PressKey: Main window accepted key\n")
			}
		}

		// If main window didn't work, try all child windows
		// Games often have a render window that's a child window
		childSuccess := false
		if !mainSuccess && len(targetChildWindows) > 0 {
			fmt.Printf("PressKey: Trying %d child windows...\n", len(targetChildWindows))
			for i, childHwnd := range targetChildWindows {
				fmt.Printf("PressKey: Trying child window %d (0x%X)...\n", i, childHwnd)
				if sendKeyToWindow(childHwnd, vkCode, true) {
					time.Sleep(20 * time.Millisecond)
					if sendKeyToWindow(childHwnd, vkCode, false) {
						childSuccess = true
						fmt.Printf("PressKey: Child window %d accepted key\n", i)
						break
					}
				}
			}
		}

		// Also try sending WM_CHAR for character keys (F key)
		if vkCode >= 0x41 && vkCode <= 0x5A { // A-Z
			charCode := vkCode + 32 // Convert to lowercase ASCII
			scanCode := MapVirtualKey(uint32(vkCode), 0)
			lParam := uintptr((scanCode << 16) | 0x00000001)

			fmt.Printf("PressKey: Also sending WM_CHAR '%c' (0x%02X) to all windows\n", charCode, charCode)

			// Send WM_CHAR to main window
			ret, _, _ := procPostMessage.Call(uintptr(targetWindowHandle), WM_CHAR, uintptr(charCode), lParam)
			if ret != 0 {
				fmt.Printf("  WM_CHAR sent to main window\n")
			}

			// Also try child windows
			for i, childHwnd := range targetChildWindows {
				ret, _, _ = procPostMessage.Call(uintptr(childHwnd), WM_CHAR, uintptr(charCode), lParam)
				if ret != 0 {
					fmt.Printf("  WM_CHAR sent to child window %d\n", i)
				}
			}
		}

		if mainSuccess || childSuccess {
			fmt.Printf("PressKey: PostMessage/SendMessage sent successfully, but game may not process it (DirectInput limitation)\n")
		}

		// PostMessage works for mouse but not keyboard in DirectInput games
		// Try SendInput with scan codes as a fallback (DirectInput may accept this)
		fmt.Printf("PressKey: Trying SendInput with scan codes (DirectInput compatible)...\n")
	}

	// Use SendInput with scan codes - DirectInput games may accept this
	scanCode := MapVirtualKey(uint32(vkCode), 0) // MAPVK_VK_TO_VSC = 0

	var inputs [2]INPUT

	// Key down with scan code
	inputs[0].Type = INPUT_KEYBOARD
	inputs[0].Ki.WVk = 0 // Don't use virtual key, use scan code instead
	inputs[0].Ki.WScan = uint16(scanCode)
	inputs[0].Ki.DwFlags = KEYEVENTF_SCANCODE // Use scan code flag
	inputs[0].Ki.Time = 0
	inputs[0].Ki.DwExtraInfo = 0

	// Key up with scan code
	inputs[1].Type = INPUT_KEYBOARD
	inputs[1].Ki.WVk = 0
	inputs[1].Ki.WScan = uint16(scanCode)
	inputs[1].Ki.DwFlags = KEYEVENTF_SCANCODE | KEYEVENTF_KEYUP
	inputs[1].Ki.Time = 0
	inputs[1].Ki.DwExtraInfo = 0

	// INPUT structure size on 64-bit Windows is 40 bytes
	const inputSize = 40

	ret, _, errno := procSendInput.Call(
		2,
		uintptr(unsafe.Pointer(&inputs[0])),
		inputSize,
	)

	if ret == 0 {
		errCode, _, _ := procGetLastError.Call()
		return fmt.Errorf("SendInput failed: error code %d (errno: %v). DirectInput games may require a keyboard filter driver", errCode, errno)
	}

	fmt.Printf("PressKey: SendInput with scan codes completed (scan code: 0x%02X)\n", scanCode)
	return nil
}

// PressEscape simulates pressing the Escape key
func PressEscape() error {
	return PressKey(VK_ESCAPE)
}

// ClickAt simulates a mouse click at the specified screen coordinates
// Uses PostMessage for games that use DirectInput (like generals.exe)
func ClickAt(x, y int32) error {
	// If we have a target window, use PostMessage
	if targetWindowHandle != 0 {
		// Convert screen coordinates to window coordinates
		// For simplicity, we'll use the screen coordinates directly
		// The game should handle the conversion
		lParam := uintptr(uint32(y)<<16 | uint32(x)&0xFFFF)

		// Send mouse move
		procPostMessage.Call(
			uintptr(targetWindowHandle),
			WM_MOUSEMOVE,
			0,
			lParam,
		)

		time.Sleep(10 * time.Millisecond)

		// Send mouse down
		ret, _, _ := procPostMessage.Call(
			uintptr(targetWindowHandle),
			WM_LBUTTONDOWN,
			0x0001, // MK_LBUTTON
			lParam,
		)
		if ret == 0 {
			errCode, _, _ := procGetLastError.Call()
			return fmt.Errorf("PostMessage (mousedown) failed: error code %d", errCode)
		}

		time.Sleep(10 * time.Millisecond)

		// Send mouse up
		ret, _, _ = procPostMessage.Call(
			uintptr(targetWindowHandle),
			WM_LBUTTONUP,
			0,
			lParam,
		)
		if ret == 0 {
			errCode, _, _ := procGetLastError.Call()
			return fmt.Errorf("PostMessage (mouseup) failed: error code %d", errCode)
		}
		return nil
	}

	// Fallback to SetCursorPos + SendInput
	// Set cursor position
	ret, _, _ := procSetCursorPos.Call(uintptr(x), uintptr(y))
	if ret == 0 {
		return fmt.Errorf("SetCursorPos failed. Try running as administrator")
	}

	// Small delay to ensure cursor is positioned
	time.Sleep(10 * time.Millisecond)

	// Create mouse input for click using unsafe to overlay MOUSEINPUT in union space
	var inputs [2]INPUT

	// Mouse down - overlay MOUSEINPUT in the union space
	inputs[0].Type = INPUT_MOUSE
	mi0 := (*MOUSEINPUT)(unsafe.Pointer(&inputs[0].Ki))
	mi0.Dx = 0
	mi0.Dy = 0
	mi0.MouseData = 0
	mi0.DwFlags = MOUSEEVENTF_LEFTDOWN
	mi0.Time = 0
	mi0.DwExtraInfo = 0

	// Mouse up
	inputs[1].Type = INPUT_MOUSE
	mi1 := (*MOUSEINPUT)(unsafe.Pointer(&inputs[1].Ki))
	mi1.Dx = 0
	mi1.Dy = 0
	mi1.MouseData = 0
	mi1.DwFlags = MOUSEEVENTF_LEFTUP
	mi1.Time = 0
	mi1.DwExtraInfo = 0

	// INPUT structure size on 64-bit Windows is 40 bytes
	const inputSize = 40

	ret, _, errno := procSendInput.Call(
		2,
		uintptr(unsafe.Pointer(&inputs[0])),
		inputSize,
	)

	if ret == 0 {
		// Get last error for more details
		errCode, _, _ := procGetLastError.Call()
		return fmt.Errorf("SendInput (mouse click) failed: error code %d (errno: %v)", errCode, errno)
	}

	// Small delay after click
	time.Sleep(10 * time.Millisecond)
	return nil
}

// ClickAtMultiple simulates multiple mouse clicks at specified coordinates
func ClickAtMultiple(coords []struct{ X, Y int32 }) error {
	for _, coord := range coords {
		if err := ClickAt(coord.X, coord.Y); err != nil {
			return fmt.Errorf("failed to click at (%d, %d): %w", coord.X, coord.Y, err)
		}
		// Small delay between clicks
		time.Sleep(2 * time.Second)
	}
	return nil
}

// DeleteAllRepFiles deletes all .rep files from the specified directory
func DeleteAllRepFiles(dir string) error {
	pattern := filepath.Join(dir, "*.rep")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("failed to glob pattern: %w", err)
	}

	for _, match := range matches {
		if err := os.Remove(match); err != nil {
			return fmt.Errorf("failed to delete %s: %w", match, err)
		}
		fmt.Printf("Deleted: %s\n", match)
	}

	fmt.Printf("Deleted %d .rep file(s) from %s\n", len(matches), dir)
	return nil
}

// MoveOneRepFile moves one .rep file from sourceDir to destDir
func MoveOneRepFile(sourceDir, destDir string) (string, error) {
	pattern := filepath.Join(sourceDir, "*.rep")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", fmt.Errorf("failed to glob pattern: %w", err)
	}

	if len(matches) == 0 {
		return "", fmt.Errorf("no .rep files found in %s", sourceDir)
	}

	// Move the first file
	sourceFile := matches[0]
	destFile := filepath.Join(destDir, filepath.Base(sourceFile))

	if err := os.Rename(sourceFile, destFile); err != nil {
		return "", fmt.Errorf("failed to move %s to %s: %w", sourceFile, destFile, err)
	}

	fmt.Printf("Moved: %s -> %s\n", sourceFile, destFile)
	return destFile, nil
}

// StartGenerals starts generals.exe with the specified path
func StartGenerals(exePath string) (*exec.Cmd, error) {
	cmd := exec.Command(exePath)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow: false,
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start generals.exe: %w", err)
	}

	fmt.Printf("Started generals.exe (PID: %d)\n", cmd.Process.Pid)
	return cmd, nil
}

// WaitForTimecodeStart waits for the timecode to start increasing
func WaitForTimecodeStart(getTimecode func() (uint32, error), timeout time.Duration) error {
	fmt.Println("Waiting for timecode to start increasing...")

	startTime := time.Now()
	var lastTimecode uint32 = 0
	increaseCount := 0
	const requiredIncreases = 3

	for {
		if time.Since(startTime) > timeout {
			return fmt.Errorf("timeout waiting for timecode to start")
		}

		currentTimecode, err := getTimecode()
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}

		if lastTimecode != 0 {
			if currentTimecode > lastTimecode {
				increaseCount++
				fmt.Printf("Timecode increased from %d to %d (%d/%d consecutive increases)\n",
					lastTimecode, currentTimecode, increaseCount, requiredIncreases)
				if increaseCount >= requiredIncreases {
					fmt.Println("Timecode started increasing!")
					return nil
				}
			} else if currentTimecode < lastTimecode {
				// Timecode went backwards, reset
				increaseCount = 0
			}
		}

		lastTimecode = currentTimecode
		time.Sleep(500 * time.Millisecond)
	}
}

// WaitForTimecodeStop waits for the timecode to stop increasing
func WaitForTimecodeStop(getTimecode func() (uint32, error), timeout time.Duration) error {
	fmt.Println("Waiting for timecode to stop increasing...")

	startTime := time.Now()
	var lastTimecode uint32 = 0
	stableCount := 0
	const requiredStable = 5 // Need 5 consecutive stable readings

	for {
		if time.Since(startTime) > timeout {
			return fmt.Errorf("timeout waiting for timecode to stop")
		}

		currentTimecode, err := getTimecode()
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}

		if lastTimecode != 0 {
			if currentTimecode == lastTimecode {
				stableCount++
				if stableCount >= requiredStable {
					fmt.Println("Timecode stopped increasing!")
					return nil
				}
			} else {
				// Timecode changed, reset stable count
				stableCount = 0
				fmt.Printf("Timecode: %d\n", currentTimecode)
			}
		}

		lastTimecode = currentTimecode
		time.Sleep(500 * time.Millisecond)
	}
}

// CountRepFiles counts the number of .rep files in a directory
func CountRepFiles(dir string) (int, error) {
	pattern := filepath.Join(dir, "*.rep")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return 0, fmt.Errorf("failed to glob pattern: %w", err)
	}
	return len(matches), nil
}
