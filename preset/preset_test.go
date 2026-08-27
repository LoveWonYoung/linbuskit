package preset

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LoveWonYoung/linbuskit/liniface"
	"github.com/LoveWonYoung/linbuskit/tplin"
)

type presetTestDriver struct {
	mu               sync.Mutex
	rx               chan *liniface.LinEvent
	pendingResponse  map[liniface.Channel]*liniface.LinEvent
	requestNADs      []byte
	channels         []liniface.Channel
	writes           []*liniface.LinEvent
	masterReadID     byte
	masterReadCh     liniface.Channel
	masterReadData   []byte
	masterReadErr    error
	masterReadCalls  int
	requestStarted   chan struct{}
	suppressResponse bool
	closeErr         error
	closeCalls       atomic.Int32
}

func newPresetTestDriver() *presetTestDriver {
	return &presetTestDriver{
		rx:              make(chan *liniface.LinEvent, 8),
		pendingResponse: make(map[liniface.Channel]*liniface.LinEvent),
	}
}

func (d *presetTestDriver) ReadEvent(timeout time.Duration, channel liniface.Channel) (*liniface.LinEvent, error) {
	d.recordChannel(channel)
	if timeout <= 0 {
		select {
		case event := <-d.rx:
			return clonePresetEvent(event), nil
		default:
			return nil, nil
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case event := <-d.rx:
		return clonePresetEvent(event), nil
	case <-timer.C:
		return nil, nil
	}
}

func (d *presetTestDriver) WriteMessage(event *liniface.LinEvent, channel liniface.Channel) error {
	d.recordChannel(channel)
	copyEvent := clonePresetEvent(event)
	d.mu.Lock()
	d.writes = append(d.writes, copyEvent)
	if event.EventID == tplin.MasterDiagnosticFrameID && len(event.EventPayload) >= 3 {
		if d.requestStarted != nil {
			select {
			case d.requestStarted <- struct{}{}:
			default:
			}
		}
		requestNAD := event.EventPayload[0]
		d.requestNADs = append(d.requestNADs, requestNAD)
		if d.suppressResponse {
			d.mu.Unlock()
			return nil
		}
		responseNAD := requestNAD
		if requestNAD == tplin.BroadcastNAD {
			responseNAD = 0x23
		}
		response := []byte{responseNAD, 0x02, event.EventPayload[2] + 0x40, 0xAA, 0xFF, 0xFF, 0xFF, 0xFF}
		d.pendingResponse[channel] = &liniface.LinEvent{
			Channel:      channel,
			EventID:      tplin.SlaveDiagnosticFrameID,
			EventPayload: response,
			ChecksumType: liniface.ClassicChecksum,
			Direction:    liniface.RX,
		}
	}
	d.mu.Unlock()
	return nil
}

func (d *presetTestDriver) ScheduleSlaveResponse(event *liniface.LinEvent, channel liniface.Channel) error {
	d.recordChannel(channel)
	return nil
}

func (d *presetTestDriver) RequestSlaveResponse(frameID byte, channel liniface.Channel) error {
	d.recordChannel(channel)
	d.mu.Lock()
	event := d.pendingResponse[channel]
	delete(d.pendingResponse, channel)
	d.mu.Unlock()
	if event != nil {
		d.rx <- clonePresetEvent(event)
	}
	return nil
}

func (d *presetTestDriver) MasterRead(frameID byte, channel liniface.Channel) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.masterReadCalls++
	d.masterReadID = frameID
	d.masterReadCh = channel
	return d.masterReadData, d.masterReadErr
}

func (d *presetTestDriver) Close() error {
	d.closeCalls.Add(1)
	return d.closeErr
}

func (d *presetTestDriver) recordChannel(channel liniface.Channel) {
	d.mu.Lock()
	d.channels = append(d.channels, channel)
	d.mu.Unlock()
}

func clonePresetEvent(event *liniface.LinEvent) *liniface.LinEvent {
	if event == nil {
		return nil
	}
	copyEvent := *event
	copyEvent.EventPayload = append([]byte(nil), event.EventPayload...)
	return &copyEvent
}

func TestPresetRequestAndFunctionRequest(t *testing.T) {
	drv := newPresetTestDriver()
	const channel = liniface.Channel(2)
	p, err := newPreset(drv, 0x11, channel)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()

	responseNAD, response, err := p.Request([]byte{0x22}, time.Second)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	if responseNAD != 0x11 || len(response) != 2 || response[0] != 0x62 || response[1] != 0xAA {
		t.Fatalf("Request response NAD=0x%02X payload=% X", responseNAD, response)
	}

	responseNAD, response, err = p.FunctionRequest([]byte{0x10}, time.Second)
	if err != nil {
		t.Fatalf("FunctionRequest failed: %v", err)
	}
	if responseNAD != 0x23 || len(response) != 2 || response[0] != 0x50 || response[1] != 0xAA {
		t.Fatalf("FunctionRequest response NAD=0x%02X payload=% X", responseNAD, response)
	}

	drv.mu.Lock()
	defer drv.mu.Unlock()
	if len(drv.requestNADs) != 2 || drv.requestNADs[0] != 0x11 || drv.requestNADs[1] != tplin.BroadcastNAD {
		t.Fatalf("request NADs = % X", drv.requestNADs)
	}
	for _, got := range drv.channels {
		if got != channel {
			t.Fatalf("driver channel = %d, want %d", got, channel)
		}
	}
}

