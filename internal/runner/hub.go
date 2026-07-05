package runner

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var ErrRunnerOffline = errors.New("runner is offline")

// ─── OnlineDevice ───

type OnlineDevice struct {
	ConnID        int64           `json:"conn_id"`
	UserID        int64           `json:"user_id"`
	DeviceName    string          `json:"device_name"`
	Status        string          `json:"status"`
	ConnectedAt   int64           `json:"connected_at"`
	WorkspaceRoot string          `json:"workspace_root,omitempty"`
	Projects      []ProjectConfig `json:"projects,omitempty"`
}

// ─── RunnerConnection ───

type RunnerConnection struct {
	ConnID  int64
	UserID  int64
	Device  OnlineDevice
	Conn    *websocket.Conn
	writeMu sync.Mutex
	hub     *Hub
}

func (rc *RunnerConnection) SendJSON(v any) error {
	rc.writeMu.Lock()
	defer rc.writeMu.Unlock()
	rc.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return rc.Conn.WriteJSON(v)
}

// ─── Hub ───

type Hub struct {
	mu         sync.RWMutex
	conns      map[int64]*RunnerConnection
	nextConnID atomic.Int64
	pending    map[string]chan *ResourceResponse
	terminalCh map[string]chan *TerminalMessage
	qaCh       map[string]chan *QAEvent
	logger     *slog.Logger
}

func NewHub(logger *slog.Logger) *Hub {
	return &Hub{
		conns:      make(map[int64]*RunnerConnection),
		pending:    make(map[string]chan *ResourceResponse),
		terminalCh: make(map[string]chan *TerminalMessage),
		qaCh:       make(map[string]chan *QAEvent),
		logger:     logger,
	}
}

// Register adds a runner connection.
func (h *Hub) Register(rc *RunnerConnection) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conns[rc.ConnID] = rc
	h.logger.Info("runner connected", "conn_id", rc.ConnID, "user_id", rc.UserID, "device", rc.Device.DeviceName, "total", len(h.conns))
}

// Unregister removes a runner connection.
func (h *Hub) Unregister(connID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.conns, connID)
	h.logger.Info("runner disconnected", "conn_id", connID, "total", len(h.conns))
}

// UpdateStatus updates the device status of a runner.
func (h *Hub) UpdateStatus(connID int64, status string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if rc, ok := h.conns[connID]; ok {
		rc.Device.Status = status
	}
}

// ListByUser returns all connected devices for a given user.
func (h *Hub) ListByUser(userID int64) []OnlineDevice {
	h.mu.RLock()
	defer h.mu.RUnlock()
	byName := make(map[string]OnlineDevice)
	for _, rc := range h.conns {
		if rc.UserID != userID {
			continue
		}
		key := strings.TrimSpace(rc.Device.DeviceName)
		if key == "" {
			key = fmt.Sprintf("conn:%d", rc.ConnID)
		}
		if current, ok := byName[key]; !ok || rc.Device.ConnectedAt > current.ConnectedAt || (rc.Device.ConnectedAt == current.ConnectedAt && rc.Device.ConnID > current.ConnID) {
			byName[key] = rc.Device
		}
	}
	devices := make([]OnlineDevice, 0, len(byName))
	for _, device := range byName {
		devices = append(devices, device)
	}
	return devices
}

func (h *Hub) SendResourceRequest(connID int64, userID int64, req ResourceRequest, timeout time.Duration) (*ResourceResponse, error) {
	h.mu.RLock()
	rc, ok := h.conns[connID]
	h.mu.RUnlock()
	if !ok || rc.UserID != userID {
		return nil, ErrRunnerOffline
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	if req.ID == "" {
		req.ID = fmt.Sprintf("res_%d_%d", time.Now().UnixNano(), rand.Int63())
	}
	ch := make(chan *ResourceResponse, 1)
	h.mu.Lock()
	h.pending[req.ID] = ch
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.pending, req.ID)
		h.mu.Unlock()
	}()
	if err := rc.SendJSON(WSMessage{Type: WSTypeResourceReq, ResourceRequest: &req}); err != nil {
		return nil, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case resp := <-ch:
		return resp, nil
	case <-timer.C:
		return nil, fmt.Errorf("runner resource request timed out")
	}
}

