//go:build windows

package driver

import (
	"encoding/binary"
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
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	vectorDLLName32 = "vxlapi.dll"
	vectorDLLName64 = "vxlapi64.dll"

	vectorBusTypeLIN       = 0x00000002
	vectorInterfaceVersion = 3
	vectorActivateNone     = 0
	vectorInvalidPort      = -1
	vectorSuccess          = 0
	vectorQueueIsEmpty     = 10

	vectorLINMessageTag = 20
	vectorLINErrorTag   = 21
	vectorLINSyncErrTag = 22
	vectorLINNoAnswer   = 23

	vectorLINMessageFlagTX       = 0x40
	vectorLINMessageFlagCRCError = 0x81
	vectorLINCalcChecksumClassic = 0x0100
	vectorLINCalcChecksumEnh     = 0x0200
	vectorLINSlaveOn             = 0xFF
	vectorLINSlaveOff            = 0x00

	vectorDefaultBaudrate   = 19200
	vectorDefaultRxQueue    = 16384
	vectorEventQueueSize    = 128
	vectorReceivePollDelay  = time.Millisecond
	vectorMaximumFrameID    = 0x3F
	vectorMaximumPayloadLen = 8
)

// VectorLINMode is the node mode passed to xlLinSetChannelParams.
type VectorLINMode uint32

const (
	VectorLINMaster VectorLINMode = 1
	VectorLINSlave  VectorLINMode = 2
)

// VectorLINVersion is the LIN protocol version passed to the Vector driver.
type VectorLINVersion uint32

const (
	VectorLINVersion13 VectorLINVersion = 1
	VectorLINVersion20 VectorLINVersion = 2
	VectorLINVersion21 VectorLINVersion = 3
)

// VectorConfig configures a Vector XL LIN port.
//
// By default logical channel N maps to hardware channel N on DeviceType and
// DeviceIndex. With UseAppConfig enabled, logical channel N instead maps to
// application channel N in Vector Hardware Config. HardwareChannels and
// AppChannels can override either mapping independently per logical channel.
type VectorConfig struct {
	AppName          string
	DLLPath          string
	UseAppConfig     bool
	DeviceType       int
	DeviceIndex      int
	HardwareChannels map[liniface.Channel]int
	AppChannels      map[liniface.Channel]uint32
	Channels         []liniface.Channel
	Mode             VectorLINMode
	Baudrate         int
	Version          VectorLINVersion
	DLC              map[byte]byte
	Checksum         map[byte]liniface.ChecksumType
	RxQueueSize      uint32
}

// DefaultVectorConfig returns a 19200-baud LIN 2.1 master configuration.
// If channels is empty, logical/hardware channel 0 is selected.
func DefaultVectorConfig(deviceType int, channels ...liniface.Channel) VectorConfig {
	if len(channels) == 0 {
		channels = []liniface.Channel{0}
	}
	return VectorConfig{
		AppName:     "linbuskit",
		DeviceType:  deviceType,
		Channels:    append([]liniface.Channel(nil), channels...),
		Mode:        VectorLINMaster,
		Baudrate:    vectorDefaultBaudrate,
		Version:     VectorLINVersion21,
		RxQueueSize: vectorDefaultRxQueue,
	}
}

type xlLINChannelParams struct {
	Mode     uint32
	Baudrate int32
	Version  uint32
	Reserved uint32
}

// xlEvent is XLevent from vxlapi.h. TagData is the 32-byte union backing all
// classic CAN/LIN event payloads. The layout is 48 bytes on both 386 and amd64.
type xlEvent struct {
	Tag          uint8
	ChannelIndex uint8
	TransID      uint16
	PortHandle   uint16
	Flags        uint8
	Reserved     uint8
	Timestamp    uint64
	TagData      [32]byte
}

type xlLINMessage struct {
	ID    uint8
	DLC   uint8
	Flags [2]byte
	Data  [8]byte
	CRC   uint8
}

type vectorChannel struct {
	index int32
	mask  uint64
}

type vectorAPI struct {
	dll *syscall.LazyDLL

	openDriver        *syscall.LazyProc
	closeDriver       *syscall.LazyProc
	getAppConfig      *syscall.LazyProc
	getChannelIndex   *syscall.LazyProc
	getChannelMask    *syscall.LazyProc
	openPort          *syscall.LazyProc
	closePort         *syscall.LazyProc
	activateChannel   *syscall.LazyProc
	deactivateChannel *syscall.LazyProc
	receive           *syscall.LazyProc
	getErrorString    *syscall.LazyProc
	linSetParams      *syscall.LazyProc
	linSetDLC         *syscall.LazyProc
	linSetChecksum    *syscall.LazyProc
	linSetSlave       *syscall.LazyProc
	linSwitchSlave    *syscall.LazyProc
	linSendRequest    *syscall.LazyProc
	linWakeUp         *syscall.LazyProc
	linSetSleepMode   *syscall.LazyProc
	flushReceiveQueue *syscall.LazyProc
	getQueueLevel     *syscall.LazyProc
}

