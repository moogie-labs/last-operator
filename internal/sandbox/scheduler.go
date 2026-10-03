package sandbox

import (
	"context"
	"sync"
)

// Priority orders queued sandbox work (spec §10.1); lower values are served first.
type Priority int

const (
	PriorityExecution Priority = iota // plan execution and teardown
	PriorityDiscovery
	PriorityBuild // image builds for probes (M4)
	PriorityProbe
	numPriorities
)

type waiter struct {
	ready   chan struct{}
	granted bool
}

// Scheduler caps concurrent sandbox and build pods cluster-wide.
type Scheduler struct {
	mu     sync.Mutex
	max    int
	inUse  int
	queues [numPriorities][]*waiter
}

func NewScheduler(max int) *Scheduler {
	if max < 1 {
		max = 1
	}
	return &Scheduler{max: max}
}

// Acquire blocks until a slot is free for p or ctx ends. The returned release is idempotent.
func (s *Scheduler) Acquire(ctx context.Context, p Priority) (func(), error) {
	if p < 0 || p >= numPriorities {
		p = PriorityProbe
	}
	s.mu.Lock()
	if s.inUse < s.max && s.queuedLocked() == 0 {
		s.inUse++
		s.mu.Unlock()
		return s.releaser(), nil
	}
	w := &waiter{ready: make(chan struct{})}
	s.queues[p] = append(s.queues[p], w)
	s.mu.Unlock()

	select {
	case <-w.ready:
		return s.releaser(), nil
	case <-ctx.Done():
		s.mu.Lock()
		defer s.mu.Unlock()
		if w.granted { // granted concurrently with cancellation: hand the slot on
			s.inUse--
			s.dispatchLocked()
		} else {
			s.removeLocked(p, w)
		}
		return nil, ctx.Err()
	}
}

// Waiting reports how many callers are queued at priority p.
func (s *Scheduler) Waiting(p Priority) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queues[p])
}

func (s *Scheduler) releaser() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.inUse--
			s.dispatchLocked()
		})
	}
}

func (s *Scheduler) dispatchLocked() {
	for s.inUse < s.max {
		w := s.popLocked()
		if w == nil {
			return
		}
		w.granted = true
		s.inUse++
		close(w.ready)
	}
}

func (s *Scheduler) popLocked() *waiter {
	for p := range s.queues {
		if q := s.queues[p]; len(q) > 0 {
			s.queues[p] = q[1:]
			return q[0]
		}
	}
	return nil
}

func (s *Scheduler) removeLocked(p Priority, w *waiter) {
	q := s.queues[p]
	for i, x := range q {
		if x == w {
			s.queues[p] = append(q[:i], q[i+1:]...)
			return
		}
	}
}

func (s *Scheduler) queuedLocked() int {
	n := 0
	for _, q := range s.queues {
		n += len(q)
	}
	return n
}
