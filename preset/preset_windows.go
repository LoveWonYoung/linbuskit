//go:build windows

package preset

import (
	"fmt"

	"github.com/LoveWonYoung/linbuskit/driver"
	"github.com/LoveWonYoung/linbuskit/liniface"
)

// NewPresetTSMaster opens one TSMaster LIN channel in master mode and binds a
// UDS client to targetNAD. The returned preset owns the driver.
func NewPresetTSMaster(targetNAD byte, channel liniface.Channel, deviceType int) (*Preset, error) {
	drv, err := driver.NewTSMaster(deviceType, channel)
	if err != nil {
		return nil, fmt.Errorf("initialize TSMaster LIN driver: %w", err)
	}
	return newPreset(drv, targetNAD, channel)
}

// NewPresetPCAN opens one PCAN/PLIN channel with its default 19200-baud master
// configuration and binds a UDS client to targetNAD. The returned preset owns
// the driver.
func NewPresetPCAN(targetNAD byte, channel liniface.Channel) (*Preset, error) {
	drv, err := driver.NewPCAN(channel)
	if err != nil {
		return nil, fmt.Errorf("initialize PCAN LIN driver: %w", err)
	}
	return newPreset(drv, targetNAD, channel)
}

// NewPresetVector opens one Vector LIN channel with its default 19200-baud LIN
// 2.1 master configuration and binds a UDS client to targetNAD. The returned
// preset owns the driver.
func NewPresetVector(targetNAD byte, channel liniface.Channel, deviceType int) (*Preset, error) {
	drv, err := driver.NewVector(deviceType, channel)
	if err != nil {
		return nil, fmt.Errorf("initialize Vector LIN driver: %w", err)
	}
	return newPreset(drv, targetNAD, channel)
}