// Vector implements liniface.Driver using the Vector XL Driver Library.
type Vector struct {
	stateMu   sync.RWMutex
	callMu    sync.Mutex
	readMu    sync.Mutex
	eventMu   sync.Mutex
	closeOnce sync.Once

	api        *vectorAPI
	config     VectorConfig
	portHandle int32
	accessMask uint64
	permission uint64
	closed     bool
	closeErr   error

	channelInfo    map[liniface.Channel]vectorChannel
	logicalByIndex map[uint8]liniface.Channel
	eventChans     map[liniface.Channel]chan *liniface.LinEvent
	dlcTable       [64]byte
	checksumTable  [64]liniface.ChecksumType
}

var _ liniface.Driver = (*Vector)(nil)

// NewVector opens the selected Vector hardware channels as a LIN master.
func NewVector(deviceType int, channels ...liniface.Channel) (*Vector, error) {
	return NewVectorWithConfig(DefaultVectorConfig(deviceType, channels...))
}

// NewVectorWithConfig loads vxlapi, opens the configured port, configures LIN,
// and activates all selected channels.
func NewVectorWithConfig(config VectorConfig) (*Vector, error) {
	normalized, dlc, checksum, err := normalizeVectorConfig(config)
	if err != nil {
		return nil, err
	}
	api, err := loadVectorAPI(normalized.DLLPath)
	if err != nil {
		return nil, err
	}
	v := &Vector{
		api:            api,
		config:         normalized,
		portHandle:     vectorInvalidPort,
		channelInfo:    make(map[liniface.Channel]vectorChannel, len(normalized.Channels)),
		logicalByIndex: make(map[uint8]liniface.Channel, len(normalized.Channels)),
		eventChans:     make(map[liniface.Channel]chan *liniface.LinEvent),
		dlcTable:       dlc,
		checksumTable:  checksum,
	}
	if err := v.open(); err != nil {
		_ = v.Close()
		return nil, err
	}
	return v, nil
}

func normalizeVectorConfig(config VectorConfig) (VectorConfig, [64]byte, [64]liniface.ChecksumType, error) {
	var dlc [64]byte
	var checksum [64]liniface.ChecksumType
	if config.AppName == "" {
		config.AppName = "linbuskit"
	}
	if strings.IndexByte(config.AppName, 0) >= 0 {
		return config, dlc, checksum, errors.New("Vector application name contains a NUL byte")
	}
	if config.Mode == 0 {
		config.Mode = VectorLINMaster
	}
	if config.Mode != VectorLINMaster && config.Mode != VectorLINSlave {
		return config, dlc, checksum, fmt.Errorf("invalid Vector LIN mode %d", config.Mode)
	}
	if config.Version == 0 {
		config.Version = VectorLINVersion21
	}
	if config.Version < VectorLINVersion13 || config.Version > VectorLINVersion21 {
		return config, dlc, checksum, fmt.Errorf("invalid Vector LIN version %d", config.Version)
	}
	if config.Baudrate == 0 {
		config.Baudrate = vectorDefaultBaudrate
	}
	if config.Baudrate < 1000 || config.Baudrate > 20000 {
		return config, dlc, checksum, fmt.Errorf("invalid Vector LIN baudrate %d (expected 1000..20000)", config.Baudrate)
	}
	if config.RxQueueSize == 0 {
		config.RxQueueSize = vectorDefaultRxQueue
	}
	if len(config.Channels) == 0 {
		config.Channels = []liniface.Channel{0}
	} else {
		config.Channels = append([]liniface.Channel(nil), config.Channels...)
	}
	config.HardwareChannels = cloneMap(config.HardwareChannels)
	config.AppChannels = cloneMap(config.AppChannels)
	config.DLC = cloneMap(config.DLC)
	config.Checksum = cloneMap(config.Checksum)
	if !config.UseAppConfig && config.DeviceType <= 0 {
		return config, dlc, checksum, fmt.Errorf("invalid Vector hardware type %d", config.DeviceType)
	}

	seen := make(map[liniface.Channel]struct{}, len(config.Channels))
	for _, channel := range config.Channels {
		if _, exists := seen[channel]; exists {
			return config, dlc, checksum, fmt.Errorf("duplicate Vector LIN channel %d", channel)
		}
		seen[channel] = struct{}{}
		if hardwareChannel, ok := config.HardwareChannels[channel]; ok && hardwareChannel < 0 {
			return config, dlc, checksum, fmt.Errorf("invalid hardware channel %d for logical channel %d", hardwareChannel, channel)
		}
	}

	for id := range dlc {
		dlc[id] = vectorMaximumPayloadLen
		if config.Version == VectorLINVersion13 || id == 0x3C || id == 0x3D {
			checksum[id] = liniface.ClassicChecksum
		} else {
			checksum[id] = liniface.EnhancedChecksum
		}
	}
	for id, length := range config.DLC {
		if id > vectorMaximumFrameID {
			return config, dlc, checksum, fmt.Errorf("invalid Vector DLC frame ID 0x%02X", id)
		}
		if length > vectorMaximumPayloadLen {
			return config, dlc, checksum, fmt.Errorf("invalid Vector DLC %d for frame 0x%02X", length, id)
		}
		if (id == 0x3C || id == 0x3D) && length != vectorMaximumPayloadLen {
			return config, dlc, checksum, fmt.Errorf("LIN diagnostic frame 0x%02X requires DLC 8", id)
		}
		dlc[id] = length
	}
	for id, kind := range config.Checksum {
		if id > vectorMaximumFrameID {
			return config, dlc, checksum, fmt.Errorf("invalid Vector checksum frame ID 0x%02X", id)
		}
		if kind != liniface.ClassicChecksum && kind != liniface.EnhancedChecksum {
			return config, dlc, checksum, fmt.Errorf("invalid Vector checksum type %d for frame 0x%02X", kind, id)
		}
		if config.Version == VectorLINVersion13 && kind != liniface.ClassicChecksum {
			return config, dlc, checksum, fmt.Errorf("LIN 1.3 frame 0x%02X requires classic checksum", id)
		}
		if (id == 0x3C || id == 0x3D) && kind != liniface.ClassicChecksum {
			return config, dlc, checksum, fmt.Errorf("LIN diagnostic frame 0x%02X requires classic checksum", id)
		}
		checksum[id] = kind
	}
	return config, dlc, checksum, nil
}

