package sysinfo

import (
	"encoding/binary"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	powrprof = syscall.NewLazyDLL("powrprof.dll")

	procGetSystemTimes                 = kernel32.NewProc("GetSystemTimes")
	procSetProcessAffinityMask         = kernel32.NewProc("SetProcessAffinityMask")
	procGetProcessAffinityMask         = kernel32.NewProc("GetProcessAffinityMask")
	procGenerateConsoleCtrlEvent       = kernel32.NewProc("GenerateConsoleCtrlEvent")
	procGetSystemPowerStatus           = kernel32.NewProc("GetSystemPowerStatus")
	procGetLogicalProcessorInformation = kernel32.NewProc("GetLogicalProcessorInformation")
	procGlobalMemoryStatusEx           = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetDiskFreeSpaceExW            = kernel32.NewProc("GetDiskFreeSpaceExW")
	procLocalFree                      = kernel32.NewProc("LocalFree")
	procPowerGetActiveScheme           = powrprof.NewProc("PowerGetActiveScheme")
	procPowerReadFriendlyName          = powrprof.NewProc("PowerReadFriendlyName")
	procPowerReadACValueIndex          = powrprof.NewProc("PowerReadACValueIndex")
	procPowerGetEffectiveOverlayScheme = powrprof.NewProc("PowerGetEffectiveOverlayScheme")
)

// Process access rights (winnt.h).
const (
	processSetInformation          = 0x0200
	processQueryLimitedInformation = 0x1000
)

// duration converts a FILETIME holding an interval (100 ns units).
func duration(f syscall.Filetime) time.Duration {
	return time.Duration(uint64(f.HighDateTime)<<32|uint64(f.LowDateTime)) * 100
}

func openProcess(pid int, access uint32) (syscall.Handle, error) {
	h, err := syscall.OpenProcess(access, false, uint32(pid))
	if err != nil {
		return 0, fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	return h, nil
}

func processTimes(h syscall.Handle) (CPUTimes, error) {
	var creation, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return CPUTimes{}, fmt.Errorf("GetProcessTimes: %w", err)
	}
	return CPUTimes{Kernel: duration(kernel), User: duration(user)}, nil
}

// ProcessTimes returns the CPU time process pid has used so far.
func ProcessTimes(pid int) (CPUTimes, error) {
	h, err := openProcess(pid, processQueryLimitedInformation)
	if err != nil {
		return CPUTimes{}, err
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	return processTimes(h)
}

// SelfTimes returns the CPU time this process has used so far.
func SelfTimes() (CPUTimes, error) {
	h, err := syscall.GetCurrentProcess() // a pseudo-handle: nothing to close
	if err != nil {
		return CPUTimes{}, err
	}
	return processTimes(h)
}

// ReadSystemTimes returns the machine's CPU time since boot. Windows counts
// idle time inside kernel time, so busy is kernel + user - idle.
func ReadSystemTimes() (SystemTimes, error) {
	var idle, kernel, user syscall.Filetime
	if r, _, err := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user))); r == 0 {
		return SystemTimes{}, fmt.Errorf("GetSystemTimes: %w", err)
	}
	i := duration(idle)
	return SystemTimes{Idle: i, Busy: duration(kernel) + duration(user) - i}, nil
}

// SetAffinity pins every thread of process pid, present and future, to
// the logical CPUs in mask: the same process affinity mask that
// `start /affinity` sets.
func SetAffinity(pid int, mask uint64) error {
	h, err := openProcess(pid, processSetInformation|processQueryLimitedInformation)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	if r, _, err := procSetProcessAffinityMask.Call(uintptr(h), uintptr(mask)); r == 0 {
		return fmt.Errorf("SetProcessAffinityMask(%d, %#x): %w", pid, mask, err)
	}
	return nil
}

