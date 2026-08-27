package tplin

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LoveWonYoung/linbuskit/liniface"
)

var (
	ErrTransportClosed = errors.New("transport is closed")
	ErrTxQueueFull     = errors.New("transport transmit queue is full")
	ErrRxQueueFull     = errors.New("transport receive queue is full")
	ErrMessageTooLong  = errors.New("LIN transport message exceeds 4095 bytes")
)

const maxTransportDataLength = 4094 // 12-bit length includes SID.

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

func newDiagnosticFramePayload() []byte {
	return []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
}

// Transport handles the logic of the LIN transport protocol (TP).
type Transport struct {
	isSlave          bool
	driver           liniface.Driver
	channel          liniface.Channel
	txQueue          chan *liniface.LinEvent
	rxQueue          chan *LinMessage
	config           TransportConfig
	scheduledTxEvent *liniface.LinEvent

	// awaitingSlaveResponse 标记 Master 是否正在等待从节点诊断响应。
	// ContinuousSlavePoll=false 时，仅在此标志为 true（或未完成多帧）时才请求 0x3D。
	awaitingSlaveResponse atomic.Bool

	// State for multi-frame reception (RWMutex for better concurrency)
	stateMutex          sync.RWMutex
	currentFrameData    []byte
	currentSID          byte
	currentNAD          byte
	currentFrameCounter byte
	remainingBytes      int
	multiFrameStartTime time.Time // 多帧接收开始时间

	// Goroutine lifecycle
	lifecycleMu   sync.Mutex
	executeMu     sync.Mutex
	running       bool
	closed        bool
	cancel        context.CancelFunc
	done          chan struct{}
	wake          chan struct{}
	errors        chan error
	receiveErrors chan error
	wg            sync.WaitGroup
	txMu          sync.Mutex
}

// LinMessage represents a fully decoded, high-level diagnostic message.
type LinMessage struct {
	NAD  byte
	SID  byte
	Data []byte
}

// NewTransport creates a new instance of the LIN transport layer with default config.
func NewTransport(isSlave bool, driver liniface.Driver, channel ...liniface.Channel) *Transport {
	return NewTransportWithConfig(isSlave, driver, DefaultTransportConfig(), channel...)
}

// NewTransportWithConfig creates a new instance of the LIN transport layer with custom config.
func NewTransportWithConfig(isSlave bool, driver liniface.Driver, config TransportConfig, channel ...liniface.Channel) *Transport {
	config = normalizeTransportConfig(config)
	var selectedChannel liniface.Channel
	if len(channel) > 0 {
		selectedChannel = channel[0]
	}
	return &Transport{
		isSlave:       isSlave,
		driver:        driver,
		channel:       selectedChannel,
		txQueue:       make(chan *liniface.LinEvent, config.TxQueueSize),
		rxQueue:       make(chan *LinMessage, config.RxQueueSize),
		config:        config,
		done:          make(chan struct{}),
		wake:          make(chan struct{}, 1),
		errors:        make(chan error, 16),
		receiveErrors: make(chan error, 16),
	}
}

func normalizeTransportConfig(config TransportConfig) TransportConfig {
	defaults := DefaultTransportConfig()
	if config.TxQueueSize <= 0 {
		config.TxQueueSize = defaults.TxQueueSize
	}
	if config.RxQueueSize <= 0 {
		config.RxQueueSize = defaults.RxQueueSize
	}
	if config.PollInterval <= 0 {
		config.PollInterval = defaults.PollInterval
	}
	if config.ReadTimeout < 0 {
		config.ReadTimeout = defaults.ReadTimeout
	}
	if config.MultiFrameTimeout <= 0 {
		config.MultiFrameTimeout = defaults.MultiFrameTimeout
	}
	return config
}

// Run starts the transport layer's background processing goroutine.
func (t *Transport) Run() {
	t.lifecycleMu.Lock()
	if t.running || t.closed {
		t.lifecycleMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	t.running = true
	t.wg.Add(1)
	t.lifecycleMu.Unlock()

	go func() {
		defer t.wg.Done()
		ticker := time.NewTicker(t.config.PollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-t.wake:
				if err := t.execute(); err != nil {
					t.reportError(err)
				}
			case <-ticker.C:
				if err := t.execute(); err != nil {
					t.reportError(err)
				}
			}
		}
	}()
}

// Close gracefully stops the background goroutine.
func (t *Transport) Close() {
	t.lifecycleMu.Lock()
	if t.closed {
		done := t.done
		t.lifecycleMu.Unlock()
		<-done
		return
	}
	t.closed = true
	cancel := t.cancel
	t.lifecycleMu.Unlock()
	if cancel != nil {
		cancel()
	}
	t.wg.Wait()
	close(t.errors)
	close(t.receiveErrors)
	close(t.done)
}

// Errors reports asynchronous transport failures.
func (t *Transport) Errors() <-chan error { return t.errors }

func (t *Transport) reportError(err error) {
	if err == nil {
		return
	}
	select {
	case t.errors <- err:
	default:
		log.Printf("transport error channel full: %v", err)
	}
	select {
	case t.receiveErrors <- err:
	default:
	}
}