func (v *Vector) open() error {
	if err := v.api.status("xlOpenDriver", v.api.openDriver); err != nil {
		return err
	}
	if err := v.resolveChannels(); err != nil {
		return err
	}

	v.permission = v.accessMask
	name, err := syscall.BytePtrFromString(v.config.AppName)
	if err != nil {
		return fmt.Errorf("encode Vector application name: %w", err)
	}
	args := []uintptr{uintptr(unsafe.Pointer(&v.portHandle)), uintptr(unsafe.Pointer(name))}
	args = appendXLAccess(args, v.accessMask)
	args = append(args,
		uintptr(unsafe.Pointer(&v.permission)),
		uintptr(v.config.RxQueueSize),
		uintptr(vectorInterfaceVersion),
		uintptr(vectorBusTypeLIN),
	)
	if err := v.api.statusArgs("xlOpenPort", v.api.openPort, args...); err != nil {
		return err
	}
	if v.permission&v.accessMask != v.accessMask {
		return fmt.Errorf("xlOpenPort did not grant init access for all Vector LIN channels (requested=0x%X granted=0x%X)", v.accessMask, v.permission)
	}

	params := xlLINChannelParams{Mode: uint32(v.config.Mode), Baudrate: int32(v.config.Baudrate), Version: uint32(v.config.Version)}
	if err := v.api.linSetChannelParams(v.portHandle, v.accessMask, &params); err != nil {
		return err
	}
	if err := v.api.statusAccess("xlLinSetDLC", v.api.linSetDLC, v.portHandle, v.accessMask, uintptr(unsafe.Pointer(&v.dlcTable[0]))); err != nil {
		return err
	}
	checksumBytes := vectorChecksumBytes(v.checksumTable)
	if err := v.api.statusAccess("xlLinSetChecksum", v.api.linSetChecksum, v.portHandle, v.accessMask, uintptr(unsafe.Pointer(&checksumBytes[0]))); err != nil {
		return err
	}
	if err := v.api.statusAccess("xlActivateChannel", v.api.activateChannel, v.portHandle, v.accessMask, uintptr(vectorBusTypeLIN), uintptr(vectorActivateNone)); err != nil {
		return err
	}
	logDriverf(
		logDeviceVector,
		"initialized channels=%v mode=%s baudrate=%d version=%s",
		v.config.Channels,
		v.config.Mode,
		v.config.Baudrate,
		v.config.Version,
	)
	return nil
}

func (v *Vector) resolveChannels() error {
	usedIndexes := make(map[int32]liniface.Channel, len(v.config.Channels))
	for _, logical := range v.config.Channels {
		hwType := v.config.DeviceType
		hwIndex := v.config.DeviceIndex
		hwChannel := int(logical)
		if configured, ok := v.config.HardwareChannels[logical]; ok {
			hwChannel = configured
		}
		if v.config.UseAppConfig {
			appChannel := uint32(logical)
			if configured, ok := v.config.AppChannels[logical]; ok {
				appChannel = configured
			}
			var typ, index, channel uint32
			name, err := syscall.BytePtrFromString(v.config.AppName)
			if err != nil {
				return err
			}
			if err := v.api.statusArgs(
				"xlGetApplConfig",
				v.api.getAppConfig,
				uintptr(unsafe.Pointer(name)),
				uintptr(appChannel),
				uintptr(unsafe.Pointer(&typ)),
				uintptr(unsafe.Pointer(&index)),
				uintptr(unsafe.Pointer(&channel)),
				uintptr(vectorBusTypeLIN),
			); err != nil {
				return fmt.Errorf("resolve Vector application channel %d: %w", appChannel, err)
			}
			hwType, hwIndex, hwChannel = int(typ), int(index), int(channel)
		}

		idxResult, _, _ := v.api.getChannelIndex.Call(uintptr(hwType), uintptr(hwIndex), uintptr(hwChannel))
		index := int32(idxResult)
		if index < 0 || index > 63 {
			return fmt.Errorf("Vector LIN channel not found (logical=%d hwType=%d hwIndex=%d hwChannel=%d)", logical, hwType, hwIndex, hwChannel)
		}
		mask := v.api.channelMask(hwType, hwIndex, hwChannel)
		if mask == 0 {
			return fmt.Errorf("Vector LIN channel mask not found (logical=%d hwType=%d hwIndex=%d hwChannel=%d)", logical, hwType, hwIndex, hwChannel)
		}
		if expected := uint64(1) << uint(index); mask != expected {
			return fmt.Errorf("Vector returned inconsistent channel mapping: index=%d mask=0x%X expected=0x%X", index, mask, expected)
		}
		if previous, exists := usedIndexes[index]; exists {
			return fmt.Errorf("logical Vector LIN channels %d and %d map to the same hardware channel index %d", previous, logical, index)
		}
		usedIndexes[index] = logical
		v.channelInfo[logical] = vectorChannel{index: index, mask: mask}
		v.logicalByIndex[uint8(index)] = logical
		v.accessMask |= mask
	}
	return nil
}

