package tplin

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LoveWonYoung/linbuskit/liniface"
)

type transportActivityDriver struct {
	readCalls    atomic.Int32
	requestCalls atomic.Int32
	readStarted  chan struct{}
	releaseRead  chan struct{}
	events       chan *liniface.LinEvent
}

func (d *transportActivityDriver) ReadEvent(time.Duration, liniface.Channel) (*liniface.LinEvent, error) {
	d.readCalls.Add(1)
	if d.readStarted != nil {
		select {
		case d.readStarted <- struct{}{}:
		default:
		}
	}
	if d.releaseRead != nil {
		<-d.releaseRead
	}
	if d.events != nil {
		select {
		case event := <-d.events:
			return event, nil
		default:
		}
	}
	return nil, nil
}

func (d *transportActivityDriver) WriteMessage(*liniface.LinEvent, liniface.Channel) error {
	return nil
}

func (d *transportActivityDriver) ScheduleSlaveResponse(*liniface.LinEvent, liniface.Channel) error {
	return nil
}

func (d *transportActivityDriver) RequestSlaveResponse(byte, liniface.Channel) error {
	d.requestCalls.Add(1)
	return nil
}

func TestMasterTransportSkipsDriverReadWhenIdle(t *testing.T) {
	driver := &transportActivityDriver{}
	transport := NewTransport(false, driver)
	if err := transport.execute(); err != nil {
		t.Fatal(err)
	}
	if got := driver.readCalls.Load(); got != 0 {
		t.Fatalf("idle master ReadEvent calls = %d, want 0", got)
	}
	if got := driver.requestCalls.Load(); got != 0 {
		t.Fatalf("idle master response requests = %d, want 0", got)
	}
}

func TestStopAwaitingSlaveResponseWaitsForActiveRead(t *testing.T) {
	driver := &transportActivityDriver{
		readStarted: make(chan struct{}, 1),
		releaseRead: make(chan struct{}),
	}
	transport := NewTransport(false, driver)
	transport.SetAwaitingSlaveResponse(true)

	executeDone := make(chan error, 1)
	go func() { executeDone <- transport.execute() }()
	select {
	case <-driver.readStarted:
	case <-time.After(time.Second):
		t.Fatal("active execute did not enter ReadEvent")
	}

	stopDone := make(chan struct{})
	go func() {
		transport.StopAwaitingSlaveResponse()
		close(stopDone)
	}()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for transport.awaitingSlaveResponse.Load() {
		select {
		case <-deadline.C:
			t.Fatal("StopAwaitingSlaveResponse did not clear awaiting state")
		default:
			runtime.Gosched()
		}
	}
	select {
	case <-stopDone:
		t.Fatal("StopAwaitingSlaveResponse returned before active ReadEvent exited")
	default:
	}

	close(driver.releaseRead)
	select {
	case err := <-executeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("execute did not finish after ReadEvent was released")
	}
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("StopAwaitingSlaveResponse did not cross execute barrier")
	}
	if got := driver.requestCalls.Load(); got != 0 {
		t.Fatalf("response header requests after stop = %d, want 0", got)
	}

	reads := driver.readCalls.Load()
	if err := transport.execute(); err != nil {
		t.Fatal(err)
	}
	if got := driver.readCalls.Load(); got != reads {
		t.Fatalf("idle execute added ReadEvent calls: before=%d after=%d", reads, got)
	}
}

func TestCompleteResponseSuppressesExtraSlaveHeader(t *testing.T) {
	driver := &transportActivityDriver{events: make(chan *liniface.LinEvent, 1)}
	driver.events <- &liniface.LinEvent{
		Channel:      0,
		EventID:      SlaveDiagnosticFrameID,
		EventPayload: []byte{0x01, 0x02, 0x62, 0xAA},
		Direction:    liniface.RX,
	}
	transport := NewTransport(false, driver)
	transport.SetAwaitingSlaveResponse(true)

	if err := transport.execute(); err != nil {
		t.Fatal(err)
	}
	if got := driver.requestCalls.Load(); got != 0 {
		t.Fatalf("extra 0x3D header requests = %d, want 0", got)
	}
	message := transport.Receive()
	if message == nil || message.SID != 0x62 || len(message.Data) != 1 || message.Data[0] != 0xAA {
		t.Fatalf("delivered message = %#v", message)
	}
	transport.StopAwaitingSlaveResponse()
}

func TestTransportRunAndCloseAreIdempotent(t *testing.T) {
	transport := NewTransport(false, NewSimulatedLinNetwork().GetMasterDriver())
	transport.Run()
	transport.Run()

	closed := make(chan struct{})
	go func() {
		transport.Close()
		transport.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("repeated Close did not return")
	}
	if err := transport.Transmit(1, 0x22, nil); !errors.Is(err, ErrTransportClosed) {
		t.Fatalf("Transmit after Close error = %v", err)
	}
}