// execute is the main processing loop called periodically by the background goroutine.
func (t *Transport) execute() error {
	t.executeMu.Lock()
	defer t.executeMu.Unlock()

	t.lifecycleMu.Lock()
	closed := t.closed
	t.lifecycleMu.Unlock()
	if closed {
		return ErrTransportClosed
	}
	t.checkMultiFrameTimeout()

	masterActive := false
	if !t.isSlave {
		masterActive = len(t.txQueue) > 0 || t.shouldRequestSlaveResponse()
		if !masterActive {
			return nil
		}
	}

	readTimeout := t.config.ReadTimeout
	if !t.isSlave && masterActive {
		// 待发 0x3C 或即将请求 0x3D 时不阻塞读，避免 ReadTimeout 叠加 PollInterval 导致帧间隔 ~18ms。
		readTimeout = 0
	}

	receivedCompleteMessage := false
	for {
		event, err := t.driver.ReadEvent(readTimeout, t.channel)
		if err != nil {
			return fmt.Errorf("failed to read event from driver: %w", err)
		}
		if event == nil {
			break
		}
		if t.receiveFromDriver(event) {
			receivedCompleteMessage = true
		}
	}

	if t.isSlave {
		if t.scheduledTxEvent == nil {
			select {
			case event := <-t.txQueue:
				if err := t.driver.ScheduleSlaveResponse(event, t.channel); err != nil {
					return fmt.Errorf("slave failed to schedule response: %w", err)
				}
				t.scheduledTxEvent = event
			default:

			}
		}
	} else { // Master logic
		select {
		case event := <-t.txQueue:
			if err := t.driver.WriteMessage(event, t.channel); err != nil {
				return fmt.Errorf("master failed to write message: %w", err)
			}
		default:
			if !receivedCompleteMessage && t.shouldRequestSlaveResponse() {
				if err := t.driver.RequestSlaveResponse(SlaveDiagnosticFrameID, t.channel); err != nil {
					return fmt.Errorf("master failed to request slave response: %w", err)
				}
			}
		}
	}
	return nil
}

// shouldRequestSlaveResponse 决定 Master 空闲时是否请求 0x3D。
func (t *Transport) shouldRequestSlaveResponse() bool {
	if t.config.ContinuousSlavePoll {
		return true
	}
	if t.awaitingSlaveResponse.Load() {
		return true
	}
	t.stateMutex.RLock()
	ongoing := t.remainingBytes > 0
	t.stateMutex.RUnlock()
	return ongoing
}