func vectorChecksumBytes(table [64]liniface.ChecksumType) [60]byte {
	var result [60]byte
	for id := range result {
		if table[id] == liniface.EnhancedChecksum {
			result[id] = 1
		}
	}
	return result
}

// ReadEvent waits for the next LIN frame on channel. A timeout returns
// (nil, nil), matching the other linbuskit hardware drivers.
func (v *Vector) ReadEvent(timeout time.Duration, channel liniface.Channel) (*liniface.LinEvent, error) {
	if err := v.validateChannel(channel); err != nil {
		return nil, err
	}
	events := v.eventChannel(channel)
	deadline := time.Now().Add(timeout)
	for {
		select {
		case event := <-events:
			return event, nil
		default:
		}

		event, empty, err := v.readOne()
		if err != nil {
			return nil, err
		}
		if !empty && event != nil {
			if event.Channel == channel {
				return event, nil
			}
			v.enqueueEvent(event.Channel, event)
			continue
		}
		if timeout <= 0 {
			return nil, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, nil
		}
		timer := time.NewTimer(min(remaining, vectorReceivePollDelay))
		select {
		case event := <-events:
			timer.Stop()
			return event, nil
		case <-timer.C:
		}
	}
}

func (v *Vector) readOne() (*liniface.LinEvent, bool, error) {
	v.readMu.Lock()
	defer v.readMu.Unlock()
	v.callMu.Lock()
	defer v.callMu.Unlock()
	if err := v.validateOpen(); err != nil {
		return nil, false, err
	}

	count := uint32(1)
	var raw xlEvent
	status, _, _ := v.api.receive.Call(uintptr(v.portHandle), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&raw)))
	code := int16(status)
	if code == vectorQueueIsEmpty || count == 0 {
		return nil, true, nil
	}
	if code != vectorSuccess {
		return nil, false, fmt.Errorf("xlReceive: %s", v.api.errorString(code))
	}
	if raw.Tag != vectorLINMessageTag {
		if raw.Tag == vectorLINErrorTag || raw.Tag == vectorLINSyncErrTag || raw.Tag == vectorLINNoAnswer {
			logDriverf(logDeviceVector, "LIN status=bus_event tag=%d channel_index=%d", raw.Tag, raw.ChannelIndex)
		}
		return nil, false, nil
	}
	logical, configured := v.logicalByIndex[raw.ChannelIndex]
	if !configured {
		return nil, false, nil
	}
	event, crc, err := decodeVectorLINEvent(&raw, logical, v.checksumTable)
	if err != nil {
		return nil, false, err
	}
	label := "RX"
	if event.Direction == liniface.TX {
		label = "TX"
	}
	logLINMessage(logDeviceVector, label, logical, event.EventID, crc, event.EventPayload)
	return event, false, nil
}

func decodeVectorLINEvent(raw *xlEvent, logical liniface.Channel, checksumTable [64]liniface.ChecksumType) (*liniface.LinEvent, byte, error) {
	if raw == nil {
		return nil, 0, errors.New("nil Vector XL event")
	}
	message := *(*xlLINMessage)(unsafe.Pointer(&raw.TagData[0]))
	flags := binary.LittleEndian.Uint16(message.Flags[:])
	if message.ID > vectorMaximumFrameID {
		return nil, message.CRC, fmt.Errorf("Vector returned invalid LIN frame ID 0x%02X", message.ID)
	}
	length := int(message.DLC)
	if length > len(message.Data) {
		return nil, message.CRC, fmt.Errorf("Vector returned invalid LIN DLC %d for frame 0x%02X", message.DLC, message.ID)
	}
	if flags&vectorLINMessageFlagCRCError == vectorLINMessageFlagCRCError {
		return nil, message.CRC, fmt.Errorf("Vector LIN checksum error on frame 0x%02X", message.ID)
	}
	direction := liniface.RX
	if flags&vectorLINMessageFlagTX != 0 {
		direction = liniface.TX
	}
	payload := append([]byte(nil), message.Data[:length]...)
	return &liniface.LinEvent{
		Channel:      logical,
		EventID:      message.ID,
		EventPayload: payload,
		ChecksumType: checksumTable[message.ID],
		Direction:    direction,
		Timestamp:    time.Now(),
	}, message.CRC, nil
}

