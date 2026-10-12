package scholar

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// SourceGate serializes requests to one external API host across every library
// root of this machine user. An OS file lock in the user cache directory keeps
// parallel processes from sharing the host, and request starts keep a minimum
// interval. Waiting is cancellable; the OS releases the lock on abnormal exit.
type SourceGate struct {
	mu        sync.Mutex
	lockPath  string
	interval  time.Duration
	lastStart time.Time
}

// NewSourceGate creates the shared gate for one host class (arxiv, s2).
func NewSourceGate(name string, interval time.Duration) *SourceGate {
	dir := filepath.Join(userCacheBase(), "reader-source-gates")
	return &SourceGate{lockPath: filepath.Join(dir, name+".lock"), interval: interval}
}

// newSourceGateAt is the test seam: the gate coordinates through lockPath.
func newSourceGateAt(lockPath string, interval time.Duration) *SourceGate {
	return &SourceGate{lockPath: lockPath, interval: interval}
}

func userCacheBase() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return dir
	}
	return os.TempDir()
}

// Acquire waits for the interval and the machine-wide lock, then reports the
// request start so the next caller keeps the spacing. Release must run after
// the request finishes.
func (g *SourceGate) Acquire(ctx context.Context) (func(), error) {
	if err := g.waitInterval(ctx, g.lastStart); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(g.lockPath), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(g.lockPath, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	release := func() {
		_ = unlockFile(f)
		_ = f.Close()
	}
	for {
		if err := tryLockFile(f); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			release()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	// Another process may have just finished; honor the shared interval too.
	last := readLastStart(f)
	if err := g.waitInterval(ctx, last); err != nil {
		release()
		return nil, err
	}
	now := time.Now()
	writeLastStart(f, now)
	g.mu.Lock()
	g.lastStart = now
	g.mu.Unlock()
	return release, nil
}

func (g *SourceGate) waitInterval(ctx context.Context, last time.Time) error {
	if last.IsZero() || g.interval <= 0 {
		return nil
	}
	wait := last.Add(g.interval).Sub(time.Now())
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func readLastStart(f *os.File) time.Time {
	buf := make([]byte, 32)
	n, _ := f.ReadAt(buf, 0)
	sec, err := strconv.ParseInt(string(buf[:n]), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.UnixMilli(sec)
}

func writeLastStart(f *os.File, t time.Time) {
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.FormatInt(t.UnixMilli(), 10)), 0)
}
