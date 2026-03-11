package liniface

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

// LinEvent represents a raw, low-level LIN frame/event.
type LinEvent struct {
	EventID      byte
	EventPayload []byte
	ChecksumType ChecksumType
	Direction    Direction
	Timestamp    time.Time
}

// Driver abstracts the underlying LIN hardware or simulation.
type Driver interface {
	ReadEvent(timeout time.Duration) (*LinEvent, error)
	WriteMessage(event *LinEvent) error
	ScheduleSlaveResponse(event *LinEvent) error
	RequestSlaveResponse(frameID byte) error
}