// WriteMessage publishes data and sends its LIN header. It is valid in master
// mode. The local response is left enabled until the same ID is requested from
// a slave or replaced by another write.
func (v *Vector) WriteMessage(event *liniface.LinEvent, channel liniface.Channel) error {
	if err := validateVectorEvent(event); err != nil {
		return err
	}
	if err := v.validateModeAndChannel(VectorLINMaster, channel); err != nil {
		return err
	}
	v.callMu.Lock()
	defer v.callMu.Unlock()
	if err := v.validateOpen(); err != nil {
		return err
	}
	info := v.channelInfo[channel]
	checksum := vectorLINCalcChecksum(event.EventID, event.ChecksumType)
	if err := v.api.linSetSlaveData(v.portHandle, info.mask, event.EventID, event.EventPayload, checksum); err != nil {
		return err
	}
	if err := v.api.linSwitchSlaveMode(v.portHandle, info.mask, event.EventID, vectorLINSlaveOn); err != nil {
		return err
	}
	if err := v.api.linSendRequestForID(v.portHandle, info.mask, event.EventID); err != nil {
		return err
	}
	logLINMessage(logDeviceVector, "TX", channel, event.EventID, 0, event.EventPayload)
	return nil
}

// ScheduleSlaveResponse installs and enables a response for a future master
// header. It is valid in slave mode.
func (v *Vector) ScheduleSlaveResponse(event *liniface.LinEvent, channel liniface.Channel) error {
	if err := validateVectorEvent(event); err != nil {
		return err
	}
	if err := v.validateModeAndChannel(VectorLINSlave, channel); err != nil {
		return err
	}
	v.callMu.Lock()
	defer v.callMu.Unlock()
	if err := v.validateOpen(); err != nil {
		return err
	}
	info := v.channelInfo[channel]
	checksum := vectorLINCalcChecksum(event.EventID, event.ChecksumType)
	if err := v.api.linSetSlaveData(v.portHandle, info.mask, event.EventID, event.EventPayload, checksum); err != nil {
		return err
	}
	if err := v.api.linSwitchSlaveMode(v.portHandle, info.mask, event.EventID, vectorLINSlaveOn); err != nil {
		return err
	}
	logLINMessage(logDeviceVector, "TX_SCHEDULE", channel, event.EventID, 0, event.EventPayload)
	return nil
}

// RequestSlaveResponse disables the local response for frameID and sends only
// the master header, allowing an external slave to publish the response.
func (v *Vector) RequestSlaveResponse(frameID byte, channel liniface.Channel) error {
	if frameID > vectorMaximumFrameID {
		return fmt.Errorf("invalid LIN frame ID 0x%02X", frameID)
	}
	if err := v.validateModeAndChannel(VectorLINMaster, channel); err != nil {
		return err
	}
	v.callMu.Lock()
	defer v.callMu.Unlock()
	if err := v.validateOpen(); err != nil {
		return err
	}
	info := v.channelInfo[channel]
	if err := v.api.linSwitchSlaveMode(v.portHandle, info.mask, frameID, vectorLINSlaveOff); err != nil {
		return err
	}
	if err := v.api.linSendRequestForID(v.portHandle, info.mask, frameID); err != nil {
		return err
	}
	logLINHeader(logDeviceVector, channel, frameID, v.dlcTable[frameID])
	return nil
}

// WakeUp transmits a LIN wake-up pulse on channel.
func (v *Vector) WakeUp(channel liniface.Channel) error {
	if err := v.validateChannel(channel); err != nil {
		return err
	}
	v.callMu.Lock()
	defer v.callMu.Unlock()
	if err := v.validateOpen(); err != nil {
		return err
	}
	return v.api.statusAccess("xlLinWakeUp", v.api.linWakeUp, v.portHandle, v.channelInfo[channel].mask)
}

// SetSleepMode puts a Vector LIN channel into sleep mode. When wakeupID is not
// nil, the hardware also configures that ID as its wake-up request.
func (v *Vector) SetSleepMode(channel liniface.Channel, wakeupID *byte) error {
	if err := v.validateChannel(channel); err != nil {
		return err
	}
	flags := uintptr(1)
	id := byte(0)
	if wakeupID != nil {
		if *wakeupID > vectorMaximumFrameID {
			return fmt.Errorf("invalid LIN wake-up frame ID 0x%02X", *wakeupID)
		}
		flags = 3
		id = *wakeupID
	}
	v.callMu.Lock()
	defer v.callMu.Unlock()
	if err := v.validateOpen(); err != nil {
		return err
	}
	return v.api.statusAccess("xlLinSetSleepMode", v.api.linSetSleepMode, v.portHandle, v.channelInfo[channel].mask, flags, uintptr(id))
}

