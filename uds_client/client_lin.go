package uds_client

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/LoveWonYoung/linbuskit/liniface"
	"github.com/LoveWonYoung/linbuskit/tplin"
)

var ErrResponseTimeout = errors.New("waiting for UDS response timed out")

// ClientConfig holds configuration options for the UDS client.
type ClientConfig struct {
	TargetNad      byte
	DefaultTimeout time.Duration
	MaxRetries     int
	// Channel selects the LIN channel used by this client.
	Channel liniface.Channel
	// ContinuousSlavePoll 透传到 Transport：true 时空闲也持续请求 0x3D。
	ContinuousSlavePoll bool
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
	master    *tplin.LinMaster
	config    ClientConfig
	requestMu sync.Mutex
}

// NewClient 创建一个新的 UDS 客户端实例。
func NewClient(driver liniface.Driver, targetNad byte) *Client {
	return NewClientWithConfig(driver, DefaultClientConfig(targetNad))
}

// NewClientWithConfig 使用自定义配置创建客户端。
func NewClientWithConfig(driver liniface.Driver, config ClientConfig) *Client {
	tpCfg := tplin.DefaultTransportConfig()
	tpCfg.ContinuousSlavePoll = config.ContinuousSlavePoll
	master := tplin.NewMasterWithConfig(driver, tpCfg, config.Channel)
	return &Client{
		master: master,
		config: config,
	}
}

// Close 优雅地关闭客户端并释放底层资源。
func (c *Client) Close() {
	c.master.Close()
}

// SendAndRec 使用默认 NAD 发送并等待响应，返回实际响应节点的 NAD。
// timeout <= 0 时使用 DefaultTimeout；仅超时错误会按 MaxRetries 重试。
func (c *Client) SendAndRec(payload []byte, timeout time.Duration) (byte, []byte, error) {
	return c.sendAndRecWithRetries(c.config.TargetNad, payload, timeout)
}

// SendAndRecWithContext 支持外部 Context，使用默认 NAD，且不自动重试。
func (c *Client) SendAndRecWithContext(ctx context.Context, payload []byte) (byte, []byte, error) {
	c.requestMu.Lock()
	defer c.requestMu.Unlock()
	return c.sendAndRec(ctx, c.config.TargetNad, payload)
}

// SendAndRecWithNAD 临时指定请求 NAD，并返回实际响应节点的 NAD。
func (c *Client) SendAndRecWithNAD(nad byte, payload []byte, timeout time.Duration) (byte, []byte, error) {
	return c.sendAndRecWithRetries(nad, payload, timeout)
}

func (c *Client) sendAndRecWithRetries(nad byte, payload []byte, timeout time.Duration) (byte, []byte, error) {
	c.requestMu.Lock()
	defer c.requestMu.Unlock()
	if timeout <= 0 {
		timeout = c.config.DefaultTimeout
	}
	attempts := c.config.MaxRetries + 1
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		responseNAD, response, err := c.sendAndRec(ctx, nad, payload)
		cancel()
		if err == nil || !errors.Is(err, ErrResponseTimeout) {
			return responseNAD, response, err
		}
		lastErr = err
	}
	return 0, nil, lastErr
}

// sendAndRec 核心实现，支持自定义 NAD 与外部 Context。
func (c *Client) sendAndRec(ctx context.Context, nad byte, payload []byte) (byte, []byte, error) {
	if len(payload) == 0 {
		return 0, nil, fmt.Errorf("UDS请求负载不能为空")
	}

	// 1. 在发送前，清空接收队列中可能存在的残留消息。
	for {
		if c.master.ReceiveDiagnostic() == nil {
			break
		}
	}

	// 2. 发送诊断请求（首字节为 SID，其余为数据）。
	sid := payload[0]
	data := payload[1:]
	if err := c.master.SendDiagnostic(nad, sid, data); err != nil {
		return 0, nil, fmt.Errorf("发送UDS请求失败: %w", err)
	}
	// 无论成功/失败/超时，结束本次会话后停止空闲 0x3D 轮询（ContinuousSlavePoll=false 时生效）。
	defer c.master.StopAwaitingSlaveResponse()

	// 3. 轮询等待响应，支持超时/NRC/响应挂起处理。
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return 0, nil, ErrResponseTimeout
			}
			return 0, nil, fmt.Errorf("操作被取消: %w", ctx.Err())
		case err, ok := <-c.master.Errors():
			if !ok {
				return 0, nil, tplin.ErrTransportClosed
			}
			if err != nil {
				return 0, nil, fmt.Errorf("LIN传输失败: %w", err)
			}
		case <-ticker.C:
			msg := c.master.ReceiveDiagnostic()
			if msg == nil {
				continue
			}
			if nad != tplin.BroadcastNAD && msg.NAD != nad {
				continue
			}
			if msg.SID == 0x7F { // Negative Response
				if len(msg.Data) < 2 || msg.Data[0] != sid {
					continue
				}
				if msg.Data[1] == 0x78 {
					// Response Pending, 继续等待（仍保持 awaiting，继续读 0x3D）
					continue
				}
				fullNrcResponse := append([]byte{msg.SID}, msg.Data...)
				return msg.NAD, fullNrcResponse, fmt.Errorf("server : %02X 收到负响应 (NRC: 0x%02X - %s)", msg.SID, msg.Data[1], GetNrcString(msg.Data[1]))
			}
			if msg.SID == (sid + 0x40) { // Positive Response
				fullPositiveResponse := append([]byte{msg.SID}, msg.Data...)
				return msg.NAD, fullPositiveResponse, nil
			}
		}
	}
}