// SetAwaitingSlaveResponse updates the master response-wait state. Enabling it
// wakes the transport immediately; disabling it includes the same execution
// barrier as StopAwaitingSlaveResponse.
func (t *Transport) SetAwaitingSlaveResponse(awaiting bool) {
	if !awaiting {
		t.StopAwaitingSlaveResponse()
		return
	}
	t.awaitingSlaveResponse.Store(true)
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// StopAwaitingSlaveResponse stops requesting 0x3D and waits for any execute
// cycle that was already reading the driver to finish. After it returns, an
// idle master transport will not call ReadEvent until another request starts.
func (t *Transport) StopAwaitingSlaveResponse() {
	t.awaitingSlaveResponse.Store(false)
	t.executeMu.Lock()
	t.executeMu.Unlock()
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

// ReceiveBlocking waits for a message, an asynchronous transport error,
// transport closure, or context cancellation.
func (t *Transport) ReceiveBlocking(ctx context.Context) (*LinMessage, error) {
	select {
	case msg := <-t.rxQueue:
		return msg, nil
	case err, ok := <-t.receiveErrors:
		if !ok {
			return nil, ErrTransportClosed
		}
		return nil, err
	case <-t.done:
		return nil, ErrTransportClosed
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

// Transmit packages and atomically queues a high-level LIN transport message.
func (t *Transport) Transmit(nad, sid byte, data []byte) error {
	frames, err := t.buildFrames(nad, sid, data)
	if err != nil {
		return err
	}

	t.txMu.Lock()
	defer t.txMu.Unlock()
	t.lifecycleMu.Lock()
	closed := t.closed
	t.lifecycleMu.Unlock()
	if closed {
		return ErrTransportClosed
	}
	if len(frames) > cap(t.txQueue)-len(t.txQueue) {
		return fmt.Errorf("%w: need %d slots, have %d", ErrTxQueueFull, len(frames), cap(t.txQueue)-len(t.txQueue))
	}
	for _, frame := range frames {
		t.txQueue <- frame
	}
	if !t.isSlave {
		t.awaitingSlaveResponse.Store(true)
	}
	select {
	case t.wake <- struct{}{}:
	default:
	}
	return nil
}

func (t *Transport) buildFrames(nad, sid byte, data []byte) ([]*liniface.LinEvent, error) {
	if len(data) > maxTransportDataLength {
		return nil, fmt.Errorf("%w: data length %d", ErrMessageTooLong, len(data))
	}
	var eventID byte
	if t.isSlave {
		eventID = SlaveDiagnosticFrameID
	} else {
		eventID = MasterDiagnosticFrameID
	}
	frames := make([]*liniface.LinEvent, 0, 1+(len(data)+1)/6)
	dataLen := len(data) + 1
	if dataLen <= 6 {
		pci := (byte(liniface.SF) << 4) | byte(dataLen)
		payload := newDiagnosticFramePayload()
		payload[0] = nad
		payload[1] = pci
		payload[2] = sid
		copy(payload[3:], data)
		frames = append(frames, &liniface.LinEvent{Channel: t.channel, EventID: eventID, EventPayload: payload, ChecksumType: liniface.ClassicChecksum})
	} else {

		pci := (byte(liniface.FF) << 4) | byte(dataLen>>8&0x0F)
		payloadFF := newDiagnosticFramePayload()
		payloadFF[0] = nad
		payloadFF[1] = pci
		payloadFF[2] = byte(dataLen & 0xFF)
		payloadFF[3] = sid
		copy(payloadFF[4:], data[:4])
		frames = append(frames, &liniface.LinEvent{Channel: t.channel, EventID: eventID, EventPayload: payloadFF, ChecksumType: liniface.ClassicChecksum})

		currentByte := 4
		currentFrame := 0
		for currentByte < len(data) {
			currentFrame = (currentFrame + 1) % 16
			pciCF := (byte(liniface.CF) << 4) | byte(currentFrame)
			payloadCF := newDiagnosticFramePayload()
			payloadCF[0] = nad
			payloadCF[1] = pciCF

			endByte := currentByte + 6
			if endByte > len(data) {
				endByte = len(data)
			}
			copy(payloadCF[2:], data[currentByte:endByte])
			currentByte = endByte
			frames = append(frames, &liniface.LinEvent{Channel: t.channel, EventID: eventID, EventPayload: payloadCF, ChecksumType: liniface.ClassicChecksum})
		}
	}
	return frames, nil
}

// receiveFromDriver processes a raw event and reports whether it delivered one
// complete diagnostic message.
func (t *Transport) receiveFromDriver(event *liniface.LinEvent) bool {
	t.stateMutex.Lock()
	defer t.stateMutex.Unlock()
	if t.isSlave && event.Direction == liniface.TX && t.scheduledTxEvent != nil {
		if t.scheduledTxEvent.EventID == event.EventID {
			t.scheduledTxEvent = nil
			select {
			case nextEvent := <-t.txQueue:
				if err := t.driver.ScheduleSlaveResponse(nextEvent, t.channel); err != nil {
					log.Printf("slave failed to schedule next response: %v", err)
				}
				t.scheduledTxEvent = nextEvent
			default:
			}
		}
		return false
	}

	// Check if the received event is a diagnostic frame relevant to our role
	isMasterReceiving := !t.isSlave && event.EventID == SlaveDiagnosticFrameID
	isSlaveReceiving := t.isSlave && event.EventID == MasterDiagnosticFrameID

	if event.Direction == liniface.RX && (isMasterReceiving || isSlaveReceiving) {
		payload := event.EventPayload
		if !t.isSlave && len(payload) == 0 {
			return false // Master ignores empty frames (no-response from slave)
		}
		if len(payload) < 2 {
			return false // Frame too short to be a valid diagnostic frame
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
				return false
			}
			sid := payload[2]
			data := append([]byte(nil), payload[3:3+dataLength]...)
			t.deliverMessage(&LinMessage{NAD: nad, SID: sid, Data: data})
			return true

		case liniface.FF:
			if t.remainingBytes > 0 {
				log.Println("Warning: Received a First-Frame before completing the last one. Previous frame dropped.")
			}
			t.resetState()
			if len(payload) < 8 {
				log.Printf("Error: FF frame is smaller than 8 bytes.")
				return false
			}
			length := (int(additionalInfo) << 8) | int(payload[2])
			if length <= 6 || length > maxTransportDataLength+1 {
				log.Printf("Error: FF with invalid transport length %d", length)
				return false
			}
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
				return false
			}
			frameCounter := additionalInfo
			nextFrameCounter := byte(t.currentFrameCounter+1) % 16
			if frameCounter != nextFrameCounter {
				log.Println("Warning: Received an out-of-order Consecutive-Frame. Discarding message.")
				t.resetState()
				return false
			}

			length := min(t.remainingBytes, 6)
			if len(payload) < 2+length {
				t.resetState()
				return false
			}
			t.remainingBytes -= length
			t.currentFrameData = append(t.currentFrameData, payload[2:2+length]...)
			t.currentFrameCounter = nextFrameCounter

			if t.remainingBytes == 0 {
				msg := &LinMessage{NAD: t.currentNAD, SID: t.currentSID, Data: t.currentFrameData}
				t.deliverMessage(msg)
				t.resetState()
				return true
			}
		}
	}
	return false
}

func (t *Transport) deliverMessage(msg *LinMessage) {
	select {
	case t.rxQueue <- msg:
	default:
		t.reportError(ErrRxQueueFull)
	}
}
