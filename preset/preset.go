// Package preset provides ready-to-use combinations of a LIN hardware driver
// and a UDS over LIN client.
package preset

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LoveWonYoung/linbuskit/liniface"
	"github.com/LoveWonYoung/linbuskit/tplin"
	"github.com/LoveWonYoung/linbuskit/uds_client"
)

var (
	// ErrNilDriver indicates that a preset was constructed without a LIN driver.
	ErrNilDriver = errors.New("LIN driver is nil")
	// ErrPresetClosed indicates that an operation was attempted after Close.
	ErrPresetClosed = errors.New("LIN preset is closed")
	// ErrInvalidFrameID indicates that a raw LIN frame ID is outside 0x00..0x3F.
	ErrInvalidFrameID = errors.New("invalid LIN frame ID")
	// ErrPayloadTooLong indicates that a raw LIN frame contains more than 8 bytes.
	ErrPayloadTooLong = errors.New("LIN frame payload exceeds 8 bytes")
	// ErrMasterReadUnsupported indicates that the preset's hardware driver does
	// not implement liniface.MasterReader.
	ErrMasterReadUnsupported = errors.New("LIN driver does not support MasterRead")
)

type driverCloser interface {
	Close() error
}

// Preset owns a LIN hardware driver and a UDS client bound to one target NAD
// and one logical channel. Preset operations are serialized with each other.
// Close is safe to call repeatedly and concurrently and wakes blocked requests.
//
// LinDevice and Client are exposed for hardware-specific and advanced UDS
// operations. Callers must not replace them or read directly from LinDevice
// while a preset request is active because the transport then owns the driver's
// receive loop.
type Preset struct {
	// Nad is the default target node address used by Request.
	Nad byte
	// Channel is the logical LIN channel used by all preset operations.
	Channel liniface.Channel
	// LinDevice is the owned hardware driver. Do not read from it while a preset
	// request is active because Client then owns its receive loop.
	LinDevice liniface.Driver
	// Client is the UDS client configured for Nad and Channel.
	Client *uds_client.Client

	closed      atomic.Bool
	operationMu sync.Mutex
	closeOnce   sync.Once
	closeErr    error
}

func newPreset(drv liniface.Driver, targetNAD byte, channel liniface.Channel) (*Preset, error) {
	if drv == nil {
		return nil, ErrNilDriver
	}
	config := uds_client.DefaultClientConfig(targetNAD)
	config.Channel = channel
	return &Preset{
		Nad:       targetNAD,
		Channel:   channel,
		LinDevice: drv,
		Client:    uds_client.NewClientWithConfig(drv, config),
	}, nil
}

func (p *Preset) lockOperation() (func(), error) {
	if p == nil || p.closed.Load() {
		return nil, ErrPresetClosed
	}
	p.operationMu.Lock()
	if p.closed.Load() {
		p.operationMu.Unlock()
		return nil, ErrPresetClosed
	}
	return p.operationMu.Unlock, nil
}

// Close stops the UDS transport and then releases the owned hardware driver.
// Repeated calls return the same driver close result.
func (p *Preset) Close() error {
	if p == nil {
		return nil
	}
	p.closeOnce.Do(func() {
		p.closed.Store(true)
		if p.Client != nil {
			p.Client.Close()
		}
		p.operationMu.Lock()
		defer p.operationMu.Unlock()
		if closer, ok := p.LinDevice.(driverCloser); ok {
			if err := closer.Close(); err != nil {
				p.closeErr = fmt.Errorf("close LIN device: %w", err)
			}
		}
	})
	return p.closeErr
}

// Request sends payload to Nad and waits up to timeout for a matching
// response. The returned NAD identifies the node that produced the response.
// Payload is borrowed only for the duration of the call.
func (p *Preset) Request(payload []byte, timeout time.Duration) (byte, []byte, error) {
	unlock, err := p.lockOperation()
	if err != nil {
		return 0, nil, err
	}
	defer unlock()
	if p.Client == nil {
		return 0, nil, ErrPresetClosed
	}
	return p.Client.SendAndRec(payload, timeout)
}

// RequestWithNAD sends payload using nad for this request without changing the
// preset's default TargetNAD. The returned response slice is owned by the caller.
func (p *Preset) RequestWithNAD(nad byte, payload []byte, timeout time.Duration) (byte, []byte, error) {
	unlock, err := p.lockOperation()
	if err != nil {
		return 0, nil, err
	}
	defer unlock()
	if p.Client == nil {
		return 0, nil, ErrPresetClosed
	}
	return p.Client.SendAndRecWithNAD(nad, payload, timeout)
}

// FunctionRequest sends a broadcast NAD request and accepts a matching response
// from any actual slave NAD. The returned NAD identifies the responding node.
func (p *Preset) FunctionRequest(payload []byte, timeout time.Duration) (byte, []byte, error) {
	return p.RequestWithNAD(tplin.BroadcastNAD, payload, timeout)
}

// MasterRead sends a master header for frameID and returns the matching slave
// payload on the preset's channel. The returned slice is owned by the caller.
// Calls through the preset are serialized with Request and FunctionRequest, and
// the UDS transport is idle before MasterRead starts.
func (p *Preset) MasterRead(frameID byte) ([]byte, error) {
	unlock, err := p.lockOperation()
	if err != nil {
		return nil, err
	}
	defer unlock()

	reader, ok := p.LinDevice.(liniface.MasterReader)
	if !ok {
		return nil, ErrMasterReadUnsupported
	}
	response, err := reader.MasterRead(frameID, p.Channel)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), response...), nil
}

// Write sends one raw LIN master frame on the preset's channel. Write validates
// the frame bounds and copies data before crossing the driver boundary. It uses
// classic checksum for diagnostic frame IDs 0x3C and 0x3D and enhanced checksum
// for other frame IDs.
func (p *Preset) Write(frameID byte, data []byte) error {
	unlock, err := p.lockOperation()
	if err != nil {
		return err
	}
	defer unlock()
	if p.LinDevice == nil {
		return ErrPresetClosed
	}
	if frameID > 0x3F {
		return fmt.Errorf("%w: 0x%02X", ErrInvalidFrameID, frameID)
	}
	if len(data) > 8 {
		return fmt.Errorf("%w: got %d bytes", ErrPayloadTooLong, len(data))
	}
	checksum := liniface.EnhancedChecksum
	if frameID == tplin.MasterDiagnosticFrameID || frameID == tplin.SlaveDiagnosticFrameID {
		checksum = liniface.ClassicChecksum
	}
	event := &liniface.LinEvent{
		Channel:      p.Channel,
		EventID:      frameID,
		EventPayload: append([]byte(nil), data...),
		ChecksumType: checksum,
		Direction:    liniface.TX,
	}
	return p.LinDevice.WriteMessage(event, p.Channel)
}
