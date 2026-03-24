//go:build windows

package driver

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/LoveWonYoung/linbuskit/liniface"
	"github.com/LoveWonYoung/linbuskit/tplin"
	"golang.org/x/sys/windows/registry"
)

var (
	UsbDeviceDLL    syscall.Handle
	UsbScanDevice   uintptr
	UsbOpenDevice   uintptr
	UsbCloseDevice  uintptr
	LinExInit       uintptr
	LinExMasterSync uintptr
	DevHandle       [10]int
	DEVIndex        = 0

	toomossMu sync.Mutex
)

const (
	LIN_EX_SUCCESS = -iota
	LIN_EX_ERR_NOT_SUPPORT
	LIN_EX_ERR_USB_WRITE_FAIL
	LIN_EX_ERR_USB_READ_FAIL
	LIN_EX_ERR_CMD_FAIL
	LIN_EX_ERR_CH_NO_INIT
	LIN_EX_ERR_READ_DATA
	LIN_EX_ERR_PARAMETER
)

const (
	LIN_EX_MSG_TYPE_UN = iota
	LIN_EX_MSG_TYPE_MW
	LIN_EX_MSG_TYPE_MR
	LIN_EX_MSG_TYPE_SW
	LIN_EX_MSG_TYPE_SR
	LIN_EX_MSG_TYPE_BK
	LIN_EX_MSG_TYPE_SY
	LIN_EX_MSG_TYPE_ID
	LIN_EX_MSG_TYPE_DT
	LIN_EX_MSG_TYPE_CK
	LIN_EX_CHECK_STD   = iota - 10 // 标准校验，不含PID
	LIN_EX_CHECK_EXT               // 增强校验，含PID
	LIN_EX_CHECK_USER              // 自定义校验类型，需要用户自行计算并传入Check，不进行自动校验
	LIN_EX_CHECK_NONE              // 不进行校验数据
	LIN_EX_CHECK_ERROR             // 接收数据校验错误
)

type LinExMsg struct {
	Timestamp uint32
	MsgType   uint8
	CheckType uint8
	DataLen   uint8
	Sync      uint8
	PID       uint8
	Data      [8]uint8
	Check     uint8
	BreakBits uint8
	Reserve1  uint8
}
type ToomossCh byte

const (
	CH1 ToomossCh = iota
	CH2
	CH3
	CH4
)

var (
	Bt     uint = 19200
	Master byte = 1
)

type Toomoss struct {
	eventChan chan *liniface.LinEvent
	channel   ToomossCh
}

func toomossReady() bool {
	return UsbDeviceDLL != 0 &&
		UsbScanDevice != 0 &&
		UsbOpenDevice != 0 &&
		UsbCloseDevice != 0 &&
		LinExInit != 0 &&
		LinExMasterSync != 0
}

func resetToomossState() {
	UsbDeviceDLL = 0
	UsbScanDevice = 0
	UsbOpenDevice = 0
	UsbCloseDevice = 0
	LinExInit = 0
	LinExMasterSync = 0
}

func ensureToomossLoaded() error {
	toomossMu.Lock()
	defer toomossMu.Unlock()

	if toomossReady() {
		return nil
	}

	resetToomossState()

	if err := loadDLLs(); err != nil {
		return err
	}

	if err := loadProcAddresses(); err != nil {
		if UsbDeviceDLL != 0 {
			_ = syscall.FreeLibrary(UsbDeviceDLL)
		}
		resetToomossState()
		return err
	}

	return nil
}

func archDLLDir() string {
	if runtime.GOARCH == "386" {
		return "windows_x86"
	}
	return ""
}

func loadDLLs() error {
	if UsbDeviceDLL != 0 {
		return nil
	}

	if runtime.GOARCH == "386" {
		if registryPath := getRegistryPath(); registryPath != "" {
			fmt.Println("Found registry path:", registryPath)
			libusbPath := filepath.Join(registryPath, "libusb-1.0.dll")
			if _, err := syscall.LoadLibrary(libusbPath); err != nil {
				fmt.Println("Warning: Failed to load libusb-1.0.dll from", libusbPath, "Error:", err)
			}

			usbPath := filepath.Join(registryPath, "USB2XXX.dll")
			if handle, err := syscall.LoadLibrary(usbPath); err == nil {
				UsbDeviceDLL = handle
				fmt.Println("Loaded DLLs from registry path:", registryPath)
				return nil
			} else {
				fmt.Println("Failed to load USB2XXX.dll from", usbPath, "Error:", err)
			}
		} else {
			fmt.Println("Registry path not found")
		}
	}

	dllDir := archDLLDir()
	libusbPath := filepath.Join(".\\bin", dllDir, "libusb-1.0.dll")
	if _, err := syscall.LoadLibrary(libusbPath); err != nil {
		log.Printf("Warning: failed to load libusb-1.0.dll from %s: %v", libusbPath, err)
	}

	usbPath := filepath.Join(".\\bin", dllDir, "USB2XXX.dll")
	handle, err := syscall.LoadLibrary(usbPath)
	if err != nil {
		return fmt.Errorf("failed to load USB2XXX.dll from %s: %w", usbPath, err)
	}
	UsbDeviceDLL = handle
	log.Printf("Loaded DLLs from default path: %s", usbPath)
	return nil
}

