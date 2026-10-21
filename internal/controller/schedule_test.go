package controller

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

const (
	fast   = time.Minute
	steady = 5 * time.Minute
	floor  = time.Minute
)

func TestScheduleDoublesOnPassUpToSteady(t *testing.T) {
	s := probeSchedule{interval: fast}
	for i, want := range []time.Duration{2 * time.Minute, 4 * time.Minute, steady, steady} {
		start := t0.Add(time.Duration(i) * time.Hour)
		var skipped int
		s, skipped = s.after(start, start, runPassed, fast, steady, floor, 0)
		if s.interval != want || !s.next.Equal(start.Add(want)) || skipped != 0 {
			t.Errorf("pass %d: interval %v, next +%v, %d skipped; want %v", i+1, s.interval, s.next.Sub(start), skipped, want)
		}
	}
}

func TestScheduleResetsOnFailure(t *testing.T) {
	s := probeSchedule{interval: steady, planHash: "h"}
	s, _ = s.after(t0, t0, runFailed, fast, steady, floor, 0)
	if s.interval != fast || !s.next.Equal(t0.Add(fast)) || s.planHash != "h" {
		t.Errorf("after a failure: %+v", s)
	}
}

func TestScheduleKeepsIntervalWhenAborted(t *testing.T) {
	s := probeSchedule{interval: 2 * time.Minute}
	s, _ = s.after(t0, t0, runAborted, fast, steady, floor, 0)
	if s.interval != 2*time.Minute || !s.next.Equal(t0.Add(2*time.Minute)) {
		t.Errorf("after an aborted run: %+v", s)
	}
}

func TestScheduleClampsToCurrentPolicy(t *testing.T) {
	s, _ := probeSchedule{interval: 30 * time.Minute}.after(t0, t0, runAborted, fast, steady, floor, 0)
	if s.interval != steady {
		t.Errorf("interval above steady = %v, want %v", s.interval, steady)
	}
	s, _ = probeSchedule{interval: time.Second}.after(t0, t0, runAborted, fast, steady, floor, 0)
	if s.interval != fast {
		t.Errorf("interval below fast = %v, want %v", s.interval, fast)
	}
}

func TestJitterStaysWithinTenPercent(t *testing.T) {
	for _, j := range []float64{-0.1, 0, 0.1} {
		s, _ := probeSchedule{interval: steady}.after(t0, t0, runAborted, fast, steady, floor, j)
		lo, hi := t0.Add(steady*9/10), t0.Add(steady*11/10)
		if s.next.Before(lo.Add(-time.Millisecond)) || s.next.After(hi.Add(time.Millisecond)) {
			t.Errorf("jitter %v: next +%v outside [%v, %v]", j, s.next.Sub(t0), lo.Sub(t0), hi.Sub(t0))
		}
	}
	if s, _ := (probeSchedule{interval: steady}).after(t0, t0, runAborted, fast, steady, floor, 0.1); !s.next.Equal(t0.Add(330 * time.Second)) {
		t.Errorf("+10%% jitter: next +%v, want 5m30s", s.next.Sub(t0))
	}
}

func TestSkipsAreCountedWhenARunOverruns(t *testing.T) {
	cases := []struct {
		took    time.Duration
		next    time.Duration
		skipped int
	}{
		{0, 5 * time.Minute, 0},
		{5 * time.Minute, 5 * time.Minute, 0}, // finished exactly when the next run is due: it still runs
		{6 * time.Minute, 10 * time.Minute, 1},
		{11 * time.Minute, 15 * time.Minute, 2},
	}
	for _, tc := range cases {
		s, skipped := probeSchedule{interval: steady}.after(t0, t0.Add(tc.took), runAborted, fast, steady, floor, 0)
		if !s.next.Equal(t0.Add(tc.next)) || skipped != tc.skipped {
			t.Errorf("run took %v: next +%v, %d skipped; want +%v, %d", tc.took, s.next.Sub(t0), skipped, tc.next, tc.skipped)
		}
	}
}

func TestFirstRunIsJitteredAndSpreadWhenOverdue(t *testing.T) {
	cases := []struct {
		name   string
		now    time.Time
		jitter float64
		want   time.Time
	}{
		{"due later", t0.Add(10 * time.Second), 0.1, t0.Add(66 * time.Second)},
		{"due later, least jitter", t0.Add(10 * time.Second), -0.1, t0.Add(time.Minute)}, // never below the floor
		{"overdue, most jitter", t0.Add(time.Hour), 0.1, t0.Add(time.Hour + 6*time.Second)},
		{"overdue, no jitter", t0.Add(time.Hour), 0, t0.Add(time.Hour + 3*time.Second)},
		{"overdue, least jitter", t0.Add(time.Hour), -0.1, t0.Add(time.Hour)},
	}
	for _, tc := range cases {
		if got := firstRun(t0, tc.now, fast, floor, tc.jitter); !got.Equal(tc.want) {
			t.Errorf("%s: first run at +%v, want +%v", tc.name, got.Sub(t0), tc.want.Sub(t0))
		}
	}
}

func TestJitterNeverStepsBelowTheFloor(t *testing.T) {
	cases := []struct {
		name                 string
		fast, steady, minGap time.Duration
		jitter               float64
		want                 time.Duration
	}{
		{"at the floor", fast, steady, floor, -0.1, time.Minute},
		{"above the floor", 2 * time.Minute, steady, floor, -0.1, 108 * time.Second},
		{"just above the floor", 62 * time.Second, steady, floor, -0.1, time.Minute},
		{"upwards", fast, steady, floor, 0.1, 66 * time.Second},
	}
	for _, tc := range cases {
		s, _ := probeSchedule{interval: tc.fast}.after(t0, t0, runFailed, tc.fast, tc.steady, tc.minGap, tc.jitter)
		if got := s.next.Sub(t0); got != tc.want {
			t.Errorf("%s: next +%v, want +%v", tc.name, got, tc.want)
		}
	}
}
