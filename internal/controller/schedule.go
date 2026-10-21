package controller

import (
	"math/rand/v2"
	"time"
)

// maxJitter is the largest fraction of its interval a scheduled run is offset
// by, in either direction (spec §10.1).
const maxJitter = 0.1

// runResult is how a probe run ended, for scheduling the next one.
type runResult int

const (
	// runPassed: the run was recorded and every probe passed.
	runPassed runResult = iota
	// runFailed: a probe failed, or the run was not recorded (error, plan mismatch).
	runFailed
	// runAborted: a run for the Intent was still in flight; nothing ran.
	runAborted
)

// probeSchedule is one Intent's in-memory probe schedule.
type probeSchedule struct {
	interval time.Duration // the interval the next run is scheduled at, before jitter
	planHash string        // the plan the interval was earned on; a new plan resets it
	next     time.Time     // when the next run is due
}

// after returns the schedule once a run that started at start and finished at
// finish ended as result, and the number of runs skipped while it was in
// flight. The interval doubles after a pass up to steady, resets to fast after
// a failure and is kept when the run was aborted; it is first clamped to
// [fast, steady], so a policy edit takes effect at the next run. The next run
// is due at start + interval·(1+jitter). If that passed before finish, the run
// overran: due times skip forward by whole jittered intervals, and each is
// counted as skipped (an Intent never has two runs in flight, spec §10.1).
func (s probeSchedule) after(start, finish time.Time, result runResult, fast, steady time.Duration, jitter float64) (probeSchedule, int) {
	interval := min(max(s.interval, fast), steady)
	switch result {
	case runPassed:
		interval = min(interval*2, steady)
	case runFailed:
		interval = fast
	}
	step := time.Duration(float64(interval) * (1 + jitter))
	if step <= 0 {
		step = fast
	}
	next := start.Add(step)
	skipped := 0
	for next.Before(finish) {
		next = next.Add(step)
		skipped++
	}
	return probeSchedule{interval: interval, planHash: s.planHash, next: next}, skipped
}

// firstRun returns when the first run is due after an operator restart, or
// for a new plan, given the newest recorded probe (last) and the jitter in
// [-maxJitter, maxJitter]: a jittered fast interval after last. If that time
// has passed, as after a restart for every Intent probed before it, the run is
// spread over the first tenth of the fast interval from now, the jitter mapped
// to [0, maxJitter]·fast, so that a restart does not make every Intent due at once.
func firstRun(last, now time.Time, fast time.Duration, jitter float64) time.Time {
	next := last.Add(time.Duration(float64(fast) * (1 + jitter)))
	if next.Before(now) {
		next = now.Add(time.Duration(float64(fast) * (jitter + maxJitter) / 2))
	}
	return next
}

// randomJitter returns a uniform value in [-maxJitter, maxJitter].
func randomJitter() float64 {
	return (rand.Float64()*2 - 1) * maxJitter
}