func getProc(name string) (uintptr, error) {
	addr, err := syscall.GetProcAddress(UsbDeviceDLL, name)
	if addr == 0 {
		if err == nil {
			err = errors.New("not found")
		}
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return addr, nil
}

func loadProcAddresses() error {
	if UsbDeviceDLL == 0 {
		return errors.New("USB2XXX.dll not loaded")
	}

	var errs []string
	var err error

	if UsbScanDevice, err = getProc("USB_ScanDevice"); err != nil {
		errs = append(errs, err.Error())
	}
	if UsbOpenDevice, err = getProc("USB_OpenDevice"); err != nil {
		errs = append(errs, err.Error())
	}
	if UsbCloseDevice, err = getProc("USB_CloseDevice"); err != nil {
		errs = append(errs, err.Error())
	}
	if LinExInit, err = getProc("LIN_EX_Init"); err != nil {
		errs = append(errs, err.Error())
	}
	if LinExMasterSync, err = getProc("LIN_EX_MasterSync"); err != nil {
		errs = append(errs, err.Error())
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func getRegistryPath() string {
	const uninstall = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`

	views := []struct {
		label  string
		access uint32
	}{
		{"64", registry.READ | registry.WOW64_64KEY},
		{"32", registry.READ | registry.WOW64_32KEY},
		{"default", registry.READ},
	}

	for _, view := range views {
		if path := findRegistryPathInView(uninstall, view.label, view.access); path != "" {
			return path
		}
	}

	return ""
}

func dirFromUninstallString(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.Trim(s, `"`)
	if i := strings.IndexByte(s, ' '); i > 0 {
		s = s[:i]
	}
	s = strings.Trim(s, `"`)
	if s == "" {
		return ""
	}
	return filepath.Dir(s)
}

func findRegistryPathInView(uninstall, label string, access uint32) string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, uninstall, access)
	if err != nil {
		fmt.Println("OpenKey HKLM", label, "view failed:", err)
		return ""
	}
	defer func(k registry.Key) {
		err := k.Close()
		if err != nil {

		}
	}(k)

	names, err := k.ReadSubKeyNames(-1)
	if err != nil {
		fmt.Println("ReadSubKeyNames failed:", err)
		return ""
	}

	fmt.Println("HKLM", label, "view entries:", len(names))

	for _, name := range names {
		sk, err := registry.OpenKey(registry.LOCAL_MACHINE, uninstall+`\`+name, access)
		if err != nil {
			continue
		}

		publisher, _, _ := sk.GetStringValue("Publisher")
		displayName, _, _ := sk.GetStringValue("DisplayName")
		install, _, _ := sk.GetStringValue("InstallLocation")
		appPath, _, _ := sk.GetStringValue("Inno Setup: App Path")
		unins, _, _ := sk.GetStringValue("UninstallString")
		err = sk.Close()
		if err != nil {
			return ""
		}

		pubL := strings.ToLower(strings.TrimSpace(publisher))
		dnL := strings.ToLower(strings.TrimSpace(displayName))

		if strings.Contains(pubL, "toomoss") || strings.Contains(dnL, "toomoss") {
			fmt.Println("Matched subkey:", name)
			fmt.Println("  DisplayName:", displayName)
			fmt.Println("  Publisher:", publisher)

			install = strings.TrimSpace(install)
			if install != "" {
				fmt.Println("  InstallLocation:", install)
				return filepath.Clean(install)
			}

			appPath = strings.TrimSpace(appPath)
			if appPath != "" {
				fmt.Println("  AppPath:", appPath)
				return filepath.Clean(appPath)
			}

			if dir := dirFromUninstallString(unins); dir != "" {
				fmt.Println("  From UninstallString:", dir)
				if hasUSB2XXXDLL(dir) {
					return dir
				}
				fmt.Println("  UninstallString path missing USB2XXX.dll")
			}

			fmt.Println("  No usable path fields")
		}
	}

	for _, name := range names {
		sk, err := registry.OpenKey(registry.LOCAL_MACHINE, uninstall+`\`+name, access)
		if err != nil {
			continue
		}

		install, _, _ := sk.GetStringValue("InstallLocation")
		appPath, _, _ := sk.GetStringValue("Inno Setup: App Path")
		unins, _, _ := sk.GetStringValue("UninstallString")
		err = sk.Close()
		if err != nil {
			return ""
		}

		install = strings.TrimSpace(install)
		if install != "" && pathLooksToomoss(install) {
			fmt.Println("Matched InstallLocation by path hint:", name)
			return filepath.Clean(install)
		}

		appPath = strings.TrimSpace(appPath)
		if appPath != "" && pathLooksToomoss(appPath) {
			fmt.Println("Matched AppPath by path hint:", name)
			return filepath.Clean(appPath)
		}

		if dir := dirFromUninstallString(unins); dir != "" && pathLooksToomoss(dir) {
			fmt.Println("Matched UninstallString by path hint:", name)
			return dir
		}
	}

	return ""
}

func hasUSB2XXXDLL(dir string) bool {
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "USB2XXX.dll"))
	return err == nil
}

func pathLooksToomoss(p string) bool {
	pl := strings.ToLower(p)
	return strings.Contains(pl, "toomoss") || strings.Contains(pl, "tcanlinpro")
}

func usbScan() (bool, error) {
	if UsbScanDevice == 0 {
		return false, errors.New("USB_ScanDevice not loaded")
	}
	ret, _, callErr := syscall.SyscallN(
		UsbScanDevice,
		uintptr(unsafe.Pointer(&DevHandle[DEVIndex])),
	)
	if callErr != 0 {
		return false, fmt.Errorf("USB_ScanDevice syscall failed: %w", callErr)
	}
	return ret > 0, nil
}

func UsbScan() bool {
	if err := ensureToomossLoaded(); err != nil {
		log.Printf("USB scan failed (load DLLs): %v", err)
		return false
	}
	ok, err := usbScan()
	if err != nil {
		log.Printf("USB scan failed: %v", err)
		return false
	}
	return ok
}

func usbOpen() (bool, error) {
	if UsbOpenDevice == 0 {
		return false, errors.New("USB_OpenDevice not loaded")
	}
	stateValue, _, callErr := syscall.SyscallN(
		UsbOpenDevice,
		uintptr(DevHandle[DEVIndex]),
	)
	if callErr != 0 {
		return false, fmt.Errorf("USB_OpenDevice syscall failed: %w", callErr)
	}
	return stateValue >= 1, nil
}

func UsbOpen() bool {
	if err := ensureToomossLoaded(); err != nil {
		log.Printf("USB open failed (load DLLs): %v", err)
		return false
	}
	ok, err := usbOpen()
	if err != nil {
		log.Printf("USB open failed: %v", err)
		return false
	}
	return ok
}

func usbClose() error {
	toomossMu.Lock()
	defer toomossMu.Unlock()

	if UsbDeviceDLL == 0 {
		return nil
	}
	if UsbCloseDevice == 0 {
		return errors.New("USB_CloseDevice not loaded")
	}
	ret, _, callErr := syscall.SyscallN(
		UsbCloseDevice,
		uintptr(DevHandle[DEVIndex]),
	)
	if callErr != 0 {
		return fmt.Errorf("USB_CloseDevice syscall failed: %w", callErr)
	}
	if ret < 1 {
		return fmt.Errorf("USB_CloseDevice returned %d", ret)
	}
	if err := syscall.FreeLibrary(UsbDeviceDLL); err != nil {
		return fmt.Errorf("FreeLibrary failed: %w", err)
	}
	resetToomossState()
	return nil
}

func ensureLinReady() error {
	if err := ensureToomossLoaded(); err != nil {
		return fmt.Errorf("load Toomoss LIN DLLs: %w", err)
	}
	return nil
}

func NewToomoss(channel ToomossCh) (*Toomoss, error) {
	if err := ensureLinReady(); err != nil {
		return nil, err
	}
	if ok := UsbScan(); !ok {
		return nil, fmt.Errorf("USB scan failed: device not found or DLL missing")
	}
	if ok := UsbOpen(); !ok {
		return nil, fmt.Errorf("USB open failed")
	}

	if tmsInit, ret, err := syscall.SyscallN(LinExInit, uintptr(DevHandle[DEVIndex]), uintptr(channel), uintptr(Bt), uintptr(Master)); tmsInit != 0 {
		return nil, fmt.Errorf("failed to initialize Toomoss LIN device: ret=%d, err=%v", ret, err)
	}

	log.Println("Toomoss LIN device initialized successfully.")

	return &Toomoss{
		eventChan: make(chan *liniface.LinEvent, 10),
		channel:   channel,
	}, nil
}

func (d *Toomoss) ReadEvent(timeout time.Duration) (*liniface.LinEvent, error) {
	select {
	case event := <-d.eventChan:
		return event, nil
	case <-time.After(timeout):
		return nil, nil
	}
}

func (d *Toomoss) WriteMessage(event *liniface.LinEvent) error {
	msg := make([]LinExMsg, 1)
	outMsg := make([]LinExMsg, 1)
	var payload [8]byte
	copy(payload[:], event.EventPayload)

	msg[0].MsgType = LIN_EX_MSG_TYPE_MW
	msg[0].DataLen = uint8(len(event.EventPayload))
	msg[0].PID = event.EventID
	msg[0].Data = payload
	if event.EventID == tplin.MasterDiagnosticFrameID || event.EventID == tplin.SlaveDiagnosticFrameID {
		msg[0].CheckType = LIN_EX_CHECK_STD
	} else {
		msg[0].CheckType = LIN_EX_CHECK_EXT
	}

	ret, _, err := syscall.SyscallN(LinExMasterSync, uintptr(DevHandle[DEVIndex]), uintptr(d.channel), uintptr(unsafe.Pointer(&msg[0])), uintptr(unsafe.Pointer(&outMsg[0])), uintptr(1))
	if ret <= 0 {
		return fmt.Errorf("toomoss LIN write failed: ret=%d, err=%v", ret, err)
	}
	log.Printf("TX LIN: ID=0x%02X, Len=%02d, CS=%02X, Data=% 02X", event.EventID, outMsg[0].DataLen, outMsg[0].Check, payload[:outMsg[0].DataLen])

	txEvent := *event
	txEvent.Direction = liniface.TX
	txEvent.Timestamp = time.Now()

	select {
	case d.eventChan <- &txEvent:
	default:
	}
	return nil
}

func (d *Toomoss) RequestSlaveResponse(frameID byte) error {
	msg := make([]LinExMsg, 1)
	outMsg := make([]LinExMsg, 1)
	msg[0].MsgType = LIN_EX_MSG_TYPE_MR
	msg[0].PID = frameID

	ret, _, _ := syscall.SyscallN(
		LinExMasterSync,
		uintptr(DevHandle[DEVIndex]),
		uintptr(d.channel),
		uintptr(unsafe.Pointer(&msg[0])),
		uintptr(unsafe.Pointer(&outMsg[0])),
		uintptr(1),
	)

	if ret <= 0 {
		log.Printf("RX : 0x%02X (No response from slave)", frameID)
		return nil
	}

	responseData := outMsg[0].Data
	dataLen := outMsg[0].DataLen
	if ret == 1 {
		log.Printf("RX LIN: ID=0x%02X, Len=%02d, CS=%02X, Data=% 02X", frameID, dataLen, outMsg[0].Check, responseData[:dataLen])
	}

	rxEvent := &liniface.LinEvent{
		EventID:      frameID,
		EventPayload: responseData[:dataLen],
		Direction:    liniface.RX,
		Timestamp:    time.Now(),
	}

	select {
	case d.eventChan <- rxEvent:
	default:
		return errors.New("toomoss event channel is full, discarding slave response")
	}
	return nil
}

func (d *Toomoss) ScheduleSlaveResponse(event *liniface.LinEvent) error {
	return errors.New("toomoss: ScheduleSlaveResponse is not supported in Master mode")
}

func (d *Toomoss) LinBreak() bool {
	LinBreak := make([]LinExMsg, 1)
	LINOutBreak := make([]LinExMsg, 1)
	LinBreak[0].MsgType = LIN_EX_MSG_TYPE_BK
	LinBreak[0].Timestamp = 20

	if sendNum, _, _ := syscall.SyscallN(
		LinExMasterSync,
		uintptr(DevHandle[DEVIndex]),
		uintptr(d.channel),
		uintptr(unsafe.Pointer(&LinBreak[0])),
		uintptr(unsafe.Pointer(&LINOutBreak[0])),
		uintptr(1),
	); sendNum <= 0 {
		log.Println("LIN break failed")
		return false
	}
	return true
}
