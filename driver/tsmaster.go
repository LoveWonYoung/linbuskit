//go:build windows

package driver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/LoveWonYoung/linbuskit/liniface"
	"github.com/LoveWonYoung/linbuskit/tplin"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	// TLIBBusToolDeviceType: TS_USB_DEVICE
	tsUSBDevice = 3
	// TLIBApplicationChannelType: APP_CAN=0, APP_LIN=1
	appChannelTypeLIN = 1
	// TLINProtocol: LIN_Protocol_13=0, LIN_Protocol_20=1, LIN_Protocol_21=2
	linProtocol21 = 2
	// TLIN_FUNCTION_TYPE: MasterNode=0
	linNodeMaster = 0
	// tsfifo_receive_lin_msgs AIncludeTx: 0=RX only, 1=TX+RX
	linFIFOIncludeTxRx = 1

	linPropertyDirTxMask = 0x01
	linPropertyBreakMask = 0x02

	masterReadTimeout = 100 * time.Millisecond
)

var defaultTSMasterChannels = []uint32{0}

type tsmasterLoader struct {
	dll     *syscall.LazyDLL
	dllPath string
}

const (
	TS_UNKNOWN_DEVICE = iota
	TSCAN_PRO
	TSCAN_Lite1
	TC1001
	TL1001
	TC1011
	TM5011
	TC1002
	TC1014
	TSCANFD2517
	TC1026
	TC1016
	TC1012
	TC1013
	TLog1002
	TC1034
	TC1018
	GW2116
	TC2115
	MP1013
	TC1113
	TC1114
	TP1013
	TC1017
	TP1018
	TF10XX
	TL1004_FD_4_LIN_2
	TE1051
	TP1051
	TP1034
	TTS9015
	TP1026
	TTS1026
	TTS1034
	TTS1018
	TL1011
	TTS1015_LiAuto
	TTS1013_LiAuto
	TTS1016Pro
	TC1054Pro
	TC1054
	TLog1038
	TO1013
	TC1034Pro
	TC1018Pro
	TC1038Pro
	TC1014Pro
	TC1034ProPlus
	TA1038
	TC1055Pro
	TC1056Pro
	TC1057Pro
	TC4016
	GW2208
	TLog1039
	GW1040
	TC3014
	TP1014
	TA825_4
	TC1013HV
	TC1052
	TTS1017Pro
	TLog1057
	TC1017Pro
	GW2202
	GW2204
	GW2212
	TA821
	TX1000
	TC1055ProPlus
	TC1043
	TS_DEV_END
)

// TSMasterMap 设备编号对照表
var TSMasterMap = map[string]int{
	"TS_UNKNOWN_DEVICE": TS_UNKNOWN_DEVICE,
	"TSCAN_PRO":         TSCAN_PRO,
	"TSCAN_Lite1":       TSCAN_Lite1,
	"TC1001":            TC1001,
	"TL1001":            TL1001,
	"TC1011":            TC1011,
	"TM5011":            TM5011,
	"TC1002":            TC1002,
	"TC1014":            TC1014,
	"TSCANFD2517":       TSCANFD2517,
	"TC1026":            TC1026,
	"TC1016":            TC1016,
	"TC1012":            TC1012,
	"TC1013":            TC1013,
	"TLog1002":          TLog1002,
	"TC1034":            TC1034,
	"TC1018":            TC1018,
	"GW2116":            GW2116,
	"TC2115":            TC2115,
	"MP1013":            MP1013,
	"TC1113":            TC1113,
	"TC1114":            TC1114,
	"TP1013":            TP1013,
	"TC1017":            TC1017,
	"TP1018":            TP1018,
	"TF10XX":            TF10XX,
	"TL1004_FD_4_LIN_2": TL1004_FD_4_LIN_2,
	"TE1051":            TE1051,
	"TP1051":            TP1051,
	"TP1034":            TP1034,
	"TTS9015":           TTS9015,
	"TP1026":            TP1026,
	"TTS1026":           TTS1026,
	"TTS1034":           TTS1034,
	"TTS1018":           TTS1018,
	"TL1011":            TL1011,
	"TTS1015_LiAuto":    TTS1015_LiAuto,
	"TTS1013_LiAuto":    TTS1013_LiAuto,
	"TTS1016Pro":        TTS1016Pro,
	"TC1054Pro":         TC1054Pro,
	"TC1054":            TC1054,
	"TLog1038":          TLog1038,
	"TO1013":            TO1013,
	"TC1034Pro":         TC1034Pro,
	"TC1018Pro":         TC1018Pro,
	"TC1038Pro":         TC1038Pro,
	"TC1014Pro":         TC1014Pro,
	"TC1034ProPlus":     TC1034ProPlus,
	"TA1038":            TA1038,
	"TC1055Pro":         TC1055Pro,
	"TC1056Pro":         TC1056Pro,
	"TC1057Pro":         TC1057Pro,
	"TC4016":            TC4016,
	"GW2208":            GW2208,
	"TLog1039":          TLog1039,
	"GW1040":            GW1040,
	"TC3014":            TC3014,
	"TP1014":            TP1014,
	"TA825_4":           TA825_4,
	"TC1013HV":          TC1013HV,
	"TC1052":            TC1052,
	"TTS1017Pro":        TTS1017Pro,
	"TLog1057":          TLog1057,
	"TC1017Pro":         TC1017Pro,
	"GW2202":            GW2202,
	"GW2204":            GW2204,
	"GW2212":            GW2212,
	"TA821":             TA821,
	"TX1000":            TX1000,
	"TC1055ProPlus":     TC1055ProPlus,
	"TC1043":            TC1043,
	"TS_DEV_END":        TS_DEV_END,
}

