package driver

import (
	"bytes"
	"log"
	"testing"

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
