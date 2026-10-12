package reader

import (
	"net/http"
	"sync"
)

// Drain statuses reported by the lifecycle API.
const (
	drainQuiescing = "quiescing"
	drainDrained   = "drained"
	drainCancelled = "cancelled"
)

// drainSession is one exit intent identified by an opaque ID. The ID is
// idempotent: starting a drain while one is active returns the same session.
type drainSession struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	// writesDone and backgroundDone are the full stop conditions this intent
	// owns; a resume waits for both before reopening admission.
	writesDone     <-chan struct{}
	backgroundDone <-chan struct{}
}

type drainState struct {
	mu       sync.Mutex
	current  *drainSession
	finished map[string]string // final status by drain id
	// stopBackground cancels timers and work, returning a channel closed once
	// background workers finish. Registered by main.
	stopBackground func() <-chan struct{}
	// startBackground rebuilds scheduling with a fresh context after a
	// cancelled drain. Registered by main.
	startBackground func()
	// closing marks the final exit decision; after it, cancel is refused.
	closing bool
}

// SetDrainHooks wires the background start/stop owners. Must be called before
// requests are served.
func (s *Server) SetDrainHooks(stop func() <-chan struct{}, start func()) {
	s.drain.mu.Lock()
	defer s.drain.mu.Unlock()
	s.drain.stopBackground = stop
	s.drain.startBackground = start
}

func readyOrWait(ch <-chan struct{}) bool {
	if ch == nil {
		return true
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// BeginDrain closes write admission, stops background work and waits for
// in-flight writes. It is idempotent while a session is active.
func (s *Server) BeginDrain() drainSession {
	s.drain.mu.Lock()
	if s.drain.current != nil && s.drain.current.Status != drainCancelled {
		session := *s.drain.current
		s.drain.mu.Unlock()
		return session
	}
	if s.drain.finished == nil {
		s.drain.finished = map[string]string{}
	}
	session := &drainSession{ID: id(), Status: drainQuiescing}
	s.drain.current = session
	stop := s.drain.stopBackground
	s.drain.mu.Unlock()

	idleWrites := s.importGate.CloseForDrain()
	var idleBackground <-chan struct{}
	if stop != nil {
		idleBackground = stop()
	}
	session.writesDone = idleWrites
	session.backgroundDone = idleBackground
	if readyOrWait(idleWrites) && readyOrWait(idleBackground) {
		s.drain.mu.Lock()
		defer s.drain.mu.Unlock()
		if session.Status == drainQuiescing {
			session.Status = drainDrained
			s.drain.finished[session.ID] = drainDrained
		}
		return *session
	}
	go func() {
		<-idleWrites
		if idleBackground != nil {
			<-idleBackground
		}
		s.drain.mu.Lock()
		defer s.drain.mu.Unlock()
		if s.drain.current != session {
			return
		}
		if session.Status == drainQuiescing {
			session.Status = drainDrained
			s.drain.finished[session.ID] = drainDrained
		}
	}()
	s.drain.mu.Lock()
	defer s.drain.mu.Unlock()
	return *session
}

// DrainStatus returns the state of one drain session.
func (s *Server) DrainStatus(drainID string) (drainSession, bool) {
	s.drain.mu.Lock()
	defer s.drain.mu.Unlock()
	if s.drain.current != nil && s.drain.current.ID == drainID {
		return *s.drain.current, true
	}
	if status, ok := s.drain.finished[drainID]; ok {
		return drainSession{ID: drainID, Status: status}, true
	}
	return drainSession{}, false
}

// CancelDrain revokes an exit intent that has not finished the final exit.
// Active commits and the old background keep running; admission reopens only
// after both this intent's stop conditions are met and only if no newer drain
// or closing decision superseded this intent.
func (s *Server) CancelDrain(drainID string) (drainSession, bool) {
	s.drain.mu.Lock()
	if s.drain.closing {
		s.drain.mu.Unlock()
		return drainSession{}, false
	}
	session := s.drain.current
	if session == nil || session.ID != drainID {
		s.drain.mu.Unlock()
		return drainSession{}, false
	}
	if session.Status == drainCancelled {
		out := *session
		s.drain.mu.Unlock()
		return out, true
	}
	session.Status = drainCancelled
	if s.drain.finished == nil {
		s.drain.finished = map[string]string{}
	}
	s.drain.finished[session.ID] = drainCancelled
	start := s.drain.startBackground
	writesDone := session.writesDone
	backgroundDone := session.backgroundDone
	s.drain.mu.Unlock()

	resume := func() {
		// Both stop conditions owned by this intent must complete first:
		// in-flight writes AND the old background workers.
		if writesDone != nil {
			<-writesDone
		}
		if backgroundDone != nil {
			<-backgroundDone
		}
		s.drain.mu.Lock()
		// A stale resume must never touch a newer intent's admission.
		if s.drain.closing || s.drain.current != session {
			s.drain.mu.Unlock()
			return
		}
		s.drain.mu.Unlock()
		s.importGate.Reopen()
		if start != nil {
			start()
		}
	}
	// With nothing in flight and background already stopped this completes
	// before the response returns.
	if readyOrWait(writesDone) && readyOrWait(backgroundDone) {
		resume()
		return *session, true
	}
	go resume()
	return *session, true
}

// MarkClosing is called when the process decides to exit after drained; later
// cancel requests are refused.
func (s *Server) MarkClosing() {
	s.drain.mu.Lock()
	s.drain.closing = true
	s.drain.mu.Unlock()
}

func (s *Server) drainStartHandler(w http.ResponseWriter, r *http.Request) {
	respond(w, 202, s.BeginDrain())
}

func (s *Server) drainStatusHandler(w http.ResponseWriter, r *http.Request) {
	session, ok := s.DrainStatus(r.PathValue("id"))
	if !ok {
		fail(w, 404, "没有这个排空请求")
		return
	}
	respond(w, 200, session)
}

func (s *Server) drainCancelHandler(w http.ResponseWriter, r *http.Request) {
	session, ok := s.CancelDrain(r.PathValue("id"))
	if !ok {
		fail(w, 409, "排空请求已结束或不存在")
		return
	}
	respond(w, 200, session)
}
