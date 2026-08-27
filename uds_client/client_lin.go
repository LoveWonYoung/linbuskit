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

var (
	ErrResponseTimeout = errors.New("waiting for UDS response timed out")
	ErrEmptyRequest    = errors.New("UDS request payload is empty")
)

// ClientConfig holds configuration options for the UDS client.
type ClientConfig struct {
	// TargetNad is the default destination NAD.
	TargetNad byte
	// DefaultTimeout is the initial response timeout used when SendAndRec gets
	// a non-positive timeout.
	DefaultTimeout time.Duration
	// ResponsePendingTimeout is the P2* timeout restarted by each matching
	// NRC 0x78 response.
	ResponsePendingTimeout time.Duration
	// MaxRetries is the number of retries after the first timeout. Retries are
	// disabled unless the request SID is also present in RetryableSIDs.
	MaxRetries int
	// RetryableSIDs explicitly identifies services that are safe to repeat.
	// The map is copied by NewClientWithConfig.
	RetryableSIDs map[byte]bool
	// Channel selects the LIN channel used by this client.
	Channel liniface.Channel
	// ContinuousSlavePoll 透传到 Transport：true 时空闲也持续请求 0x3D。
	ContinuousSlavePoll bool
}

// DefaultClientConfig returns a configuration with sensible defaults.
func DefaultClientConfig(targetNad byte) ClientConfig {
	return ClientConfig{
		TargetNad:              targetNad,
		DefaultTimeout:         2 * time.Second,
		ResponsePendingTimeout: 5 * time.Second,
	}
}

func normalizeClientConfig(config ClientConfig) ClientConfig {
	defaults := DefaultClientConfig(config.TargetNad)
	if config.DefaultTimeout <= 0 {
		config.DefaultTimeout = defaults.DefaultTimeout
	}
	if config.ResponsePendingTimeout <= 0 {
		config.ResponsePendingTimeout = defaults.ResponsePendingTimeout
	}
	if config.MaxRetries < 0 {
		config.MaxRetries = 0
	}
	if config.RetryableSIDs != nil {
		retryableSIDs := make(map[byte]bool, len(config.RetryableSIDs))
		for sid, enabled := range config.RetryableSIDs {
			retryableSIDs[sid] = enabled
		}
		config.RetryableSIDs = retryableSIDs
	}
	return config
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
	config = normalizeClientConfig(config)
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
// timeout <= 0 时使用 DefaultTimeout。仅当 SID 在 RetryableSIDs 中显式启用时，
// 超时才会按 MaxRetries 重试。
func (c *Client) SendAndRec(payload []byte, timeout time.Duration) (byte, []byte, error) {
	return c.sendAndRecWithRetries(c.config.TargetNad, payload, timeout)
}

// SendAndRecWithContext 支持外部 Context，使用默认 NAD，且不自动重试。
func (c *Client) SendAndRecWithContext(ctx context.Context, payload []byte) (byte, []byte, error) {
	c.requestMu.Lock()
	defer c.requestMu.Unlock()
	return c.sendAndRec(ctx, c.config.TargetNad, payload, 0)
}

// SendAndRecWithNAD 临时指定请求 NAD，并返回实际响应节点的 NAD。
func (c *Client) SendAndRecWithNAD(nad byte, payload []byte, timeout time.Duration) (byte, []byte, error) {
	return c.sendAndRecWithRetries(nad, payload, timeout)
}

func (c *Client) sendAndRecWithRetries(nad byte, payload []byte, timeout time.Duration) (byte, []byte, error) {
	if len(payload) == 0 {
		return 0, nil, ErrEmptyRequest
	}
	c.requestMu.Lock()
	defer c.requestMu.Unlock()
	if timeout <= 0 {
		timeout = c.config.DefaultTimeout
	}
	maxRetries := 0
	if c.config.RetryableSIDs[payload[0]] {
		maxRetries = c.config.MaxRetries
	}
	for attempt := 0; ; attempt++ {
		responseNAD, response, err := c.sendAndRec(context.Background(), nad, payload, timeout)
		if err == nil || !errors.Is(err, ErrResponseTimeout) || attempt >= maxRetries {
			return responseNAD, response, err
		}
	}
}

// sendAndRec 核心实现，支持自定义 NAD 与外部 Context。
func (c *Client) sendAndRec(ctx context.Context, nad byte, payload []byte, responseTimeout time.Duration) (byte, []byte, error) {
	if len(payload) == 0 {
		return 0, nil, ErrEmptyRequest
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
		return 0, nil, fmt.Errorf("send UDS request: %w", err)
	}
	// 无论成功/失败/超时，结束本次会话后停止空闲 0x3D 轮询（ContinuousSlavePoll=false 时生效）。
	defer c.master.StopAwaitingSlaveResponse()

	// 3. 阻塞等待响应。收到 NRC 0x78 后切换到 P2* 超时，并允许后续
	// Response Pending 重新开始 P2* 计时；外部 Context 始终是总上限。
	waitCtx, cancelWait := responseWaitContext(ctx, responseTimeout)
	defer func() { cancelWait() }()

	for {
		msg, err := c.master.ReceiveDiagnosticWithContext(waitCtx)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				if ctx.Err() != nil {
					return 0, nil, fmt.Errorf("UDS request context ended: %w", ctx.Err())
				}
				return 0, nil, ErrResponseTimeout
			}
			if errors.Is(err, context.Canceled) {
				return 0, nil, fmt.Errorf("UDS request context ended: %w", err)
			}
			return 0, nil, fmt.Errorf("LIN transport failed: %w", err)
		}
		if nad != tplin.BroadcastNAD && msg.NAD != nad {
			continue
		}
		if msg.SID == 0x7F { // Negative Response
			if len(msg.Data) < 2 || msg.Data[0] != sid {
				continue
			}
			if msg.Data[1] == 0x78 {
				cancelWait()
				waitCtx, cancelWait = responseWaitContext(ctx, c.config.ResponsePendingTimeout)
				continue
			}
			fullNrcResponse := append([]byte{msg.SID}, msg.Data...)
			return msg.NAD, fullNrcResponse, &tplin.NegativeResponseError{
				RequestedSID: msg.Data[0],
				NRC:          msg.Data[1],
			}
		}
		if msg.SID == (sid + 0x40) { // Positive Response
			fullPositiveResponse := append([]byte{msg.SID}, msg.Data...)
			return msg.NAD, fullPositiveResponse, nil
		}
	}
}

func responseWaitContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
}