// deviceNameFromType 根据设备编号反查设备名称
func deviceNameFromType(deviceType int) (string, error) {
	for name, id := range TSMasterMap {
		if id == deviceType && name != "TS_UNKNOWN_DEVICE" && name != "TS_DEV_END" {
			return name, nil
		}
	}
	return "", fmt.Errorf("unsupported TSMaster device type: %d", deviceType)
}
func newTSMasterLoader() (*tsmasterLoader, error) {
	loader := &tsmasterLoader{}

	dllPath, err := getTSMasterDLLFromRegistry()
	if err != nil {
		return nil, fmt.Errorf("failed to get TSMaster DLL path from registry: %w", err)
	}
	if dllPath == "" {
		return nil, fmt.Errorf("TSMaster DLL path from registry is empty")
	}

	// libTSMaster 依赖同目录其它 DLL；先切搜索目录再 Load，否则会报 module not found。
	if err := windows.SetDllDirectory(filepath.Dir(dllPath)); err != nil {
		return nil, fmt.Errorf("failed to set TSMaster DLL directory: %w", err)
	}

	loader.dllPath = dllPath
	loader.dll = syscall.NewLazyDLL(dllPath)
	if err := loader.dll.Load(); err != nil {
		return nil, fmt.Errorf("failed to load TSMaster.dll: %w", err)
	}

	return loader, nil
}

func getTSMasterDLLFromRegistry() (string, error) {
	regPath := `Software\TOSUN\TSMaster`
	key, err := registry.OpenKey(
		registry.LOCAL_MACHINE,
		regPath,
		registry.QUERY_VALUE|registry.WOW64_32KEY,
	)
	if err != nil {
		return "", err
	}
	defer key.Close()

	keyName := "libTSMaster_x64"
	if runtime.GOARCH == "386" {
		keyName = "libTSMaster_x86"
	}

	value, _, err := key.GetStringValue(keyName)
	return value, err
}

func (l *tsmasterLoader) proc(name string) *syscall.LazyProc {
	if l.dll == nil {
		return nil
	}
	return l.dll.NewProc(name)
}

func (l *tsmasterLoader) close() {
	l.dll = nil
}

// 注意：TSMaster C侧使用pack(1)，这里用[8]byte存时间戳避免Go插入填充导致字段错位。
type tsmasterLIN struct {
	FIdxChn     uint8
	FErrStatus  uint8
	FProperties uint8
	FDLC        uint8
	FIdentifier uint8
	FChecksum   uint8
	FStatus     uint8
	FTimeUs     [8]byte
	FData       [8]uint8
}

type TSMaster struct {
	mu        sync.Mutex
	callMu    sync.Mutex
	closeOnce sync.Once

	loader     *tsmasterLoader
	channels   []uint32
	eventMu    sync.Mutex
	eventChans map[liniface.Channel]chan *liniface.LinEvent
	errCh      chan error

	transmitProc *syscall.LazyProc
	receiveProc  *syscall.LazyProc
	linCb        uintptr // tsapp_register_event_lin stdcall 回调，必须挂在结构体上防 GC

	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	connected  bool
	deviceType int

	dedupMu   sync.Mutex
	lastFrame tsmasterLastFrame
}

