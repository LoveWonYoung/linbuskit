//go:build !windows

package uds_client

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/LoveWonYoung/linbuskit/liniface"
	"github.com/LoveWonYoung/linbuskit/tplin"
)

// MockUDSDriver 是一个模拟 UDS 从节点的驱动
type MockUDSDriver struct {
	mu              sync.Mutex
	rxQueue         chan *liniface.LinEvent
	txLog           []*liniface.LinEvent
	responseHandler func(sid byte, data []byte) (byte, []byte, error)
}

func NewMockUDSDriver() *MockUDSDriver {
	return &MockUDSDriver{
		rxQueue: make(chan *liniface.LinEvent, 50),
		txLog:   make([]*liniface.LinEvent, 0),
	}
}

// SetHandler 设置 UDS 请求处理函数
// handler 返回: (响应SID, 响应数据, 错误)
func (d *MockUDSDriver) SetHandler(handler func(sid byte, data []byte) (byte, []byte, error)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.responseHandler = handler
}

func (d *MockUDSDriver) ReadEvent(timeout time.Duration) (*liniface.LinEvent, error) {
	select {
	case e := <-d.rxQueue:
		return e, nil
	case <-time.After(timeout):
		return nil, nil
	}
}

func (d *MockUDSDriver) WriteMessage(event *liniface.LinEvent) error {
	d.mu.Lock()
	d.txLog = append(d.txLog, event)
	handler := d.responseHandler
	d.mu.Unlock()

	// 将 TX 事件放回队列
	txCopy := *event
	txCopy.Direction = liniface.TX
	d.rxQueue <- &txCopy

	// 解析请求并生成响应
	if handler != nil && event.EventID == tplin.MasterDiagnosticFrameID {
		go d.processRequest(event, handler)
	}

	return nil
}

func (d *MockUDSDriver) processRequest(event *liniface.LinEvent, handler func(byte, []byte) (byte, []byte, error)) {
	payload := event.EventPayload
	if len(payload) < 3 {
		return
	}

	nad := payload[0]
	pci := payload[1]
	pciType := liniface.PCIType(pci >> 4)

	if pciType != liniface.SF {
		return // 简化处理，只处理单帧
	}

	sid := payload[2]
	dataLen := int(pci&0x0F) - 1
	var data []byte
	if dataLen > 0 && len(payload) >= 3+dataLen {
		data = payload[3 : 3+dataLen]
	}

	// 模拟处理延迟
	time.Sleep(5 * time.Millisecond)

	respSid, respData, err := handler(sid, data)

	// 构造响应
	response := make([]byte, 8)
	for i := range response {
		response[i] = 0xFF
	}
	response[0] = nad

	if err != nil {
		// 否定响应
		response[1] = byte((liniface.SF << 4) | 3)
		response[2] = 0x7F // NRC SID
		response[3] = sid
		response[4] = 0x10 // GeneralReject
	} else {
		// 正响应
		respLen := 1 + len(respData)
		response[1] = byte(liniface.SF<<4) | byte(respLen)
		response[2] = respSid
		copy(response[3:], respData)
	}

	d.rxQueue <- &liniface.LinEvent{
		EventID:      tplin.SlaveDiagnosticFrameID,
		EventPayload: response,
		Direction:    liniface.RX,
	}
}

func (d *MockUDSDriver) ScheduleSlaveResponse(event *liniface.LinEvent) error {
	return nil
}

func (d *MockUDSDriver) RequestSlaveResponse(frameID byte) error {
	return nil
}

// =============================================================================
// Client 测试
// =============================================================================

