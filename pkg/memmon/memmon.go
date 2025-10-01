//go:build windows

package memmon

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32                     = windows.NewLazySystemDLL("kernel32.dll")
	procCreateToolhelp32Snapshot = kernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32FirstW          = kernel32.NewProc("Process32FirstW")
	procProcess32NextW           = kernel32.NewProc("Process32NextW")
	procModule32FirstW           = kernel32.NewProc("Module32FirstW")
	procModule32NextW            = kernel32.NewProc("Module32NextW")
	procOpenProcess              = kernel32.NewProc("OpenProcess")
	procReadProcessMemory        = kernel32.NewProc("ReadProcessMemory")
	procCloseHandle              = kernel32.NewProc("CloseHandle")
)

const (
	PROCESS_VM_READ           = 0x0010
	PROCESS_QUERY_INFORMATION = 0x0400

	TH32CS_SNAPPROCESS  = 0x00000002
	TH32CS_SNAPMODULE   = 0x00000008
	TH32CS_SNAPMODULE32 = 0x00000010
	MAX_PATH            = 260
	MAX_MODULE_NAME32   = 255
)

type PROCESSENTRY32 struct {
	DwSize              uint32
	CntUsage            uint32
	Th32ProcessID       uint32
	Th32DefaultHeapID   uintptr
	Th32ModuleID        uint32
	CntThreads          uint32
	Th32ParentProcessID uint32
	PcPriClassBase      int32
	DwFlags             uint32
	SzExeFile           [MAX_PATH]uint16
}

type MODULEENTRY32 struct {
	DwSize        uint32
	Th32ModuleID  uint32
	Th32ProcessID uint32
	GlblcntUsage  uint32
	ProccntUsage  uint32
	ModBaseAddr   *byte // BYTE*
	ModBaseSize   uint32
	HModule       windows.Handle
	SzModule      [MAX_MODULE_NAME32 + 1]uint16
	SzExePath     [MAX_PATH]uint16
}

type Reader struct {
	hProc        windows.Handle
	base         uintptr // process base
	initialAddr  uintptr // cached initial address found via AOB
	initialFound bool    // whether initial address has been found
	processName  string  // process name for AOB search
}

// Init attaches to the specified process and caches process handle + module base.
func Init(processName string) (*Reader, error) {
	pid, err := findProcessID(processName)
	if err != nil {
		return nil, err
	}
	h, err := openProcess(pid)
	if err != nil {
		return nil, err
	}
	base, err := moduleBase(pid, processName)
	if err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	return &Reader{hProc: h, base: base, processName: processName}, nil
}

func (r *Reader) Close() {
	if r.hProc != 0 {
		_ = windows.CloseHandle(r.hProc)
		r.hProc = 0
	}
}

// Poll returns money for players P1..P8 (len=8). -1 means read failed for that slot.
func (r *Reader) Poll() [8]int32 {
	var out [8]int32
	for i := range out {
		out[i] = -1
	}
	if r == nil || r.hProc == 0 || r.base == 0 {
		return out
	}

	// Find initial address using AOB search if not already found
	if !r.initialFound {
		log.Printf("AOB Search: Starting pattern search for initial address...")
		initialAddr, err := r.findInitialAddress(r.processName)
		if err != nil {
			log.Printf("AOB Search: Pattern search failed (%v), falling back to hardcoded address 0x%X", err, 0x0062BAA0)
			// If AOB search fails, fall back to hardcoded address as backup
			initialAddr = uintptr(0x0062BAA0)
		} else {
			log.Printf("AOB Search: Pattern found at address 0x%X (offset from base: 0x%X)", initialAddr, initialAddr-r.base)
		}
		r.initialAddr = initialAddr
		r.initialFound = true
	}

	playerOffsets := [9]uint32{0x1C, 0x20, 0x24, 0x28, 0x2C, 0x30, 0x34, 0x38, 0x3C}

	// root = *(base + initial)
	// log.Printf("AOB Poll: Using cached initial address 0x%X (offset: 0x%X)", r.initialAddr, r.initialAddr-r.base)
	// root, ok := r.rpmU32(r.base + r.initialAddr)
	root, ok := r.rpmU32(r.initialAddr)
	if !ok {
		log.Printf("AOB Poll: Failed to read root pointer at 0x%X", r.base+r.initialAddr)
		return out
	}
	// log.Printf("AOB Poll: Root pointer value: 0x%X", root)

	// Read all 9 values first
	var tempValues [9]int32
	for i := 0; i < 9; i++ {
		mid, ok := r.rpmU32(uintptr(root) + uintptr(playerOffsets[i]))
		if !ok {
			tempValues[i] = -1
			continue
		}
		val, ok := r.rpmI32(uintptr(mid) + 0x38)
		if !ok {
			tempValues[i] = -1
			continue
		}
		tempValues[i] = val
	}

	// Find the last positive value and replace it and following zeros with -1
	lastPositiveIndex := -1
	for i := 0; i < 9; i++ {
		if tempValues[i] > 0 {
			lastPositiveIndex = i
		}
	}

	// Replace the last positive value and any following zeros with -1
	if lastPositiveIndex >= 0 {
		for i := lastPositiveIndex; i < 9; i++ {
			if tempValues[i] == 0 {
				tempValues[i] = -1
			}
		}
		// Also replace the last positive value itself
		tempValues[lastPositiveIndex] = -1
	}

	// Copy the first 8 values to the output array
	for i := 0; i < 8; i++ {
		out[i] = tempValues[i]
	}
	return out
}