// Affinity reads process pid's affinity mask back.
func Affinity(pid int) (uint64, error) {
	h, err := openProcess(pid, processQueryLimitedInformation)
	if err != nil {
		return 0, err
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	var proc, sys uintptr
	if r, _, err := procGetProcessAffinityMask.Call(uintptr(h), uintptr(unsafe.Pointer(&proc)), uintptr(unsafe.Pointer(&sys))); r == 0 {
		return 0, fmt.Errorf("GetProcessAffinityMask(%d): %w", pid, err)
	}
	return uint64(proc), nil
}

func interruptible(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

// Interrupt sends CTRL_BREAK to the process group led by pid (a process
// started with Interruptible). Go delivers it as os.Interrupt, so the
// engine shuts down gracefully. It fails if this process has no console.
func Interrupt(pid int) error {
	const ctrlBreakEvent = 1
	if r, _, err := procGenerateConsoleCtrlEvent.Call(ctrlBreakEvent, uintptr(pid)); r == 0 {
		return fmt.Errorf("GenerateConsoleCtrlEvent: %w", err)
	}
	return nil
}

// DiskFree returns the bytes available to this user on dir's volume.
func DiskFree(dir string) (uint64, error) {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var avail, total, free uint64
	if r, _, err := procGetDiskFreeSpaceExW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&avail)),
		uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&free))); r == 0 {
		return 0, fmt.Errorf("GetDiskFreeSpaceEx(%s): %w", dir, err)
	}
	return avail, nil
}

// ---------------------------------------------------------------------------
// Power

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

func (g guid) String() string {
	return fmt.Sprintf("%08x-%04x-%04x-%02x%02x-%02x%02x%02x%02x%02x%02x", g.Data1, g.Data2, g.Data3,
		g.Data4[0], g.Data4[1], g.Data4[2], g.Data4[3], g.Data4[4], g.Data4[5], g.Data4[6], g.Data4[7])
}

func mustGUID(s string) guid {
	s = strings.ReplaceAll(s, "-", "")
	if len(s) != 32 {
		panic("bad guid " + s)
	}
	var b [16]byte
	for i := range b {
		v, err := strconv.ParseUint(s[2*i:2*i+2], 16, 8)
		if err != nil {
			panic("bad guid " + s)
		}
		b[i] = byte(v)
	}
	return guid{
		Data1: uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]),
		Data2: uint16(b[4])<<8 | uint16(b[5]),
		Data3: uint16(b[6])<<8 | uint16(b[7]),
		Data4: [8]byte(b[8:16]),
	}
}

var (
	// winnt.h: NO_SUBGROUP_GUID and GUID_POWERSCHEME_PERSONALITY.
	noSubgroup  = mustGUID("fea3413e-7e05-4911-9a71-700331f1c294")
	personality = mustGUID("245d8541-3943-4422-b025-13a784f679b7")
)

// personalities: GUID_POWERSCHEME_PERSONALITY's values, which say what kind
// of plan a scheme is however it's named. TestPersonalities checks them
// against the three built-in plans.
var personalities = map[uint32]string{0: "power saver", 1: "high performance", 2: "balanced"}

// overlays: the power-mode slider's overlay schemes (Settings > System >
// Power). An unlisted GUID is reported as unknown, with the GUID recorded.
var overlays = map[string]string{
	"00000000-0000-0000-0000-000000000000": "none",
	"961cc777-2547-4f9d-8174-7d86181b8a7a": "best power efficiency",
	"ded574b5-45a0-4f42-8737-46345c09c238": "best performance",
}

// systemPowerStatus is SYSTEM_POWER_STATUS.
type systemPowerStatus struct {
	ACLineStatus        byte
	BatteryFlag         byte
	BatteryLifePercent  byte
	SystemStatusFlag    byte
	BatteryLifeTime     uint32
	BatteryFullLifeTime uint32
}

// ReadPower reports the power source and the active plan.
func ReadPower() (Power, error) {
	var p Power
	var st systemPowerStatus
	if r, _, err := procGetSystemPowerStatus.Call(uintptr(unsafe.Pointer(&st))); r == 0 {
		return p, fmt.Errorf("GetSystemPowerStatus: %w", err)
	}
	p.AC = st.ACLineStatus == 1
	p.Battery = -1
	if st.BatteryLifePercent <= 100 {
		p.Battery = int(st.BatteryLifePercent)
	}

	var active *guid
	if r, _, _ := procPowerGetActiveScheme.Call(0, uintptr(unsafe.Pointer(&active))); r != 0 {
		return p, fmt.Errorf("PowerGetActiveScheme: error %d", r)
	}
	scheme := *active
	_, _, _ = procLocalFree.Call(uintptr(unsafe.Pointer(active)))
	p.SchemeGUID = scheme.String()
	p.SchemeName = schemeName(&scheme)
	p.Personality = schemePersonality(&scheme)

	if procPowerGetEffectiveOverlayScheme.Find() == nil {
		var ov guid
		if r, _, _ := procPowerGetEffectiveOverlayScheme.Call(uintptr(unsafe.Pointer(&ov))); r == 0 {
			p.OverlayGUID = ov.String()
			p.Overlay = overlays[p.OverlayGUID]
			if p.Overlay == "" {
				p.Overlay = "unknown"
			}
		}
	}
	return p, nil
}

