package controller

import (
	"testing"
	"time"
)

func TestNextRun(t *testing.T) {
	start := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		took    time.Duration
		next    time.Duration
		skipped int
	}{
		{0, 5 * time.Minute, 0},
		{4 * time.Minute, 5 * time.Minute, 0},
		{5 * time.Minute, 5 * time.Minute, 0}, // finished exactly when the next run is due: it still runs
		{6 * time.Minute, 10 * time.Minute, 1},
		{11 * time.Minute, 15 * time.Minute, 2},
	}
	for _, tc := range cases {
		next, skipped := nextRun(start, start.Add(tc.took), 5*time.Minute)
		if !next.Equal(start.Add(tc.next)) || skipped != tc.skipped {
			t.Errorf("run took %v: nextRun = +%v, %d skipped; want +%v, %d",
				tc.took, next.Sub(start), skipped, tc.next, tc.skipped)
		}
	}
	if next, _ := nextRun(start, start, 0); !next.After(start) {
		t.Errorf("a zero interval scheduled the next run at %v", next)
	}
}
