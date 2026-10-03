package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSchedulerCancelVsGrant(t *testing.T) {
	s := NewScheduler(1)
	ctx := context.Background()

	// Fill the cap. The holder's slot is handed to the waiter by hand below (standing in for its
	// release), so the holder's release func is deliberately never called.
	if _, err := s.Acquire(ctx, PriorityProbe); err != nil {
		t.Fatal(err)
	}

	// Queue a waiter with a cancellable ctx
	cancelCtx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan error, 1)
	go func() {
		_, err := s.Acquire(cancelCtx, PriorityProbe)
		resultCh <- err
	}()

	// Wait for the waiter to queue
	for s.Waiting(PriorityProbe) == 0 {
		time.Sleep(time.Millisecond)
	}

	// Under s.mu, do what the holder's release does (free the slot, hand it to the
	// head waiter) but leave w.ready open, so the waiter can only observe ctx.Done
	// and must take the granted==true branch.
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

	// Assert no slot is leaked: with cap 1 and no holders, exactly one immediate
	// acquire succeeds and the next one blocks. A leak would make the first block.
	quick := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 200*time.Millisecond)
	}
	c1, cancel1 := quick()
	defer cancel1()
	r1, err := s.Acquire(c1, PriorityProbe)
	if err != nil {
		t.Fatalf("slot leaked: %v", err)
	}
	c2, cancel2 := quick()
	defer cancel2()
	if _, err := s.Acquire(c2, PriorityProbe); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second acquire while full: err = %v, want DeadlineExceeded (slot double-counted?)", err)
	}
	r1()
}
