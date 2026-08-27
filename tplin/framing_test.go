package tplin

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/LoveWonYoung/linbuskit/liniface"
)

// fakeLINDriver 是一个仅供本包测试使用的最小驱动。
type fakeLINDriver struct {
	mu        sync.Mutex
	rxEvents  chan *liniface.LinEvent
	txEvents  []*liniface.LinEvent
	responses map[byte]*liniface.LinEvent
}

func newFakeLINDriver() *fakeLINDriver {
	return &fakeLINDriver{
		rxEvents:  make(chan *liniface.LinEvent, 50),
		txEvents:  make([]*liniface.LinEvent, 0),
		responses: make(map[byte]*liniface.LinEvent),
	}
}

func (d *fakeLINDriver) ReadEvent(timeout time.Duration, channel liniface.Channel) (*liniface.LinEvent, error) {
	select {
	case e := <-d.rxEvents:
		return e, nil
	case <-time.After(timeout):
		return nil, nil
	}
}

func (d *fakeLINDriver) WriteMessage(event *liniface.LinEvent, channel liniface.Channel) error {
	d.mu.Lock()
	eventCopy := *event
	eventCopy.EventPayload = append([]byte(nil), event.EventPayload...)
	d.txEvents = append(d.txEvents, &eventCopy)
	d.mu.Unlock()
	// 将 TX 事件放回队列供 transport 确认
	txCopy := eventCopy
	txCopy.Channel = channel
	txCopy.Direction = liniface.TX
	d.rxEvents <- &txCopy
	return nil
}

func (d *fakeLINDriver) ScheduleSlaveResponse(event *liniface.LinEvent, channel liniface.Channel) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	eventCopy := *event
	eventCopy.EventPayload = append([]byte(nil), event.EventPayload...)
	d.responses[event.EventID] = &eventCopy
	return nil
}

func (d *fakeLINDriver) RequestSlaveResponse(frameID byte, channel liniface.Channel) error {
	d.mu.Lock()
	if resp, ok := d.responses[frameID]; ok {
		delete(d.responses, frameID)
		d.mu.Unlock()
		rxCopy := *resp
		rxCopy.EventPayload = append([]byte(nil), resp.EventPayload...)
		rxCopy.Channel = channel
		rxCopy.Direction = liniface.RX
		d.rxEvents <- &rxCopy
		return nil
	}
	d.mu.Unlock()
	return nil
}

func (d *fakeLINDriver) TxEvents() []*liniface.LinEvent {
	d.mu.Lock()
	defer d.mu.Unlock()
	result := make([]*liniface.LinEvent, len(d.txEvents))
	for i, event := range d.txEvents {
		eventCopy := *event
		eventCopy.EventPayload = append([]byte(nil), event.EventPayload...)
		result[i] = &eventCopy
	}
	return result
}

func (d *fakeLINDriver) InjectRxEvent(event *liniface.LinEvent) {
	eventCopy := *event
	eventCopy.EventPayload = append([]byte(nil), event.EventPayload...)
	d.rxEvents <- &eventCopy
}

// =============================================================================
// 组帧测试
// =============================================================================