func (h *Hub) StartQA(connID int64, userID int64, req QARequest) (<-chan *QAEvent, func(), error) {
	h.mu.RLock()
	rc, ok := h.conns[connID]
	h.mu.RUnlock()
	if !ok || rc.UserID != userID {
		return nil, nil, ErrRunnerOffline
	}
	if req.ID == "" {
		req.ID = fmt.Sprintf("qa_%d_%d", time.Now().UnixNano(), rand.Int63())
	}
	ch := make(chan *QAEvent, 256)
	h.mu.Lock()
	h.qaCh[req.ID] = ch
	h.mu.Unlock()
	cleanup := func() {
		h.mu.Lock()
		current := h.qaCh[req.ID]
		if current == ch {
			delete(h.qaCh, req.ID)
			close(ch)
		}
		h.mu.Unlock()
	}
	if err := rc.SendJSON(WSMessage{Type: WSTypeQARequest, QARequest: &req}); err != nil {
		cleanup()
		return nil, nil, err
	}
	return ch, cleanup, nil

	// StopQA sends a cancel/stop message to the runner for a QA session.
}
func (h *Hub) StopQA(connID int64, userID int64, sessionID int64) error {
	h.mu.RLock()
	rc, ok := h.conns[connID]
	h.mu.RUnlock()
	if !ok || rc.UserID != userID {
		return ErrRunnerOffline
	}
	return rc.SendJSON(WSMessage{
		Type:   WSTypeQAStop,
		QAStop: &QAStop{SessionID: sessionID},
	})
}

type TerminalSession struct {
	ID string
	C  <-chan *TerminalMessage
	h  *Hub
	rc *RunnerConnection
}

func (h *Hub) OpenTerminal(connID int64, userID int64, msg TerminalMessage) (*TerminalSession, error) {
	h.mu.RLock()
	rc, ok := h.conns[connID]
	h.mu.RUnlock()
	if !ok || rc.UserID != userID {
		return nil, ErrRunnerOffline
	}
	if msg.ID == "" {
		msg.ID = fmt.Sprintf("term_%d_%d", time.Now().UnixNano(), rand.Int63())
	}
	ch := make(chan *TerminalMessage, 128)
	h.mu.Lock()
	h.terminalCh[msg.ID] = ch
	h.mu.Unlock()
	if err := rc.SendJSON(WSMessage{Type: WSTypeTerminalOpen, Terminal: &msg}); err != nil {
		h.mu.Lock()
		delete(h.terminalCh, msg.ID)
		h.mu.Unlock()
		close(ch)
		return nil, err
	}
	return &TerminalSession{ID: msg.ID, C: ch, h: h, rc: rc}, nil
}

func (s *TerminalSession) SendInput(data []byte) error {
	return s.rc.SendJSON(WSMessage{Type: WSTypeTerminalInput, Terminal: &TerminalMessage{
		ID: s.ID, Data: base64.StdEncoding.EncodeToString(data),
	}})
}

func (s *TerminalSession) Resize(cols, rows uint16) error {
	return s.rc.SendJSON(WSMessage{Type: WSTypeTerminalResize, Terminal: &TerminalMessage{ID: s.ID, Cols: cols, Rows: rows}})
}