type tsmasterLastFrame struct {
	valid bool
	ch    liniface.Channel
	id    byte
	dir   liniface.Direction
	n     int
	data  [8]byte
	at    time.Time
}

var _ liniface.Driver = (*TSMaster)(nil)

func NewTSMaster(deviceType int, channels ...liniface.Channel) (*TSMaster, error) {
	ctx, cancel := context.WithCancel(context.Background())
	configuredChannels := append([]uint32(nil), defaultTSMasterChannels...)
	if len(channels) > 0 {
		configuredChannels = make([]uint32, len(channels))
		for i, channel := range channels {
			configuredChannels[i] = uint32(channel)
		}
	}
	t := &TSMaster{
		channels:   configuredChannels,
		eventChans: make(map[liniface.Channel]chan *liniface.LinEvent),
		errCh:      make(chan error, 32),
		ctx:        ctx,
		cancel:     cancel,
		deviceType: deviceType,
	}

	if err := t.open(); err != nil {
		t.Close()
		return nil, err
	}

	return t, nil
}

func (t *TSMaster) Errors() <-chan error {
	if t == nil {
		return nil
	}
	return t.errCh
}

func (t *TSMaster) open() error {
	loader, err := newTSMasterLoader()
	if err != nil {
		return err
	}
	t.loader = loader

	if err := t.callInitProcedures(); err != nil {
		return err
	}

	t.transmitProc = t.loader.proc("tsapp_transmit_lin_async")
	if t.transmitProc == nil {
		return errors.New("tsapp_transmit_lin_async not found")
	}
	if err := t.transmitProc.Find(); err != nil {
		return fmt.Errorf("tsapp_transmit_lin_async not found: %w", err)
	}

	t.receiveProc = t.loader.proc("tsfifo_receive_lin_msgs")
	if t.receiveProc == nil {
		return errors.New("tsfifo_receive_lin_msgs not found")
	}

	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		t.receiveLoop()
	}()

	return nil
}

