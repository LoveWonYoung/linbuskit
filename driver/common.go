package driver

import "log"
import "sync/atomic"

var printLog atomic.Bool

func SetPrintLog(b bool) {
	printLog.Store(b)
}

func printLogEnabled() bool {
	return printLog.Load()
}

func logLINMessage(direction string, id byte, len_ byte, cs byte, data []byte) {
	if !printLogEnabled() {
		return
	}
	format := "%s LIN: ID=0x%02X, Len=%02d, CS=%02X, Data=% 02X"
	log.Printf(format, direction, id, len_, cs, data)
}
