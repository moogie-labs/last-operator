package controller

import (
	"time"

	"github.com/moogie-labs/last-operator/internal/policy"
)

// nextRun returns when the run after one that started at start and finished at
// finish is due, on the fixed grid start + k·interval. Grid points that passed
// while the run was in flight are skipped and counted: an Intent never has two
// probe runs in flight (spec §10.1).
func nextRun(start, finish time.Time, interval time.Duration) (time.Time, int) {
	if interval <= 0 {
		interval = policy.DefaultMinProbeInterval
	}
	next := start.Add(interval)
	skipped := 0
	for next.Before(finish) {
		next = next.Add(interval)
		skipped++
	}
	return next, skipped
}