func (s *TerminalSession) Close() {
	_ = s.rc.SendJSON(WSMessage{Type: WSTypeTerminalClose, Terminal: &TerminalMessage{ID: s.ID}})
	s.h.mu.Lock()
	ch := s.h.terminalCh[s.ID]
	delete(s.h.terminalCh, s.ID)
	s.h.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

func (h *Hub) handleResourceResponse(resp *ResourceResponse) {
	if resp == nil || resp.ID == "" {
		return
	}
	h.mu.RLock()
	ch := h.pending[resp.ID]
	h.mu.RUnlock()
	if ch == nil {
		return
	}
	select {
	case ch <- resp:
	default:
	}
}

func (h *Hub) handleTerminalMessage(msg *TerminalMessage) {
	if msg == nil || msg.ID == "" {
		return
	}
	h.mu.Lock()
	ch := h.terminalCh[msg.ID]
	if ch == nil {
		h.mu.Unlock()
		return
	}
	select {
	case ch <- msg:
	default:
		h.logger.Warn("runner terminal channel full", "id", msg.ID)
	}
	if msg.Code != 0 {
		if h.terminalCh[msg.ID] == ch {
			delete(h.terminalCh, msg.ID)
		}
		close(ch)
	}
	h.mu.Unlock()
}

func (h *Hub) handleQAEvent(evt *QAEvent) {
	if evt == nil || evt.ID == "" {
		return
	}
	h.mu.Lock()
	ch := h.qaCh[evt.ID]
	if ch == nil {
		h.mu.Unlock()
		return
	}
	select {
	case ch <- evt:
	default:
		h.logger.Warn("runner qa channel full", "id", evt.ID)
	}
	if evt.EventType == "done" || evt.EventType == "error" {
		if h.qaCh[evt.ID] == ch {
			delete(h.qaCh, evt.ID)
		}
		close(ch)
	}
	h.mu.Unlock()
}

// HandleRunnerWS handles a WebSocket connection from a runner client.
func (h *Hub) HandleRunnerWS(conn *websocket.Conn, authenticate func(token string) (userID int64, err error)) {
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		h.logger.Warn("runner ws: read auth failed", "error", err)
		return
	}

	var msg WSMessage
	if err := json.Unmarshal(raw, &msg); err != nil || msg.Type != WSTypeAuth {
		conn.WriteJSON(WSMessage{Type: WSTypeAuthError, Message: "auth required"})
		return
	}

	userID, err := authenticate(msg.RunnerToken)
	if err != nil {
		conn.WriteJSON(WSMessage{Type: WSTypeAuthError, Message: err.Error()})
		return
	}

	connID := h.nextConnID.Add(1)

	deviceName := msg.DeviceName
	if deviceName == "" {
		deviceName = "unknown"
	}

	conn.WriteJSON(WSMessage{Type: WSTypeAuthOK, RunnerID: connID})

	rc := &RunnerConnection{
		ConnID: connID,
		UserID: userID,
		Device: OnlineDevice{
			ConnID:        connID,
			UserID:        userID,
			DeviceName:    deviceName,
			Status:        "online",
			ConnectedAt:   time.Now().Unix(),
			WorkspaceRoot: msg.WorkspaceRoot,
			Projects:      msg.Projects,
		},
		Conn: conn,
		hub:  h,
	}
	h.Register(rc)
	defer h.Unregister(connID)

	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if err := rc.SendJSON(WSMessage{Type: WSTypePing}); err != nil {
					h.logger.Warn("runner ws: send ping failed", "conn_id", connID, "error", err)
					return
				}
			}
		}
	}()

	for {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}

		var m WSMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}

		switch m.Type {
		case WSTypePong:
			// heartbeat reply
		case WSTypeRunnerState:
			h.UpdateStatus(connID, m.State)
		case WSTypeResourceResp:
			h.handleResourceResponse(m.ResourceResponse)
		case WSTypeTerminalOut, WSTypeTerminalDone:
			h.handleTerminalMessage(m.Terminal)
		case WSTypeQAEvent:
			h.handleQAEvent(m.QAEvent)
		default:
			h.logger.Warn("unknown runner ws message", "type", m.Type)
		}
	}
}