func (t *TSMaster) callInitProcedures() error {
	initProc := t.loader.proc("initialize_lib_tsmaster")
	if initProc == nil {
		return errors.New("initialize_lib_tsmaster not found")
	}
	appName, _ := syscall.BytePtrFromString("linbuskit")
	r, _, _ := initProc.Call(uintptr(unsafe.Pointer(appName)))
	if r != 0 {
		return fmt.Errorf("initialize_lib_tsmaster failed: %d", r)
	}

	enumProc := t.loader.proc("tsapp_enumerate_hw_devices")
	if enumProc == nil {
		return errors.New("tsapp_enumerate_hw_devices not found")
	}
	var deviceCount int32
	r, _, _ = enumProc.Call(uintptr(unsafe.Pointer(&deviceCount)))
	if r != 0 {
		return fmt.Errorf("tsapp_enumerate_hw_devices failed: %d", r)
	}
	if deviceCount <= 0 {
		return errors.New("no TSMaster devices found")
	}

	showWindowProc := t.loader.proc("tsapp_show_tsmaster_window")
	if showWindowProc == nil {
		return errors.New("tsapp_show_tsmaster_window not found")
	}
	hardwareName, _ := syscall.BytePtrFromString("Hardware")
	r, _, _ = showWindowProc.Call(uintptr(unsafe.Pointer(hardwareName)), uintptr(1))
	if r != 0 {
		return fmt.Errorf("tsapp_show_tsmaster_window failed: %d", r)
	}

	if setCANCountProc := t.loader.proc("tsapp_set_can_channel_count"); setCANCountProc != nil {
		if r, _, _ = setCANCountProc.Call(0); r != 0 {
			return fmt.Errorf("tsapp_set_can_channel_count failed: %d", r)
		}
	}

	setCountProc := t.loader.proc("tsapp_set_lin_channel_count")
	if setCountProc == nil {
		return errors.New("tsapp_set_lin_channel_count not found")
	}
	r, _, _ = setCountProc.Call(uintptr(len(t.channels)))
	if r != 0 {
		return fmt.Errorf("tsapp_set_lin_channel_count failed: %d", r)
	}

	setMappingProc := t.loader.proc("tsapp_set_mapping_verbose")
	if setMappingProc == nil {
		return errors.New("tsapp_set_mapping_verbose not found")
	}
	configureBaudProc := t.loader.proc("tsapp_configure_baudrate_lin")
	if configureBaudProc == nil {
		return errors.New("tsapp_configure_baudrate_lin not found")
	}

	devName, err := deviceNameFromType(t.deviceType)
	if err != nil {
		return err
	}
	deviceName, _ := syscall.BytePtrFromString(devName)
	for _, ch := range t.channels {
		r, _, _ = setMappingProc.Call(
			uintptr(unsafe.Pointer(appName)),
			uintptr(appChannelTypeLIN),
			uintptr(ch),
			uintptr(unsafe.Pointer(deviceName)),
			uintptr(tsUSBDevice),
			uintptr(t.deviceType),
			uintptr(0),
			uintptr(ch),
			uintptr(1),
		)
		if r != 0 {
			return fmt.Errorf("tsapp_set_mapping_verbose failed for channel %d: %d", ch, r)
		}

		r, _, _ = configureBaudProc.Call(
			uintptr(ch),
			uintptr(math.Float32bits(float32(19.2))),
			uintptr(linProtocol21),
		)
		if r != 0 {
			return fmt.Errorf("tsapp_configure_baudrate_lin failed for channel %d: %d", ch, r)
		}
	}

	connectProc := t.loader.proc("tsapp_connect")
	if connectProc == nil {
		return errors.New("tsapp_connect not found")
	}
	r, _, _ = connectProc.Call()
	if r != 0 {
		return fmt.Errorf("tsapp_connect failed: %d", r)
	}
	t.connected = true

	if enableFIFOProc := t.loader.proc("tsfifo_enable_receive_fifo"); enableFIFOProc == nil {
		return errors.New("tsfifo_enable_receive_fifo not found")
	} else {
		enableFIFOProc.Call()
	}

	// 官方 C# LIN demo：connect 之后立刻 register_event_lin，收包走回调。
	if err := t.registerLinEvent(); err != nil {
		log.Printf("tsapp_register_event_lin: %v (fallback to FIFO)", err)
	}

	// 必须在 connect 之后设置，否则返回 81（通道尚未连接到硬件）。
	nodeTypeProc := t.loader.proc("tslin_set_node_functiontype")
	if nodeTypeProc == nil {
		return errors.New("tslin_set_node_functiontype not found")
	}
	for _, ch := range t.channels {
		r, _, _ = nodeTypeProc.Call(uintptr(ch), uintptr(linNodeMaster))
		if r != 0 {
			return fmt.Errorf("tslin_set_node_functiontype failed for channel %d: %d", ch, r)
		}
	}

	return nil
}

func (t *TSMaster) registerLinEvent() error {
	p := t.loader.proc("tsapp_register_event_lin")
	if p == nil {
		return errors.New("tsapp_register_event_lin not found")
	}
	self := t
	t.linCb = syscall.NewCallback(func(obj, pMsg uintptr) uintptr {
		self.onLinEvent(pMsg)
		return 0
	})
	r, _, _ := p.Call(0, t.linCb)
	if r != 0 {
		t.linCb = 0
		return fmt.Errorf("failed: %d (%s)", r, t.errorText(r))
	}
	return nil
}

func (t *TSMaster) onLinEvent(pMsg uintptr) {
	if pMsg == 0 {
		return
	}
	msg := *(*tsmasterLIN)(unsafe.Pointer(pMsg))
	t.dispatchLIN(msg, liniface.Channel(msg.FIdxChn))
}

func (t *TSMaster) receiveLoop() {
	const rxBufferCapacity = 64
	linBuffer := make([]tsmasterLIN, rxBufferCapacity)

	for {
		select {
		case <-t.ctx.Done():
			return
		default:
		}

		hasFrames := false
		for _, ch := range t.channels {
			bufferSize := int32(len(linBuffer))
			t.callMu.Lock()
			r, _, _ := t.receiveProc.Call(
				uintptr(unsafe.Pointer(&linBuffer[0])),
				uintptr(unsafe.Pointer(&bufferSize)),
				uintptr(ch),
				uintptr(linFIFOIncludeTxRx),
			)
			t.callMu.Unlock()
			if r != 0 {
				t.pushError(fmt.Errorf("tsfifo_receive_lin_msgs failed for channel %d: %d", ch, r))
				continue
			}
			if bufferSize <= 0 {
				continue
			}
			hasFrames = true

			if bufferSize > int32(len(linBuffer)) {
				bufferSize = int32(len(linBuffer))
			}

			for i := 0; i < int(bufferSize); i++ {
				t.dispatchLIN(linBuffer[i], liniface.Channel(ch))
			}
		}

		if !hasFrames {
			time.Sleep(2 * time.Millisecond)
		}
	}
}

