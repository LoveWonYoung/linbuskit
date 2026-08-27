//go:build windows

package driver

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/LoveWonYoung/linbuskit/liniface"
	"golang.org/x/sys/windows/registry"
)

const (
	pcanMinBaudrate = 1000
	pcanMaxBaudrate = 20000

	pcanDirectionPublisher             = 1
	pcanDirectionSubscriber            = 2
	pcanChecksumClassic                = 1
	pcanChecksumEnhanced               = 2
	pcanMessageTypeStandard            = 0
	pcanFrameFlagResponseEnable        = 0x01
	pcanFrameFlagSingleShot            = 0x02
	pcanErrorOK                 uint32 = 0
	pcanErrorReceiveQueueEmpty         = 3
	pcanErrorBufferInsufficient        = 13

	pcanHardwareParamDeviceNumber  = 2
	pcanHardwareParamChannelNumber = 3
	pcanHardwareParamBaudrate      = 6
	pcanHardwareParamMode          = 7

	pcanEventQueueSize = 128
	pcanReadPollDelay  = time.Millisecond
)

var pcanDLLName = "PLinApi.dll"

// PCANMode is the operating mode passed to the PEAK PLIN API.
type PCANMode byte

const (
	PCANSlave  PCANMode = 1
	PCANMaster PCANMode = 2
)

// PCANConfig controls creation of a PCAN/PLIN driver.
//
// Without HardwareHandles, a logical channel selects the hardware at the same
// index in LIN_GetAvailableHardware. HardwareHandles can be used when a stable,
// explicit mapping is required for a multi-device setup.
type PCANConfig struct {
	ClientName      string
	Mode            PCANMode
	Baudrate        uint16
	Channels        []liniface.Channel
	HardwareHandles map[liniface.Channel]uint16
}

// DefaultPCANConfig returns a 19200-baud master configuration. If channels is
// empty, logical channel 0 is used.
func DefaultPCANConfig(channels ...liniface.Channel) PCANConfig {
	if len(channels) == 0 {
		channels = []liniface.Channel{0}
	}
	return PCANConfig{
		ClientName: "linbuskit",
		Mode:       PCANMaster,
		Baudrate:   19200,
		Channels:   append([]liniface.Channel(nil), channels...),
	}
}

// PCANError is an error code returned by PLinApi.dll.
type PCANError uint32

func (e PCANError) Error() string {
	if name, ok := pcanErrorNames[uint32(e)]; ok {
		return fmt.Sprintf("%s (%d)", name, uint32(e))
	}
	return fmt.Sprintf("PLIN error %d", uint32(e))
}

var pcanErrorNames = map[uint32]string{
	0:      "success",
	1:      "transmit queue full",
	2:      "illegal period",
	3:      "receive queue empty",
	4:      "illegal checksum type",
	5:      "illegal hardware handle",
	6:      "illegal client handle",
	7:      "wrong parameter type",
	8:      "wrong parameter value",
	9:      "illegal direction",
	10:     "illegal length",
	11:     "illegal baudrate",
	12:     "illegal frame ID",
	13:     "buffer insufficient",
	14:     "illegal schedule number",
	15:     "illegal slot count",
	16:     "illegal index",
	17:     "illegal byte range",
	18:     "illegal hardware state",
	19:     "illegal scheduler state",
	20:     "illegal frame configuration",
	21:     "schedule slot pool full",
	22:     "illegal schedule",
	23:     "illegal hardware mode",
	1001:   "out of resources",
	1002:   "LIN manager not loaded",
	1003:   "LIN manager not responding",
	1004:   "memory access violation",
	0xFFFE: "not implemented",
	0xFFFF: "unknown PLIN error",
}

type pcanMessage struct {
	FrameID      byte
	Length       byte
	Direction    byte
	ChecksumType byte
	Data         [8]byte
	Checksum     byte
}

