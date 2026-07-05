package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type CloudClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewCloudClient(baseURL, token string) *CloudClient {
	return &CloudClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

type CloudRun struct {
	RunID            string `json:"runId"`
	TaskID           string `json:"taskId"`
	Stage            string `json:"stage"`
	ProjectID        string `json:"projectId"`
	BaseBranch       string `json:"baseBranch"`
	FeatureBranch    string `json:"featureBranch"`
	OpenSpecPath     string `json:"openspecPath"`
	Executor         string `json:"executor"`
	Status           string `json:"status"`
	RunnerID         string `json:"runnerId"`
	AssignedRunnerID string `json:"assignedRunnerId"`
	Worktree         string `json:"worktree"`
	Branch           string `json:"branch"`
	Prompt           string `json:"prompt"`
	RunNumber        int    `json:"run_number"`
}

type RunResult struct {
	RunID        string `json:"runId"`
	Status       string `json:"status"`
	Summary      string `json:"summary"`
	ChangedFiles string `json:"changedFiles"`
	DiffStat     string `json:"diffStat"`
	DiffSummary  string `json:"diffSummary"`
	TestResult   string `json:"testResult"`
	Risk         string `json:"risk"`
	CommitHash   string `json:"commitHash"`
}

func (c *CloudClient) Register(ctx context.Context, cfg *Config) error {
	body := map[string]any{
		"runnerId":          cfg.RunnerID,
		"runnerName":        cfg.RunnerName,
		"version":           "0.1.0",
		"projects":          cfg.ProjectIDs(),
		"executors":         cfg.ExecutorNames(),
		"maxConcurrentRuns": cfg.MaxConcurrentRuns,
	}
	return c.doJSON(ctx, http.MethodPost, "/api/runners/register", body, nil)
}

func (c *CloudClient) Heartbeat(ctx context.Context, runnerID, status string, currentRuns int) error {
	body := map[string]any{
		"runnerId":    runnerID,
		"status":      status,
		"currentRuns": currentRuns,
	}
	return c.doJSON(ctx, http.MethodPost, "/api/runners/heartbeat", body, nil)
}

func (c *CloudClient) PollRuns(ctx context.Context, runnerID string) ([]CloudRun, error) {
	var resp struct {
		Runs []CloudRun `json:"runs"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/runners/tasks?runnerId="+runnerID, nil, &resp)
	return resp.Runs, err
}

func (c *CloudClient) ClaimRun(ctx context.Context, runID, runnerID string) (*CloudRun, error) {
	var run CloudRun
	err := c.doJSON(ctx, http.MethodPost, "/api/runs/"+runID+"/claim", map[string]string{"runnerId": runnerID}, &run)
	if err != nil {
		return nil, err
	}
	return &run, nil
}

func (c *CloudClient) SendEvent(ctx context.Context, runID, eventType, status, message string) error {
	body := map[string]string{"type": eventType, "status": status, "message": message}
	return c.doJSON(ctx, http.MethodPost, "/api/runs/"+runID+"/events", body, nil)
}

func (c *CloudClient) SendRuntime(ctx context.Context, runID, status, message, worktree, branch string) error {
	body := map[string]string{"type": "status", "status": status, "message": message, "worktree": worktree, "branch": branch}
	return c.doJSON(ctx, http.MethodPost, "/api/runs/"+runID+"/events", body, nil)
}

func (c *CloudClient) SendLog(ctx context.Context, runID string, seq int64, stream, content string) error {
	body := map[string]any{
		"seq":       seq,
		"stream":    stream,
		"content":   content,
		"timestamp": time.Now().Unix(),
	}
	return c.doJSON(ctx, http.MethodPost, "/api/runs/"+runID+"/logs", body, nil)
}

func (c *CloudClient) SendResult(ctx context.Context, result RunResult) error {
	return c.doJSON(ctx, http.MethodPost, "/api/runs/"+result.RunID+"/result", result, nil)
}

func (c *CloudClient) doJSON(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s failed: %s: %s", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return err
		}
	}
	return nil
}
