//go:build windows

package driver

import (
	"errors"
	"fmt"
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
	UsbDeviceDLL      syscall.Handle
	UsbScanDevice     uintptr
	UsbOpenDevice     uintptr
	UsbCloseDevice    uintptr
	LinExInit         uintptr
	LinExMasterSync   uintptr
	LinEXSlaveGetData uintptr
	DevHandle         [10]int
	DEVIndex          = 0

	toomossMu             sync.Mutex
	toomossInstanceMu     sync.Mutex
	toomossInstanceActive bool
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
type ToomossCh = liniface.Channel

const (
	CH1 ToomossCh = iota
	CH2
	CH3
	CH4
)

var (
	Bt        uint = 19200
	Master    byte = 1
	SlaveMode byte = 0
)

type Toomoss struct {
	callMu     sync.Mutex
	stateMu    sync.RWMutex
	closeOnce  sync.Once
	closed     bool
	channels   map[liniface.Channel]struct{}
	eventMu    sync.Mutex
	eventChans map[liniface.Channel]chan *liniface.LinEvent
}

var _ liniface.Driver = (*Toomoss)(nil)
var _ liniface.MasterReader = (*Toomoss)(nil)

func toomossReady() bool {
	return UsbDeviceDLL != 0 &&
		UsbScanDevice != 0 &&
		UsbOpenDevice != 0 &&
		UsbCloseDevice != 0 &&
		LinExInit != 0 &&
		LinExMasterSync != 0 &&
		LinEXSlaveGetData != 0
}

func resetToomossState() {
	UsbDeviceDLL = 0
	UsbScanDevice = 0
	UsbOpenDevice = 0
	UsbCloseDevice = 0
	LinExInit = 0
	LinExMasterSync = 0
	LinEXSlaveGetData = 0
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
			logDriverf(logDeviceToomoss, "registry_path=%s", registryPath)
			libusbPath := filepath.Join(registryPath, "libusb-1.0.dll")
			if _, err := syscall.LoadLibrary(libusbPath); err != nil {
				logDriverf(logDeviceToomoss, "library=libusb-1.0.dll path=%s status=load_failed error=%v", libusbPath, err)
			}

			usbPath := filepath.Join(registryPath, "USB2XXX.dll")
			if handle, err := syscall.LoadLibrary(usbPath); err == nil {
				UsbDeviceDLL = handle
				logDriverf(logDeviceToomoss, "library=USB2XXX.dll path=%s status=loaded", usbPath)
				return nil
			} else {
				logDriverf(logDeviceToomoss, "library=USB2XXX.dll path=%s status=load_failed error=%v", usbPath, err)
			}
		} else {
			logDriverf(logDeviceToomoss, "registry_path status=not_found")
		}
	}

	dllDir := archDLLDir()
	libusbPath := filepath.Join(".\\bin", dllDir, "libusb-1.0.dll")
	if _, err := syscall.LoadLibrary(libusbPath); err != nil {
		logDriverf(logDeviceToomoss, "library=libusb-1.0.dll path=%s status=load_failed error=%v", libusbPath, err)
	}

	usbPath := filepath.Join(".\\bin", dllDir, "USB2XXX.dll")
	handle, err := syscall.LoadLibrary(usbPath)
	if err != nil {
		return fmt.Errorf("failed to load USB2XXX.dll from %s: %w", usbPath, err)
	}
	UsbDeviceDLL = handle
	logDriverf(logDeviceToomoss, "library=USB2XXX.dll path=%s status=loaded", usbPath)
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
	if LinEXSlaveGetData, err = getProc("LIN_EX_SlaveGetData"); err != nil {
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
		logDriverf(logDeviceToomoss, "registry_view=%s status=open_failed error=%v", label, err)
		return ""
	}
	defer func(k registry.Key) {
		err := k.Close()
		if err != nil {

		}
	}(k)

	names, err := k.ReadSubKeyNames(-1)
	if err != nil {
		logDriverf(logDeviceToomoss, "registry_view=%s status=read_failed error=%v", label, err)
		return ""
	}

	logDriverf(logDeviceToomoss, "registry_view=%s entries=%d", label, len(names))

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
			logDriverf(logDeviceToomoss, "registry_match subkey=%s display_name=%q publisher=%q", name, displayName, publisher)

			install = strings.TrimSpace(install)
			if install != "" {
				logDriverf(logDeviceToomoss, "registry_match source=install_location path=%s", install)
				return filepath.Clean(install)
			}

			appPath = strings.TrimSpace(appPath)
			if appPath != "" {
				logDriverf(logDeviceToomoss, "registry_match source=app_path path=%s", appPath)
				return filepath.Clean(appPath)
			}

			if dir := dirFromUninstallString(unins); dir != "" {
				logDriverf(logDeviceToomoss, "registry_match source=uninstall_string path=%s", dir)
				if hasUSB2XXXDLL(dir) {
					return dir
				}
			}

			logDriverf(logDeviceToomoss, "registry_match subkey=%s status=no_usable_path", name)
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
			logDriverf(logDeviceToomoss, "registry_hint subkey=%s source=install_location path=%s", name, install)
			return filepath.Clean(install)
		}

		appPath = strings.TrimSpace(appPath)
		if appPath != "" && pathLooksToomoss(appPath) {
			logDriverf(logDeviceToomoss, "registry_hint subkey=%s source=app_path path=%s", name, appPath)
			return filepath.Clean(appPath)
		}

		if dir := dirFromUninstallString(unins); dir != "" && pathLooksToomoss(dir) {
			logDriverf(logDeviceToomoss, "registry_hint subkey=%s source=uninstall_string path=%s", name, dir)
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
		logDriverf(logDeviceToomoss, "usb_scan status=failed error=%v", err)
		return false
	}
	ok, err := usbScan()
	if err != nil {
		logDriverf(logDeviceToomoss, "usb_scan status=failed error=%v", err)
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
		logDriverf(logDeviceToomoss, "usb_open status=failed error=%v", err)
		return false
	}
	ok, err := usbOpen()
	if err != nil {
		logDriverf(logDeviceToomoss, "usb_open status=failed error=%v", err)
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

func NewToomoss(channel []ToomossCh, mode byte) (*Toomoss, error) {
	toomossInstanceMu.Lock()
	defer toomossInstanceMu.Unlock()
	if toomossInstanceActive {
		return nil, errors.New("a Toomoss device instance is already active; configure all LIN channels on that instance")
	}
	if len(channel) == 0 {
		return nil, errors.New("at least one LIN channel is required")
	}
	if err := ensureLinReady(); err != nil {
		return nil, err
	}
	if ok := UsbScan(); !ok {
		return nil, fmt.Errorf("USB scan failed: device not found or DLL missing")
	}
	if ok := UsbOpen(); !ok {
		return nil, fmt.Errorf("USB open failed")
	}
	for _, ch := range channel {
		if tmsInit, ret, err := syscall.SyscallN(LinExInit, uintptr(DevHandle[DEVIndex]), uintptr(ch), uintptr(Bt), uintptr(mode)); tmsInit != 0 {
			_ = usbClose()
			return nil, fmt.Errorf("failed to initialize Toomoss LIN device: ret=%d, err=%v", ret, err)
		}
	}
	logDriverf(logDeviceToomoss, "initialized channels=%v mode=%d baudrate=%d", channel, mode, Bt)

	initializedChannels := make(map[liniface.Channel]struct{}, len(channel))
	for _, ch := range channel {
		initializedChannels[ch] = struct{}{}
	}
	toomossInstanceActive = true
	return &Toomoss{
		channels:   initializedChannels,
		eventChans: make(map[liniface.Channel]chan *liniface.LinEvent),
	}, nil
}

func (d *Toomoss) LinMasterSync(msg, outMsg []LinExMsg, channel ToomossCh) (uintptr, error) {
	if len(outMsg) == 0 || len(msg) == 0 {
		return 0, fmt.Errorf("LinMasterSync called with empty outMsg")
	}
	if len(msg) != len(outMsg) {
		return 0, fmt.Errorf("LinMasterSync: len(msg) != len(outMsg)")
	}
	d.callMu.Lock()
	defer d.callMu.Unlock()
	if err := d.validateChannel(channel); err != nil {
		return 0, err
	}
	ret, _, err := syscall.SyscallN(
		LinExMasterSync,
		uintptr(DevHandle[DEVIndex]),
		uintptr(channel),
		uintptr(unsafe.Pointer(&msg[0])),
		uintptr(unsafe.Pointer(&outMsg[0])),
		uintptr(len(msg)),
	)
	return ret, err
}

func (d *Toomoss) ReadEvent(timeout time.Duration, channel liniface.Channel) (*liniface.LinEvent, error) {
	if err := d.validateChannel(channel); err != nil {
		return nil, err
	}
	eventChan := d.eventChannel(channel)
	deadline := time.Now().Add(timeout)
	for {
		select {
		case event := <-eventChan:
			return event, nil
		default:
		}
		messages, err := d.LinExSlaveGetData(channel)
		if err != nil {
			return nil, err
		}
		if len(messages) > 0 {
			for i := 1; i < len(messages); i++ {
				select {
				case eventChan <- toomossEvent(messages[i], channel):
				default:
				}
			}
			return toomossEvent(messages[0], channel), nil
		}
		if timeout <= 0 || !time.Now().Before(deadline) {
			return nil, nil
		}
		time.Sleep(time.Millisecond)
	}
}

func toomossEvent(message LinExMsg, channel liniface.Channel) *liniface.LinEvent {
	dataLen := min(int(message.DataLen), len(message.Data))
	payload := append([]byte(nil), message.Data[:dataLen]...)
	direction := liniface.RX
	if message.MsgType == LIN_EX_MSG_TYPE_SW {
		direction = liniface.TX
	}
	checksumType := liniface.EnhancedChecksum
	if message.CheckType == LIN_EX_CHECK_STD {
		checksumType = liniface.ClassicChecksum
	}
	return &liniface.LinEvent{
		Channel:      channel,
		EventID:      message.PID & 0x3F,
		EventPayload: payload,
		ChecksumType: checksumType,
		Direction:    direction,
		Timestamp:    time.Now(),
	}
}

func (d *Toomoss) WriteMessage(event *liniface.LinEvent, channel liniface.Channel) error {
	if event == nil {
		return errors.New("nil LIN event")
	}
	if len(event.EventPayload) > 8 {
		return fmt.Errorf("invalid LIN payload length %d (max 8)", len(event.EventPayload))
	}
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

	ret, err := d.LinMasterSync(msg, outMsg, channel)
	if ret <= 0 {
		return fmt.Errorf("toomoss LIN write failed: ret=%d, err=%v", ret, err)
	}
	logLINMessage(logDeviceToomoss, "TX", channel, event.EventID, outMsg[0].Check, payload[:outMsg[0].DataLen])
	txEvent := *event
	txEvent.EventPayload = append([]byte(nil), event.EventPayload...)
	txEvent.Channel = channel
	txEvent.Direction = liniface.TX
	txEvent.Timestamp = time.Now()

	select {
	case d.eventChannel(channel) <- &txEvent:
	default:
		logDriverf(logDeviceToomoss, "queue_overflow channel=%d id=0x%02X action=drop", channel, event.EventID)
	}
	return nil
}

func (d *Toomoss) MasterWrite(frameID byte, data []byte, channel ToomossCh) error {
	if len(data) > 8 {
		return fmt.Errorf("toomoss MasterWrite: data length %d exceeds 8", len(data))
	}

	msg := make([]LinExMsg, 1)
	outMsg := make([]LinExMsg, 1)
	var payload [8]byte
	copy(payload[:], data)

	msg[0].MsgType = LIN_EX_MSG_TYPE_MW
	msg[0].DataLen = uint8(len(data))
	msg[0].PID = frameID
	msg[0].Data = payload
	if frameID == tplin.MasterDiagnosticFrameID || frameID == tplin.SlaveDiagnosticFrameID {
		msg[0].CheckType = LIN_EX_CHECK_STD
	} else {
		msg[0].CheckType = LIN_EX_CHECK_EXT
	}
	ret, err := d.LinMasterSync(msg, outMsg, channel)

	if ret <= 0 {
		return fmt.Errorf("toomoss LIN write failed: ret=%d, err=%v", ret, err)
	}
	logLINMessage(logDeviceToomoss, "TX", liniface.Channel(channel), frameID, outMsg[0].Check, payload[:outMsg[0].DataLen])
	return nil
}

// MasterRead performs a synchronous master-header request and returns the slave
// payload on channel. The returned payload is owned by the caller. It must not
// run concurrently with another receive consumer on the same channel.
func (d *Toomoss) MasterRead(frameID byte, channel ToomossCh) ([]byte, error) {
	if frameID > 0x3F {
		return nil, fmt.Errorf("invalid LIN frame ID 0x%02X", frameID)
	}
	msg := make([]LinExMsg, 1)
	outMsg := make([]LinExMsg, 1)
	msg[0].MsgType = LIN_EX_MSG_TYPE_MR
	msg[0].PID = frameID
	ret, _ := d.LinMasterSync(msg, outMsg, channel)

	if ret <= 0 {
		return nil, ErrNoSlaveResponse
	}

	dataLen := int(outMsg[0].DataLen)
	if dataLen > len(outMsg[0].Data) {
		dataLen = len(outMsg[0].Data)
	}
	result := make([]byte, dataLen)
	copy(result, outMsg[0].Data[:dataLen])
	logLINMessage(logDeviceToomoss, "RX", liniface.Channel(channel), frameID, outMsg[0].Check, outMsg[0].Data[:dataLen])
	return result, nil
}

func (d *Toomoss) RequestSlaveResponse(frameID byte, channel liniface.Channel) error {
	msg := make([]LinExMsg, 1)
	outMsg := make([]LinExMsg, 1)
	msg[0].MsgType = LIN_EX_MSG_TYPE_MR
	msg[0].PID = frameID
	ret, _ := d.LinMasterSync(msg, outMsg, channel)

	if ret <= 0 {
		logLINNoResponse(logDeviceToomoss, channel, frameID)
		return nil
	}

	responseData := outMsg[0].Data
	dataLen := outMsg[0].DataLen
	if int(dataLen) > len(responseData) {
		dataLen = byte(len(responseData))
	}
	if dataLen == 0 {
		logLINNoResponse(logDeviceToomoss, channel, frameID)
		return nil
	}
	if ret == 1 {
		logLINMessage(logDeviceToomoss, "RX", channel, frameID, outMsg[0].Check, responseData[:dataLen])
	}

	rxEvent := &liniface.LinEvent{
		Channel:      channel,
		EventID:      frameID,
		EventPayload: responseData[:dataLen],
		Direction:    liniface.RX,
		Timestamp:    time.Now(),
	}

	select {
	case d.eventChannel(channel) <- rxEvent:
	default:
		return errors.New("toomoss event channel is full, discarding slave response")
	}
	return nil
}

func (d *Toomoss) ScheduleSlaveResponse(event *liniface.LinEvent, channel liniface.Channel) error {
	return errors.New("toomoss: ScheduleSlaveResponse is not supported in Master mode")
}

func (d *Toomoss) eventChannel(channel liniface.Channel) chan *liniface.LinEvent {
	d.eventMu.Lock()
	defer d.eventMu.Unlock()
	eventChan := d.eventChans[channel]
	if eventChan == nil {
		eventChan = make(chan *liniface.LinEvent, 10)
		d.eventChans[channel] = eventChan
	}
	return eventChan
}

// Close releases the USB adapter and loaded driver library.
func (d *Toomoss) Close() error {
	if d == nil {
		return nil
	}
	var closeErr error
	d.closeOnce.Do(func() {
		d.stateMu.Lock()
		d.closed = true
		d.stateMu.Unlock()
		d.callMu.Lock()
		closeErr = usbClose()
		d.callMu.Unlock()
		if closeErr != nil {
			logDriverf(logDeviceToomoss, "disconnect status=failed error=%v", closeErr)
		} else {
			logDriverf(logDeviceToomoss, "disconnected")
		}
		toomossInstanceMu.Lock()
		toomossInstanceActive = false
		toomossInstanceMu.Unlock()
	})
	return closeErr
}

func (d *Toomoss) LinBreak(channel ToomossCh) error {
	msg := make([]LinExMsg, 1)
	outMsg := make([]LinExMsg, 1)
	msg[0].MsgType = LIN_EX_MSG_TYPE_BK
	msg[0].Timestamp = 20
	if ret, _ := d.LinMasterSync(msg, outMsg, channel); ret <= 0 {
		return errors.New("LIN break failed")
	}
	return nil
}

const linExSlaveGetDataMaxFrames = 512

func (d *Toomoss) LinExSlaveGetData(channel ToomossCh) ([]LinExMsg, error) {
	if LinEXSlaveGetData == 0 {
		return nil, errors.New("LIN_EX_SlaveGetData not loaded")
	}

	linMsgs := make([]LinExMsg, linExSlaveGetDataMaxFrames)
	d.callMu.Lock()
	if err := d.validateChannel(channel); err != nil {
		d.callMu.Unlock()
		return nil, err
	}
	ret, _, callErr := syscall.SyscallN(
		LinEXSlaveGetData,
		uintptr(DevHandle[DEVIndex]),
		uintptr(channel),
		uintptr(unsafe.Pointer(&linMsgs[0])),
	)
	d.callMu.Unlock()
	if callErr != 0 {
		return nil, fmt.Errorf("LIN_EX_SlaveGetData syscall failed: %w", callErr)
	}
	if int(ret) < 0 {
		return nil, fmt.Errorf("LIN_EX_SlaveGetData failed: ret=%d", int(ret))
	}

	count := int(ret)
	if count > len(linMsgs) {
		count = len(linMsgs)
	}
	return linMsgs[:count], nil
}

func (d *Toomoss) validateChannel(channel liniface.Channel) error {
	if d == nil {
		return liniface.ErrDriverClosed
	}
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	if d.closed {
		return liniface.ErrDriverClosed
	}
	if _, ok := d.channels[channel]; !ok {
		return fmt.Errorf("%w: %d", liniface.ErrInvalidChannel, channel)
	}
	return nil
}
