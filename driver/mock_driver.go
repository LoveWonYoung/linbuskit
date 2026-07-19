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
	rxQueues map[liniface.Channel]chan *liniface.LinEvent

	// 发送记录 - 记录所有发送的消息
	txLog []*liniface.LinEvent

	// 预设响应 - 当收到特定帧ID的请求时自动响应
	scheduledResponses map[liniface.Channel]map[byte]*liniface.LinEvent

	// 回调函数 - 用于自定义响应逻辑
	responseHandler func(frameID byte) *liniface.LinEvent

	// 配置
	closed bool
}

// NewMockDriver 创建一个新的模拟驱动实例。
func NewMockDriver() *MockDriver {
	return &MockDriver{
		rxQueues:           make(map[liniface.Channel]chan *liniface.LinEvent),
		txLog:              make([]*liniface.LinEvent, 0),
		scheduledResponses: make(map[liniface.Channel]map[byte]*liniface.LinEvent),
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
	d.mu.Lock()
	rxQueue := d.rxQueue(event.Channel)
	d.mu.Unlock()
	select {
	case rxQueue <- event:
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
	if d.closed {
		return
	}
	d.closed = true
	for _, rxQueue := range d.rxQueues {
		close(rxQueue)
	}
}

// --- 实现 liniface.Driver 接口 ---

func (d *MockDriver) ReadEvent(timeout time.Duration, channel liniface.Channel) (*liniface.LinEvent, error) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, liniface.ErrDriverClosed
	}
	rxQueue := d.rxQueue(channel)
	d.mu.Unlock()
	select {
	case event, ok := <-rxQueue:
		if !ok {
			return nil, errors.New("driver closed")
		}
		return event, nil
	case <-time.After(timeout):
		return nil, nil // 超时不是错误
	}
}

func (d *MockDriver) WriteMessage(event *liniface.LinEvent, channel liniface.Channel) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return errors.New("driver closed")
	}

	// 记录发送
	eventCopy := *event
	eventCopy.Channel = channel
	eventCopy.Direction = liniface.TX
	eventCopy.Timestamp = time.Now()
	d.txLog = append(d.txLog, &eventCopy)

	// 同时将 TX 事件放入接收队列（模拟回环）
	txEvent := eventCopy
	select {
	case d.rxQueue(channel) <- &txEvent:
	default:
	}

	return nil
}

func (d *MockDriver) ScheduleSlaveResponse(event *liniface.LinEvent, channel liniface.Channel) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.scheduledResponses[channel] == nil {
		d.scheduledResponses[channel] = make(map[byte]*liniface.LinEvent)
	}
	d.scheduledResponses[channel][event.EventID] = event
	return nil
}

func (d *MockDriver) RequestSlaveResponse(frameID byte, channel liniface.Channel) error {
	d.mu.Lock()

	// 检查是否有预设响应
	if resp, ok := d.scheduledResponses[channel][frameID]; ok {
		delete(d.scheduledResponses[channel], frameID)
		d.mu.Unlock()

		rxEvent := *resp
		rxEvent.Channel = channel
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
			rxEvent.Channel = channel
			rxEvent.Direction = liniface.RX
			rxEvent.Timestamp = time.Now()
			d.InjectEvent(&rxEvent)
		}
	}

	return nil
}

func (d *MockDriver) rxQueue(channel liniface.Channel) chan *liniface.LinEvent {
	rxQueue := d.rxQueues[channel]
	if rxQueue == nil {
		rxQueue = make(chan *liniface.LinEvent, 50)
		d.rxQueues[channel] = rxQueue
	}
	return rxQueue
}