// --- Win32 helpers ---

func findProcessID(exe string) (uint32, error) {
	hsnap, _, _ := procCreateToolhelp32Snapshot.Call(TH32CS_SNAPPROCESS, 0)
	if hsnap == 0 || hsnap == uintptr(windows.InvalidHandle) {
		return 0, errors.New("CreateToolhelp32Snapshot failed")
	}
	defer procCloseHandle.Call(hsnap)

	var pe PROCESSENTRY32
	pe.DwSize = uint32(unsafe.Sizeof(pe))

	ret, _, _ := procProcess32FirstW.Call(hsnap, uintptr(unsafe.Pointer(&pe)))
	if ret == 0 {
		return 0, errors.New("Process32FirstW failed")
	}
	target := strings.ToLower(exe)

	for {
		name := windows.UTF16ToString(pe.SzExeFile[:])
		if strings.EqualFold(name, target) {
			return pe.Th32ProcessID, nil
		}
		ret, _, _ = procProcess32NextW.Call(hsnap, uintptr(unsafe.Pointer(&pe)))
		if ret == 0 {
			break
		}
	}
	return 0, errors.New("process not found: " + exe)
}

func moduleBase(pid uint32, modName string) (uintptr, error) {
	flags := uintptr(TH32CS_SNAPMODULE | TH32CS_SNAPMODULE32)
	hsnap, _, _ := procCreateToolhelp32Snapshot.Call(flags, uintptr(pid))
	if hsnap == 0 || hsnap == uintptr(windows.InvalidHandle) {
		return 0, errors.New("CreateToolhelp32Snapshot(MODULE) failed")
	}
	defer procCloseHandle.Call(hsnap)

	var me MODULEENTRY32
	me.DwSize = uint32(unsafe.Sizeof(me))
	ret, _, _ := procModule32FirstW.Call(hsnap, uintptr(unsafe.Pointer(&me)))
	if ret == 0 {
		return 0, errors.New("Module32FirstW failed")
	}
	target := strings.ToLower(modName)

	for {
		name := windows.UTF16ToString(me.SzModule[:])
		if strings.EqualFold(name, target) {
			return uintptr(unsafe.Pointer(me.ModBaseAddr)), nil
		}
		ret, _, _ = procModule32NextW.Call(hsnap, uintptr(unsafe.Pointer(&me)))
		if ret == 0 {
			break
		}
	}
	return 0, errors.New("module not found: " + modName)
}

func openProcess(pid uint32) (windows.Handle, error) {
	h, _, e := procOpenProcess.Call(PROCESS_VM_READ|PROCESS_QUERY_INFORMATION, 0, uintptr(pid))
	if h == 0 {
		if e != syscall.Errno(0) {
			return 0, e
		}
		return 0, errors.New("OpenProcess failed")
	}
	return windows.Handle(h), nil
}

func (r *Reader) rpmRaw(addr uintptr, buf []byte) (bool, uintptr) {
	var read uintptr
	ret, _, _ := procReadProcessMemory.Call(
		uintptr(r.hProc),
		addr,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
		uintptr(unsafe.Pointer(&read)),
	)
	return ret != 0 && read == uintptr(len(buf)), read
}

func (r *Reader) rpmU32(addr uintptr) (uint32, bool) {
	var b [4]byte
	ok, _ := r.rpmRaw(addr, b[:])
	if !ok {
		return 0, false
	}
	return *(*uint32)(unsafe.Pointer(&b[0])), true
}