// FlushReceiveQueue discards all queued Vector events for this port.
func (v *Vector) FlushReceiveQueue() error {
	v.callMu.Lock()
	defer v.callMu.Unlock()
	if err := v.validateOpen(); err != nil {
		return err
	}
	return v.api.statusArgs("xlFlushReceiveQueue", v.api.flushReceiveQueue, uintptr(v.portHandle))
}

// ReceiveQueueLevel returns the number of events currently queued by vxlapi.
func (v *Vector) ReceiveQueueLevel() (int, error) {
	v.callMu.Lock()
	defer v.callMu.Unlock()
	if err := v.validateOpen(); err != nil {
		return 0, err
	}
	var level int32
	if err := v.api.statusArgs("xlGetReceiveQueueLevel", v.api.getQueueLevel, uintptr(v.portHandle), uintptr(unsafe.Pointer(&level))); err != nil {
		return 0, err
	}
	return int(level), nil
}

func validateVectorEvent(event *liniface.LinEvent) error {
	if event == nil {
		return errors.New("nil LIN event")
	}
	if event.EventID > vectorMaximumFrameID {
		return fmt.Errorf("invalid LIN frame ID 0x%02X", event.EventID)
	}
	if len(event.EventPayload) == 0 || len(event.EventPayload) > vectorMaximumPayloadLen {
		return fmt.Errorf("invalid LIN payload length %d (expected 1..8)", len(event.EventPayload))
	}
	if event.ChecksumType != liniface.ClassicChecksum && event.ChecksumType != liniface.EnhancedChecksum {
		return fmt.Errorf("invalid LIN checksum type %d", event.ChecksumType)
	}
	if (event.EventID == 0x3C || event.EventID == 0x3D) && event.ChecksumType != liniface.ClassicChecksum {
		return fmt.Errorf("LIN diagnostic frame 0x%02X requires classic checksum", event.EventID)
	}
	return nil
}

func vectorLINCalcChecksum(frameID byte, checksum liniface.ChecksumType) uint16 {
	if frameID == 0x3C || frameID == 0x3D || checksum == liniface.ClassicChecksum {
		return vectorLINCalcChecksumClassic
	}
	return vectorLINCalcChecksumEnh
}

func (v *Vector) validateModeAndChannel(mode VectorLINMode, channel liniface.Channel) error {
	if err := v.validateChannel(channel); err != nil {
		return err
	}
	if v.config.Mode != mode {
		return fmt.Errorf("Vector LIN channel %d is initialized in %s mode; operation requires %s mode", channel, v.config.Mode, mode)
	}
	return nil
}

func (v *Vector) validateChannel(channel liniface.Channel) error {
	if v == nil {
		return liniface.ErrDriverClosed
	}
	v.stateMu.RLock()
	defer v.stateMu.RUnlock()
	if v.closed || v.api == nil || v.portHandle == vectorInvalidPort {
		return liniface.ErrDriverClosed
	}
	if _, ok := v.channelInfo[channel]; !ok {
		return fmt.Errorf("%w: %d", liniface.ErrInvalidChannel, channel)
	}
	return nil
}

func (v *Vector) validateOpen() error {
	if v == nil {
		return liniface.ErrDriverClosed
	}
	v.stateMu.RLock()
	defer v.stateMu.RUnlock()
	if v.closed || v.api == nil || v.portHandle == vectorInvalidPort {
		return liniface.ErrDriverClosed
	}
	return nil
}

func (v *Vector) eventChannel(channel liniface.Channel) chan *liniface.LinEvent {
	v.eventMu.Lock()
	defer v.eventMu.Unlock()
	events := v.eventChans[channel]
	if events == nil {
		events = make(chan *liniface.LinEvent, vectorEventQueueSize)
		v.eventChans[channel] = events
	}
	return events
}

func (v *Vector) enqueueEvent(channel liniface.Channel, event *liniface.LinEvent) {
	select {
	case v.eventChannel(channel) <- event:
	default:
		logDriverf(logDeviceVector, "queue_overflow channel=%d id=0x%02X action=drop", channel, event.EventID)
	}
}

// Config returns a defensive copy of the active Vector configuration.
func (v *Vector) Config() VectorConfig {
	if v == nil {
		return VectorConfig{}
	}
	v.stateMu.RLock()
	defer v.stateMu.RUnlock()
	result := v.config
	result.Channels = append([]liniface.Channel(nil), v.config.Channels...)
	result.HardwareChannels = cloneMap(v.config.HardwareChannels)
	result.AppChannels = cloneMap(v.config.AppChannels)
	result.DLC = cloneMap(v.config.DLC)
	result.Checksum = cloneMap(v.config.Checksum)
	return result
}