func TestTransportCloseUnblocksReceiveBlocking(t *testing.T) {
	transport := NewTransport(false, NewSimulatedLinNetwork().GetMasterDriver())
	result := make(chan error, 1)
	go func() {
		_, err := transport.ReceiveBlocking(context.Background())
		result <- err
	}()

	transport.Close()
	select {
	case err := <-result:
		if !errors.Is(err, ErrTransportClosed) {
			t.Fatalf("ReceiveBlocking after Close error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock ReceiveBlocking")
	}
}

func TestSlaveRunAndStopAreIdempotent(t *testing.T) {
	network := NewSimulatedLinNetwork()
	slave := NewSlave(1, 1, 1, 1, nil, network.CreateSlaveDriver())
	slave.Run()
	slave.Run()

	var wg sync.WaitGroup
	wg.Add(2)
	for range 2 {
		go func() {
			defer wg.Done()
			slave.Stop()
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("concurrent Stop calls did not return")
	}

	// A closed slave cannot be restarted.
	slave.Run()
}

func TestMasterIgnoresUnrelatedAndPendingNegativeResponses(t *testing.T) {
	transport := NewTransport(false, NewSimulatedLinNetwork().GetMasterDriver())
	master := &LinMaster{transport: transport}
	transport.deliverMessage(&LinMessage{NAD: 1, SID: 0x7F, Data: []byte{0x10, 0x13}})
	transport.deliverMessage(&LinMessage{NAD: 1, SID: 0x7F, Data: []byte{0x22, 0x78}})
	transport.deliverMessage(&LinMessage{NAD: 1, SID: 0x62, Data: []byte{0xF1, 0x89}})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	message, err := master.waitForResponseWithContext(ctx, 0x62, 1)
	if err != nil {
		t.Fatal(err)
	}
	if message.SID != 0x62 {
		t.Fatalf("response SID = 0x%02X", message.SID)
	}
}

func TestTransportRejectsOversizedMessageAndFullQueue(t *testing.T) {
	config := DefaultTransportConfig()
	config.TxQueueSize = 1
	transport := NewTransportWithConfig(false, NewSimulatedLinNetwork().GetMasterDriver(), config)

	if err := transport.Transmit(1, 0x22, make([]byte, maxTransportDataLength+1)); !errors.Is(err, ErrMessageTooLong) {
		t.Fatalf("oversized Transmit error = %v", err)
	}
	if err := transport.Transmit(1, 0x22, nil); err != nil {
		t.Fatalf("first Transmit: %v", err)
	}
	if err := transport.Transmit(1, 0x22, nil); !errors.Is(err, ErrTxQueueFull) {
		t.Fatalf("full queue error = %v", err)
	}
}

func TestTransportQueuesMaximumLengthMessage(t *testing.T) {
	transport := NewTransport(false, NewSimulatedLinNetwork().GetMasterDriver())
	if err := transport.Transmit(1, 0x36, make([]byte, maxTransportDataLength)); err != nil {
		t.Fatalf("maximum-length Transmit: %v", err)
	}
	if got := len(transport.txQueue); got != DefaultTxQueueSize {
		t.Fatalf("queued frame count = %d, want %d", got, DefaultTxQueueSize)
	}
}

func TestTransportRejectsMalformedFirstFrame(t *testing.T) {
	transport := NewTransport(false, NewSimulatedLinNetwork().GetMasterDriver())
	transport.receiveFromDriver(&liniface.LinEvent{
		EventID:      SlaveDiagnosticFrameID,
		EventPayload: []byte{1, 0x10, 0, 0x62, 0, 0, 0, 0},
		Direction:    liniface.RX,
	})
	if message := transport.Receive(); message != nil {
		t.Fatalf("malformed FF produced message: %#v", message)
	}
}

func TestReceiveQueueOverflowDoesNotBlock(t *testing.T) {
	config := DefaultTransportConfig()
	config.RxQueueSize = 1
	transport := NewTransportWithConfig(false, NewSimulatedLinNetwork().GetMasterDriver(), config)
	event := &liniface.LinEvent{
		EventID:      SlaveDiagnosticFrameID,
		EventPayload: []byte{1, 1, 0x62},
		Direction:    liniface.RX,
	}
	transport.receiveFromDriver(event)
	transport.receiveFromDriver(event)
	select {
	case err := <-transport.Errors():
		if !errors.Is(err, ErrRxQueueFull) {
			t.Fatalf("overflow error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("receive queue overflow was not reported")
	}
}
