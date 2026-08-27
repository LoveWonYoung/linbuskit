//go:build windows || (darwin && cgo)

package preset

import (
	"fmt"

	"github.com/LoveWonYoung/linbuskit/driver"
	"github.com/LoveWonYoung/linbuskit/liniface"
)

// NewPresetToomoss opens one Toomoss LIN channel in master mode and binds a
// UDS client to targetNAD. The returned preset owns the driver.
func NewPresetToomoss(targetNAD byte, channel liniface.Channel) (*Preset, error) {
	drv, err := driver.NewToomoss([]driver.ToomossCh{channel}, driver.Master)
	if err != nil {
		return nil, fmt.Errorf("initialize Toomoss LIN driver: %w", err)
	}
	return newPreset(drv, targetNAD, channel)
}
