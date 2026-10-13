package reader

import (
	"context"
	"sync"
	"time"
)

type modelRateGate struct {
	once      sync.Once
	admission chan struct{}
	last      time.Time
}

// The optional module RPM applies at native send points to all model tasks.
// Waiting for its slot or spacing is cancellable and holds no SQL transaction.
func (s *Server) waitModelRPM(ctx context.Context) error {
	g := &s.modelRate
	g.once.Do(func() { g.admission = make(chan struct{}, 1) })
	select {
	case g.admission <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-g.admission }()
	cfg, err := s.Store.CollectionSettings()
	if err != nil {
		return err
	}
	if cfg.RequestRPM > 0 && !g.last.IsZero() {
		delay := time.Until(g.last.Add(time.Minute / time.Duration(cfg.RequestRPM)))
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	g.last = time.Now()
	return nil
}