func (t *TSMaster) dispatchLIN(msg tsmasterLIN, channel liniface.Channel) {
	dlc := int(msg.FDLC)
	if dlc < 0 {
		dlc = 0
	}
	if dlc > len(msg.FData) {
		dlc = len(msg.FData)
	}

	payload := make([]byte, dlc)
	copy(payload, msg.FData[:dlc])

	id := msg.FIdentifier & 0x3F
	// 0x3E/0x3F 为 LIN 保留 ID。未接 VBAT 时收发器会把总线噪声报成
	// 固定内容的 0x3F 假帧，不能当成 slave 诊断应答。
	if id >= 0x3E {
		return
	}

	markedTx := msg.FProperties&linPropertyDirTxMask != 0
	// Master header-only request (0x3D) may come back with TX bit set
	// even though the data bytes are from the slave.
	slaveResponse := id == tplin.SlaveDiagnosticFrameID && dlc > 0 && msg.FErrStatus == 0
	direction := liniface.RX
	if markedTx && !slaveResponse {
		direction = liniface.TX
	}
	if direction == liniface.TX {
		return
	}
	if dlc == 0 {
		return
	}

	if t.isDuplicate(channel, id, direction, payload) {
		return
	}

	log.Printf("RX LIN: ID=0x%02X, Len=%02d, CS=%02X, Err=%02X, Data=% 02X", msg.FIdentifier, dlc, msg.FChecksum, msg.FErrStatus, payload)

	evt := &liniface.LinEvent{
		Channel:      channel,
		EventID:      id,
		EventPayload: payload,
		Direction:    direction,
		ChecksumType: checksumTypeFromID(id),
		Timestamp:    time.Now(),
	}
	select {
	case t.eventChannel(evt.Channel) <- evt:
	default:
	}
}

func (t *TSMaster) isDuplicate(ch liniface.Channel, id byte, dir liniface.Direction, payload []byte) bool {
	const window = 8 * time.Millisecond
	now := time.Now()
	t.dedupMu.Lock()
	defer t.dedupMu.Unlock()

	last := t.lastFrame
	same := last.valid &&
		last.ch == ch &&
		last.id == id &&
		last.dir == dir &&
		last.n == len(payload) &&
		now.Sub(last.at) < window
	if same {
		for i := 0; i < len(payload); i++ {
			if last.data[i] != payload[i] {
				same = false
				break
			}
		}
	}

	var data [8]byte
	copy(data[:], payload)
	t.lastFrame = tsmasterLastFrame{
		valid: true,
		ch:    ch,
		id:    id,
		dir:   dir,
		n:     len(payload),
		data:  data,
		at:    now,
	}
	return same
}

func checksumTypeFromID(id byte) liniface.ChecksumType {
	if id == tplin.MasterDiagnosticFrameID || id == tplin.SlaveDiagnosticFrameID {
		return liniface.ClassicChecksum
	}
	return liniface.EnhancedChecksum
}

func (t *TSMaster) ReadEvent(timeout time.Duration, channel liniface.Channel) (*liniface.LinEvent, error) {
	if t == nil {
		return nil, errors.New("tsmaster driver is nil")
	}
	if err := t.validateChannel(channel); err != nil {
		return nil, err
	}
	eventChan := t.eventChannel(channel)

	if timeout <= 0 {
		select {
		case evt, ok := <-eventChan:
			if !ok {
				return nil, errors.New("tsmaster driver closed")
			}
			return evt, nil
		default:
			return nil, nil
		}
	}

	select {
	case evt, ok := <-eventChan:
		if !ok {
			return nil, errors.New("tsmaster driver closed")
		}
		return evt, nil
	case <-time.After(timeout):
		return nil, nil
	}
}

