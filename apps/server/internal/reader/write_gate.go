package reader

import (
	"errors"
	"sync"
)

// errAdmissionClosed reports that library writes are not admitted because the
// process is draining toward exit.
var errAdmissionClosed = errors.New("书库写入已关闭，请稍后重试")

// libraryWriteGate serializes every library-mutating path (upload, resolve,
// Zotero, merge, trash, collection import) and lets drain wait for in-flight
// writes before the store may close. Callers hold it across the whole write
// decision-to-commit range and never nest acquisitions; helpers take an
// already-held context instead.
type libraryWriteGate struct {
	mu       sync.Mutex
	stateMu  sync.Mutex
	inflight int
	closed   bool
	idle     chan struct{}
}

// Acquire registers the caller as in-flight before waiting on the mutex, so a
// concurrent drain cannot observe zero and finish early. Release is idempotent.
func (g *libraryWriteGate) Acquire() (func(), error) {
	g.stateMu.Lock()
	if g.closed {
		g.stateMu.Unlock()
		return nil, errAdmissionClosed
	}
	g.inflight++
	g.stateMu.Unlock()
	g.mu.Lock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			g.mu.Unlock()
			g.stateMu.Lock()
			g.inflight--
			if g.closed && g.inflight == 0 && g.idle != nil {
				close(g.idle)
				g.idle = nil
			}
			g.stateMu.Unlock()
		})
	}
	return release, nil
}

// TryAcquire is Acquire without waiting. It is used to assert that a write is
// currently holding the gate.
func (g *libraryWriteGate) TryAcquire() (func(), bool) {
	g.stateMu.Lock()
	if g.closed {
		g.stateMu.Unlock()
		return nil, false
	}
	g.inflight++
	g.stateMu.Unlock()
	if !g.mu.TryLock() {
		g.stateMu.Lock()
		g.inflight--
		if g.closed && g.inflight == 0 && g.idle != nil {
			close(g.idle)
			g.idle = nil
		}
		g.stateMu.Unlock()
		return nil, false
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			g.mu.Unlock()
			g.stateMu.Lock()
			g.inflight--
			if g.closed && g.inflight == 0 && g.idle != nil {
				close(g.idle)
				g.idle = nil
			}
			g.stateMu.Unlock()
		})
	}
	return release, true
}

// CloseForDrain refuses new writers and returns a channel closed when the last
// in-flight write releases. It is safe to call when already draining: the
// channel of the active drain is returned.
func (g *libraryWriteGate) CloseForDrain() <-chan struct{} {
	g.stateMu.Lock()
	defer g.stateMu.Unlock()
	g.closed = true
	if g.inflight == 0 {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	if g.idle == nil {
		g.idle = make(chan struct{})
	}
	return g.idle
}

// Reopen restores admission after a cancelled drain. Pending drain waiters are
// not expected here: cancel only happens before the process decides to exit.
func (g *libraryWriteGate) Reopen() {
	g.stateMu.Lock()
	defer g.stateMu.Unlock()
	g.closed = false
}

// Draining reports whether admission is currently closed.
func (g *libraryWriteGate) Draining() bool {
	g.stateMu.Lock()
	defer g.stateMu.Unlock()
	return g.closed
}
