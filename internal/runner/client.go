package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Client 通过 WebSocket 与 FixForge 服务端通信。
// HTTP 仅用于 bind（一次性操作）。
type Client struct {
	serverURL     string
	runnerToken   string
	deviceName    string
	workspaceRoot string
	projects      []ProjectConfig
	logger        *slog.Logger

	mu      sync.Mutex
	conn    *websocket.Conn
	writeMu sync.Mutex // 保护写操作

	// Server-driven callbacks.
	onResourceRequest func(context.Context, *ResourceRequest) *ResourceResponse
	onTerminalOpen    func(*TerminalMessage)
	onTerminalInput   func(*TerminalMessage)
	onTerminalResize  func(*TerminalMessage)
	onTerminalClose   func(*TerminalMessage)
	onQARequest       func(context.Context, *QARequest)
	onQAStop          func(*QAStop)

	// 重连控制
	stopped        bool
	reconnectCount int // used to suppress duplicate logs
	stopCh         chan struct{}
}

// NewClient 创建一个新的 WebSocket 客户端。
func NewClient(serverURL, runnerToken, deviceName, workspaceRoot string, projects []ProjectConfig, logger *slog.Logger) *Client {
	serverURL = normalizeLoopbackHost(serverURL)
	wsURL := strings.Replace(serverURL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
	wsURL = strings.TrimRight(wsURL, "/") + "/ws/runner"

	return &Client{
		serverURL:     wsURL,
		runnerToken:   runnerToken,
		deviceName:    deviceName,
		workspaceRoot: workspaceRoot,
		projects:      projects,
		logger:        logger,
		stopCh:        make(chan struct{}),
	}
}

// SetToken 更新 token（bind 后调用）。
func (c *Client) SetToken(token string) {
	c.runnerToken = token
}

func (c *Client) OnResourceRequest(fn func(context.Context, *ResourceRequest) *ResourceResponse) {
	c.onResourceRequest = fn
}

func (c *Client) OnTerminalOpen(fn func(*TerminalMessage)) {
	c.onTerminalOpen = fn
}

func (c *Client) OnTerminalInput(fn func(*TerminalMessage)) {
	c.onTerminalInput = fn
}

func (c *Client) OnTerminalResize(fn func(*TerminalMessage)) {
	c.onTerminalResize = fn
}

func (c *Client) OnTerminalClose(fn func(*TerminalMessage)) {
	c.onTerminalClose = fn
}

func (c *Client) OnQARequest(fn func(context.Context, *QARequest)) {
	c.onQARequest = fn
}

func (c *Client) OnQAStop(fn func(*QAStop)) {
	c.onQAStop = fn
}

// Connect 建立 WebSocket 连接并开始消息循环，自动重连。
// 阻塞直到 Stop 被调用。
func (c *Client) Connect(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.stopCh:
			return nil
		default:
		}

		conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.serverURL, nil)
		if err != nil {
			if c.reconnectCount == 0 {
				c.logger.Warn("websocket connect failed, retrying every 10s",
					"url", c.serverURL, "error", err,
				)
			}
			c.reconnectCount++
			if !c.sleepReconnect(ctx) {
				return ctx.Err()
			}
			continue
		}

		// Connected — log and reset counters
		c.logger.Info("websocket connected", "url", c.serverURL,
			"attempts", c.reconnectCount+1,
		)
		c.reconnectCount = 0
		c.mu.Lock()
		c.conn = conn
		c.mu.Unlock()

		// Authenticate
		if err := c.sendAuth(); err != nil {
			c.logger.Warn("send auth failed", "error", err)
			conn.Close()
			if !c.sleepReconnect(ctx) {
				return ctx.Err()
			}
			continue
		}

		// Read loop (blocks until disconnect)
		c.readLoop()

		// Disconnected
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.stopCh:
			return nil
		default:
		}

		c.reconnectCount = 0
		c.logger.Warn("websocket disconnected, reconnecting in 10s")
		if !c.sleepReconnect(ctx) {
			return ctx.Err()
		}
	}
}

// Stop 关闭连接并停止重连。
func (c *Client) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.stopped = true
	select {
	case <-c.stopCh:
		// already closed
	default:
		close(c.stopCh)
	}

	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

// SendRunnerState 通知服务端 Runner 当前状态（online / busy）。
func (c *Client) SendRunnerState(state string) error {
	return c.writeJSON(WSMessage{
		Type:  WSTypeRunnerState,
		State: state,
	})
}

// ─── 内部方法 ───