func schemeName(g *guid) string {
	var size uint32
	_, _, _ = procPowerReadFriendlyName.Call(0, uintptr(unsafe.Pointer(g)), 0, 0, 0, uintptr(unsafe.Pointer(&size)))
	if size == 0 {
		return "?"
	}
	buf := make([]uint16, size/2+1)
	if r, _, _ := procPowerReadFriendlyName.Call(0, uintptr(unsafe.Pointer(g)), 0, 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r != 0 {
		return "?"
	}
	return syscall.UTF16ToString(buf)
}

func schemePersonality(g *guid) string {
	var v uint32
	if r, _, _ := procPowerReadACValueIndex.Call(0, uintptr(unsafe.Pointer(g)), uintptr(unsafe.Pointer(&noSubgroup)),
		uintptr(unsafe.Pointer(&personality)), uintptr(unsafe.Pointer(&v))); r != 0 {
		return "unknown"
	}
	if name, ok := personalities[v]; ok {
		return name
	}
	return fmt.Sprintf("unknown (%d)", v)
}

// ---------------------------------------------------------------------------
// Hardware

// slpi is SYSTEM_LOGICAL_PROCESSOR_INFORMATION on amd64: 32 bytes.
type slpi struct {
	mask         uintptr
	relationship uint32
	_            uint32
	union        [16]byte
}

// LOGICAL_PROCESSOR_RELATIONSHIP and PROCESSOR_CACHE_TYPE values.
const (
	relationProcessorCore    = 0
	relationCache            = 2
	relationProcessorPackage = 3
)

var cacheTypes = map[uint32]string{0: "unified", 1: "instruction", 2: "data", 3: "trace"}

// memoryStatusEx is MEMORYSTATUSEX.
type memoryStatusEx struct {
	length                  uint32
	memoryLoad              uint32
	totalPhys, availPhys    uint64
	totalPageFile, availPF  uint64
	totalVirtual, availVirt uint64
	availExtendedVirtual    uint64
}

// ReadMachine describes the CPU (with which logical CPUs share a core and
// which share each cache), memory and OS.
func ReadMachine() (Machine, error) {
	var m Machine
	m.CPU = strings.TrimSpace(regString(`HARDWARE\DESCRIPTION\System\CentralProcessor\0`, "ProcessorNameString"))

	var size uint32
	_, _, _ = procGetLogicalProcessorInformation.Call(0, uintptr(unsafe.Pointer(&size)))
	if size == 0 {
		return m, fmt.Errorf("GetLogicalProcessorInformation: no size")
	}
	info := make([]slpi, size/uint32(unsafe.Sizeof(slpi{}))+1)
	if r, _, err := procGetLogicalProcessorInformation.Call(uintptr(unsafe.Pointer(&info[0])), uintptr(unsafe.Pointer(&size))); r == 0 {
		return m, fmt.Errorf("GetLogicalProcessorInformation: %w", err)
	}
	info = info[:size/uint32(unsafe.Sizeof(slpi{}))]
	for _, e := range info {
		cpus := MaskCPUs(uint64(e.mask))
		switch e.relationship {
		case relationProcessorCore:
			m.Cores = append(m.Cores, Core{Logical: cpus})
			m.Logical += len(cpus)
		case relationProcessorPackage:
			m.Packages++
		case relationCache:
			level := int(e.union[0])
			bytes := int64(*(*uint32)(unsafe.Pointer(&e.union[4])))
			typ := *(*uint32)(unsafe.Pointer(&e.union[8]))
			m.Caches = append(m.Caches, Cache{Level: level, Type: cacheTypes[typ], Bytes: bytes, Logical: cpus})
		}
	}

	ms := memoryStatusEx{length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms))); r == 0 {
		return m, fmt.Errorf("GlobalMemoryStatusEx: %w", err)
	}
	m.RAMBytes = ms.totalPhys
	m.OS = osName()
	return m, nil
}

// osName: ProductName still says "Windows 10" on Windows 11 (Microsoft
// kept the value for compatibility), so the build number decides: Windows
// 11 is build 22000 and later.
func osName() string {
	const key = `SOFTWARE\Microsoft\Windows NT\CurrentVersion`
	name := regString(key, "ProductName")
	build := regString(key, "CurrentBuild")
	if b, err := strconv.Atoi(build); err == nil && b >= 22000 {
		name = strings.Replace(name, "Windows 10", "Windows 11", 1)
	}
	if v := regString(key, "DisplayVersion"); v != "" {
		name += " " + v
	}
	return name + " (build " + build + ")"
}

