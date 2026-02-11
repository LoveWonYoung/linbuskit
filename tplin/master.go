package tplin

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// Pre-defined errors for cleaner API
var (
	ErrTimeout            = errors.New("timed out waiting for slave to respond")
	ErrNegativeResponse   = errors.New("slave returned a negative response")
	ErrUnsupportedFeature = errors.New("slave does not support this feature")
	ErrCanceled           = errors.New("operation was canceled")
	ErrInvalidPayload     = errors.New("invalid payload length")
)

// NegativeResponseError provides detailed information about a negative response.
type NegativeResponseError struct {
	RequestedSID byte
	NRC          byte
}

func (e *NegativeResponseError) Error() string {
	return fmt.Sprintf("negative response: SID=0x%02X, NRC=0x%02X", e.RequestedSID, e.NRC)
}

func (e *NegativeResponseError) Unwrap() error {
	return ErrNegativeResponse
}

// LinMaster provides a high-level API for interacting with LIN slaves.
type LinMaster struct {
	transport *Transport
}

// NewMaster creates and initializes a new LinMaster instance.
// It starts the underlying transport layer's background processing.
func NewMaster(driver Driver) *LinMaster {
	// a transport layer instance is created for the master
	transport := NewTransport(false, driver) // isSlave = false
	transport.Run()

	return &LinMaster{
		transport: transport,
	}
}

// NewMasterWithConfig creates a LinMaster with custom transport configuration.
func NewMasterWithConfig(driver Driver, config TransportConfig) *LinMaster {
	transport := NewTransportWithConfig(false, driver, config)
	transport.Run()
	return &LinMaster{transport: transport}
}

// Close stops the transport layer's background processing.
// It should be called when the master is no longer needed.
func (m *LinMaster) Close() {
	m.transport.Close()
}

// waitForResponse is a helper function that waits for a specific response SID.
// This is the Go equivalent of the blocking while-loops in the Python version.
func (m *LinMaster) waitForResponse(expectedRsid byte, timeout time.Duration) (*LinMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return m.waitForResponseWithContext(ctx, expectedRsid)
}

// waitForResponseWithContext waits for a specific response SID with context support.
// This allows for external cancellation of the wait operation.
func (m *LinMaster) waitForResponseWithContext(ctx context.Context, expectedRsid byte) (*LinMessage, error) {
	msg, err := m.transport.ReceiveBlocking(ctx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrTimeout
		}
		return nil, err // ErrCanceled
	}

	if msg.SID == expectedRsid {
		return msg, nil
	} else if msg.SID == 0x7F { // Negative Response SID
		if len(msg.Data) >= 2 {
			return nil, &NegativeResponseError{
				RequestedSID: msg.Data[0],
				NRC:          msg.Data[1],
			}
		}
		return nil, ErrNegativeResponse
	}
	// If we received a message with an unexpected SID, we could either:
	// 1. Return an error (strict)
	// 2. Loop and wait (loose, but risky if many messages)
	// For now, lets returning an error as it's simpler and safer for a master expecting a specific response.
	return nil, fmt.Errorf("unexpected response SID: 0x%02X (expected 0x%02X)", msg.SID, expectedRsid)
}

// AssignSlaveNad sends an Assign NAD command and waits for a response.
func (m *LinMaster) AssignSlaveNad(newNad byte, supplierID, functionID uint16, nad byte, timeout time.Duration) (byte, error) {
	sid := AssignNadSID
	payload := make([]byte, 5)
	binary.LittleEndian.PutUint16(payload[0:], supplierID)
	binary.LittleEndian.PutUint16(payload[2:], functionID)
	payload[4] = newNad

	m.transport.Transmit(nad, byte(sid), payload)

	msg, err := m.waitForResponse(byte(sid)+0x40, timeout)
	if err != nil {
		return 0, err
	}
	return msg.NAD, nil
}

// ReadByIdentifier sends a Read By Identifier command and waits for the data.
func (m *LinMaster) ReadByIdentifier(identifier byte, supplierID, functionID uint16, nad byte, timeout time.Duration) (byte, []byte, error) {
	sid := ReadByIdentifierSID
	payload := make([]byte, 5)
	payload[0] = identifier
	binary.LittleEndian.PutUint16(payload[1:], supplierID)
	binary.LittleEndian.PutUint16(payload[3:], functionID)

	m.transport.Transmit(nad, byte(sid), payload)

	msg, err := m.waitForResponse(byte(sid)+0x40, timeout)
	if err != nil {
		return 0, nil, err
	}
	return msg.NAD, msg.Data, nil
}

// GetSlaveProductIdentifier is a helper that calls ReadByIdentifier for product info.
func (m *LinMaster) GetSlaveProductIdentifier(supplierID, functionID uint16, nad byte, timeout time.Duration) (byte, uint16, uint16, byte, error) {
	respNad, payload, err := m.ReadByIdentifier(DataIdentifierLinProductIdentifier, supplierID, functionID, nad, timeout)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if len(payload) < 5 {
		return 0, 0, 0, 0, errors.New("invalid payload length for product identifier")
	}

	respSupplierID := binary.LittleEndian.Uint16(payload[0:])
	respFunctionID := binary.LittleEndian.Uint16(payload[2:])
	variantID := payload[4]

	return respNad, respSupplierID, respFunctionID, variantID, nil
}

// GetSlaveSerialNumber is a helper that calls ReadByIdentifier for the serial number.
func (m *LinMaster) GetSlaveSerialNumber(supplierID, functionID uint16, nad byte, timeout time.Duration) (byte, []byte, error) {
	respNad, payload, err := m.ReadByIdentifier(DataIdentifierSerialNumber, supplierID, functionID, nad, timeout)
	if err != nil {
		return 0, nil, err
	}
	if len(payload) < 4 {
		return 0, nil, errors.New("invalid payload length for serial number")
	}
	return respNad, payload[:4], nil
}

// SendDiagnostic sends a raw diagnostic frame without waiting for a response.
func (m *LinMaster) SendDiagnostic(nad, sid byte, payload []byte) {
	m.transport.Transmit(nad, sid, payload)
}

// ReceiveDiagnostic performs a single, non-blocking check for any diagnostic message.
func (m *LinMaster) ReceiveDiagnostic() *LinMessage {
	return m.transport.Receive()
}