// TestSingleFrameTransmit 测试单帧发送
func TestSingleFrameTransmit(t *testing.T) {
	driver := newFakeLINDriver()
	transport := NewTransport(false, driver) // Master mode
	transport.Run()
	defer transport.Close()

	// 发送一个短消息（≤5字节数据）
	nad := byte(0x7F)
	sid := byte(0x22)
	data := []byte{0xF1, 0x89}

	if err := transport.Transmit(nad, sid, data); err != nil {
		t.Fatal(err)
	}

	// 等待发送
	time.Sleep(50 * time.Millisecond)

	txEvents := driver.TxEvents()
	if len(txEvents) == 0 {
		t.Fatal("没有发送任何帧")
	}

	frame := txEvents[0]
	t.Logf("发送的帧: ID=0x%02X, Payload=% 02X", frame.EventID, frame.EventPayload)

	// 验证帧内容
	if frame.EventID != MasterDiagnosticFrameID {
		t.Errorf("帧ID错误: 期望 0x%02X, 实际 0x%02X", MasterDiagnosticFrameID, frame.EventID)
	}

	payload := frame.EventPayload
	if payload[0] != nad {
		t.Errorf("NAD错误: 期望 0x%02X, 实际 0x%02X", nad, payload[0])
	}

	// PCI: SF (0x0) | length (3 = SID + 2 bytes data)
	expectedPCI := byte((liniface.SF << 4) | 3)
	if payload[1] != expectedPCI {
		t.Errorf("PCI错误: 期望 0x%02X, 实际 0x%02X", expectedPCI, payload[1])
	}

	if payload[2] != sid {
		t.Errorf("SID错误: 期望 0x%02X, 实际 0x%02X", sid, payload[2])
	}

	if payload[3] != 0xF1 || payload[4] != 0x89 {
		t.Errorf("数据错误: 期望 [F1 89], 实际 [%02X %02X]", payload[3], payload[4])
	}

	t.Log("✅ 单帧发送测试通过")
}

// TestMultiFrameTransmit 测试多帧发送
func TestMultiFrameTransmit(t *testing.T) {
	driver := newFakeLINDriver()
	transport := NewTransport(false, driver)
	transport.Run()
	defer transport.Close()

	nad := byte(0x7F)
	sid := byte(0x36) // TransferData SID
	// 创建一个需要多帧传输的数据（>5字节）
	data := make([]byte, 20)
	for i := range data {
		data[i] = byte(i)
	}

	if err := transport.Transmit(nad, sid, data); err != nil {
		t.Fatal(err)
	}

	// 等待发送
	time.Sleep(100 * time.Millisecond)

	txEvents := driver.TxEvents()
	if len(txEvents) < 2 {
		t.Fatalf("期望至少2帧，实际发送 %d 帧", len(txEvents))
	}

	t.Logf("共发送 %d 帧", len(txEvents))

	// 验证第一帧 (FF)
	ff := txEvents[0]
	t.Logf("FF: % 02X", ff.EventPayload)

	pciType := liniface.PCIType(ff.EventPayload[1] >> 4)
	if pciType != liniface.FF {
		t.Errorf("第一帧类型错误: 期望 FF(1), 实际 %d", pciType)
	}

	// 验证后续帧 (CF)
	for i := 1; i < len(txEvents); i++ {
		cf := txEvents[i]
		t.Logf("CF %d: % 02X", i, cf.EventPayload)

		pciType := liniface.PCIType(cf.EventPayload[1] >> 4)
		if pciType != liniface.CF {
			t.Errorf("帧 %d 类型错误: 期望 CF(2), 实际 %d", i, pciType)
		}

		// 验证帧计数器
		frameCounter := cf.EventPayload[1] & 0x0F
		expectedCounter := byte(i % 16)
		if frameCounter != expectedCounter {
			t.Errorf("帧 %d 计数器错误: 期望 %d, 实际 %d", i, expectedCounter, frameCounter)
		}
	}

	t.Log("✅ 多帧发送测试通过")
}

