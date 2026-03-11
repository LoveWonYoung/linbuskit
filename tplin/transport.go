package tplin

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/LoveWonYoung/linbuskit/liniface"
)

// DefaultTransportConfig returns a configuration with sensible defaults.
func DefaultTransportConfig() TransportConfig {
	return TransportConfig{
		TxQueueSize:       DefaultTxQueueSize,
		RxQueueSize:       DefaultRxQueueSize,
		PollInterval:      DefaultPollInterval,
		ReadTimeout:       DefaultReadTimeout,
		MultiFrameTimeout: DefaultMultiFrameTimeout,
	}
}

// bufferPool is used to reduce memory allocations for frame payloads.
var bufferPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 8)
		return &buf
	},
}

// getBuffer retrieves a buffer from the pool.
func getBuffer() *[]byte {
	return bufferPool.Get().(*[]byte)
}

// putBuffer returns a buffer to the pool after resetting it.
func putBuffer(buf *[]byte) {
	for i := range *buf {
		(*buf)[i] = 0xFF
	}
	bufferPool.Put(buf)
}

// Transport handles the logic of the LIN transport protocol (TP).
type Transport struct {
	isSlave          bool
	driver           liniface.Driver
	txQueue          chan *liniface.LinEvent
	rxQueue          chan *LinMessage
	config           TransportConfig
	scheduledTxEvent *liniface.LinEvent

	// State for multi-frame reception (RWMutex for better concurrency)
	stateMutex          sync.RWMutex
	currentFrameData    []byte
	currentSID          byte
	currentNAD          byte
	currentFrameCounter byte
	remainingBytes      int
	multiFrameStartTime time.Time // 多帧接收开始时间

	// Goroutine lifecycle
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// LinMessage represents a fully decoded, high-level diagnostic message.
type LinMessage struct {
	NAD  byte
	SID  byte
	Data []byte
}

// NewTransport creates a new instance of the LIN transport layer with default config.
func NewTransport(isSlave bool, driver liniface.Driver) *Transport {
	return NewTransportWithConfig(isSlave, driver, DefaultTransportConfig())
}

// NewTransportWithConfig creates a new instance of the LIN transport layer with custom config.
func NewTransportWithConfig(isSlave bool, driver liniface.Driver, config TransportConfig) *Transport {
	return &Transport{
		isSlave: isSlave,
		driver:  driver,
		txQueue: make(chan *liniface.LinEvent, config.TxQueueSize),
		rxQueue: make(chan *LinMessage, config.RxQueueSize),
		config:  config,
	}
}

// Run starts the transport layer's background processing goroutine.
func (t *Transport) Run() {
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	t.wg.Add(1)

	go func() {
		defer t.wg.Done()
		ticker := time.NewTicker(t.config.PollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := t.execute(); err != nil {
					log.Printf("Error in transport execution cycle: %v", err)
				}
			}
		}
	}()
}

// Close gracefully stops the background goroutine.
func (t *Transport) Close() {
	if t.cancel != nil {
		t.cancel()
	}
	t.wg.Wait()
}

// execute is the main processing loop called periodically by the background goroutine.
func (t *Transport) execute() error {
	t.checkMultiFrameTimeout()
	for {
		event, err := t.driver.ReadEvent(t.config.ReadTimeout)
		if err != nil {
			return fmt.Errorf("failed to read event from driver: %w", err)
		}
		if event == nil {
			break
		}
		t.receiveFromDriver(event)
	}

	if t.isSlave {
		if t.scheduledTxEvent == nil {
			select {
			case event := <-t.txQueue:
				if err := t.driver.ScheduleSlaveResponse(event); err != nil {
					return fmt.Errorf("slave failed to schedule response: %w", err)
				}
				t.scheduledTxEvent = event
			default:

			}
		}
	} else { // Master logic
		select {
		case event := <-t.txQueue:
			if err := t.driver.WriteMessage(event); err != nil {
				return fmt.Errorf("master failed to write message: %w", err)
			}
		default:
			if err := t.driver.RequestSlaveResponse(SlaveDiagnosticFrameID); err != nil {
				return fmt.Errorf("master failed to request slave response: %w", err)
			}
		}
	}
	return nil
}

