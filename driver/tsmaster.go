//go:build windows

package driver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/LoveWonYoung/linbuskit/liniface"
	"github.com/LoveWonYoung/linbuskit/tplin"
	"golang.org/x/sys/windows/registry"
)

const (
	tsmasterDeviceType   = 3
	linPropertyDirTxMask = 0x01
	linPropertyBreakMask = 0x02
)

var defaultTSMasterChannels = []uint32{0}

type tsmasterLoader struct {
	dll     *syscall.LazyDLL
	dllPath string
}

func newTSMasterLoader() (*tsmasterLoader, error) {
	loader := &tsmasterLoader{}

	dllPath, err := loader.findDLLPath()
	if err != nil {
		return nil, err
	}

	loader.dllPath = dllPath
	loader.dll = syscall.NewLazyDLL(dllPath)
	if err := loader.dll.Load(); err != nil {
		return nil, fmt.Errorf("failed to load TSMaster.dll: %w", err)
	}

	return loader, nil
}

func (l *tsmasterLoader) findDLLPath() (string, error) {
	basePath := `C:\Program Files (x86)\TOSUN\TSMaster`

	var dllPath string
	if runtime.GOARCH == "386" {
		dllPath = filepath.Join(basePath, "bin", "TSMaster.dll")
	} else {
		dllPath = filepath.Join(basePath, "bin64", "TSMaster.dll")
	}

	if fileExists(dllPath) {
		return dllPath, nil
	}

	if path, err := l.getDLLFromRegistry(); err == nil && path != "" {
		registryPath := filepath.Join(filepath.Dir(path), "TSMaster.dll")
		if fileExists(registryPath) {
			return registryPath, nil
		}
	}

	return "", fmt.Errorf("TSMaster.dll not found in default or registry paths")
}

func (l *tsmasterLoader) getDLLFromRegistry() (string, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\TOSUN\TSMaster`, registry.QUERY_VALUE)
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

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
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

	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	connected  bool
	deviceType TSMasterDeviceType
}
type TSMasterDeviceType byte

const (
	TC1016 TSMasterDeviceType = 11
	TL1001 TSMasterDeviceType = 4
)

var _ liniface.Driver = (*TSMaster)(nil)

func NewTSMaster(deviceType TSMasterDeviceType, channels ...liniface.Channel) (*TSMaster, error) {
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
	appName, _ := syscall.UTF16PtrFromString("linbuskit")
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

	deviceName, _ := syscall.UTF16PtrFromString("TC1016")
	for _, ch := range t.channels {
		r, _, _ = setMappingProc.Call(
			uintptr(unsafe.Pointer(appName)),
			uintptr(0),
			uintptr(ch),
			uintptr(unsafe.Pointer(deviceName)),
			uintptr(tsmasterDeviceType),
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
			uintptr(2),
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

	nodeTypeProc := t.loader.proc("tslin_set_node_functiontype")
	if nodeTypeProc == nil {
		return errors.New("tslin_set_node_functiontype not found")
	}
	for _, ch := range t.channels {
		r, _, _ = nodeTypeProc.Call(uintptr(ch), uintptr(0))
		if r != 0 {
			return fmt.Errorf("tslin_set_node_functiontype failed for channel %d: %d", ch, r)
		}
	}

	return nil
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
				uintptr(0),
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
				msg := linBuffer[i]
				dlc := int(msg.FDLC)
				if dlc < 0 {
					dlc = 0
				}
				if dlc > len(msg.FData) {
					dlc = len(msg.FData)
				}

				payload := make([]byte, dlc)
				copy(payload, msg.FData[:dlc])

				evt := &liniface.LinEvent{
					Channel:      liniface.Channel(msg.FIdxChn),
					EventID:      msg.FIdentifier,
					EventPayload: payload,
					Direction:    liniface.RX,
					ChecksumType: checksumTypeFromID(msg.FIdentifier),
					Timestamp:    time.Now(),
				}
				if msg.FProperties&linPropertyDirTxMask != 0 {
					evt.Direction = liniface.TX
				} else {
					log.Printf("RX LIN: ID=0x%02X, Len=%02d, CS=%02X, Data=% 02X", msg.FIdentifier, dlc, msg.FChecksum, payload)
				}

				select {
				case t.eventChannel(evt.Channel) <- evt:
				default:
				}
			}
		}

		if !hasFrames {
			time.Sleep(2 * time.Millisecond)
		}
	}
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

func (t *TSMaster) RequestSlaveResponse(frameID byte, channel liniface.Channel) error {
	if t == nil {
		return errors.New("tsmaster driver is nil")
	}
	if t.transmitProc == nil {
		return errors.New("tsapp_transmit_lin_async not initialized")
	}

	msg := tsmasterLIN{
		FIdxChn:     uint8(channel),
		FProperties: 0,
		FDLC:        8,
		FIdentifier: frameID,
	}

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
		t.loader = nil
		t.connected = false
		t.mu.Unlock()

		if cancel != nil {
			cancel()
		}
		t.wg.Wait()
		t.callMu.Lock()
		defer t.callMu.Unlock()

		if loader != nil && connected {
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
func (t *TSMaster) LinBreak(channels ...liniface.Channel) error {
	if t == nil {
		return errors.New("tsmaster driver is nil")
	}
	if t.transmitProc == nil {
		return errors.New("tsapp_transmit_lin_async not initialized")
	}
	if len(t.channels) == 0 {
		return errors.New("tsmaster has no configured channel")
	}

	channel := liniface.Channel(t.channels[0])
	if len(channels) > 0 {
		channel = channels[0]
	}
	// FProperties bit1=1: send break, bit0=1: TX direction.
	msg := tsmasterLIN{
		FIdxChn:     uint8(channel),
		FProperties: linPropertyDirTxMask | linPropertyBreakMask,
		FDLC:        0,
	}

	t.callMu.Lock()
	if err := t.validateChannel(channel); err != nil {
		t.callMu.Unlock()
		return err
	}
	r, _, _ := t.transmitProc.Call(uintptr(unsafe.Pointer(&msg)))
	t.callMu.Unlock()
	if r != 0 {
		return fmt.Errorf("tsapp_transmit_lin_async (LIN break) failed: %d", r)
	}
	return nil
}
