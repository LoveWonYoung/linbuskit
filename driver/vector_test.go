//go:build windows

package driver

import (
	"encoding/binary"
	"testing"
	"unsafe"

	"github.com/LoveWonYoung/linbuskit/liniface"
)

func TestVectorXLStructLayouts(t *testing.T) {
	if size := unsafe.Sizeof(xlLINChannelParams{}); size != 16 {
		t.Fatalf("XLlinStatPar size = %d, want 16", size)
	}
	if size := unsafe.Sizeof(xlLINMessage{}); size != 13 {
		t.Fatalf("s_xl_lin_msg size = %d, want 13", size)
	}
	var event xlEvent
	if size := unsafe.Sizeof(event); size != 48 {
		t.Fatalf("XLevent size = %d, want 48", size)
	}
	if offset := unsafe.Offsetof(event.Timestamp); offset != 8 {
		t.Fatalf("XLevent.timeStamp offset = %d, want 8", offset)
	}
	if offset := unsafe.Offsetof(event.TagData); offset != 16 {
		t.Fatalf("XLevent.tagData offset = %d, want 16", offset)
	}
}

func TestNormalizeVectorConfigDefaults(t *testing.T) {
	config, dlc, checksum, err := normalizeVectorConfig(DefaultVectorConfig(59))
	if err != nil {
		t.Fatal(err)
	}
	if config.Mode != VectorLINMaster || config.Version != VectorLINVersion21 || config.Baudrate != 19200 {
		t.Fatalf("unexpected defaults: %+v", config)
	}
	if len(config.Channels) != 1 || config.Channels[0] != 0 {
		t.Fatalf("unexpected channels: %v", config.Channels)
	}
	for id, length := range dlc {
		if length != 8 {
			t.Fatalf("DLC[%d] = %d, want 8", id, length)
		}
	}
	if checksum[0x10] != liniface.EnhancedChecksum {
		t.Fatalf("checksum[0x10] = %d, want enhanced", checksum[0x10])
	}
	if checksum[0x3C] != liniface.ClassicChecksum || checksum[0x3D] != liniface.ClassicChecksum {
		t.Fatal("diagnostic IDs must use classic checksum")
	}
	bytes := vectorChecksumBytes(checksum)
	if bytes[0x10] != 1 || bytes[0x3B] != 1 {
		t.Fatalf("unexpected xlLinSetChecksum table values: 0x10=%d 0x3B=%d", bytes[0x10], bytes[0x3B])
	}
}

func TestNormalizeVectorConfigOverrides(t *testing.T) {
	config := DefaultVectorConfig(59, 1, 2)
	config.Mode = VectorLINSlave
	config.Version = VectorLINVersion20
	config.DLC = map[byte]byte{0x12: 4}
	config.Checksum = map[byte]liniface.ChecksumType{0x12: liniface.ClassicChecksum}

	normalized, dlc, checksum, err := normalizeVectorConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Mode != VectorLINSlave || dlc[0x12] != 4 || checksum[0x12] != liniface.ClassicChecksum {
		t.Fatalf("overrides not applied: mode=%v dlc=%d checksum=%d", normalized.Mode, dlc[0x12], checksum[0x12])
	}
}

func TestNormalizeVectorConfigRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*VectorConfig)
	}{
		{"hardware type", func(config *VectorConfig) { config.DeviceType = 0 }},
		{"mode", func(config *VectorConfig) { config.Mode = 99 }},
		{"version", func(config *VectorConfig) { config.Version = 99 }},
		{"baudrate", func(config *VectorConfig) { config.Baudrate = 20001 }},
		{"duplicate channel", func(config *VectorConfig) { config.Channels = []liniface.Channel{1, 1} }},
		{"DLC", func(config *VectorConfig) { config.DLC = map[byte]byte{1: 9} }},
		{"diagnostic DLC", func(config *VectorConfig) { config.DLC = map[byte]byte{0x3D: 4} }},
		{"diagnostic checksum", func(config *VectorConfig) {
			config.Checksum = map[byte]liniface.ChecksumType{0x3D: liniface.EnhancedChecksum}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := DefaultVectorConfig(59)
			test.mutate(&config)
			if _, _, _, err := normalizeVectorConfig(config); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestVectorLINCalcChecksum(t *testing.T) {
	if got := vectorLINCalcChecksum(0x10, liniface.ClassicChecksum); got != vectorLINCalcChecksumClassic {
		t.Fatalf("classic flag = 0x%X", got)
	}
	if got := vectorLINCalcChecksum(0x10, liniface.EnhancedChecksum); got != vectorLINCalcChecksumEnh {
		t.Fatalf("enhanced flag = 0x%X", got)
	}
	if got := vectorLINCalcChecksum(0x3D, liniface.EnhancedChecksum); got != vectorLINCalcChecksumClassic {
		t.Fatalf("diagnostic checksum flag = 0x%X, want classic", got)
	}
}

func TestValidateVectorEvent(t *testing.T) {
	valid := &liniface.LinEvent{
		EventID:      0x3C,
		EventPayload: make([]byte, 8),
		ChecksumType: liniface.ClassicChecksum,
	}
	if err := validateVectorEvent(valid); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}
	invalid := *valid
	invalid.EventID = 0x40
	if err := validateVectorEvent(&invalid); err == nil {
		t.Fatal("invalid frame ID accepted")
	}
	invalid = *valid
	invalid.EventPayload = make([]byte, 9)
	if err := validateVectorEvent(&invalid); err == nil {
		t.Fatal("oversized payload accepted")
	}
}

func TestDecodeVectorLINEvent(t *testing.T) {
	_, _, checksum, err := normalizeVectorConfig(DefaultVectorConfig(59))
	if err != nil {
		t.Fatal(err)
	}
	message := xlLINMessage{ID: 0x22, DLC: 3, Data: [8]byte{1, 2, 3}, CRC: 0xAA}
	binary.LittleEndian.PutUint16(message.Flags[:], vectorLINMessageFlagTX)
	var raw xlEvent
	messageBytes := unsafe.Slice((*byte)(unsafe.Pointer(&message)), int(unsafe.Sizeof(message)))
	copy(raw.TagData[:], messageBytes)

	event, crc, err := decodeVectorLINEvent(&raw, 2, checksum)
	if err != nil {
		t.Fatal(err)
	}
	if event.Channel != 2 || event.EventID != 0x22 || event.Direction != liniface.TX || event.ChecksumType != liniface.EnhancedChecksum {
		t.Fatalf("unexpected decoded event: %+v", event)
	}
	if crc != 0xAA || len(event.EventPayload) != 3 || event.EventPayload[0] != 1 || event.EventPayload[2] != 3 {
		t.Fatalf("unexpected decoded payload/CRC: payload=%v crc=0x%02X", event.EventPayload, crc)
	}

	binary.LittleEndian.PutUint16(message.Flags[:], vectorLINMessageFlagCRCError)
	messageBytes = unsafe.Slice((*byte)(unsafe.Pointer(&message)), int(unsafe.Sizeof(message)))
	copy(raw.TagData[:], messageBytes)
	if _, _, err := decodeVectorLINEvent(&raw, 2, checksum); err == nil {
		t.Fatal("checksum error flag was accepted")
	}
}