func (c *Client) sendAuth() error {
	return c.writeJSON(WSMessage{
		Type:          WSTypeAuth,
		RunnerToken:   c.runnerToken,
		DeviceName:    c.deviceName,
		WorkspaceRoot: c.workspaceRoot,
		Projects:      c.projects,
	})
}

func (c *Client) readLoop() {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	if conn == nil {
		return
	}

	// 设置初始读超时
	conn.SetReadDeadline(time.Now().Add(wsPingTimeout + 30*time.Second))

	// WebSocket 协议层 pong（服务端也可用这个做心跳，双保险）
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(wsPingTimeout + 30*time.Second))
		return nil
	})

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if !c.stopped {
				c.logger.Warn("websocket read error", "error", err)
			}
			return
		}

		// 每收到一条消息就刷新读超时（服务端 JSON ping / 任务消息 都算）
		conn.SetReadDeadline(time.Now().Add(wsPingTimeout + 30*time.Second))

		var msg WSMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			c.logger.Warn("decode ws message failed", "error", err, "raw", string(raw))
			continue
		}

		c.handleMessage(msg)
	}
}

func (c *Client) handleMessage(msg WSMessage) {
	switch msg.Type {
	case WSTypePing:
		// 服务端心跳 → 回复 pong
		c.writeJSON(WSMessage{Type: WSTypePong})

	case WSTypeAuthOK:
		c.logger.Info("auth ok", "runner_id", msg.RunnerID)

	case WSTypeAuthError:
		c.logger.Error("auth failed", "message", msg.Message)

	case WSTypeResourceReq:
		if msg.ResourceRequest == nil {
			c.logger.Warn("resource_request without payload")
			return
		}
		go c.handleResourceRequest(msg.ResourceRequest)

	case WSTypeTerminalOpen:
		if msg.Terminal != nil && c.onTerminalOpen != nil {
			go c.onTerminalOpen(msg.Terminal)
		}

	case WSTypeTerminalInput:
		if msg.Terminal != nil && c.onTerminalInput != nil {
			c.onTerminalInput(msg.Terminal)
		}

	case WSTypeTerminalResize:
		if msg.Terminal != nil && c.onTerminalResize != nil {
			c.onTerminalResize(msg.Terminal)
		}

	case WSTypeTerminalClose:
		if msg.Terminal != nil && c.onTerminalClose != nil {
			c.onTerminalClose(msg.Terminal)
		}

	case WSTypeQARequest:
		if msg.QARequest != nil && c.onQARequest != nil {
			go c.onQARequest(context.Background(), msg.QARequest)
		}

	case WSTypeQAStop:
		if msg.QAStop != nil && c.onQAStop != nil {
			c.onQAStop(msg.QAStop)
		}

	default:
		c.logger.Warn("unknown ws message type", "type", msg.Type)
	}
}

func (c *Client) handleResourceRequest(req *ResourceRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var resp *ResourceResponse
	if c.onResourceRequest == nil {
		resp = &ResourceResponse{ID: req.ID, OK: false, Error: "resource handler not configured"}
	} else {
		resp = c.onResourceRequest(ctx, req)
		if resp == nil {
			resp = &ResourceResponse{ID: req.ID, OK: false, Error: "empty resource response"}
		}
	}
	resp.ID = req.ID
	if err := c.writeJSON(WSMessage{Type: WSTypeResourceResp, ResourceResponse: resp}); err != nil {
		c.logger.Warn("send resource response failed", "id", req.ID, "error", err)
	}
}

func (c *Client) SendTerminalOutput(msg TerminalMessage) error {
	return c.writeJSON(WSMessage{Type: WSTypeTerminalOut, Terminal: &msg})
}

func (c *Client) SendTerminalClosed(msg TerminalMessage) error {
	return c.writeJSON(WSMessage{Type: WSTypeTerminalDone, Terminal: &msg})
}

func (c *Client) SendQAEvent(evt QAEvent) error {
	return c.writeJSON(WSMessage{Type: WSTypeQAEvent, QAEvent: &evt})
}

func (c *Client) writeJSON(msg WSMessage) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("not connected")
	}

	conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return conn.WriteJSON(msg)
}

// sleepReconnect 等待固定 10 秒，ctx 取消或 Stop 时返回 false。
func (c *Client) sleepReconnect(ctx context.Context) bool {
	timer := time.NewTimer(wsReconnectInterval)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-c.stopCh:
		return false
	case <-timer.C:
	}

	return true
}