func TestPresetWriteValidatesAndCopiesFrame(t *testing.T) {
	drv := newPresetTestDriver()
	p, err := newPreset(drv, 0x01, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()

	data := []byte{0x12, 0x34}
	if err := p.Write(0x22, data); err != nil {
		t.Fatal(err)
	}
	data[0] = 0xFF
	if err := p.Write(tplin.MasterDiagnosticFrameID, []byte{0x01}); err != nil {
		t.Fatal(err)
	}

	drv.mu.Lock()
	if len(drv.writes) != 2 {
		drv.mu.Unlock()
		t.Fatalf("write count = %d, want 2", len(drv.writes))
	}
	regular := clonePresetEvent(drv.writes[0])
	diagnostic := clonePresetEvent(drv.writes[1])
	drv.mu.Unlock()

	if regular.Channel != 3 || regular.Direction != liniface.TX || regular.ChecksumType != liniface.EnhancedChecksum {
		t.Fatalf("regular event = %+v", regular)
	}
	if regular.EventPayload[0] != 0x12 {
		t.Fatalf("Write retained caller payload: % X", regular.EventPayload)
	}
	if diagnostic.ChecksumType != liniface.ClassicChecksum {
		t.Fatalf("diagnostic checksum = %d, want classic", diagnostic.ChecksumType)
	}

	if err := p.Write(0x40, nil); !errors.Is(err, ErrInvalidFrameID) {
		t.Fatalf("invalid ID error = %v", err)
	}
	if err := p.Write(0x01, make([]byte, 9)); !errors.Is(err, ErrPayloadTooLong) {
		t.Fatalf("oversized payload error = %v", err)
	}
}

func TestPresetMasterReadDelegatesAndCopiesResponse(t *testing.T) {
	drv := newPresetTestDriver()
	drv.masterReadData = []byte{0x12, 0x34}
	p, err := newPreset(drv, 0x01, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()

	response, err := p.MasterRead(0x22)
	if err != nil {
		t.Fatal(err)
	}
	drv.mu.Lock()
	drv.masterReadData[0] = 0xFF
	calls := drv.masterReadCalls
	frameID := drv.masterReadID
	channel := drv.masterReadCh
	drv.mu.Unlock()

	if calls != 1 || frameID != 0x22 || channel != 3 {
		t.Fatalf("MasterRead calls=%d frame=0x%02X channel=%d", calls, frameID, channel)
	}
	if len(response) != 2 || response[0] != 0x12 || response[1] != 0x34 {
		t.Fatalf("MasterRead response=% X", response)
	}
}

func TestPresetMasterReadRejectsUnsupportedDriver(t *testing.T) {
	drv := newPresetTestDriver()
	withoutMasterRead := struct{ liniface.Driver }{Driver: drv}
	p, err := newPreset(withoutMasterRead, 0x01, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()

	if _, err := p.MasterRead(0x22); !errors.Is(err, ErrMasterReadUnsupported) {
		t.Fatalf("MasterRead error=%v", err)
	}
}

func TestPresetCloseIsConcurrentAndPreservesError(t *testing.T) {
	closeFailure := errors.New("close failure")
	drv := newPresetTestDriver()
	drv.closeErr = closeFailure
	p, err := newPreset(drv, 0x01, 0)
	if err != nil {
		t.Fatal(err)
	}

	const callers = 8
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			errs <- p.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, closeFailure) {
			t.Fatalf("Close error = %v", err)
		}
	}
	if got := drv.closeCalls.Load(); got != 1 {
		t.Fatalf("driver Close calls = %d, want 1", got)
	}
	if _, _, err := p.Request([]byte{0x22}, time.Second); !errors.Is(err, ErrPresetClosed) {
		t.Fatalf("Request after Close error = %v", err)
	}
	if err := p.Write(0x01, nil); !errors.Is(err, ErrPresetClosed) {
		t.Fatalf("Write after Close error = %v", err)
	}
	if _, err := p.MasterRead(0x01); !errors.Is(err, ErrPresetClosed) {
		t.Fatalf("MasterRead after Close error = %v", err)
	}
}

func TestPresetCloseWakesBlockedRequest(t *testing.T) {
	drv := newPresetTestDriver()
	drv.requestStarted = make(chan struct{}, 1)
	drv.suppressResponse = true
	p, err := newPreset(drv, 0x01, 0)
	if err != nil {
		t.Fatal(err)
	}

	requestDone := make(chan error, 1)
	go func() {
		_, _, err := p.Request([]byte{0x22}, time.Hour)
		requestDone <- err
	}()

	select {
	case <-drv.requestStarted:
	case <-time.After(time.Second):
		t.Fatal("request did not reach driver")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- p.Close() }()

	select {
	case err := <-requestDone:
		if err == nil {
			t.Fatal("blocked request returned nil error")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not wake blocked request")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not finish")
	}
}

func TestNewPresetRejectsNilDriver(t *testing.T) {
	p, err := newPreset(nil, 0x01, 0)
	if p != nil || !errors.Is(err, ErrNilDriver) {
		t.Fatalf("newPreset(nil) = (%v, %v)", p, err)
	}
}
