//go:build !windows

package driver

import (
	"errors"
	"log"
	"sync"
	"time"

	"github.com/LoveWonYoung/linbuskit/liniface"
)

// MockDriver 是一个用于测试的模拟 LIN 驱动。
// 它可以在任何平台上运行，包括 Darwin (macOS)。
type MockDriver struct {
	mu sync.Mutex

	// 接收队列 - 模拟从总线接收到的事件
	rxQueue chan *liniface.LinEvent

	// 发送记录 - 记录所有发送的消息
	txLog []*liniface.LinEvent

	// 预设响应 - 当收到特定帧ID的请求时自动响应
	scheduledResponses map[byte]*liniface.LinEvent

	// 回调函数 - 用于自定义响应逻辑
	responseHandler func(frameID byte) *liniface.LinEvent

	// 配置
	closed bool
}

// NewMockDriver 创建一个新的模拟驱动实例。
func NewMockDriver() *MockDriver {
	return &MockDriver{
		rxQueue:            make(chan *liniface.LinEvent, 50),
		txLog:              make([]*liniface.LinEvent, 0),
		scheduledResponses: make(map[byte]*liniface.LinEvent),
	}
}

// SetResponseHandler 设置自定义响应处理函数。
// 当 RequestSlaveResponse 被调用时，会使用此函数生成响应。
func (d *MockDriver) SetResponseHandler(handler func(frameID byte) *liniface.LinEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.responseHandler = handler
}

// InjectEvent 向接收队列注入一个事件（模拟从总线收到数据）。
func (d *MockDriver) InjectEvent(event *liniface.LinEvent) {
	select {
	case d.rxQueue <- event:
	default:
		log.Println("MockDriver: RX queue is full, dropping event")
	}
}

// GetTxLog 返回所有发送记录的副本。
func (d *MockDriver) GetTxLog() []*liniface.LinEvent {
	d.mu.Lock()
	defer d.mu.Unlock()
	result := make([]*liniface.LinEvent, len(d.txLog))
	copy(result, d.txLog)
	return result
}

// ClearTxLog 清空发送记录。
func (d *MockDriver) ClearTxLog() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.txLog = d.txLog[:0]
}

// Close 关闭模拟驱动。
func (d *MockDriver) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	close(d.rxQueue)
}

// --- 实现 liniface.Driver 接口 ---

func (d *MockDriver) ReadEvent(timeout time.Duration) (*liniface.LinEvent, error) {
	select {
	case event, ok := <-d.rxQueue:
		if !ok {
			return nil, errors.New("driver closed")
		}
		return event, nil
	case <-time.After(timeout):
		return nil, nil // 超时不是错误
	}
}

func (d *MockDriver) WriteMessage(event *liniface.LinEvent) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return errors.New("driver closed")
	}

	// 记录发送
	eventCopy := *event
	eventCopy.Direction = liniface.TX
	eventCopy.Timestamp = time.Now()
	d.txLog = append(d.txLog, &eventCopy)

	// 同时将 TX 事件放入接收队列（模拟回环）
	txEvent := eventCopy
	select {
	case d.rxQueue <- &txEvent:
	default:
	}

	return nil
}

func (d *MockDriver) ScheduleSlaveResponse(event *liniface.LinEvent) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.scheduledResponses[event.EventID] = event
	return nil
}

func (d *MockDriver) RequestSlaveResponse(frameID byte) error {
	d.mu.Lock()

	// 检查是否有预设响应
	if resp, ok := d.scheduledResponses[frameID]; ok {
		delete(d.scheduledResponses, frameID)
		d.mu.Unlock()

		rxEvent := *resp
		rxEvent.Direction = liniface.RX
		rxEvent.Timestamp = time.Now()
		d.InjectEvent(&rxEvent)
		return nil
	}

	// 检查是否有响应处理函数
	handler := d.responseHandler
	d.mu.Unlock()

	if handler != nil {
		if resp := handler(frameID); resp != nil {
			rxEvent := *resp
			rxEvent.Direction = liniface.RX
			rxEvent.Timestamp = time.Now()
			d.InjectEvent(&rxEvent)
		}
	}

	return nil
}
