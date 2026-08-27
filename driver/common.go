package driver

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LoveWonYoung/linbuskit/liniface"
)

const masterReadTimeout = 100 * time.Millisecond

var (
	// ErrNoSlaveResponse indicates that MasterRead did not receive a matching
	// slave response before the driver's read timeout expired.
	ErrNoSlaveResponse = errors.New("no response from slave")
)

type channelMutexes struct {
	mu    sync.Mutex
	locks map[liniface.Channel]*sync.Mutex
}

func (m *channelMutexes) lock(channel liniface.Channel) func() {
	m.mu.Lock()
	if m.locks == nil {
		m.locks = make(map[liniface.Channel]*sync.Mutex)
	}
	channelLock := m.locks[channel]
	if channelLock == nil {
		channelLock = &sync.Mutex{}
		m.locks[channel] = channelLock
	}
	m.mu.Unlock()

	channelLock.Lock()
	return channelLock.Unlock
}

func readMasterResponse(
	frameID byte,
	channel liniface.Channel,
	timeout time.Duration,
	request func(byte, liniface.Channel) error,
	read func(time.Duration, liniface.Channel) (*liniface.LinEvent, error),
) ([]byte, error) {
	if frameID > 0x3F {
		return nil, fmt.Errorf("invalid LIN frame ID 0x%02X", frameID)
	}
	if err := request(frameID, channel); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, ErrNoSlaveResponse
		}
		event, err := read(remaining, channel)
		if err != nil {
			return nil, err
		}
		if event == nil || event.Channel != channel || event.EventID != frameID || event.Direction != liniface.RX {
			continue
		}
		result := make([]byte, len(event.EventPayload))
		copy(result, event.EventPayload)
		return result, nil
	}
}

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
