package sandbox

import (
	"context"
	"testing"
)

func TestSchedulerCancelVsGrant(t *testing.T) {
	s := NewScheduler(1)
	ctx := context.Background()

	// Fill the cap
	hold, _ := s.Acquire(ctx, PriorityProbe)

	// Queue a waiter with a cancellable ctx
	cancelCtx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan error, 1)
	go func() {
		_, err := s.Acquire(cancelCtx, PriorityProbe)
		resultCh <- err
	}()

	// Wait for the waiter to queue
	for s.Waiting(PriorityProbe) == 0 {
	}

	// Under s.mu, mark the waiter as granted without closing w.ready
	s.mu.Lock()
	s.inUse--
	var grantedWaiter *waiter
	if len(s.queues[PriorityProbe]) > 0 {
		grantedWaiter = s.queues[PriorityProbe][0]
		s.queues[PriorityProbe] = s.queues[PriorityProbe][1:]
		grantedWaiter.granted = true
		s.inUse++
	}
	s.mu.Unlock()

	// Cancel the ctx - now the waiter will see cancellation with granted==true
	cancel()

	// Confirm Acquire returns ctx.Err() (cancelled case, with granted==true)
	err := <-resultCh
	if err != cancelCtx.Err() {
		t.Fatalf("err = %v, want %v", err, cancelCtx.Err())
	}

	// Release the holder and dispatch should have happened
	hold()

	// Assert no slot is leaked: exactly 1 immediate acquire succeeds
	r1, err := s.Acquire(context.Background(), PriorityProbe)
	if err != nil {
		t.Fatalf("slot leaked: %v", err)
	}
	r1()

	// Next acquire should also succeed (cap is 1, no holders)
	r2, err := s.Acquire(context.Background(), PriorityProbe)
	if err != nil {
		t.Fatalf("second acquire failed: %v", err)
	}
	r2()
}
