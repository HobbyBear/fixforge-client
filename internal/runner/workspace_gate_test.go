package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestWorkspaceGateSerializesSameRootAndAllowsOtherRoots(t *testing.T) {
	gate := newWorkspaceGate()
	releaseFirst, queued, err := gate.acquire(context.Background(), t.TempDir(), nil)
	if err != nil || queued {
		t.Fatalf("first acquire: queued=%t err=%v", queued, err)
	}
	defer releaseFirst()

	otherRoot := t.TempDir()
	releaseOther, otherQueued, err := gate.acquire(context.Background(), otherRoot, nil)
	if err != nil || otherQueued {
		t.Fatalf("other root should acquire concurrently: queued=%t err=%v", otherQueued, err)
	}
	releaseOther()
}

func TestQAStopCancelsWorkspaceQueueWait(t *testing.T) {
	gate := newWorkspaceGate()
	root := t.TempDir()
	releaseFirst, _, err := gate.acquire(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()

	ctx, cancel := context.WithCancel(context.Background())
	daemon := &Daemon{
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		qaRunning: map[int64]*qaExecution{21: {cancel: cancel}},
	}
	queuedSignal := make(chan struct{}, 1)
	result := make(chan error, 1)
	go func() {
		_, _, acquireErr := gate.acquire(ctx, root, func() { queuedSignal <- struct{}{} })
		result <- acquireErr
	}()
	<-queuedSignal
	daemon.handleQAStop(&QAStop{SessionID: 21})

	select {
	case acquireErr := <-result:
		if !errors.Is(acquireErr, context.Canceled) {
			t.Fatalf("queued execution returned %v, want context.Canceled", acquireErr)
		}
	case <-time.After(time.Second):
		t.Fatal("QA stop did not cancel the queued workspace request")
	}
}

func TestWorkspaceGateQueuedAcquireCanBeCancelled(t *testing.T) {
	gate := newWorkspaceGate()
	root := t.TempDir()
	releaseFirst, _, err := gate.acquire(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()

	ctx, cancel := context.WithCancel(context.Background())
	queuedSignal := make(chan struct{}, 1)
	result := make(chan error, 1)
	go func() {
		_, queued, acquireErr := gate.acquire(ctx, root, func() { queuedSignal <- struct{}{} })
		if !queued {
			result <- errors.New("same root did not report queued state")
			return
		}
		result <- acquireErr
	}()

	select {
	case <-queuedSignal:
	case <-time.After(time.Second):
		t.Fatal("queued callback was not called")
	}
	cancel()
	select {
	case acquireErr := <-result:
		if !errors.Is(acquireErr, context.Canceled) {
			t.Fatalf("queued acquire returned %v, want context.Canceled", acquireErr)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled acquire did not return")
	}
}

func TestWorkspaceGateQueuedAcquireRunsAfterRelease(t *testing.T) {
	gate := newWorkspaceGate()
	root := t.TempDir()
	releaseFirst, _, err := gate.acquire(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}

	queuedSignal := make(chan struct{}, 1)
	acquired := make(chan func(), 1)
	go func() {
		release, queued, acquireErr := gate.acquire(context.Background(), root, func() { queuedSignal <- struct{}{} })
		if acquireErr != nil || !queued {
			acquired <- nil
			return
		}
		acquired <- release
	}()
	<-queuedSignal
	select {
	case <-acquired:
		t.Fatal("same workspace acquired before the current owner released it")
	default:
	}
	releaseFirst()
	select {
	case release := <-acquired:
		if release == nil {
			t.Fatal("queued workspace acquire failed")
		}
		release()
	case <-time.After(time.Second):
		t.Fatal("queued workspace did not acquire after release")
	}
}
