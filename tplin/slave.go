package tplin

import (
	"context"
	"encoding/binary"
	"log"
	"sync"
	"time"

	"github.com/LoveWonYoung/linbuskit/liniface"
)

// LinSlave represents the application layer of a LIN slave node.
type LinSlave struct {
	// Properties
	nad          byte
	savedNad     byte
	supplierID   uint16
	functionID   uint16
	variantID    byte
	serialNumber []byte

	frameIdentifiers []byte

	// Internal
	transport *Transport
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	mu        sync.Mutex // Protects access to slave properties
}

// NewSlave creates and initializes a new LinSlave instance.
func NewSlave(nad, variantID byte, supplierID, functionID uint16, serialNumber []byte, driver liniface.Driver) *LinSlave {
	if serialNumber == nil {
		serialNumber = []byte{0x01, 0x02, 0x03, 0x04}
	}
	// A transport layer instance is created for the slave
	transport := NewTransport(true, driver) // isSlave = true

	return &LinSlave{
		nad:              nad,
		variantID:        variantID,
		supplierID:       supplierID,
		functionID:       functionID,
		serialNumber:     serialNumber,
		frameIdentifiers: make([]byte, 5),
		transport:        transport,
	}
}

// Run starts the slave's main processing loop in a new goroutine.
func (s *LinSlave) Run() {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg.Add(1)

	go func() {
		defer s.wg.Done()
		log.Printf("Slave (NAD 0x%X): Starting simulation loop", s.nad)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				log.Printf("Slave (NAD 0x%X): Stopping simulation loop", s.nad)
				return
			case <-ticker.C:
				s.simulate()
			}
		}
	}()
}

// Stop gracefully terminates the slave's processing loop.
func (s *LinSlave) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

// simulate is the core logic loop, equivalent to the Python version's method.
func (s *LinSlave) simulate() {
	// Execute the transport layer to handle raw frame I/O
	err := s.transport.execute()
	if err != nil {
		log.Printf("Error executing command: %v", err)
		return
	}

	// Check if a complete message has been received
	msg := s.transport.Receive()
	if msg == nil {
		return // No message to process
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if the message is for this slave (or broadcast)
	if msg.NAD != s.nad && msg.NAD != BroadcastNAD {
		return
	}

	log.Printf("Slave (NAD 0x%X): Received command with SID 0x%X", s.nad, msg.SID)

	switch msg.SID {
	case ReadByIdentifierSID:
		s.handleReadByIdentifier(msg)
	case SaveConfigurationSID:
		s.savedNad = s.nad
		s.transport.Transmit(s.nad, msg.SID+0x40, []byte{})
	case AssignNadSID:
		s.handleAssignNad(msg)
	// Add other SID handlers here as needed
	default:
		// Silently ignore unhandled SIDs
	}
}

// transmitNegativeResponse is a helper to send standardized error responses.
func (s *LinSlave) transmitNegativeResponse(requestedSID, errorCode byte) {
	s.transport.Transmit(s.nad, 0x7F, []byte{requestedSID, errorCode})
}

func (s *LinSlave) matchesID(reqSupplierID, reqFunctionID uint16) bool {
	supplierMatch := reqSupplierID == s.supplierID || reqSupplierID == BroadcastSupplierID
	functionMatch := reqFunctionID == s.functionID || reqFunctionID == BroadcastFunctionID
	return supplierMatch && functionMatch
}

func (s *LinSlave) getIDBytes(idType byte) []byte {
	switch idType {
	case DataIdentifierLinProductIdentifier:
		resp := make([]byte, 5)
		binary.LittleEndian.PutUint16(resp[0:], s.supplierID)
		binary.LittleEndian.PutUint16(resp[2:], s.functionID)
		resp[4] = s.variantID
		return resp
	case DataIdentifierSerialNumber:
		return s.serialNumber
	default:
		log.Printf("Slave (NAD 0x%X): Unsupported identifier type: %d", s.nad, idType)
		return nil
	}
}

// --- Specific SID Handlers ---

func (s *LinSlave) handleReadByIdentifier(msg *LinMessage) {
	if len(msg.Data) < 5 {
		s.transmitNegativeResponse(msg.SID, 0x13) // IncorrectMessageLength
		return
	}
	identifier := msg.Data[0]
	supplierID := binary.LittleEndian.Uint16(msg.Data[1:])
	functionID := binary.LittleEndian.Uint16(msg.Data[3:])

	if s.matchesID(supplierID, functionID) {
		response := s.getIDBytes(identifier)
		if response != nil {
			s.transport.Transmit(s.nad, msg.SID+0x40, response)
		} else {
			s.transmitNegativeResponse(msg.SID, 0x12) // SubFunctionNotSupported
		}
	}
}

func (s *LinSlave) handleAssignNad(msg *LinMessage) {
	if len(msg.Data) < 5 {
		return
	}
	supplierID := binary.LittleEndian.Uint16(msg.Data[0:])
	functionID := binary.LittleEndian.Uint16(msg.Data[2:])
	newNad := msg.Data[4]

	if s.matchesID(supplierID, functionID) {
		// Per spec, response is sent with the old NAD
		s.transport.Transmit(s.nad, msg.SID+0x40, []byte{})
		// Then the NAD is changed
		s.nad = newNad
		log.Printf("Slave NAD changed to 0x%X", s.nad)
	}
}