func (t *TSMaster) WriteMessage(event *liniface.LinEvent, channel liniface.Channel) error {
	if t == nil {
		return errors.New("tsmaster driver is nil")
	}
	if event == nil {
		return errors.New("nil LIN event")
	}
	if len(event.EventPayload) > 8 {
		return fmt.Errorf("invalid LIN payload length %d (max 8)", len(event.EventPayload))
	}
	if t.transmitProc == nil {
		return errors.New("tsapp_transmit_lin_async not initialized")
	}

	msg := tsmasterLIN{
		FIdxChn:     uint8(channel),
		FProperties: linPropertyDirTxMask,
		FDLC:        uint8(len(event.EventPayload)),
		FIdentifier: event.EventID,
	}
	copy(msg.FData[:], event.EventPayload)

	t.callMu.Lock()
	if err := t.validateChannel(channel); err != nil {
		t.callMu.Unlock()
		return err
	}
	r, _, _ := t.transmitProc.Call(uintptr(unsafe.Pointer(&msg)))
	t.callMu.Unlock()
	if r != 0 {
		return fmt.Errorf("tsapp_transmit_lin_async failed: %d", r)
	}
	log.Printf("TX LIN: ID=0x%02X, Len=%02d, CS=%02X, Data=% 02X", msg.FIdentifier, msg.FDLC, msg.FChecksum, msg.FData[:msg.FDLC])

	txCopy := *event
	txCopy.Channel = channel
	txCopy.Direction = liniface.TX
	txCopy.Timestamp = time.Now()
	select {
	case t.eventChannel(channel) <- &txCopy:
	default:
	}

	return nil
}

func (t *TSMaster) MasterWrite(frameID byte, data []byte, channel liniface.Channel) error {
	if t == nil {
		return errors.New("tsmaster driver is nil")
	}
	if len(data) > 8 {
		return fmt.Errorf("tsmaster MasterWrite: data length %d exceeds 8", len(data))
	}
	if t.transmitProc == nil {
		return errors.New("tsapp_transmit_lin_async not initialized")
	}
	if err := t.validateChannel(channel); err != nil {
		return err
	}

	msg := tsmasterLIN{
		FIdxChn:     uint8(channel),
		FProperties: linPropertyDirTxMask,
		FDLC:        uint8(len(data)),
		FIdentifier: frameID,
	}
	copy(msg.FData[:], data)

	t.callMu.Lock()
	r, _, _ := t.transmitProc.Call(uintptr(unsafe.Pointer(&msg)))
	t.callMu.Unlock()
	if r != 0 {
		return fmt.Errorf("tsapp_transmit_lin_async failed: %d (%s)", r, t.errorText(r))
	}
	logLINMessage("TX", frameID, msg.FDLC, msg.FChecksum, msg.FData[:msg.FDLC])
	return nil
}

func (t *TSMaster) MasterRead(frameID byte, channel liniface.Channel) ([]byte, error) {
	if t == nil {
		return nil, errors.New("tsmaster driver is nil")
	}
	if err := t.RequestSlaveResponse(frameID, channel); err != nil {
		return nil, err
	}

	wantID := frameID & 0x3F
	deadline := time.Now().Add(masterReadTimeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, errors.New("no response from slave")
		}
		evt, err := t.ReadEvent(remaining, channel)
		if err != nil {
			return nil, err
		}
		if evt == nil || evt.EventID != wantID || evt.Direction != liniface.RX || len(evt.EventPayload) == 0 {
			continue
		}
		result := append([]byte(nil), evt.EventPayload...)
		logLINMessage("RX", frameID, byte(len(result)), 0, result)
		return result, nil
	}
}

func (t *TSMaster) RequestSlaveResponse(frameID byte, channel liniface.Channel) error {
	if t == nil {
		return errors.New("tsmaster driver is nil")
	}
	if err := t.validateChannel(channel); err != nil {
		return err
	}
	if t.transmitProc == nil {
		return errors.New("tsapp_transmit_lin_async not initialized")
	}

	// 官方 C# LIN demo：new TLIBLIN(chn, id, 8, isTx=false) + tsapp_transmit_lin_async。
	// FProperties bit0=0 表示主节点只发头，从机数据走 register_event_lin / FIFO。
	msg := tsmasterLIN{
		FIdxChn:     uint8(channel),
		FProperties: 0,
		FDLC:        8,
		FIdentifier: frameID,
	}

	t.callMu.Lock()
	r, _, _ := t.transmitProc.Call(uintptr(unsafe.Pointer(&msg)))
	t.callMu.Unlock()
	log.Printf("TX HDR LIN: ID=0x%02X, Len=%02d, ret=%d", frameID, msg.FDLC, r)
	if r != 0 {
		return fmt.Errorf("tsapp_transmit_lin_async (header) failed: %d (%s)", r, t.errorText(r))
	}
	return nil
}

