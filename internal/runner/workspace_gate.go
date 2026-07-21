package runner

import (
	"context"
	"path/filepath"
	"sync"
)

// workspaceGate serializes operations that can checkout or write the same
// real workspace while allowing unrelated project roots to run concurrently.
type workspaceGate struct {
	mu    sync.Mutex
	locks map[string]*workspaceLock
}

type workspaceLock struct {
	sem  chan struct{}
	refs int
}

func newWorkspaceGate() *workspaceGate {
	return &workspaceGate{locks: make(map[string]*workspaceLock)}
}

func workspaceGateKey(root string) string {
	cleaned, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		cleaned = filepath.Clean(root)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(cleaned); resolveErr == nil {
		cleaned = resolved
	}
	return cleaned
}

// acquire returns whether the caller had to queue. Waiting is cancellable so
// a Stop request does not linger behind another session.
func (g *workspaceGate) acquire(ctx context.Context, root string, onQueued func()) (release func(), queued bool, err error) {
	key := workspaceGateKey(root)
	g.mu.Lock()
	lock := g.locks[key]
	if lock == nil {
		lock = &workspaceLock{sem: make(chan struct{}, 1)}
		g.locks[key] = lock
	}
	lock.refs++
	g.mu.Unlock()

	select {
	case lock.sem <- struct{}{}:
	case <-ctx.Done():
		g.releaseRef(key, lock)
		return nil, false, ctx.Err()
	default:
		queued = true
		if onQueued != nil {
			onQueued()
		}
		select {
		case lock.sem <- struct{}{}:
		case <-ctx.Done():
			g.releaseRef(key, lock)
			return nil, true, ctx.Err()
		}
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			<-lock.sem
			g.releaseRef(key, lock)
		})
	}, queued, nil
}

func (g *workspaceGate) releaseRef(key string, lock *workspaceLock) {
	g.mu.Lock()
	defer g.mu.Unlock()
	lock.refs--
	if lock.refs == 0 && len(lock.sem) == 0 && g.locks[key] == lock {
		delete(g.locks, key)
	}
}
