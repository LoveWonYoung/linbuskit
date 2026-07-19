package tplin

import (
	"errors"
	"testing"
	"time"

	"github.com/LoveWonYoung/linbuskit/liniface"
)

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
