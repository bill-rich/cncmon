//go:build windows

package memmon

import (
	"errors"
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
	hProc windows.Handle
	base  uintptr // generals.exe base
}

// Init attaches to generals.exe and caches process handle + module base.
func Init() (*Reader, error) {
	pid, err := findProcessID("generals.exe")
	if err != nil {
		return nil, err
	}
	h, err := openProcess(pid)
	if err != nil {
		return nil, err
	}
	base, err := moduleBase(pid, "generals.exe")
	if err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	return &Reader{hProc: h, base: base}, nil
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

	const initial = uintptr(0x0062BAA0)
	playerOffsets := [8]uint32{0x0C, 0x10, 0x14, 0x18, 0x1C, 0x20, 0x24, 0x28}

	// root = *(base + initial)
	root, ok := r.rpmU32(r.base + initial)
	if !ok {
		return out
	}

	for i := 0; i < 8; i++ {
		mid, ok := r.rpmU32(uintptr(root) + uintptr(playerOffsets[i]))
		if !ok {
			continue
		}
		val, ok := r.rpmI32(uintptr(mid) + 0x38)
		if !ok {
			continue
		}
		out[i] = val
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