func cloneMap[K comparable, V any](source map[K]V) map[K]V {
	if source == nil {
		return nil
	}
	result := make(map[K]V, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func (m VectorLINMode) String() string {
	switch m {
	case VectorLINMaster:
		return "master"
	case VectorLINSlave:
		return "slave"
	default:
		return fmt.Sprintf("mode(%d)", m)
	}
}

func (v VectorLINVersion) String() string {
	switch v {
	case VectorLINVersion13:
		return "1.3"
	case VectorLINVersion20:
		return "2.0"
	case VectorLINVersion21:
		return "2.1"
	default:
		return fmt.Sprintf("version(%d)", v)
	}
}

// Close deactivates the channels and closes the XL port and driver.
func (v *Vector) Close() error {
	if v == nil {
		return nil
	}
	v.closeOnce.Do(func() {
		v.stateMu.Lock()
		v.closed = true
		v.stateMu.Unlock()

		v.callMu.Lock()
		defer v.callMu.Unlock()
		var errs []error
		if v.api != nil && v.portHandle != vectorInvalidPort {
			if v.accessMask != 0 {
				if err := v.api.statusAccess("xlDeactivateChannel", v.api.deactivateChannel, v.portHandle, v.accessMask); err != nil {
					errs = append(errs, err)
				}
			}
			if err := v.api.statusArgs("xlClosePort", v.api.closePort, uintptr(v.portHandle)); err != nil {
				errs = append(errs, err)
			}
			v.portHandle = vectorInvalidPort
		}
		if v.api != nil {
			if err := v.api.status("xlCloseDriver", v.api.closeDriver); err != nil {
				errs = append(errs, err)
			}
		}
		v.closeErr = errors.Join(errs...)
		if v.closeErr != nil {
			logDriverf(logDeviceVector, "disconnect status=failed error=%v", v.closeErr)
		} else {
			logDriverf(logDeviceVector, "disconnected")
		}
	})
	return v.closeErr
}

func vectorDLLName() string {
	if runtime.GOARCH == "386" {
		return vectorDLLName32
	}
	return vectorDLLName64
}

func vectorDLLCandidates(override string) []string {
	if override != "" {
		return []string{override}
	}
	dllName := vectorDLLName()
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
	if path, err := getVectorDLLFromRegistry(dllName); err == nil {
		add(path)
	}
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	if runtime.GOARCH == "386" {
		add(filepath.Join(systemRoot, "SysWOW64", dllName))
	}
	add(filepath.Join(systemRoot, "System32", dllName))
	add(filepath.Join(".", "bin", dllName))
	add(dllName)
	return candidates
}

func getVectorDLLFromRegistry(dllName string) (string, error) {
	access := uint32(registry.QUERY_VALUE)
	if runtime.GOARCH == "386" {
		access |= registry.WOW64_32KEY
	} else {
		access |= registry.WOW64_64KEY
	}
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\SharedDlls`, access)
	if err != nil {
		return "", err
	}
	defer key.Close()
	names, err := key.ReadValueNames(-1)
	if err != nil {
		return "", err
	}
	for _, name := range names {
		if strings.EqualFold(filepath.Base(name), dllName) {
			return name, nil
		}
	}
	return "", fmt.Errorf("%s not found in SharedDlls", dllName)
}

func loadVectorAPI(override string) (*vectorAPI, error) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "386" {
		return nil, fmt.Errorf("Vector XL LIN is unsupported on Windows/%s", runtime.GOARCH)
	}
	var loadErrors []string
	for _, candidate := range vectorDLLCandidates(override) {
		dll := syscall.NewLazyDLL(candidate)
		if err := dll.Load(); err != nil {
			loadErrors = append(loadErrors, fmt.Sprintf("%s: %v", candidate, err))
			continue
		}
		api := &vectorAPI{
			dll:               dll,
			openDriver:        dll.NewProc("xlOpenDriver"),
			closeDriver:       dll.NewProc("xlCloseDriver"),
			getAppConfig:      dll.NewProc("xlGetApplConfig"),
			getChannelIndex:   dll.NewProc("xlGetChannelIndex"),
			getChannelMask:    dll.NewProc("xlGetChannelMask"),
			openPort:          dll.NewProc("xlOpenPort"),
			closePort:         dll.NewProc("xlClosePort"),
			activateChannel:   dll.NewProc("xlActivateChannel"),
			deactivateChannel: dll.NewProc("xlDeactivateChannel"),
			receive:           dll.NewProc("xlReceive"),
			getErrorString:    dll.NewProc("xlGetErrorString"),
			linSetParams:      dll.NewProc("xlLinSetChannelParams"),
			linSetDLC:         dll.NewProc("xlLinSetDLC"),
			linSetChecksum:    dll.NewProc("xlLinSetChecksum"),
			linSetSlave:       dll.NewProc("xlLinSetSlave"),
			linSwitchSlave:    dll.NewProc("xlLinSwitchSlave"),
			linSendRequest:    dll.NewProc("xlLinSendRequest"),
			linWakeUp:         dll.NewProc("xlLinWakeUp"),
			linSetSleepMode:   dll.NewProc("xlLinSetSleepMode"),
			flushReceiveQueue: dll.NewProc("xlFlushReceiveQueue"),
			getQueueLevel:     dll.NewProc("xlGetReceiveQueueLevel"),
		}
		procedures := map[string]*syscall.LazyProc{
			"xlOpenDriver":           api.openDriver,
			"xlCloseDriver":          api.closeDriver,
			"xlGetApplConfig":        api.getAppConfig,
			"xlGetChannelIndex":      api.getChannelIndex,
			"xlGetChannelMask":       api.getChannelMask,
			"xlOpenPort":             api.openPort,
			"xlClosePort":            api.closePort,
			"xlActivateChannel":      api.activateChannel,
			"xlDeactivateChannel":    api.deactivateChannel,
			"xlReceive":              api.receive,
			"xlGetErrorString":       api.getErrorString,
			"xlLinSetChannelParams":  api.linSetParams,
			"xlLinSetDLC":            api.linSetDLC,
			"xlLinSetChecksum":       api.linSetChecksum,
			"xlLinSetSlave":          api.linSetSlave,
			"xlLinSwitchSlave":       api.linSwitchSlave,
			"xlLinSendRequest":       api.linSendRequest,
			"xlLinWakeUp":            api.linWakeUp,
			"xlLinSetSleepMode":      api.linSetSleepMode,
			"xlFlushReceiveQueue":    api.flushReceiveQueue,
			"xlGetReceiveQueueLevel": api.getQueueLevel,
		}
		names := make([]string, 0, len(procedures))
		for name := range procedures {
			names = append(names, name)
		}
		sort.Strings(names)
		missing := ""
		for _, name := range names {
			if err := procedures[name].Find(); err != nil {
				missing = fmt.Sprintf("%s: %v", name, err)
				break
			}
		}
		if missing != "" {
			loadErrors = append(loadErrors, fmt.Sprintf("%s: %s", candidate, missing))
			continue
		}
		return api, nil
	}
	return nil, fmt.Errorf("failed to load %s (%s)", vectorDLLName(), strings.Join(loadErrors, "; "))
}

func appendXLAccess(args []uintptr, mask uint64) []uintptr {
	args = append(args, uintptr(mask))
	if runtime.GOARCH == "386" {
		args = append(args, uintptr(mask>>32))
	}
	return args
}

func (api *vectorAPI) status(operation string, proc *syscall.LazyProc) error {
	return api.statusArgs(operation, proc)
}

func (api *vectorAPI) statusArgs(operation string, proc *syscall.LazyProc, args ...uintptr) error {
	result, _, _ := proc.Call(args...)
	status := int16(result)
	if status != vectorSuccess {
		return fmt.Errorf("%s: %s", operation, api.errorString(status))
	}
	return nil
}

func (api *vectorAPI) statusAccess(operation string, proc *syscall.LazyProc, port int32, mask uint64, suffix ...uintptr) error {
	args := []uintptr{uintptr(port)}
	args = appendXLAccess(args, mask)
	args = append(args, suffix...)
	return api.statusArgs(operation, proc, args...)
}

func (api *vectorAPI) linSetChannelParams(port int32, mask uint64, params *xlLINChannelParams) error {
	args := []uintptr{uintptr(port)}
	args = appendXLAccess(args, mask)
	if runtime.GOARCH == "386" {
		// xlLinSetChannelParams takes XLlinStatPar by value. On x86 its four
		// 32-bit fields are copied directly onto the stdcall stack.
		args = append(args, uintptr(params.Mode), uintptr(uint32(params.Baudrate)), uintptr(params.Version), uintptr(params.Reserved))
	} else {
		// The Windows x64 ABI passes a 16-byte aggregate indirectly.
		args = append(args, uintptr(unsafe.Pointer(params)))
	}
	return api.statusArgs("xlLinSetChannelParams", api.linSetParams, args...)
}

func (api *vectorAPI) linSetSlaveData(port int32, mask uint64, id byte, payload []byte, checksum uint16) error {
	var data [8]byte
	copy(data[:], payload)
	return api.statusAccess(
		"xlLinSetSlave",
		api.linSetSlave,
		port,
		mask,
		uintptr(id),
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(payload)),
		uintptr(checksum),
	)
}

func (api *vectorAPI) linSwitchSlaveMode(port int32, mask uint64, id byte, mode byte) error {
	return api.statusAccess("xlLinSwitchSlave", api.linSwitchSlave, port, mask, uintptr(id), uintptr(mode))
}

func (api *vectorAPI) linSendRequestForID(port int32, mask uint64, id byte) error {
	return api.statusAccess("xlLinSendRequest", api.linSendRequest, port, mask, uintptr(id), 0)
}

func (api *vectorAPI) channelMask(hwType, hwIndex, hwChannel int) uint64 {
	low, high, _ := api.getChannelMask.Call(uintptr(hwType), uintptr(hwIndex), uintptr(hwChannel))
	if runtime.GOARCH == "386" {
		return uint64(uint32(low)) | uint64(uint32(high))<<32
	}
	return uint64(low)
}

func (api *vectorAPI) errorString(status int16) string {
	if api == nil || api.getErrorString == nil {
		return fmt.Sprintf("XLstatus %d", status)
	}
	pointer, _, _ := api.getErrorString.Call(uintptr(uint16(status)))
	if pointer == 0 {
		return fmt.Sprintf("XLstatus %d", status)
	}
	return fmt.Sprintf("%s (XLstatus %d)", windows.BytePtrToString((*byte)(unsafe.Pointer(pointer))), status)
}