// regString reads a REG_SZ value under HKEY_LOCAL_MACHINE, or "".
func regString(path, name string) string {
	p, _ := syscall.UTF16PtrFromString(path)
	var k syscall.Handle
	if err := syscall.RegOpenKeyEx(syscall.HKEY_LOCAL_MACHINE, p, 0, syscall.KEY_READ, &k); err != nil {
		return ""
	}
	defer func() { _ = syscall.RegCloseKey(k) }()
	n, _ := syscall.UTF16PtrFromString(name)
	buf := make([]uint16, 256)
	size := uint32(len(buf) * 2)
	var typ uint32
	if err := syscall.RegQueryValueEx(k, n, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &size); err != nil || typ != syscall.REG_SZ {
		return ""
	}
	return syscall.UTF16ToString(buf)
}

// ---------------------------------------------------------------------------
// Every process at once

var (
	ntdll                        = syscall.NewLazyDLL("ntdll.dll")
	procNtQuerySystemInformation = ntdll.NewProc("NtQuerySystemInformation")
)

// SYSTEM_INFORMATION_CLASS values (winternl.h).
const (
	systemProcessInformation              = 5
	systemProcessorPerformanceInfo        = 8
	statusInfoLengthMismatch       uint32 = 0xC0000004
)

func querySystem(class uint32, size int) ([]byte, error) {
	for {
		buf := make([]byte, size)
		var ret uint32
		st, _, _ := procNtQuerySystemInformation.Call(uintptr(class), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&ret)))
		switch uint32(st) {
		case 0:
			return buf[:ret], nil
		case statusInfoLengthMismatch:
			size = max(2*size, int(ret)+64<<10)
		default:
			return nil, fmt.Errorf("NtQuerySystemInformation(%d): status %#x", class, uint32(st))
		}
	}
}

func le64(b []byte, off int) int64 { return int64(binary.LittleEndian.Uint64(b[off:])) }

// ReadSnapshot reads every process's CPU time (NtQuerySystemInformation's
// process list, as Task Manager does) and the CPUs' interrupt and DPC time.
func ReadSnapshot() (Snapshot, error) {
	s := Snapshot{Procs: map[int]ProcCPU{}}
	buf, err := querySystem(systemProcessInformation, 512<<10)
	if err != nil {
		return s, err
	}
	// SYSTEM_PROCESS_INFORMATION on amd64: NextEntryOffset u32 @0, then
	// (in what winternl.h calls Reserved1) UserTime @40 and KernelTime @48,
	// ImageName (UNICODE_STRING: length u16 @56, buffer pointer @64), and
	// UniqueProcessId @80. TestSnapshotMatchesProcessTimes checks the
	// offsets against GetProcessTimes.
	base := uintptr(unsafe.Pointer(&buf[0]))
	for off := 0; off+88 <= len(buf); {
		e := buf[off:]
		next := int(binary.LittleEndian.Uint32(e))
		pid := int(binary.LittleEndian.Uint64(e[80:]))
		cpu := time.Duration(le64(e, 40)+le64(e, 48)) * 100
		name := "idle"
		if n := int(binary.LittleEndian.Uint16(e[56:])); n > 0 {
			if p := uintptr(binary.LittleEndian.Uint64(e[64:])); p >= base && int(p-base)+n <= len(buf) {
				at := int(p - base)
				u := make([]uint16, n/2)
				for i := range u {
					u[i] = binary.LittleEndian.Uint16(buf[at+2*i:])
				}
				name = syscall.UTF16ToString(u)
			}
		}
		s.Procs[pid] = ProcCPU{PID: pid, Name: name, CPU: cpu}
		if next == 0 {
			break
		}
		off += next
	}

	// SYSTEM_PROCESSOR_PERFORMANCE_INFORMATION, one per CPU, 48 bytes:
	// idle, kernel, user, DPC, interrupt time (i64 each), interrupt count.
	cpus, err := querySystem(systemProcessorPerformanceInfo, 48*64)
	if err != nil {
		return s, err
	}
	for off := 0; off+48 <= len(cpus); off += 48 {
		s.Interrupts += time.Duration(le64(cpus, off+24)+le64(cpus, off+32)) * 100
	}
	return s, nil
}
