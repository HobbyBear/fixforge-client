package runner

import (
	"encoding/json"
	"time"
)

// ─── WebSocket 消息协议 ───

type WSMessage struct {
	Type string `json:"type"`

	// Auth
	RunnerToken   string          `json:"runner_token,omitempty"`
	DeviceName    string          `json:"device_name,omitempty"`
	RunnerName    string          `json:"runner_name,omitempty"`
	ConnID        int64           `json:"conn_id,omitempty"`
	RunnerID      int64           `json:"runner_id,omitempty"` // legacy auth_ok conn id
	Message       string          `json:"message,omitempty"`
	State         string          `json:"state,omitempty"`
	WorkspaceRoot string          `json:"workspace_root,omitempty"`
	Projects      []ProjectConfig `json:"projects,omitempty"`

	// Resource RPC: server proxies Omnigent-style filesystem/shell requests
	// to the selected runner device.
	ResourceRequest  *ResourceRequest  `json:"resource_request,omitempty"`
	ResourceResponse *ResourceResponse `json:"resource_response,omitempty"`
	Terminal         *TerminalMessage  `json:"terminal,omitempty"`
	QARequest        *QARequest        `json:"qa_request,omitempty"`
	QAEvent          *QAEvent          `json:"qa_event,omitempty"`
	QAStop           *QAStop           `json:"qa_stop,omitempty"`
	QAApproval       *QAApproval       `json:"qa_approval,omitempty"`
}

const (
	// 客户端 → 服务端
	WSTypeAuth         = "auth"
	WSTypePong         = "pong"
	WSTypeRunnerState  = "runner_state"
	WSTypeResourceResp = "resource_response"
	// WSTypeTerminalOut carries TerminalMessage frames for the optional xterm
	// attach tunnel. Chat QA terminal frames use WSTypeQAEvent with
	// QAEvent.EventType == "terminal_output" instead.
	WSTypeTerminalOut  = "terminal_output"
	WSTypeTerminalDone = "terminal_closed"
	WSTypeQAEvent      = "qa_event"

	// 服务端 → 客户端
	WSTypePing           = "ping"
	WSTypeAuthOK         = "auth_ok"
	WSTypeAuthError      = "auth_error"
	WSTypeResourceReq    = "resource_request"
	WSTypeTerminalOpen   = "terminal_open"
	WSTypeTerminalInput  = "terminal_input"
	WSTypeTerminalResize = "terminal_resize"
	WSTypeTerminalClose  = "terminal_close"
	WSTypeQARequest      = "qa_request"
	WSTypeQAStop         = "qa_stop"
	WSTypeQAApproval     = "qa_approval_response"
)

type ResourceRequest struct {
	ID                string   `json:"id"`
	Operation         string   `json:"operation"`
	ProjectName       string   `json:"project_name"`
	RepoAppPath       string   `json:"repo_app_path"`
	Path              string   `json:"path,omitempty"`
	Content           string   `json:"content,omitempty"`
	Encoding          string   `json:"encoding,omitempty"`
	Command           string   `json:"command,omitempty"`
	Timeout           int      `json:"timeout,omitempty"`
	Branch            string   `json:"branch,omitempty"`
	TargetBranch      string   `json:"target_branch,omitempty"`
	Ref               string   `json:"ref,omitempty"`
	Hash              string   `json:"hash,omitempty"`
	Message           string   `json:"message,omitempty"`
	Files             []string `json:"files,omitempty"`
	OpenSpecOperation string   `json:"openspec_operation,omitempty"`
	Change            string   `json:"change,omitempty"`
	WorkflowMode      string   `json:"workflow_mode,omitempty"`
}

type ResourceResponse struct {
	ID      string          `json:"id"`
	OK      bool            `json:"ok"`
	Error   string          `json:"error,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type TerminalMessage struct {
	ID          string `json:"id"`
	SessionID   int64  `json:"session_id,omitempty"`
	TerminalID  string `json:"terminal_id,omitempty"`
	ProjectName string `json:"project_name,omitempty"`
	RepoAppPath string `json:"repo_app_path,omitempty"`
	RunID       string `json:"run_id,omitempty"`
	Data        string `json:"data,omitempty"`
	Cols        uint16 `json:"cols,omitempty"`
	Rows        uint16 `json:"rows,omitempty"`
	Code        int    `json:"code,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

type QARequest struct {
	ID                         string    `json:"id"`
	RunID                      string    `json:"run_id,omitempty"`
	SessionID                  int64     `json:"session_id,omitempty"`
	ProjectName                string    `json:"project_name,omitempty"`
	RepoAppPath                string    `json:"repo_app_path,omitempty"`
	Branch                     string    `json:"branch,omitempty"`
	Executor                   string    `json:"executor,omitempty"`
	Prompt                     string    `json:"prompt,omitempty"`
	AskedElapsedBeforeRunnerMS int64     `json:"asked_elapsed_before_runner_ms,omitempty"`
	RunnerReceivedAt           time.Time `json:"-"`
}

type QAStop struct {
	SessionID int64 `json:"session_id,omitempty"`
}

type QAApproval struct {
	ID          string `json:"id,omitempty"`
	SessionID   int64  `json:"session_id,omitempty"`
	RunID       string `json:"run_id,omitempty"`
	QARequestID string `json:"qa_request_id,omitempty"`
	ApprovalID  string `json:"approval_id,omitempty"`
	Decision    string `json:"decision,omitempty"`
	Reason      string `json:"reason,omitempty"`
	RequestedAt string `json:"requested_at,omitempty"`
	DecidedAt   string `json:"decided_at,omitempty"`
}

type QAEvent struct {
	ID          string `json:"id"`
	EventType   string `json:"event_type"`
	RunID       string `json:"run_id,omitempty"`
	QARequestID string `json:"qa_request_id,omitempty"`

	Chunk      string `json:"chunk,omitempty"`
	Text       string `json:"text,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolInput  string `json:"tool_input,omitempty"`
	ToolOutput string `json:"tool_output,omitempty"`
	Answer     string `json:"answer,omitempty"`
	Thinking   string `json:"thinking,omitempty"`
	RawOutput  string `json:"raw_output,omitempty"`
	Error      string `json:"error,omitempty"`
	Status     string `json:"status,omitempty"`
	Phase      string `json:"phase,omitempty"`

	TerminalID       string `json:"terminal_id,omitempty"`
	Seq              int64  `json:"seq,omitempty"`
	ByteOffset       int64  `json:"byte_offset,omitempty"`
	PayloadHash      string `json:"payload_hash,omitempty"`
	TerminalData     string `json:"terminal_data,omitempty"`
	TerminalEncoding string `json:"terminal_encoding,omitempty"`
	CreatedAt        string `json:"created_at,omitempty"`

	ApprovalID       string `json:"approval_id,omitempty"`
	ApprovalType     string `json:"approval_type,omitempty"`
	ApprovalTitle    string `json:"approval_title,omitempty"`
	ApprovalBody     string `json:"approval_body,omitempty"`
	ApprovalCommand  string `json:"approval_command,omitempty"`
	ApprovalPath     string `json:"approval_path,omitempty"`
	ApprovalDiff     string `json:"approval_diff,omitempty"`
	ApprovalDecision string `json:"approval_decision,omitempty"`
}

// ─── 重连 & 超时参数 ───

const (
	wsReconnectInterval = 10 * time.Second
	wsPingTimeout       = 30 * time.Second
	wsWriteTimeout      = 10 * time.Second
)