func (r *Reader) rpmI32(addr uintptr) (int32, bool) {
	var b [4]byte
	ok, _ := r.rpmRaw(addr, b[:])
	if !ok {
		return 0, false
	}
	return *(*int32)(unsafe.Pointer(&b[0])), true
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

// searchAOBPattern searches for the AOB pattern in process memory
func (r *Reader) searchAOBPattern(pattern string, processName string) (uintptr, error) {
	patternBytes, wildcards, err := ParseAOBPattern(pattern)
	if err != nil {
		return 0, fmt.Errorf("failed to parse pattern: %w", err)
	}

	// Get module information to determine search range
	pid, err := findProcessID(processName)
	if err != nil {
		return 0, fmt.Errorf("failed to find process: %w", err)
	}

	base, err := moduleBase(pid, processName)
	if err != nil {
		return 0, fmt.Errorf("failed to get module base: %w", err)
	}

	log.Printf("AOB Search: Module base at 0x%X, PID: %d", base, pid)

	// Search in a reasonable range around the module base
	// We'll search from base to base + 0x1000000 (16MB) which should be enough
	searchStart := base
	searchEnd := base + 0x1000000

	log.Printf("AOB Search: Searching range 0x%X to 0x%X (0x%X bytes)", searchStart, searchEnd, searchEnd-searchStart)

	// Read memory in chunks to search for the pattern
	chunkSize := uintptr(0x10000) // 64KB chunks
	buffer := make([]byte, chunkSize)
	chunksSearched := 0
	bytesSearched := uintptr(0)

	for addr := searchStart; addr < searchEnd; addr += chunkSize {
		// Adjust chunk size for the last chunk
		remaining := searchEnd - addr
		if remaining < chunkSize {
			chunkSize = remaining
			buffer = make([]byte, chunkSize)
		}

		// Read memory chunk
		ok, bytesRead := r.rpmRaw(addr, buffer)
		if !ok || bytesRead == 0 {
			log.Printf("AOB Search: Failed to read memory at 0x%X (chunk %d)", addr, chunksSearched)
			continue
		}

		chunksSearched++
		bytesSearched += bytesRead

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
				foundAddr := addr + uintptr(i)
				// The pattern is "a1 ?? ?? ?? ?? 8b 40 0c 85 c0 74 78"
				// We want the address of the wildcard bytes (?? ?? ?? ??), not the "a1"
				// The wildcards start at offset 1 from the beginning of the pattern
				wildcardAddr := foundAddr + 1
				log.Printf("AOB Search: Pattern found at 0x%X, wildcard section at 0x%X (offset from base: 0x%X) after searching %d chunks (%d bytes)",
					foundAddr, wildcardAddr, wildcardAddr-base, chunksSearched, bytesSearched)
				return wildcardAddr, nil
			}
		}

		// Log progress every 100 chunks
		if chunksSearched%100 == 0 {
			log.Printf("AOB Search: Progress - searched %d chunks (%d bytes)", chunksSearched, bytesSearched)
		}
	}

	log.Printf("AOB Search: Pattern not found after searching %d chunks (%d bytes)", chunksSearched, bytesSearched)
	return 0, errors.New("pattern not found in memory")
}

// findInitialAddress uses AOB search to find the initial address
func (r *Reader) findInitialAddress(processName string) (uintptr, error) {
	// Try the primary pattern first - used in generlas.exe
	primaryPattern := "a1 ?? ?? ?? ?? 8b 40 0c 85 c0 74 78"
	log.Printf("AOB Find: Searching for initial address using primary pattern: %s", primaryPattern)

	patternAddr, err := r.searchAOBPattern(primaryPattern, processName)
	if err != nil {
		log.Printf("AOB Find: Primary pattern not found, trying fallback pattern...")

		// Try the fallback pattern - Used in GeneralsOnlineZH_30.exe
		fallbackPattern := "a1 ?? ?? ?? ?? 8b 48 0c 85 c9 74 74"
		log.Printf("AOB Find: Searching for initial address using fallback pattern: %s", fallbackPattern)

		patternAddr, err = r.searchAOBPattern(fallbackPattern, processName)
		if err != nil {
			return 0, fmt.Errorf("both primary and fallback patterns not found: %w", err)
		}
	}

	log.Printf("AOB Find: Pattern found at 0x%X, reading 4-byte pointer...", patternAddr)

	// The pattern location contains a pointer to the actual initial address
	// Read the 4-byte pointer value at the found location
	initialAddr, ok := r.rpmU32(patternAddr)
	if !ok {
		return 0, fmt.Errorf("failed to read pointer at pattern location 0x%X", patternAddr)
	}

	log.Printf("AOB Find: Pointer value at 0x%X is 0x%X (offset from base: 0x%X)",
		patternAddr, initialAddr, uintptr(initialAddr)-r.base)

	return uintptr(initialAddr), nil
}
