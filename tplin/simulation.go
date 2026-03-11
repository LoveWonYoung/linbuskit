package tplin

import (
	"log"
	"sync"
	"time"

	"github.com/LoveWonYoung/linbuskit/liniface"
)

// SimulatedLinNetwork simulates the entire LIN bus network.
// It is responsible for routing messages between the master and slaves.
// It is safe for concurrent use.
type SimulatedLinNetwork struct {
	slaveResponses map[byte]*liniface.LinEvent
	masterDriver   *SimulatedLinDriver
	slaveDrivers   []*SimulatedLinDriver
	mu             sync.Mutex
}

// NewSimulatedLinNetwork creates a new simulation network instance.
func NewSimulatedLinNetwork() *SimulatedLinNetwork {
	return &SimulatedLinNetwork{
		slaveResponses: make(map[byte]*liniface.LinEvent),
	}
}

// deepCopyEvent creates a deep copy of a LinEvent.
func deepCopyEvent(original *liniface.LinEvent) *liniface.LinEvent {
	if original == nil {
		return nil
	}
	cpy := &liniface.LinEvent{
		EventID:      original.EventID,
		ChecksumType: original.ChecksumType,
		Direction:    original.Direction,
		Timestamp:    original.Timestamp,
	}
	cpy.EventPayload = make([]byte, len(original.EventPayload))
	copy(cpy.EventPayload, original.EventPayload)
	return cpy
}

// --- Network methods that simulate the bus behavior ---

func (n *SimulatedLinNetwork) writeMessage(linEvent *liniface.LinEvent) {
	n.mu.Lock()
	defer n.mu.Unlock()

	eventTime := time.Now()

	// 1. Deliver to Master's own queue as a TX event
	if n.masterDriver != nil {
		masterTxEvent := deepCopyEvent(linEvent)
		masterTxEvent.Timestamp = eventTime
		masterTxEvent.Direction = liniface.TX
		n.masterDriver.pushEvent(masterTxEvent)
	}

	// 2. Broadcast to all slaves as an RX event
	for _, slaveDriver := range n.slaveDrivers {
		slaveRxEvent := deepCopyEvent(linEvent)
		slaveRxEvent.Direction = liniface.RX
		slaveRxEvent.Timestamp = eventTime
		slaveDriver.pushEvent(slaveRxEvent)
	}
}

func (n *SimulatedLinNetwork) requestSlaveResponse(messageID byte) {
	n.mu.Lock()
	result, ok := n.slaveResponses[messageID]
	if !ok {
		n.mu.Unlock()
		return // No scheduled response, master will time out
	}
	delete(n.slaveResponses, messageID)
	n.mu.Unlock()

	eventTime := time.Now()

	// 1. Deliver response to Master as an RX event
	if n.masterDriver != nil {
		masterRxEvent := deepCopyEvent(result)
		masterRxEvent.Direction = liniface.RX
		masterRxEvent.Timestamp = eventTime
		n.masterDriver.pushEvent(masterRxEvent)
	}

	// 2. Notify all slaves that the response was sent (as a TX event)
	for _, slaveDriver := range n.slaveDrivers {
		slaveTxEvent := deepCopyEvent(result)
		slaveTxEvent.Direction = liniface.TX
		slaveTxEvent.Timestamp = eventTime
		slaveDriver.pushEvent(slaveTxEvent)
	}
}

func (n *SimulatedLinNetwork) scheduleSlaveResponse(linEvent *liniface.LinEvent) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.slaveResponses[linEvent.EventID] = linEvent
}

// --- Driver factory methods ---

// GetMasterDriver creates and returns a driver for the master node.
func (n *SimulatedLinNetwork) GetMasterDriver() liniface.Driver {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.masterDriver == nil {
		n.masterDriver = newSimulatedLinDriver(n, false)
	}
	return n.masterDriver
}

// CreateSlaveDriver creates and returns a new driver for a slave node.
func (n *SimulatedLinNetwork) CreateSlaveDriver() liniface.Driver {
	n.mu.Lock()
	defer n.mu.Unlock()

	slaveDriver := newSimulatedLinDriver(n, true)
	n.slaveDrivers = append(n.slaveDrivers, slaveDriver)
	return slaveDriver
}

// SimulatedLinDriver implements the Driver interface for simulation purposes.
type SimulatedLinDriver struct {
	isSlave    bool
	network    *SimulatedLinNetwork
	eventQueue chan *liniface.LinEvent
}

func newSimulatedLinDriver(network *SimulatedLinNetwork, isSlave bool) *SimulatedLinDriver {
	return &SimulatedLinDriver{
		isSlave:    isSlave,
		network:    network,
		eventQueue: make(chan *liniface.LinEvent, 20),
	}
}

func (d *SimulatedLinDriver) pushEvent(event *liniface.LinEvent) {
	select {
	case d.eventQueue <- event:
	default:
		log.Println("SimulatedLinDriver: Event queue is full. Discarding event.")
	}
}

// --- Implementation of the Driver interface ---

func (d *SimulatedLinDriver) ReadEvent(timeout time.Duration) (*liniface.LinEvent, error) {
	select {
	case event := <-d.eventQueue:
		return event, nil
	case <-time.After(timeout):
		return nil, nil // Timeout is not an error
	}
}

func (d *SimulatedLinDriver) WriteMessage(linEvent *liniface.LinEvent) error {
	if !d.isSlave {
		d.network.writeMessage(linEvent)
	}
	return nil
}

func (d *SimulatedLinDriver) ScheduleSlaveResponse(linEvent *liniface.LinEvent) error {
	if d.isSlave {
		d.network.scheduleSlaveResponse(linEvent)
	}
	return nil
}

func (d *SimulatedLinDriver) RequestSlaveResponse(messageID byte) error {
	if !d.isSlave {
		d.network.requestSlaveResponse(messageID)
	}
	return nil
}
