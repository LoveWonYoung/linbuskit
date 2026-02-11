package uds_client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gitee.com/lovewonyoung/tp_driver/tplin"
)

// ClientConfig holds configuration options for the UDS client.
type ClientConfig struct {
	TargetNad      byte
	DefaultTimeout time.Duration
	MaxRetries     int
}

// DefaultClientConfig returns a configuration with sensible defaults.
func DefaultClientConfig(targetNad byte) ClientConfig {
	return ClientConfig{
		TargetNad:      targetNad,
		DefaultTimeout: 2 * time.Second,
		MaxRetries:     3,
	}
}

// Client 是一个高阶 UDS 客户端，用于与单个 LIN 从节点（ECU）进行诊断通信。
type Client struct {
	master *tplin.LinMaster
	config ClientConfig
}

// NewClient 创建一个新的 UDS 客户端实例。
func NewClient(driver tplin.Driver, targetNad byte) *Client {
	return NewClientWithConfig(driver, DefaultClientConfig(targetNad))
}

// NewClientWithConfig 使用自定义配置创建客户端。
func NewClientWithConfig(driver tplin.Driver, config ClientConfig) *Client {
	master := tplin.NewMaster(driver)
	return &Client{
		master: master,
		config: config,
	}
}

// Close 优雅地关闭客户端并释放底层资源。
func (c *Client) Close() {
	c.master.Close()
}

// SendAndRec 是最常用的接口，使用默认 NAD 发送并等待响应。
func (c *Client) SendAndRec(payload []byte, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.sendAndRec(ctx, c.config.TargetNad, payload)
}

// SendAndRecWithContext 支持外部传入 Context，仍使用默认 NAD。
func (c *Client) SendAndRecWithContext(ctx context.Context, payload []byte) ([]byte, error) {
	return c.sendAndRec(ctx, c.config.TargetNad, payload)
}

// SendAndRecWithNAD 允许调用时临时指定 NAD，而无需重新创建 Client。
func (c *Client) SendAndRecWithNAD(nad byte, payload []byte, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.sendAndRec(ctx, nad, payload)
}

// sendAndRec 核心实现，支持自定义 NAD 与外部 Context。
func (c *Client) sendAndRec(ctx context.Context, nad byte, payload []byte) ([]byte, error) {
	if len(payload) == 0 {
		return nil, fmt.Errorf("UDS请求负载不能为空")
	}

	// 1. 在发送前，清空接收队列中可能存在的残留消息。
	for i := 0; i < 10; i++ {
		if c.master.ReceiveDiagnostic() == nil {
			break
		}
	}

	// 2. 发送诊断请求（首字节为 SID，其余为数据）。
	sid := payload[0]
	data := payload[1:]
	c.master.SendDiagnostic(nad, sid, data)

	// 3. 轮询等待响应，支持超时/NRC/响应挂起处理。
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("等待UDS响应超时")
			}
			return nil, fmt.Errorf("操作被取消: %w", ctx.Err())
		case <-ticker.C:
			msg := c.master.ReceiveDiagnostic()
			if msg == nil {
				continue
			}
			if msg.SID == 0x7F { // Negative Response
				if len(msg.Data) >= 2 && msg.Data[0] == sid && msg.Data[1] == 0x78 {
					// Response Pending, 继续等待
					continue
				}
				fullNrcResponse := append([]byte{msg.SID}, msg.Data...)
				return fullNrcResponse, fmt.Errorf("server : %02X 收到负响应 (NRC: 0x%02X - %s)", msg.SID, msg.Data[1], GetNrcString(msg.Data[1]))
			}
			if msg.SID == (sid + 0x40) { // Positive Response
				fullPositiveResponse := append([]byte{msg.SID}, msg.Data...)
				return fullPositiveResponse, nil
			}
		}
	}
}
