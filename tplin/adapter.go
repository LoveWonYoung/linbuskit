package tplin

import "time"

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

// TransportConfig holds configuration options for the transport layer.
type TransportConfig struct {
	TxQueueSize       int
	RxQueueSize       int
	PollInterval      time.Duration
	ReadTimeout       time.Duration
	MultiFrameTimeout time.Duration // 多帧接收超时
	// ContinuousSlavePoll 为 true 时，Master 在空闲时也会持续请求 0x3D；
	// 为 false（默认）时，仅在等待从节点响应或多帧接收未完成时才请求 0x3D。
	ContinuousSlavePoll bool
}