// checkMultiFrameTimeout 检查多帧接收是否超时
func (t *Transport) checkMultiFrameTimeout() {
	t.stateMutex.RLock()
	hasOngoing := t.remainingBytes > 0
	startTime := t.multiFrameStartTime
	t.stateMutex.RUnlock()

	if hasOngoing && time.Since(startTime) > t.config.MultiFrameTimeout {
		t.stateMutex.Lock()
		log.Printf("Warning: Multi-frame reception timed out after %v, discarding incomplete message", t.config.MultiFrameTimeout)
		t.resetState()
		t.stateMutex.Unlock()
	}
}

// Receive pops a single, fully reassembled message from the receive queue.
func (t *Transport) Receive() *LinMessage {
	select {
	case msg := <-t.rxQueue:
		return msg
	default:
		return nil
	}
}

// ReceiveBlocking waits for a message or context cancellation.
func (t *Transport) ReceiveBlocking(ctx context.Context) (*LinMessage, error) {
	select {
	case msg := <-t.rxQueue:
		return msg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// resetState safely resets the multi-frame reception state machine.
func (t *Transport) resetState() {
	t.currentFrameData = []byte{}
	t.currentSID = 0
	t.currentNAD = 0
	t.currentFrameCounter = 0
	t.remainingBytes = 0
	t.multiFrameStartTime = time.Time{} // 清除超时跟踪
}

// Transmit takes high-level message data, packages it into one or more
func (t *Transport) Transmit(nad, sid byte, data []byte) {
	var eventID byte
	if t.isSlave {
		eventID = SlaveDiagnosticFrameID
	} else {
		eventID = MasterDiagnosticFrameID
	}
	dataLen := len(data) + 1
	if dataLen <= 6 {
		pci := (byte(liniface.SF) << 4) | byte(dataLen)
		bufPtr := getBuffer()
		payload := *bufPtr
		payload[0] = nad
		payload[1] = pci
		payload[2] = sid
		copy(payload[3:], data)
		eventPayload := make([]byte, 8)
		copy(eventPayload, payload)
		putBuffer(bufPtr)
		t.txQueue <- &liniface.LinEvent{EventID: eventID, EventPayload: eventPayload, ChecksumType: liniface.ClassicChecksum}
	} else {

		pci := (byte(liniface.FF) << 4) | byte(dataLen>>8&0x0F)
		bufPtrFF := getBuffer()
		payloadFF := *bufPtrFF
		payloadFF[0] = nad
		payloadFF[1] = pci
		payloadFF[2] = byte(dataLen & 0xFF)
		payloadFF[3] = sid
		copy(payloadFF[4:], data[:4])
		eventPayloadFF := make([]byte, 8)
		copy(eventPayloadFF, payloadFF)
		putBuffer(bufPtrFF)
		t.txQueue <- &liniface.LinEvent{EventID: eventID, EventPayload: eventPayloadFF, ChecksumType: liniface.ClassicChecksum}

		currentByte := 4
		currentFrame := 0
		for currentByte < len(data) {
			currentFrame = (currentFrame + 1) % 16
			pciCF := (byte(liniface.CF) << 4) | byte(currentFrame)
			bufPtrCF := getBuffer()
			payloadCF := *bufPtrCF
			payloadCF[0] = nad
			payloadCF[1] = pciCF

			endByte := currentByte + 6
			if endByte > len(data) {
				endByte = len(data)
			}
			copy(payloadCF[2:], data[currentByte:endByte])
			currentByte = endByte
			eventPayloadCF := make([]byte, 8)
			copy(eventPayloadCF, payloadCF)
			putBuffer(bufPtrCF)
			t.txQueue <- &liniface.LinEvent{EventID: eventID, EventPayload: eventPayloadCF, ChecksumType: liniface.ClassicChecksum}
		}
	}
}

// receiveFromDriver processes a raw event from the driver and updates the TP state.
func (t *Transport) receiveFromDriver(event *liniface.LinEvent) {
	t.stateMutex.Lock()
	defer t.stateMutex.Unlock()
	if t.isSlave && event.Direction == liniface.TX && t.scheduledTxEvent != nil {
		if t.scheduledTxEvent.EventID == event.EventID {
			t.scheduledTxEvent = nil
			select {
			case nextEvent := <-t.txQueue:
				if err := t.driver.ScheduleSlaveResponse(nextEvent); err != nil {
					log.Printf("slave failed to schedule next response: %v", err)
				}
				t.scheduledTxEvent = nextEvent
			default:
			}
		}
		return
	}

	// Check if the received event is a diagnostic frame relevant to our role
	isMasterReceiving := !t.isSlave && event.EventID == SlaveDiagnosticFrameID
	isSlaveReceiving := t.isSlave && event.EventID == MasterDiagnosticFrameID

	if event.Direction == liniface.RX && (isMasterReceiving || isSlaveReceiving) {
		payload := event.EventPayload
		if !t.isSlave && len(payload) == 0 {
			return // Master ignores empty frames (no-response from slave)
		}
		if len(payload) < 2 {
			return // Frame too short to be a valid diagnostic frame
		}

		nad, pci := payload[0], payload[1]
		pciType := liniface.PCIType(pci >> 4)
		additionalInfo := pci & 0x0F

		switch pciType {
		case liniface.SF:
			if t.remainingBytes > 0 {
				log.Println("Warning: Received a Single-Frame before completing the last multi-frame. Previous frame dropped.")
			}
			t.resetState()
			length := int(additionalInfo)
			dataLength := length - 1
			if len(payload) < 3+dataLength || dataLength < 0 {
				log.Printf("Error: SF with invalid length field. Payload len: %d, PCI len: %d", len(payload), length)
				return
			}
			sid := payload[2]
			data := payload[3 : 3+dataLength]
			t.rxQueue <- &LinMessage{NAD: nad, SID: sid, Data: data}

		case liniface.FF:
			if t.remainingBytes > 0 {
				log.Println("Warning: Received a First-Frame before completing the last one. Previous frame dropped.")
			}
			t.resetState()
			if len(payload) < 8 {
				log.Printf("Error: FF frame is smaller than 8 bytes.")
				return
			}
			length := (int(additionalInfo) << 8) | int(payload[2])
			sid := payload[3]
			t.remainingBytes = length - 1 - 4
			t.currentFrameData = make([]byte, 0, 4+t.remainingBytes)
			t.currentFrameData = append(t.currentFrameData, payload[4:]...)
			t.currentNAD = nad
			t.currentSID = sid
			t.multiFrameStartTime = time.Now()
		case liniface.CF:
			if t.remainingBytes == 0 {
				log.Println("Warning: Received a Consecutive-Frame but was not expecting more bytes. Discarding.")
				t.resetState()
				return
			}
			frameCounter := additionalInfo
			nextFrameCounter := byte(t.currentFrameCounter+1) % 16
			if frameCounter != nextFrameCounter {
				log.Println("Warning: Received an out-of-order Consecutive-Frame. Discarding message.")
				t.resetState()
				return
			}

			length := min(t.remainingBytes, 6)
			if len(payload) < 2+length {
				t.resetState()
				return
			}
			t.remainingBytes -= length
			t.currentFrameData = append(t.currentFrameData, payload[2:2+length]...)
			t.currentFrameCounter = nextFrameCounter

			if t.remainingBytes == 0 {
				msg := &LinMessage{NAD: t.currentNAD, SID: t.currentSID, Data: t.currentFrameData}
				t.rxQueue <- msg
				t.resetState()
			}
		}
	}
}