// The explicit padding keeps the TLINRcvMsg layout identical on amd64 and 386.
type pcanReceiveMessage struct {
	Type         byte
	FrameID      byte
	Length       byte
	Direction    byte
	ChecksumType byte
	Data         [8]byte
	Checksum     byte
	padding0     [2]byte
	ErrorFlags   uint32
	padding1     [4]byte
	Timestamp    uint64
	Hardware     uint16
	padding2     [6]byte
}

type pcanFrameEntry struct {
	FrameID      byte
	Length       byte
	Direction    byte
	ChecksumType byte
	Flags        uint16
	InitialData  [8]byte
}

type pcanAPI struct {
	dll syscall.Handle

	registerClient     uintptr
	removeClient       uintptr
	connectClient      uintptr
	disconnectClient   uintptr
	resetClient        uintptr
	setClientFilter    uintptr
	read               uintptr
	write              uintptr
	initializeHardware uintptr
	getAvailableHW     uintptr
	getHardwareParam   uintptr
	setFrameEntry      uintptr
	updateByteArray    uintptr
	calculateChecksum  uintptr
}

func pcanDLLCandidates() []string {
	var candidates []string
	seen := make(map[string]struct{})

	add := func(path string) {
		if path == "" {
			return
		}
		normalized := strings.ToLower(filepath.Clean(path))
		if _, exists := seen[normalized]; exists {
			return
		}
		seen[normalized] = struct{}{}
		candidates = append(candidates, path)
	}

	if path, err := getPCANBasicDLLFromRegistry(); err == nil && path != "" {
		add(path)
	}

	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	if runtime.GOARCH == "386" {
		add(filepath.Join(systemRoot, "SysWOW64", pcanDLLName))
	}
	add(filepath.Join(systemRoot, "System32", pcanDLLName))
	add(filepath.Join(".", "bin", pcanDLLName))
	add(pcanDLLName)
	return candidates
}

