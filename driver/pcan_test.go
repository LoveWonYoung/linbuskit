//go:build windows

package driver

import (
	"testing"
	"unsafe"

	"github.com/LoveWonYoung/linbuskit/liniface"
)

func TestPCANNativeStructureLayout(t *testing.T) {
	tests := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"TLINMsg size", unsafe.Sizeof(pcanMessage{}), 13},
		{"TLINRcvMsg size", unsafe.Sizeof(pcanReceiveMessage{}), 40},
		{"TLINRcvMsg ErrorFlags offset", unsafe.Offsetof(pcanReceiveMessage{}.ErrorFlags), 16},
		{"TLINRcvMsg TimeStamp offset", unsafe.Offsetof(pcanReceiveMessage{}.Timestamp), 24},
		{"TLINRcvMsg hHw offset", unsafe.Offsetof(pcanReceiveMessage{}.Hardware), 32},
		{"TLINFrameEntry size", unsafe.Sizeof(pcanFrameEntry{}), 14},
		{"TLINFrameEntry Flags offset", unsafe.Offsetof(pcanFrameEntry{}.Flags), 4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.got != test.want {
				t.Fatalf("got %d, want %d", test.got, test.want)
			}
		})
	}
}

func TestProtectedLINID(t *testing.T) {
	tests := []struct {
		id  byte
		pid byte
	}{
		{0x00, 0x80},
		{0x01, 0xC1},
		{0x3C, 0x3C},
		{0x3D, 0x7D},
		{0x3F, 0xBF},
	}
	for _, test := range tests {
		if got := protectedLINID(test.id); got != test.pid {
			t.Errorf("protectedLINID(0x%02X) = 0x%02X, want 0x%02X", test.id, got, test.pid)
		}
	}
}

func TestPCANChecksumAndDefaultLength(t *testing.T) {
	if got := pcanChecksumType(0x3D, liniface.EnhancedChecksum); got != pcanChecksumClassic {
		t.Fatalf("diagnostic checksum = %d, want classic", got)
	}
	if got := pcanChecksumType(0x20, liniface.EnhancedChecksum); got != pcanChecksumEnhanced {
		t.Fatalf("enhanced checksum = %d, want enhanced", got)
	}

	for id, want := range map[byte]byte{0x00: 2, 0x1F: 2, 0x20: 4, 0x2F: 4, 0x30: 8, 0x3D: 8} {
		if got := defaultPCANFrameLength(id); got != want {
			t.Errorf("defaultPCANFrameLength(0x%02X) = %d, want %d", id, got, want)
		}
	}
}

func TestNormalizePCANConfig(t *testing.T) {
	config, err := normalizePCANConfig(PCANConfig{})
	if err != nil {
		t.Fatalf("normalize default config: %v", err)
	}
	if config.Mode != PCANMaster || config.Baudrate != 19200 || len(config.Channels) != 1 || config.Channels[0] != 0 {
		t.Fatalf("unexpected defaults: %+v", config)
	}

	_, err = normalizePCANConfig(PCANConfig{
		Mode:     PCANMaster,
		Baudrate: 19200,
		Channels: []liniface.Channel{1, 1},
	})
	if err == nil {
		t.Fatal("duplicate channels were accepted")
	}
}
