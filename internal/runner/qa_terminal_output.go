package runner

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"
)

type qaTerminalOutputEmitter struct {
	client     *Client
	requestID  string
	runID      string
	terminalID string
	mu         sync.Mutex
	seq        int64
	byteOffset int64
	closed     bool
}

func newQATerminalOutputEmitter(client *Client, req *QARequest) *qaTerminalOutputEmitter {
	if client == nil || req == nil || strings.TrimSpace(req.ID) == "" {
		return nil
	}
	runID := strings.TrimSpace(req.RunID)
	if runID == "" {
		runID = strings.TrimSpace(req.ID)
	}
	return &qaTerminalOutputEmitter{
		client:     client,
		requestID:  strings.TrimSpace(req.ID),
		runID:      runID,
		terminalID: runID,
	}
}

func (e *qaTerminalOutputEmitter) EmitLine(line string) {
	if e == nil {
		return
	}
	e.Emit([]byte(line + "\n"))
}

func (e *qaTerminalOutputEmitter) Emit(data []byte) {
	if e == nil || len(data) == 0 {
		return
	}
	copied := append([]byte(nil), data...)
	hash := sha256.Sum256(copied)

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	seq := e.seq
	byteOffset := e.byteOffset
	e.seq++
	e.byteOffset += int64(len(copied))
	e.mu.Unlock()

	_ = e.client.SendQAEvent(QAEvent{
		ID:               e.requestID,
		QARequestID:      e.requestID,
		RunID:            e.runID,
		EventType:        "terminal_output",
		TerminalID:       e.terminalID,
		Seq:              seq,
		ByteOffset:       byteOffset,
		PayloadHash:      fmt.Sprintf("%x", hash[:]),
		TerminalData:     base64.StdEncoding.EncodeToString(copied),
		TerminalEncoding: "ansi/base64",
		CreatedAt:        time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func (e *qaTerminalOutputEmitter) Close() {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
}
