package tplin

import "time"

type Direction int
type ChecksumType int
type PCIType byte

const (
	ClassicChecksum  ChecksumType = iota // Up to LIN 1.3
	EnhancedChecksum                     // LIN 2.0 and later
)
const (
	RX Direction = iota // Received from the bus
	TX                  // Transmitted to the bus
)
const (
	SF PCIType = 0 // Single Frame
	FF PCIType = 1 // First Frame
	CF PCIType = 2 // Consecutive Frame
)

const (
	// MasterDiagnosticFrameID Frame IDs
	MasterDiagnosticFrameID = 0x3C
	SlaveDiagnosticFrameID  = 0x3D

	// BroadcastNAD NADs
	BroadcastNAD = 0x7F

	// ReadByIdentifierSID SIDs (Service Identifiers)
	ReadByIdentifierSID           = 0xB2
	AssignFrameIdentifierRangeSID = 0xB7
	AssignNadSID                  = 0xB0
	SaveConfigurationSID          = 0xB6
	ConditionalChangeNadSID       = 0xB3
	AssignFrameIdentifierSID      = 0xB1
	DataDumpSID                   = 0xB4
	AssignNadViaSnpdSID           = 0xB5

	// DataIdentifierLinProductIdentifier Data Identifiers
	DataIdentifierLinProductIdentifier = 0
	DataIdentifierSerialNumber         = 1

	// BroadcastSupplierID Broadcast Identifiers
	BroadcastSupplierID = 0x7FFF
	BroadcastFunctionID = 0xFFFF

	// DefaultTxQueueSize Default configuration values
	DefaultTxQueueSize       = 10
	DefaultRxQueueSize       = 10
	DefaultPollInterval      = 10 * time.Millisecond
	DefaultReadTimeout       = 10 * time.Millisecond
	DefaultMultiFrameTimeout = 1 * time.Second // 多帧接收超时
)

// LinEvent represents a raw, low-level LIN frame/event.
type LinEvent struct {
	EventID      byte
	EventPayload []byte
	ChecksumType ChecksumType
	Direction    Direction
	Timestamp    time.Time
}

// Driver is the interface that abstracts the underlying LIN hardware or simulation.
type Driver interface {
	ReadEvent(timeout time.Duration) (*LinEvent, error)
	WriteMessage(event *LinEvent) error
	ScheduleSlaveResponse(event *LinEvent) error
	RequestSlaveResponse(frameID byte) error
}

// TransportConfig holds configuration options for the transport layer.
type TransportConfig struct {
	TxQueueSize       int
	RxQueueSize       int
	PollInterval      time.Duration
	ReadTimeout       time.Duration
	MultiFrameTimeout time.Duration // 多帧接收超时
}
