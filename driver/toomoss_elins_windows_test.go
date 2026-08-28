//go:build windows

package driver

import (
	"errors"
	"testing"
	"unsafe"

	"github.com/LoveWonYoung/linbuskit/liniface"
)

func TestELINSMessageLayout(t *testing.T) {
	message := ElinsMsg{}
	tests := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{name: "size", got: unsafe.Sizeof(message), want: 88},
		{name: "timestamp", got: unsafe.Offsetof(message.TimeStamp), want: 8},
		{name: "command", got: unsafe.Offsetof(message.CmdCode), want: 12},
		{name: "register", got: unsafe.Offsetof(message.RegAddr), want: 14},
		{name: "data", got: unsafe.Offsetof(message.Data), want: 18},
		{name: "ack", got: unsafe.Offsetof(message.ACKValue), want: 82},
	}
	for _, test := range tests {
		if test.got != test.want {
			t.Errorf("%s = %d, want %d", test.name, test.got, test.want)
		}
	}
}

func TestNormalizeELINSConfig(t *testing.T) {
	tests := []struct {
		name   string
		config ELINSConfig
		want   error
	}{
		{name: "valid", config: ELINSConfig{Channels: []ToomossCh{CH1, CH2}, Baudrate: 2000000, ResEnable: 1, Version: ELINS_VER_IND83220}},
		{name: "no channels", config: ELINSConfig{Baudrate: 2000000}, want: errors.New("invalid")},
		{name: "low baudrate", config: ELINSConfig{Channels: []ToomossCh{CH1}, Baudrate: 1999}, want: errors.New("invalid")},
		{name: "high baudrate", config: ELINSConfig{Channels: []ToomossCh{CH1}, Baudrate: 5000001}, want: errors.New("invalid")},
		{name: "resistor", config: ELINSConfig{Channels: []ToomossCh{CH1}, Baudrate: 2000000, ResEnable: 2}, want: errors.New("invalid")},
		{name: "version", config: ELINSConfig{Channels: []ToomossCh{CH1}, Baudrate: 2000000, Version: 3}, want: errors.New("invalid")},
		{name: "channel", config: ELINSConfig{Channels: []ToomossCh{CH4 + 1}, Baudrate: 2000000}, want: liniface.ErrInvalidChannel},
		{name: "duplicate", config: ELINSConfig{Channels: []ToomossCh{CH1, CH1}, Baudrate: 2000000}, want: errors.New("invalid")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			normalized, err := normalizeELINSConfig(test.config)
			if test.want == nil {
				if err != nil {
					t.Fatal(err)
				}
				if normalized.ReceiveTimeoutUS != defaultELINSReceiveTimeoutUS {
					t.Fatalf("receive timeout = %d", normalized.ReceiveTimeoutUS)
				}
				return
			}
			if err == nil {
				t.Fatal("invalid config was accepted")
			}
			if errors.Is(test.want, liniface.ErrInvalidChannel) && !errors.Is(err, liniface.ErrInvalidChannel) {
				t.Fatalf("error = %v, want ErrInvalidChannel", err)
			}
		})
	}
}

func TestNormalizeELINSConfigCopiesChannels(t *testing.T) {
	channels := []ToomossCh{CH1, CH2}
	normalized, err := normalizeELINSConfig(ELINSConfig{Channels: channels, Baudrate: 2000000})
	if err != nil {
		t.Fatal(err)
	}
	channels[0] = CH4
	if normalized.Channels[0] != CH1 {
		t.Fatal("normalized config retained caller-owned channel slice")
	}
}

func TestToomossSeparatesLINAndELINSChannels(t *testing.T) {
	driver := &Toomoss{
		channels:      map[liniface.Channel]struct{}{CH1: {}},
		elinsChannels: map[liniface.Channel]struct{}{CH2: {}},
	}
	if err := driver.validateChannel(CH1); err != nil {
		t.Fatalf("LIN CH1 rejected: %v", err)
	}
	if !errors.Is(driver.validateChannel(CH2), liniface.ErrInvalidChannel) {
		t.Fatal("ELINS-only CH2 accepted as LIN")
	}
	if err := driver.validateELINSChannel(CH2); err != nil {
		t.Fatalf("ELINS CH2 rejected: %v", err)
	}
	if !errors.Is(driver.validateELINSChannel(CH1), liniface.ErrInvalidChannel) {
		t.Fatal("LIN-only CH1 accepted as ELINS")
	}
	if err := driver.validateConfiguredChannel(CH1); err != nil {
		t.Fatalf("power validation rejected LIN channel: %v", err)
	}
	if err := driver.validateConfiguredChannel(CH2); err != nil {
		t.Fatalf("power validation rejected ELINS channel: %v", err)
	}
}