// TestClientReadDataByIdentifier 测试读取数据标识符服务 (0x22)
func TestClientReadDataByIdentifier(t *testing.T) {
	driver := NewMockUDSDriver()

	// 设置模拟 ECU 响应
	// 注意: 单帧最多可携带 5 字节数据 (SID + 5 bytes = 6 bytes total in PCI length)
	driver.SetHandler(func(sid byte, data []byte) (byte, []byte, error) {
		if sid == 0x22 && len(data) >= 2 {
			did := (uint16(data[0]) << 8) | uint16(data[1])
			t.Logf("ECU收到读取请求: SID=0x%02X, DID=0x%04X", sid, did)

			switch did {
			case 0xF189: // Application Software Identification
				// 返回: DID(2字节) + 版本号(2字节) = 4字节
				return 0x62, []byte{0xF1, 0x89, 0x01, 0x00}, nil
			case 0xF190: // VIN
				return 0x62, []byte{0xF1, 0x90, 'W', 'V'}, nil
			default:
				return 0, nil, errors.New("unknown DID")
			}
		}
		return 0, nil, errors.New("invalid request")
	})

	client := NewClient(driver, 0x7F)
	defer client.Close()

	// 测试读取软件版本
	resp, err := client.SendAndRec([]byte{0x22, 0xF1, 0x89}, 2*time.Second)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}

	t.Logf("收到响应: % 02X", resp)

	if resp[0] != 0x62 {
		t.Errorf("响应SID错误: 期望 0x62, 实际 0x%02X", resp[0])
	}

	// 验证 DID echo
	if len(resp) >= 3 && (resp[1] != 0xF1 || resp[2] != 0x89) {
		t.Errorf("DID错误: 期望 [F1 89], 实际 [%02X %02X]", resp[1], resp[2])
	}

	t.Log("✅ ReadDataByIdentifier 测试通过")
}

// TestClientWithContext 测试带 Context 的请求
func TestClientWithContext(t *testing.T) {
	driver := NewMockUDSDriver()

	// 设置一个会延迟响应的处理器
	driver.SetHandler(func(sid byte, data []byte) (byte, []byte, error) {
		time.Sleep(500 * time.Millisecond) // 模拟慢速 ECU
		return sid + 0x40, data, nil
	})

	client := NewClient(driver, 0x7F)
	defer client.Close()

	// 使用一个会超时的 Context
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := client.SendAndRecWithContext(ctx, []byte{0x22, 0xF1, 0x89})
	if err == nil {
		t.Fatal("期望超时错误，但没有错误")
	}

	t.Logf("预期的超时错误: %v", err)
	t.Log("✅ Context 超时测试通过")
}

// TestClientNegativeResponse 测试否定响应处理
func TestClientNegativeResponse(t *testing.T) {
	driver := NewMockUDSDriver()

	driver.SetHandler(func(sid byte, data []byte) (byte, []byte, error) {
		// 总是返回否定响应
		return 0, nil, errors.New("service not supported")
	})

	client := NewClient(driver, 0x7F)
	defer client.Close()

	resp, err := client.SendAndRec([]byte{0x22, 0xFF, 0xFF}, 2*time.Second)
	if err == nil {
		t.Fatal("期望错误，但没有错误")
	}

	t.Logf("收到错误: %v", err)
	t.Logf("响应数据: % 02X", resp)

	if resp == nil || resp[0] != 0x7F {
		t.Error("否定响应格式错误")
	}

	t.Log("✅ 否定响应测试通过")
}

// TestClientDiagnosticSession 测试诊断会话控制服务 (0x10)
func TestClientDiagnosticSession(t *testing.T) {
	driver := NewMockUDSDriver()

	driver.SetHandler(func(sid byte, data []byte) (byte, []byte, error) {
		if sid == 0x10 && len(data) >= 1 {
			sessionType := data[0]
			t.Logf("ECU收到会话切换请求: SessionType=0x%02X", sessionType)

			// P2 Server Max = 50ms, P2* Server Max = 5000ms
			return 0x50, []byte{sessionType, 0x00, 0x32, 0x01, 0xF4}, nil
		}
		return 0, nil, errors.New("invalid request")
	})

	client := NewClient(driver, 0x7F)
	defer client.Close()

	// 切换到扩展诊断会话 (0x03)
	resp, err := client.SendAndRec([]byte{0x10, 0x03}, 2*time.Second)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}

	t.Logf("收到响应: % 02X", resp)

	if resp[0] != 0x50 {
		t.Errorf("响应SID错误: 期望 0x50, 实际 0x%02X", resp[0])
	}

	t.Log("✅ DiagnosticSessionControl 测试通过")
}

// =============================================================================
// 基准测试
// =============================================================================

func BenchmarkClientSendAndRec(b *testing.B) {
	driver := NewMockUDSDriver()

	driver.SetHandler(func(sid byte, data []byte) (byte, []byte, error) {
		return sid + 0x40, data, nil
	})

	client := NewClient(driver, 0x7F)
	defer client.Close()

	payload := []byte{0x22, 0xF1, 0x89}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = client.SendAndRec(payload, 1*time.Second)
	}
}