func getPCANBasicDLLFromRegistry() (string, error) {
	access := uint32(registry.QUERY_VALUE)
	if runtime.GOARCH == "386" {
		access |= registry.WOW64_32KEY
	} else {
		access |= registry.WOW64_64KEY
	}

	key, err := registry.OpenKey(
		registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows\CurrentVersion\SharedDlls`,
		access,
	)
	if err != nil {
		return "", err
	}
	defer key.Close()

	names, err := key.ReadValueNames(-1)
	if err != nil {
		return "", err
	}
	for _, name := range names {
		if strings.EqualFold(filepath.Base(name), pcanDLLName) {
			return name, nil
		}
	}
	return "", fmt.Errorf("%s not found in SharedDlls", pcanDLLName)
}

func loadPCANAPI() (*pcanAPI, error) {
	var loadErrors []string
	for _, candidate := range pcanDLLCandidates() {
		handle, err := syscall.LoadLibrary(candidate)
		if err != nil {
			loadErrors = append(loadErrors, fmt.Sprintf("%s: %v", candidate, err))
			continue
		}

		api := &pcanAPI{dll: handle}
		if err := api.loadProcedures(); err != nil {
			_ = syscall.FreeLibrary(handle)
			loadErrors = append(loadErrors, fmt.Sprintf("%s: %v", candidate, err))
			continue
		}
		return api, nil
	}

	return nil, fmt.Errorf("load %s: %s", pcanDLLName, strings.Join(loadErrors, "; "))
}

func (a *pcanAPI) loadProcedures() error {
	procedures := []struct {
		name string
		dst  *uintptr
	}{
		{"LIN_RegisterClient", &a.registerClient},
		{"LIN_RemoveClient", &a.removeClient},
		{"LIN_ConnectClient", &a.connectClient},
		{"LIN_DisconnectClient", &a.disconnectClient},
		{"LIN_ResetClient", &a.resetClient},
		{"LIN_SetClientFilter", &a.setClientFilter},
		{"LIN_Read", &a.read},
		{"LIN_Write", &a.write},
		{"LIN_InitializeHardware", &a.initializeHardware},
		{"LIN_GetAvailableHardware", &a.getAvailableHW},
		{"LIN_GetHardwareParam", &a.getHardwareParam},
		{"LIN_SetFrameEntry", &a.setFrameEntry},
		{"LIN_UpdateByteArray", &a.updateByteArray},
		{"LIN_CalculateChecksum", &a.calculateChecksum},
	}

	var missing []string
	for _, procedure := range procedures {
		address, err := syscall.GetProcAddress(a.dll, procedure.name)
		if err != nil || address == 0 {
			missing = append(missing, procedure.name)
			continue
		}
		*procedure.dst = address
	}
	if len(missing) != 0 {
		return fmt.Errorf("missing procedures: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (a *pcanAPI) close() error {
	if a == nil || a.dll == 0 {
		return nil
	}
	err := syscall.FreeLibrary(a.dll)
	a.dll = 0
	return err
}

func pcanCall(operation string, procedure uintptr, args ...uintptr) error {
	result, _, _ := syscall.SyscallN(procedure, args...)
	code := uint32(result)
	if code != pcanErrorOK {
		return fmt.Errorf("%s: %w", operation, PCANError(code))
	}
	return nil
}

type PCAN struct {
	stateMu   sync.RWMutex
	callMu    sync.Mutex
	readMu    sync.Mutex
	eventMu   sync.Mutex
	closeOnce sync.Once

	api               *pcanAPI
	client            byte
	mode              PCANMode
	baudrate          uint16
	closed            bool
	hardwareByChannel map[liniface.Channel]uint16
	channelByHardware map[uint16]liniface.Channel
	connectedHardware []uint16
	eventChans        map[liniface.Channel]chan *liniface.LinEvent
	closeErr          error
}

var _ liniface.Driver = (*PCAN)(nil)

// NewPCAN opens the selected logical channels using the default PCAN master
// configuration.
func NewPCAN(channels ...liniface.Channel) (*PCAN, error) {
	return NewPCANWithConfig(DefaultPCANConfig(channels...))
}

// NewPCANWithConfig registers a PLIN client, connects the selected hardware,
// initializes it and enables reception of all LIN frame IDs.
func NewPCANWithConfig(config PCANConfig) (*PCAN, error) {
	normalized, err := normalizePCANConfig(config)
	if err != nil {
		return nil, err
	}

	api, err := loadPCANAPI()
	if err != nil {
		return nil, err
	}

	p := &PCAN{
		api:               api,
		mode:              normalized.Mode,
		baudrate:          normalized.Baudrate,
		hardwareByChannel: make(map[liniface.Channel]uint16, len(normalized.Channels)),
		channelByHardware: make(map[uint16]liniface.Channel, len(normalized.Channels)),
		eventChans:        make(map[liniface.Channel]chan *liniface.LinEvent),
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = p.Close()
		}
	}()

	if err := p.register(normalized.ClientName); err != nil {
		return nil, err
	}

	available, err := p.availableHardware()
	if err != nil {
		return nil, err
	}
	if err := p.mapHardware(normalized, available); err != nil {
		return nil, err
	}

	for _, channel := range normalized.Channels {
		hardware := p.hardwareByChannel[channel]
		if err := p.connectAndInitialize(hardware); err != nil {
			return nil, fmt.Errorf("initialize PCAN channel %d (hardware 0x%04X): %w", channel, hardware, err)
		}
		logDriverf(
			logDevicePCAN,
			"initialized channel=%d hardware=0x%04X mode=%s baudrate=%d",
			channel,
			hardware,
			p.mode,
			p.baudrate,
		)
	}

	cleanup = false
	return p, nil
}

func normalizePCANConfig(config PCANConfig) (PCANConfig, error) {
	if config.ClientName == "" {
		config.ClientName = "linbuskit"
	}
	if strings.IndexByte(config.ClientName, 0) >= 0 {
		return PCANConfig{}, errors.New("PCAN client name contains a NUL byte")
	}
	if config.Mode == 0 {
		config.Mode = PCANMaster
	}
	if config.Mode != PCANMaster && config.Mode != PCANSlave {
		return PCANConfig{}, fmt.Errorf("invalid PCAN mode %d", config.Mode)
	}
	if config.Baudrate == 0 {
		config.Baudrate = 19200
	}
	if config.Baudrate < pcanMinBaudrate || config.Baudrate > pcanMaxBaudrate {
		return PCANConfig{}, fmt.Errorf("invalid PCAN baudrate %d (expected %d..%d)", config.Baudrate, pcanMinBaudrate, pcanMaxBaudrate)
	}
	if len(config.Channels) == 0 {
		config.Channels = []liniface.Channel{0}
	} else {
		config.Channels = append([]liniface.Channel(nil), config.Channels...)
	}

	seen := make(map[liniface.Channel]struct{}, len(config.Channels))
	for _, channel := range config.Channels {
		if _, exists := seen[channel]; exists {
			return PCANConfig{}, fmt.Errorf("duplicate PCAN channel %d", channel)
		}
		seen[channel] = struct{}{}
		if handle, explicitlyMapped := config.HardwareHandles[channel]; explicitlyMapped && handle == 0 {
			return PCANConfig{}, fmt.Errorf("invalid hardware handle 0 for PCAN channel %d", channel)
		}
	}
	return config, nil
}

func (p *PCAN) register(clientName string) error {
	name, err := syscall.BytePtrFromString(clientName)
	if err != nil {
		return fmt.Errorf("encode PCAN client name: %w", err)
	}
	if err := pcanCall(
		"LIN_RegisterClient",
		p.api.registerClient,
		uintptr(unsafe.Pointer(name)),
		0,
		uintptr(unsafe.Pointer(&p.client)),
	); err != nil {
		return err
	}
	if p.client == 0 {
		return errors.New("LIN_RegisterClient returned an invalid client handle")
	}
	return nil
}

func (p *PCAN) availableHardware() ([]uint16, error) {
	var count uint32
	result, _, _ := syscall.SyscallN(
		p.api.getAvailableHW,
		0,
		0,
		uintptr(unsafe.Pointer(&count)),
	)
	code := uint32(result)
	if code != pcanErrorOK && code != pcanErrorBufferInsufficient {
		return nil, fmt.Errorf("LIN_GetAvailableHardware(count): %w", PCANError(code))
	}
	if count == 0 {
		return nil, errors.New("no PCAN LIN hardware found")
	}
	if count > uint32(^uint16(0)/2) {
		return nil, fmt.Errorf("invalid PCAN hardware count %d", count)
	}

	handles := make([]uint16, int(count))
	var returned uint32
	result, _, _ = syscall.SyscallN(
		p.api.getAvailableHW,
		uintptr(unsafe.Pointer(&handles[0])),
		uintptr(uint16(len(handles)*2)),
		uintptr(unsafe.Pointer(&returned)),
	)
	code = uint32(result)
	if code != pcanErrorOK {
		return nil, fmt.Errorf("LIN_GetAvailableHardware: %w", PCANError(code))
	}
	if returned > uint32(len(handles)) {
		return nil, fmt.Errorf("PCAN hardware list changed while reading (need %d, have %d)", returned, len(handles))
	}
	return handles[:returned], nil
}

func (p *PCAN) mapHardware(config PCANConfig, available []uint16) error {
	availableSet := make(map[uint16]struct{}, len(available))
	for _, handle := range available {
		availableSet[handle] = struct{}{}
	}
	used := make(map[uint16]liniface.Channel, len(config.Channels))

	for _, channel := range config.Channels {
		handle, explicit := config.HardwareHandles[channel]
		if !explicit {
			index := int(channel)
			if index >= len(available) {
				return fmt.Errorf("PCAN channel %d selects hardware index %d, but only %d hardware channels are available", channel, index, len(available))
			}
			handle = available[index]
		} else if _, present := availableSet[handle]; !present {
			return fmt.Errorf("PCAN hardware handle 0x%04X for channel %d is not available", handle, channel)
		}
		if previous, exists := used[handle]; exists {
			return fmt.Errorf("PCAN channels %d and %d map to the same hardware handle 0x%04X", previous, channel, handle)
		}
		used[handle] = channel
		p.hardwareByChannel[channel] = handle
		p.channelByHardware[handle] = channel
	}
	return nil
}

func (p *PCAN) connectAndInitialize(hardware uint16) error {
	if err := pcanCall("LIN_ConnectClient", p.api.connectClient, uintptr(p.client), uintptr(hardware)); err != nil {
		return err
	}
	p.connectedHardware = append(p.connectedHardware, hardware)

	currentMode, err := p.hardwareParameter(hardware, pcanHardwareParamMode)
	if err != nil {
		return err
	}
	currentBaudrate, err := p.hardwareParameter(hardware, pcanHardwareParamBaudrate)
	if err != nil {
		return err
	}
	if currentMode != int32(p.mode) || currentBaudrate != int32(p.baudrate) {
		if err := pcanCall(
			"LIN_InitializeHardware",
			p.api.initializeHardware,
			uintptr(p.client),
			uintptr(hardware),
			uintptr(p.mode),
			uintptr(p.baudrate),
		); err != nil {
			return err
		}
	}

	filterArgs := []uintptr{uintptr(p.client), uintptr(hardware), ^uintptr(0)}
	if runtime.GOARCH == "386" {
		// UInt64 arguments occupy two stack words with the 32-bit stdcall ABI.
		filterArgs = append(filterArgs, ^uintptr(0))
	}
	if err := pcanCall("LIN_SetClientFilter", p.api.setClientFilter, filterArgs...); err != nil {
		return err
	}
	return nil
}

func (p *PCAN) hardwareParameter(hardware uint16, parameter uint16) (int32, error) {
	var value int32
	if err := pcanCall(
		"LIN_GetHardwareParam",
		p.api.getHardwareParam,
		uintptr(hardware),
		uintptr(parameter),
		uintptr(unsafe.Pointer(&value)),
		0,
	); err != nil {
		return 0, err
	}
	return value, nil
}

func (p *PCAN) ReadEvent(timeout time.Duration, channel liniface.Channel) (*liniface.LinEvent, error) {
	if err := p.validateChannel(channel); err != nil {
		return nil, err
	}
	events := p.eventChannel(channel)
	deadline := time.Now().Add(timeout)

	for {
		select {
		case event := <-events:
			return event, nil
		default:
		}
		if timeout > 0 && !time.Now().Before(deadline) {
			return nil, nil
		}

		message, empty, err := p.readOne()
		if err != nil {
			return nil, err
		}
		if !empty {
			if message.Type != pcanMessageTypeStandard {
				continue
			}
			messageChannel, configured := p.channelByHardware[message.Hardware]
			if !configured {
				continue
			}
			if message.ErrorFlags != 0 {
				logDriverf(
					logDevicePCAN,
					"LIN channel=%d id=0x%02X status=error error_flags=0x%X",
					messageChannel,
					message.FrameID,
					message.ErrorFlags,
				)
				continue
			}

			event := pcanEvent(message, messageChannel)
			direction := "RX"
			if event.Direction == liniface.TX {
				direction = "TX"
			}
			logLINMessage(logDevicePCAN, direction, messageChannel, event.EventID, message.Checksum, event.EventPayload)
			if messageChannel == channel {
				return event, nil
			}
			p.enqueueEvent(messageChannel, event)
			continue
		}

		if timeout <= 0 {
			return nil, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, nil
		}
		wait := min(remaining, pcanReadPollDelay)
		timer := time.NewTimer(wait)
		select {
		case event := <-events:
			timer.Stop()
			return event, nil
		case <-timer.C:
		}
	}
}

func (p *PCAN) readOne() (pcanReceiveMessage, bool, error) {
	p.readMu.Lock()
	defer p.readMu.Unlock()
	p.callMu.Lock()
	defer p.callMu.Unlock()
	if err := p.validateChannelStateOnly(); err != nil {
		return pcanReceiveMessage{}, false, err
	}

	var message pcanReceiveMessage
	result, _, _ := syscall.SyscallN(
		p.api.read,
		uintptr(p.client),
		uintptr(unsafe.Pointer(&message)),
	)
	code := uint32(result)
	if code == pcanErrorReceiveQueueEmpty {
		return pcanReceiveMessage{}, true, nil
	}
	if code != pcanErrorOK {
		return pcanReceiveMessage{}, false, fmt.Errorf("LIN_Read: %w", PCANError(code))
	}
	return message, false, nil
}

func pcanEvent(message pcanReceiveMessage, channel liniface.Channel) *liniface.LinEvent {
	length := min(int(message.Length), len(message.Data))
	direction := liniface.RX
	if message.Direction == pcanDirectionPublisher {
		direction = liniface.TX
	}
	checksum := liniface.EnhancedChecksum
	if message.ChecksumType == pcanChecksumClassic {
		checksum = liniface.ClassicChecksum
	}
	return &liniface.LinEvent{
		Channel:      channel,
		EventID:      message.FrameID & 0x3F,
		EventPayload: append([]byte(nil), message.Data[:length]...),
		ChecksumType: checksum,
		Direction:    direction,
		Timestamp:    time.Now(),
	}
}

func (p *PCAN) WriteMessage(event *liniface.LinEvent, channel liniface.Channel) error {
	if err := validatePCANEvent(event); err != nil {
		return err
	}
	if err := p.validateModeAndChannel(PCANMaster, channel); err != nil {
		return err
	}

	message := pcanMessage{
		FrameID:      protectedLINID(event.EventID),
		Length:       byte(len(event.EventPayload)),
		Direction:    pcanDirectionPublisher,
		ChecksumType: pcanChecksumType(event.EventID, event.ChecksumType),
	}
	copy(message.Data[:], event.EventPayload)
	if err := p.write(&message, channel); err != nil {
		return err
	}
	logLINMessage(logDevicePCAN, "TX", channel, event.EventID, message.Checksum, message.Data[:message.Length])

	txEvent := *event
	txEvent.Channel = channel
	txEvent.EventPayload = append([]byte(nil), event.EventPayload...)
	txEvent.Direction = liniface.TX
	txEvent.Timestamp = time.Now()
	p.enqueueEvent(channel, &txEvent)
	return nil
}

func (p *PCAN) write(message *pcanMessage, channel liniface.Channel) error {
	if message == nil {
		return errors.New("nil PCAN message")
	}
	p.callMu.Lock()
	defer p.callMu.Unlock()
	if err := p.validateChannelStateOnly(); err != nil {
		return err
	}
	hardware, ok := p.hardwareByChannel[channel]
	if !ok {
		return fmt.Errorf("%w: %d", liniface.ErrInvalidChannel, channel)
	}
	if err := pcanCall(
		"LIN_CalculateChecksum",
		p.api.calculateChecksum,
		uintptr(unsafe.Pointer(message)),
	); err != nil {
		return err
	}
	if err := pcanCall(
		"LIN_Write",
		p.api.write,
		uintptr(p.client),
		uintptr(hardware),
		uintptr(unsafe.Pointer(message)),
	); err != nil {
		return err
	}
	return nil
}

func (p *PCAN) RequestSlaveResponse(frameID byte, channel liniface.Channel) error {
	if frameID > 0x3F {
		return fmt.Errorf("invalid LIN frame ID 0x%02X", frameID)
	}
	if err := p.validateModeAndChannel(PCANMaster, channel); err != nil {
		return err
	}
	message := pcanMessage{
		FrameID:      protectedLINID(frameID),
		Length:       defaultPCANFrameLength(frameID),
		Direction:    pcanDirectionSubscriber,
		ChecksumType: pcanChecksumType(frameID, liniface.EnhancedChecksum),
	}
	if err := p.write(&message, channel); err != nil {
		return err
	}
	logLINHeader(logDevicePCAN, channel, frameID, message.Length)
	return nil
}

func (p *PCAN) ScheduleSlaveResponse(event *liniface.LinEvent, channel liniface.Channel) error {
	if err := validatePCANEvent(event); err != nil {
		return err
	}
	if err := p.validateModeAndChannel(PCANSlave, channel); err != nil {
		return err
	}

	entry := pcanFrameEntry{
		FrameID:      event.EventID,
		Length:       byte(len(event.EventPayload)),
		Direction:    pcanDirectionPublisher,
		ChecksumType: pcanChecksumType(event.EventID, event.ChecksumType),
		Flags:        pcanFrameFlagResponseEnable | pcanFrameFlagSingleShot,
	}
	copy(entry.InitialData[:], event.EventPayload)

	p.callMu.Lock()
	defer p.callMu.Unlock()
	if err := p.validateChannelStateOnly(); err != nil {
		return err
	}
	hardware, ok := p.hardwareByChannel[channel]
	if !ok {
		return fmt.Errorf("%w: %d", liniface.ErrInvalidChannel, channel)
	}
	if err := pcanCall(
		"LIN_SetFrameEntry",
		p.api.setFrameEntry,
		uintptr(p.client),
		uintptr(hardware),
		uintptr(unsafe.Pointer(&entry)),
	); err != nil {
		return err
	}
	if err := pcanCall(
		"LIN_UpdateByteArray",
		p.api.updateByteArray,
		uintptr(p.client),
		uintptr(hardware),
		uintptr(event.EventID),
		0,
		uintptr(len(event.EventPayload)),
		uintptr(unsafe.Pointer(&entry.InitialData[0])),
	); err != nil {
		return err
	}
	logLINMessage(logDevicePCAN, "TX_SCHEDULE", channel, event.EventID, 0, event.EventPayload)
	return nil
}

func validatePCANEvent(event *liniface.LinEvent) error {
	if event == nil {
		return errors.New("nil LIN event")
	}
	if event.EventID > 0x3F {
		return fmt.Errorf("invalid LIN frame ID 0x%02X", event.EventID)
	}
	if len(event.EventPayload) == 0 || len(event.EventPayload) > 8 {
		return fmt.Errorf("invalid LIN payload length %d (expected 1..8)", len(event.EventPayload))
	}
	return nil
}

func pcanChecksumType(frameID byte, checksum liniface.ChecksumType) byte {
	if frameID == 0x3C || frameID == 0x3D || checksum == liniface.ClassicChecksum {
		return pcanChecksumClassic
	}
	return pcanChecksumEnhanced
}

func defaultPCANFrameLength(frameID byte) byte {
	if frameID == 0x3C || frameID == 0x3D {
		return 8
	}
	switch {
	case frameID <= 0x1F:
		return 2
	case frameID <= 0x2F:
		return 4
	default:
		return 8
	}
}

func protectedLINID(frameID byte) byte {
	id := frameID & 0x3F
	p0 := ((id >> 0) ^ (id >> 1) ^ (id >> 2) ^ (id >> 4)) & 1
	p1 := (^((id >> 1) ^ (id >> 3) ^ (id >> 4) ^ (id >> 5))) & 1
	return id | p0<<6 | p1<<7
}

func (p *PCAN) eventChannel(channel liniface.Channel) chan *liniface.LinEvent {
	p.eventMu.Lock()
	defer p.eventMu.Unlock()
	events := p.eventChans[channel]
	if events == nil {
		events = make(chan *liniface.LinEvent, pcanEventQueueSize)
		p.eventChans[channel] = events
	}
	return events
}

func (p *PCAN) enqueueEvent(channel liniface.Channel, event *liniface.LinEvent) {
	select {
	case p.eventChannel(channel) <- event:
	default:
		logDriverf(logDevicePCAN, "queue_overflow channel=%d id=0x%02X action=drop", channel, event.EventID)
	}
}

func (p *PCAN) validateModeAndChannel(mode PCANMode, channel liniface.Channel) error {
	if err := p.validateChannel(channel); err != nil {
		return err
	}
	if p.mode != mode {
		return fmt.Errorf("PCAN channel %d is initialized in %s mode; operation requires %s mode", channel, p.mode, mode)
	}
	return nil
}

func (m PCANMode) String() string {
	switch m {
	case PCANMaster:
		return "master"
	case PCANSlave:
		return "slave"
	default:
		return fmt.Sprintf("mode(%d)", byte(m))
	}
}

func (p *PCAN) validateChannel(channel liniface.Channel) error {
	if p == nil {
		return liniface.ErrDriverClosed
	}
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	if p.closed || p.api == nil || p.client == 0 {
		return liniface.ErrDriverClosed
	}
	if _, ok := p.hardwareByChannel[channel]; !ok {
		return fmt.Errorf("%w: %d", liniface.ErrInvalidChannel, channel)
	}
	return nil
}

func (p *PCAN) validateChannelStateOnly() error {
	if p == nil {
		return liniface.ErrDriverClosed
	}
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	if p.closed || p.api == nil || p.client == 0 {
		return liniface.ErrDriverClosed
	}
	return nil
}

// HardwareHandles returns a copy of the logical-channel to PLIN-hardware map.
func (p *PCAN) HardwareHandles() map[liniface.Channel]uint16 {
	if p == nil {
		return nil
	}
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	result := make(map[liniface.Channel]uint16, len(p.hardwareByChannel))
	for channel, hardware := range p.hardwareByChannel {
		result[channel] = hardware
	}
	return result
}

// HardwareInfo returns the PEAK device and physical channel numbers for a
// configured logical channel.
func (p *PCAN) HardwareInfo(channel liniface.Channel) (deviceNumber, hardwareChannel int32, err error) {
	if err = p.validateChannel(channel); err != nil {
		return 0, 0, err
	}
	p.callMu.Lock()
	defer p.callMu.Unlock()
	if err = p.validateChannelStateOnly(); err != nil {
		return 0, 0, err
	}
	hardware := p.hardwareByChannel[channel]
	deviceNumber, err = p.hardwareParameterUnlocked(hardware, pcanHardwareParamDeviceNumber)
	if err != nil {
		return 0, 0, err
	}
	hardwareChannel, err = p.hardwareParameterUnlocked(hardware, pcanHardwareParamChannelNumber)
	return deviceNumber, hardwareChannel, err
}

func (p *PCAN) hardwareParameterUnlocked(hardware uint16, parameter uint16) (int32, error) {
	var value int32
	if err := pcanCall(
		"LIN_GetHardwareParam",
		p.api.getHardwareParam,
		uintptr(hardware),
		uintptr(parameter),
		uintptr(unsafe.Pointer(&value)),
		0,
	); err != nil {
		return 0, err
	}
	return value, nil
}

// Reset clears the PLIN client's receive queue and counters.
func (p *PCAN) Reset() error {
	if err := p.validateChannelStateOnly(); err != nil {
		return err
	}
	p.callMu.Lock()
	defer p.callMu.Unlock()
	if err := p.validateChannelStateOnly(); err != nil {
		return err
	}
	return pcanCall("LIN_ResetClient", p.api.resetClient, uintptr(p.client))
}

// Close disconnects all hardware, removes the PLIN client and unloads the DLL.
func (p *PCAN) Close() error {
	if p == nil {
		return nil
	}
	p.closeOnce.Do(func() {
		p.stateMu.Lock()
		p.closed = true
		p.stateMu.Unlock()

		p.callMu.Lock()
		defer p.callMu.Unlock()
		var errs []error
		if p.api != nil && p.client != 0 {
			handles := append([]uint16(nil), p.connectedHardware...)
			sort.Slice(handles, func(i, j int) bool { return handles[i] < handles[j] })
			for _, hardware := range handles {
				if err := pcanCall(
					"LIN_DisconnectClient",
					p.api.disconnectClient,
					uintptr(p.client),
					uintptr(hardware),
				); err != nil {
					errs = append(errs, err)
				} else {
					logDriverf(logDevicePCAN, "disconnected hardware=0x%04X", hardware)
				}
			}
			if err := pcanCall("LIN_RemoveClient", p.api.removeClient, uintptr(p.client)); err != nil {
				errs = append(errs, err)
			}
			p.client = 0
		}
		if p.api != nil {
			if err := p.api.close(); err != nil {
				errs = append(errs, fmt.Errorf("unload %s: %w", pcanDLLName, err))
			}
			p.api = nil
		}
		p.closeErr = errors.Join(errs...)
	})
	return p.closeErr
}
