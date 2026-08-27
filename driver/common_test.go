package driver

import (
	"bytes"
	"errors"
	"log"
	"testing"
	"time"

	"github.com/LoveWonYoung/linbuskit/liniface"
)

func TestDeviceLoggingRequiresExplicitEnableAndUsesCommonFormat(t *testing.T) {
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	previousPrefix := log.Prefix()
	previousLogging := printLogEnabled()
	defer func() {
		SetPrintLog(previousLogging)
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
		log.SetPrefix(previousPrefix)
	}()

	var output bytes.Buffer
	log.SetOutput(&output)
	log.SetFlags(0)
	log.SetPrefix("")

	SetPrintLog(false)
	logLINMessage(logDevicePCAN, "RX", liniface.Channel(2), 0x3D, 0xA5, []byte{0x01, 0x02})
	if output.Len() != 0 {
		t.Fatalf("disabled logging produced output: %q", output.String())
	}

	SetPrintLog(true)
	logLINMessage(logDevicePCAN, "RX", liniface.Channel(2), 0x3D, 0xA5, []byte{0x01, 0x02})
	want := "[PCAN] LIN direction=RX channel=2 id=0x3D length=2 checksum=0xA5 data=01 02\n"
	if output.String() != want {
		t.Fatalf("log output = %q, want %q", output.String(), want)
	}
}

func TestReadMasterResponseFiltersAndCopiesPayload(t *testing.T) {
	channel := liniface.Channel(2)
	payload := []byte{0x11, 0x22}
	events := []*liniface.LinEvent{
		{Channel: 1, EventID: 0x22, Direction: liniface.RX, EventPayload: []byte{0x01}},
		{Channel: channel, EventID: 0x21, Direction: liniface.RX, EventPayload: []byte{0x02}},
		{Channel: channel, EventID: 0x22, Direction: liniface.TX, EventPayload: []byte{0x03}},
		{Channel: channel, EventID: 0x22, Direction: liniface.RX, EventPayload: payload},
	}
	requestCalls := 0
	response, err := readMasterResponse(
		0x22,
		channel,
		time.Second,
		func(frameID byte, gotChannel liniface.Channel) error {
			requestCalls++
			if frameID != 0x22 || gotChannel != channel {
				t.Fatalf("request frame=0x%02X channel=%d", frameID, gotChannel)
			}
			return nil
		},
		func(_ time.Duration, gotChannel liniface.Channel) (*liniface.LinEvent, error) {
			if gotChannel != channel {
				t.Fatalf("read channel=%d, want %d", gotChannel, channel)
			}
			event := events[0]
			events = events[1:]
			return event, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if requestCalls != 1 {
		t.Fatalf("request calls=%d, want 1", requestCalls)
	}
	payload[0] = 0xFF
	if len(response) != 2 || response[0] != 0x11 || response[1] != 0x22 {
		t.Fatalf("response=% X", response)
	}
}

func TestReadMasterResponseErrors(t *testing.T) {
	requestFailure := errors.New("request failure")
	readFailure := errors.New("read failure")
	requestCalled := false
	_, err := readMasterResponse(
		0x40,
		0,
		time.Second,
		func(byte, liniface.Channel) error { requestCalled = true; return nil },
		func(time.Duration, liniface.Channel) (*liniface.LinEvent, error) { return nil, nil },
	)
	if err == nil {
		t.Fatal("invalid frame ID was accepted")
	}
	if requestCalled {
		t.Fatal("invalid frame ID reached request callback")
	}

	tests := []struct {
		name    string
		timeout time.Duration
		request func(byte, liniface.Channel) error
		read    func(time.Duration, liniface.Channel) (*liniface.LinEvent, error)
		want    error
	}{
		{
			name:    "request failure",
			timeout: time.Second,
			request: func(byte, liniface.Channel) error { return requestFailure },
			read:    func(time.Duration, liniface.Channel) (*liniface.LinEvent, error) { return nil, nil },
			want:    requestFailure,
		},
		{
			name:    "read failure",
			timeout: time.Second,
			request: func(byte, liniface.Channel) error { return nil },
			read:    func(time.Duration, liniface.Channel) (*liniface.LinEvent, error) { return nil, readFailure },
			want:    readFailure,
		},
		{
			name:    "timeout",
			timeout: 0,
			request: func(byte, liniface.Channel) error { return nil },
			read:    func(time.Duration, liniface.Channel) (*liniface.LinEvent, error) { return nil, nil },
			want:    ErrNoSlaveResponse,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := readMasterResponse(0x22, 0, test.timeout, test.request, test.read)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v, want %v", err, test.want)
			}
		})
	}
}
