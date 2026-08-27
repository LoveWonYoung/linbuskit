package tplin

import (
	"errors"
	"log"
	"sync"
	"time"

	"github.com/LoveWonYoung/linbuskit/liniface"
)

// SimulatedLinNetwork simulates the entire LIN bus network.
// It is responsible for routing messages between the master and slaves.
// It is safe for concurrent use.
type SimulatedLinNetwork struct {
	slaveResponses map[simulatedResponseKey]*liniface.LinEvent
	masterDriver   *SimulatedLinDriver
	slaveDrivers   []*SimulatedLinDriver
	mu             sync.Mutex
}

type simulatedResponseKey struct {
	channel liniface.Channel
	frameID byte
}

// NewSimulatedLinNetwork creates a new simulation network instance.
func NewSimulatedLinNetwork() *SimulatedLinNetwork {
	return &SimulatedLinNetwork{
		slaveResponses: make(map[simulatedResponseKey]*liniface.LinEvent),
	}
}

// deepCopyEvent creates a deep copy of a LinEvent.
func deepCopyEvent(original *liniface.LinEvent) *liniface.LinEvent {
	if original == nil {
		return nil
	}
	cpy := &liniface.LinEvent{
		Channel:      original.Channel,
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

func (n *SimulatedLinNetwork) requestSlaveResponse(messageID byte, channel liniface.Channel) {
	n.mu.Lock()
	key := simulatedResponseKey{channel: channel, frameID: messageID}
	result, ok := n.slaveResponses[key]
	if !ok {
		n.mu.Unlock()
		return // No scheduled response, master will time out
	}
	delete(n.slaveResponses, key)
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
	key := simulatedResponseKey{channel: linEvent.Channel, frameID: linEvent.EventID}
	n.slaveResponses[key] = deepCopyEvent(linEvent)
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
	isSlave     bool
	network     *SimulatedLinNetwork
	eventMu     sync.Mutex
	eventQueues map[liniface.Channel]chan *liniface.LinEvent
	readStates  map[liniface.Channel]*simulatedReadState
}

type simulatedReadState struct {
	mu    sync.Mutex
	timer *time.Timer
}

func newSimulatedLinDriver(network *SimulatedLinNetwork, isSlave bool) *SimulatedLinDriver {
	return &SimulatedLinDriver{
		isSlave:     isSlave,
		network:     network,
		eventQueues: make(map[liniface.Channel]chan *liniface.LinEvent),
		readStates:  make(map[liniface.Channel]*simulatedReadState),
	}
}

func (d *SimulatedLinDriver) pushEvent(event *liniface.LinEvent) {
	select {
	case d.eventQueue(event.Channel) <- event:
	default:
		log.Println("SimulatedLinDriver: Event queue is full. Discarding event.")
	}
}

// --- Implementation of the Driver interface ---

func (d *SimulatedLinDriver) ReadEvent(timeout time.Duration, channel liniface.Channel) (*liniface.LinEvent, error) {
	eventQueue := d.eventQueue(channel)
	if timeout <= 0 {
		select {
		case event := <-eventQueue:
			return event, nil
		default:
			return nil, nil
		}
	}

	state := d.readState(channel)
	state.mu.Lock()
	defer state.mu.Unlock()
	resetTimer(state.timer, timeout)
	select {
	case event := <-eventQueue:
		stopTimer(state.timer)
		return event, nil
	case <-state.timer.C:
		return nil, nil // Timeout is not an error
	}
}

func (d *SimulatedLinDriver) WriteMessage(linEvent *liniface.LinEvent, channel liniface.Channel) error {
	if linEvent == nil {
		return errors.New("nil LIN event")
	}
	if !d.isSlave {
		event := deepCopyEvent(linEvent)
		event.Channel = channel
		d.network.writeMessage(event)
	}
	return nil
}

func (d *SimulatedLinDriver) ScheduleSlaveResponse(linEvent *liniface.LinEvent, channel liniface.Channel) error {
	if linEvent == nil {
		return errors.New("nil LIN event")
	}
	if d.isSlave {
		event := deepCopyEvent(linEvent)
		event.Channel = channel
		d.network.scheduleSlaveResponse(event)
	}
	return nil
}

func (d *SimulatedLinDriver) RequestSlaveResponse(messageID byte, channel liniface.Channel) error {
	if !d.isSlave {
		d.network.requestSlaveResponse(messageID, channel)
	}
	return nil
}

func (d *SimulatedLinDriver) eventQueue(channel liniface.Channel) chan *liniface.LinEvent {
	d.eventMu.Lock()
	defer d.eventMu.Unlock()
	eventQueue := d.eventQueues[channel]
	if eventQueue == nil {
		eventQueue = make(chan *liniface.LinEvent, 20)
		d.eventQueues[channel] = eventQueue
	}
	return eventQueue
}

func (d *SimulatedLinDriver) readState(channel liniface.Channel) *simulatedReadState {
	d.eventMu.Lock()
	defer d.eventMu.Unlock()
	state := d.readStates[channel]
	if state == nil {
		timer := time.NewTimer(time.Hour)
		timer.Stop()
		state = &simulatedReadState{timer: timer}
		d.readStates[channel] = state
	}
	return state
}

func resetTimer(timer *time.Timer, timeout time.Duration) {
	stopTimer(timer)
	timer.Reset(timeout)
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}
