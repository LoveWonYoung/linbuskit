package driver

import (
	"log"
	"sync/atomic"

	"github.com/LoveWonYoung/linbuskit/liniface"
)

const (
	logDevicePCAN     = "PCAN"
	logDeviceToomoss  = "TOOMOSS"
	logDeviceTSMaster = "TSMASTER"
	logDeviceVector   = "VECTOR"
)

var printLog atomic.Bool

// SetPrintLog enables or disables all device-driver logs. Logging is disabled
// by default. The setting is process-wide and safe for concurrent use.
func SetPrintLog(enabled bool) {
	printLog.Store(enabled)
}

func printLogEnabled() bool {
	return printLog.Load()
}

func logDriverf(device, format string, args ...any) {
	if !printLogEnabled() {
		return
	}
	log.Printf("[%s] "+format, append([]any{device}, args...)...)
}

func logLINMessage(device, direction string, channel liniface.Channel, id, checksum byte, data []byte) {
	logDriverf(
		device,
		"LIN direction=%s channel=%d id=0x%02X length=%d checksum=0x%02X data=% X",
		direction,
		channel,
		id,
		len(data),
		checksum,
		data,
	)
}

func logLINHeader(device string, channel liniface.Channel, id, length byte) {
	logDriverf(
		device,
		"LIN direction=TX_HEADER channel=%d id=0x%02X length=%d",
		channel,
		id,
		length,
	)
}

func logLINNoResponse(device string, channel liniface.Channel, id byte) {
	logDriverf(
		device,
		"LIN direction=RX channel=%d id=0x%02X status=no_response",
		channel,
		id,
	)
}
