package tplin

import (
	"testing"
	"time"

	"github.com/LoveWonYoung/linbuskit/liniface"
)

func TestTransportUsesConfiguredChannel(t *testing.T) {
	network := NewSimulatedLinNetwork()
	driver := network.GetMasterDriver()
	channel := liniface.Channel(2)
	transport := NewTransport(false, driver, channel)

	if err := transport.Transmit(0x01, 0x22, []byte{0xF1, 0x89}); err != nil {
		t.Fatal(err)
	}
	if err := transport.execute(); err != nil {
		t.Fatalf("execute transport: %v", err)
	}

	if event, err := driver.ReadEvent(0, 0); err != nil || event != nil {
		t.Fatalf("default channel received event: event=%v err=%v", event, err)
	}

	event, err := driver.ReadEvent(time.Millisecond, channel)
	if err != nil {
		t.Fatalf("read configured channel: %v", err)
	}
	if event == nil {
		t.Fatal("configured channel did not receive transmitted event")
	}
	if event.Channel != channel {
		t.Fatalf("event channel = %d, want %d", event.Channel, channel)
	}
}

func TestSimulatedDriverSeparatesSlaveResponsesByChannel(t *testing.T) {
	network := NewSimulatedLinNetwork()
	master := network.GetMasterDriver()
	slave := network.CreateSlaveDriver()
	channel1 := liniface.Channel(1)
	channel2 := liniface.Channel(2)

	response := &liniface.LinEvent{
		EventID:      SlaveDiagnosticFrameID,
		EventPayload: []byte{0x01, 0x01, 0x62},
	}
	if err := slave.ScheduleSlaveResponse(response, channel2); err != nil {
		t.Fatalf("schedule response: %v", err)
	}
	if err := master.RequestSlaveResponse(SlaveDiagnosticFrameID, channel1); err != nil {
		t.Fatalf("request on channel 1: %v", err)
	}
	if event, err := master.ReadEvent(0, channel1); err != nil || event != nil {
		t.Fatalf("channel 1 received channel 2 response: event=%v err=%v", event, err)
	}

	if err := master.RequestSlaveResponse(SlaveDiagnosticFrameID, channel2); err != nil {
		t.Fatalf("request on channel 2: %v", err)
	}
	event, err := master.ReadEvent(time.Millisecond, channel2)
	if err != nil {
		t.Fatalf("read channel 2: %v", err)
	}
	if event == nil || event.Channel != channel2 {
		t.Fatalf("channel 2 response = %#v", event)
	}
}

func TestSimulatedDriverOwnsScheduledEvent(t *testing.T) {
	network := NewSimulatedLinNetwork()
	master := network.GetMasterDriver()
	slave := network.CreateSlaveDriver()
	channel := liniface.Channel(2)
	response := &liniface.LinEvent{
		EventID:      SlaveDiagnosticFrameID,
		EventPayload: []byte{0x01, 0x01, 0x62},
	}
	if err := slave.ScheduleSlaveResponse(response, channel); err != nil {
		t.Fatal(err)
	}

	response.Channel = 9
	response.EventPayload[0] = 0x7F
	if err := master.RequestSlaveResponse(SlaveDiagnosticFrameID, channel); err != nil {
		t.Fatal(err)
	}
	event, err := master.ReadEvent(time.Millisecond, channel)
	if err != nil {
		t.Fatal(err)
	}
	if event == nil || event.Channel != channel || event.EventPayload[0] != 0x01 {
		t.Fatalf("scheduled event was aliased: %#v", event)
	}
}