// TestSingleFrameReceive 测试单帧接收
func TestSingleFrameReceive(t *testing.T) {
	driver := newFakeLINDriver()
	transport := NewTransport(false, driver)
	transport.Run()
	defer transport.Close()
	transport.SetAwaitingSlaveResponse(true)

	// 构造一个单帧响应
	nad := byte(0x7F)
	sid := byte(0x62) // 正响应 SID (0x22 + 0x40)
	responseData := []byte{0xF1, 0x89, 0x01, 0x02}

	sfPayload := make([]byte, 8)
	for i := range sfPayload {
		sfPayload[i] = 0xFF
	}
	sfPayload[0] = nad
	sfPayload[1] = byte(liniface.SF<<4) | byte(len(responseData)+1) // PCI: SF | length
	sfPayload[2] = sid
	copy(sfPayload[3:], responseData)

	// 注入响应
	driver.InjectRxEvent(&liniface.LinEvent{
		EventID:      SlaveDiagnosticFrameID,
		EventPayload: sfPayload,
		Direction:    liniface.RX,
	})

	// 等待处理
	time.Sleep(50 * time.Millisecond)

	msg := transport.Receive()
	if msg == nil {
		t.Fatal("没有收到消息")
	}

	t.Logf("收到消息: NAD=0x%02X, SID=0x%02X, Data=% 02X", msg.NAD, msg.SID, msg.Data)

	if msg.NAD != nad {
		t.Errorf("NAD错误: 期望 0x%02X, 实际 0x%02X", nad, msg.NAD)
	}

	if msg.SID != sid {
		t.Errorf("SID错误: 期望 0x%02X, 实际 0x%02X", sid, msg.SID)
	}

	if !bytes.Equal(msg.Data, responseData) {
		t.Errorf("数据错误: 期望 % 02X, 实际 % 02X", responseData, msg.Data)
	}

	t.Log("✅ 单帧接收测试通过")
}

// TestNegativeResponse 测试否定响应处理
func TestNegativeResponse(t *testing.T) {
	driver := newFakeLINDriver()
	transport := NewTransport(false, driver)
	transport.Run()
	defer transport.Close()
	transport.SetAwaitingSlaveResponse(true)

	// 构造一个否定响应
	nad := byte(0x7F)
	nrcSid := byte(0x7F)     // 否定响应 SID
	requestSid := byte(0x22) // 原始请求 SID
	nrcCode := byte(0x31)    // RequestOutOfRange

	sfPayload := make([]byte, 8)
	for i := range sfPayload {
		sfPayload[i] = 0xFF
	}
	sfPayload[0] = nad
	sfPayload[1] = byte((liniface.SF << 4) | 3) // PCI: SF | length=3
	sfPayload[2] = nrcSid
	sfPayload[3] = requestSid
	sfPayload[4] = nrcCode

	driver.InjectRxEvent(&liniface.LinEvent{
		EventID:      SlaveDiagnosticFrameID,
		EventPayload: sfPayload,
		Direction:    liniface.RX,
	})

	time.Sleep(50 * time.Millisecond)

	msg := transport.Receive()
	if msg == nil {
		t.Fatal("没有收到消息")
	}

	t.Logf("收到否定响应: SID=0x%02X, Data=% 02X", msg.SID, msg.Data)

	if msg.SID != 0x7F {
		t.Errorf("否定响应 SID 错误: 期望 0x7F, 实际 0x%02X", msg.SID)
	}

	if len(msg.Data) < 2 || msg.Data[0] != requestSid || msg.Data[1] != nrcCode {
		t.Errorf("否定响应数据错误")
	}

	t.Log("✅ 否定响应测试通过")
}

// =============================================================================
// 基准测试
// =============================================================================

func BenchmarkSingleFrameTransmit(b *testing.B) {
	driver := newFakeLINDriver()
	transport := NewTransport(false, driver)

	nad := byte(0x7F)
	sid := byte(0x22)
	data := []byte{0xF1, 0x89}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := transport.Transmit(nad, sid, data); err != nil {
			b.Fatal(err)
		}
		<-transport.txQueue
	}
}

func BenchmarkMultiFrameTransmit(b *testing.B) {
	driver := newFakeLINDriver()
	config := DefaultTransportConfig()
	config.TxQueueSize = 32
	transport := NewTransportWithConfig(false, driver, config)

	nad := byte(0x7F)
	sid := byte(0x36)
	data := make([]byte, 100)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := transport.Transmit(nad, sid, data); err != nil {
			b.Fatal(err)
		}
		for len(transport.txQueue) > 0 {
			<-transport.txQueue
		}
	}
}
