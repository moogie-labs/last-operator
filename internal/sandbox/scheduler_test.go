package sandbox_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/moogie-labs/last-operator/internal/sandbox"
)

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSchedulerEnforcesTheCap(t *testing.T) {
	s := sandbox.NewScheduler(2)
	ctx := context.Background()
	r1, _ := s.Acquire(ctx, sandbox.PriorityProbe)
	r2, _ := s.Acquire(ctx, sandbox.PriorityProbe)
	got := make(chan struct{})
	go func() {
		r3, err := s.Acquire(ctx, sandbox.PriorityProbe)
		if err == nil {
			defer r3()
		}
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("third acquire succeeded above the cap")
	case <-time.After(50 * time.Millisecond):
	}
	r1()
	<-got
	r2()
}

func TestSchedulerServesHigherPriorityFirst(t *testing.T) {
	s := sandbox.NewScheduler(1)
	ctx := context.Background()
	hold, _ := s.Acquire(ctx, sandbox.PriorityExecution)
	order := make(chan sandbox.Priority, 2)
	var wg sync.WaitGroup
	start := func(p sandbox.Priority) {
		wg.Go(func() {
			r, err := s.Acquire(ctx, p)
			if err != nil {
				return
			}
			order <- p
			r()
		})
	}
	start(sandbox.PriorityProbe)
	waitFor(t, func() bool { return s.Waiting(sandbox.PriorityProbe) == 1 })
	start(sandbox.PriorityExecution)
	waitFor(t, func() bool { return s.Waiting(sandbox.PriorityExecution) == 1 })
	hold()
	wg.Wait()
	if first := <-order; first != sandbox.PriorityExecution {
		t.Fatalf("first served = %d, want execution", first)
	}
}

func TestSchedulerIsFIFOWithinAClass(t *testing.T) {
	s := sandbox.NewScheduler(1)
	ctx := context.Background()
	hold, _ := s.Acquire(ctx, sandbox.PriorityProbe)
	order := make(chan int, 3)
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, _ := s.Acquire(ctx, sandbox.PriorityProbe)
			order <- i
			r()
		}(i)
		waitFor(t, func() bool { return s.Waiting(sandbox.PriorityProbe) == i+1 })
	}
	hold()
	wg.Wait()
	for want := range 3 {
		if got := <-order; got != want {
			t.Fatalf("served %d, want %d", got, want)
		}
	}
}

func TestSchedulerCancelledWaiterLeavesNoLeak(t *testing.T) {
	s := sandbox.NewScheduler(1)
	hold, _ := s.Acquire(context.Background(), sandbox.PriorityExecution)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := s.Acquire(ctx, sandbox.PriorityProbe)
		errc <- err
	}()
	waitFor(t, func() bool { return s.Waiting(sandbox.PriorityProbe) == 1 })
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if s.Waiting(sandbox.PriorityProbe) != 0 {
		t.Fatal("cancelled waiter still queued")
	}
	hold()
	hold() // idempotent
	r, err := s.Acquire(context.Background(), sandbox.PriorityProbe)
	if err != nil {
		t.Fatalf("slot leaked: %v", err)
	}
	r()
}

func TestSchedulerUnderRace(t *testing.T) {
	s := sandbox.NewScheduler(3)
	var wg sync.WaitGroup
	var mu sync.Mutex
	active, peak := 0, 0
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := s.Acquire(context.Background(), sandbox.Priority(i%4))
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			active++
			peak = max(peak, active)
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
			r()
		}(i)
	}
	wg.Wait()
	if peak > 3 {
		t.Fatalf("peak concurrency %d above cap 3", peak)
	}
}

func TestSchedulerNoBargingHigherPriority(t *testing.T) {
	s := sandbox.NewScheduler(1)
	ctx := context.Background()
	hold, _ := s.Acquire(ctx, sandbox.PriorityProbe)
	order := make(chan sandbox.Priority, 2)
	var wg sync.WaitGroup

	// Queue a Probe waiter
	wg.Go(func() {
		r, _ := s.Acquire(ctx, sandbox.PriorityProbe)
		order <- sandbox.PriorityProbe
		r()
	})
	waitFor(t, func() bool { return s.Waiting(sandbox.PriorityProbe) == 1 })

	// Release the holder, allowing Probe to acquire
	hold()

	// Queue an Execution waiter (higher priority) while Probe is running
	wg.Go(func() {
		r, _ := s.Acquire(ctx, sandbox.PriorityExecution)
		order <- sandbox.PriorityExecution
		r()
	})

	// Give Execution a chance to queue
	time.Sleep(50 * time.Millisecond)

	wg.Wait()

	// Verify Probe was served first, then Execution
	if first := <-order; first != sandbox.PriorityProbe {
		t.Fatalf("first served = %d, want Probe", first)
	}
	if second := <-order; second != sandbox.PriorityExecution {
		t.Fatalf("second served = %d, want Execution", second)
	}
}

func TestSchedulerFastPathHonorsCtx(t *testing.T) {
	s := sandbox.NewScheduler(10)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Acquire(ctx, sandbox.PriorityProbe)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want Canceled", err)
	}
}

func TestSchedulerWaitingClamps(t *testing.T) {
	s := sandbox.NewScheduler(1)
	if n := s.Waiting(sandbox.Priority(999)); n != 0 {
		t.Fatalf("Waiting(999) = %d, want 0", n)
	}
}
