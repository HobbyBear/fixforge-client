package runner

import (
	"fmt"
	"time"
)

type runnerQALatencyTracker struct {
	source                   string
	executor                 string
	startMetric              string
	sessionID                int64
	requestID                string
	questionElapsedBeforeRun time.Duration
	runnerReceivedAt         time.Time
	execStartedAt            time.Time
	firstVisibleLogged       bool
	firstResponseLogged      bool
}

func newRunnerQALatencyTracker(req *QARequest, source, executor, startMetric string, execStartedAt time.Time) *runnerQALatencyTracker {
	t := &runnerQALatencyTracker{
		source:        source,
		executor:      executor,
		startMetric:   startMetric,
		execStartedAt: execStartedAt,
	}
	if req != nil {
		t.sessionID = req.SessionID
		t.requestID = req.ID
		t.runnerReceivedAt = req.RunnerReceivedAt
		if req.AskedElapsedBeforeRunnerMS > 0 {
			t.questionElapsedBeforeRun = time.Duration(req.AskedElapsedBeforeRunnerMS) * time.Millisecond
		}
	}
	return t
}

func (t *runnerQALatencyTracker) logExecStart(command, workdir string, promptChars int) {
	if t == nil || t.execStartedAt.IsZero() {
		return
	}
	fmt.Printf("[runner.qa.exec_start] source=%s executor=%s session_id=%d qa_id=%s command=%q workdir=%q since_question_approx=%s since_runner_received=%s prompt_chars=%d\n",
		t.source,
		t.executor,
		t.sessionID,
		t.requestID,
		command,
		workdir,
		t.sinceQuestionApprox(t.execStartedAt),
		formatRunnerLatencySince(t.runnerReceivedAt, t.execStartedAt),
		promptChars)
}

func (t *runnerQALatencyTracker) logFirstResponse(text string) {
	if t == nil || t.firstResponseLogged {
		return
	}
	t.firstResponseLogged = true
	now := time.Now()
	fmt.Printf("[runner.qa.first_char] source=%s executor=%s session_id=%d qa_id=%s since_question_approx=%s since_runner_received=%s %s=%s first_chunk_chars=%d first_chunk_bytes=%d preview=%q\n",
		t.source,
		t.executor,
		t.sessionID,
		t.requestID,
		t.sinceQuestionApprox(now),
		formatRunnerLatencySince(t.runnerReceivedAt, now),
		t.startMetric,
		formatRunnerLatencySince(t.execStartedAt, now),
		len([]rune(text)),
		len(text),
		summarizePlainText(text, 120))
}

func (t *runnerQALatencyTracker) logFirstVisible(kind, text string) {
	if t == nil || t.firstVisibleLogged || text == "" {
		return
	}
	t.firstVisibleLogged = true
	now := time.Now()
	fmt.Printf("[runner.qa.first_visible] source=%s executor=%s session_id=%d qa_id=%s kind=%s since_question_approx=%s since_runner_received=%s %s=%s first_chunk_chars=%d first_chunk_bytes=%d preview=%q\n",
		t.source,
		t.executor,
		t.sessionID,
		t.requestID,
		kind,
		t.sinceQuestionApprox(now),
		formatRunnerLatencySince(t.runnerReceivedAt, now),
		t.startMetric,
		formatRunnerLatencySince(t.execStartedAt, now),
		len([]rune(text)),
		len(text),
		summarizePlainText(text, 120))
}

func (t *runnerQALatencyTracker) sinceQuestionApprox(now time.Time) string {
	if t == nil || now.IsZero() {
		return "unknown"
	}
	d := t.questionElapsedBeforeRun
	if !t.runnerReceivedAt.IsZero() {
		d += now.Sub(t.runnerReceivedAt)
	}
	if d <= 0 {
		return "unknown"
	}
	return formatRunnerLatencyDuration(d)
}

func formatRunnerLatencySince(start, end time.Time) string {
	if start.IsZero() || end.IsZero() {
		return "unknown"
	}
	d := end.Sub(start)
	if d < 0 {
		d = 0
	}
	return formatRunnerLatencyDuration(d)
}

func formatRunnerLatencyDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%.3fs", d.Seconds())
}