func (t *TSMaster) ScheduleSlaveResponse(event *liniface.LinEvent, channel liniface.Channel) error {
	if t == nil {
		return errors.New("tsmaster driver is nil")
	}
	return errors.New("tsmaster: ScheduleSlaveResponse is not supported in master mode")
}

func (t *TSMaster) eventChannel(channel liniface.Channel) chan *liniface.LinEvent {
	t.eventMu.Lock()
	defer t.eventMu.Unlock()
	eventChan := t.eventChans[channel]
	if eventChan == nil {
		eventChan = make(chan *liniface.LinEvent, 128)
		t.eventChans[channel] = eventChan
	}
	return eventChan
}

func (t *TSMaster) validateChannel(channel liniface.Channel) error {
	if t == nil {
		return liniface.ErrDriverClosed
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.connected {
		return liniface.ErrDriverClosed
	}
	for _, configured := range t.channels {
		if configured == uint32(channel) {
			return nil
		}
	}
	return fmt.Errorf("%w: %d", liniface.ErrInvalidChannel, channel)
}

func (t *TSMaster) Close() error {
	if t == nil {
		return nil
	}
	t.closeOnce.Do(func() {
		t.mu.Lock()
		cancel := t.cancel
		loader := t.loader
		connected := t.connected
		linCb := t.linCb
		t.loader = nil
		t.connected = false
		t.linCb = 0
		t.mu.Unlock()

		if cancel != nil {
			cancel()
		}
		t.wg.Wait()
		t.callMu.Lock()
		defer t.callMu.Unlock()

		if loader != nil && connected {
			if linCb != 0 {
				if p := loader.proc("tsapp_unregister_event_lin"); p != nil {
					p.Call(0, linCb)
				}
			}
			if p := loader.proc("tsfifo_disable_receive_fifo"); p != nil {
				p.Call()
			}
			if p := loader.proc("tsapp_disconnect"); p != nil {
				p.Call()
			}
		}
		if loader != nil {
			loader.close()
		}

		t.eventMu.Lock()
		for _, eventChan := range t.eventChans {
			close(eventChan)
		}
		t.eventMu.Unlock()
		close(t.errCh)
	})

	return nil
}

func (t *TSMaster) pushError(err error) {
	if err == nil {
		return
	}
	select {
	case t.errCh <- err:
	default:
	}
}
func (t *TSMaster) errorText(code uintptr) string {
	if t == nil || t.loader == nil || code == 0 {
		return ""
	}
	p := t.loader.proc("tsapp_get_error_description")
	if p == nil {
		return ""
	}
	var desc *byte
	r, _, _ := p.Call(code, uintptr(unsafe.Pointer(&desc)))
	if r != 0 || desc == nil {
		return ""
	}
	return windows.BytePtrToString(desc)
}

func (t *TSMaster) LinBreak(channels ...liniface.Channel) error {
	if t == nil {
		return errors.New("tsmaster driver is nil")
	}
	if len(t.channels) == 0 {
		return errors.New("tsmaster has no configured channel")
	}

	channel := liniface.Channel(t.channels[0])
	if len(channels) > 0 {
		channel = channels[0]
	}

	t.callMu.Lock()
	if err := t.validateChannel(channel); err != nil {
		t.callMu.Unlock()
		return err
	}
	if wakeupProc := t.loader.proc("tsapp_transmit_lin_wakeup_async"); wakeupProc != nil {
		// 官方：tsapp_transmit_lin_wakeup_async(chn, wakeupLength, interval, times)
		r, _, _ := wakeupProc.Call(uintptr(channel), uintptr(500), uintptr(20), uintptr(3))
		t.callMu.Unlock()
		if r != 0 {
			return fmt.Errorf("tsapp_transmit_lin_wakeup_async failed: %d (%s)", r, t.errorText(r))
		}
		return nil
	}
	if t.transmitProc == nil {
		t.callMu.Unlock()
		return errors.New("tsapp_transmit_lin_async not initialized")
	}
	msg := tsmasterLIN{
		FIdxChn:     uint8(channel),
		FProperties: linPropertyDirTxMask | linPropertyBreakMask,
		FDLC:        0,
	}
	r, _, _ := t.transmitProc.Call(uintptr(unsafe.Pointer(&msg)))
	t.callMu.Unlock()
	if r != 0 {
		return fmt.Errorf("tsapp_transmit_lin_async (LIN break) failed: %d", r)
	}
	return nil
}
