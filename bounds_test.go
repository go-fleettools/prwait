package main

import (
	"sync/atomic"
	"testing"
	"time"
)

// ⛔ A TEST OF A LOOP MUST FAIL WHEN THE LOOP NEVER ENDS, NOT HANG.
//
// `mutsweep -sites all` could only answer HUNG for three mutations of run()
// (the quota-stop `err != nil`, the timeout comparison and the `len(left) > 0`
// before the sleep), and a HUNG is unproven: removing any one of them leaves
// the wait with nothing that stops it, and every test then ran until the
// 10-minute go test timeout. These clocks and sleeps bound the loop, so the
// same mutation turns the test red in seconds, naming what happened.
//
// The clock and the sleep are counted separately because each mutation silences
// one of them: a deleted timeout comparison stops READING the clock, a deleted
// sleep stops SLEEPING.

const maxClockReads = 100000

// loopLost is set by the first bound that trips. Every later test then fails on
// its first call instead of waiting out its own bound: the suite is red anyway,
// and a mutated run would otherwise spend a full bound per waiting test.
var loopLost atomic.Bool

func lose(t *testing.T, format string, args ...any) {
	t.Helper()
	loopLost.Store(true)
	t.Fatalf(format, args...)
}

// boundedNow is a simulated clock that reads *at, and fails the test when it
// has been read more than maxClockReads times.
func boundedNow(t *testing.T, at *time.Time) func() time.Time {
	t.Helper()
	reads := 0
	return func() time.Time {
		reads++
		if loopLost.Load() || reads > maxClockReads {
			lose(t, "the wait read its clock %d times and never ended: a loop lost the condition that stops it", reads)
		}
		return *at
	}
}

// boundedSleep is the simulated clock's sleep: it advances *at by d+extra, and
// fails the test after maxClockReads calls.
func boundedSleep(t *testing.T, at *time.Time, extra time.Duration) func(time.Duration) {
	t.Helper()
	calls := 0
	return func(d time.Duration) {
		calls++
		if loopLost.Load() || calls > maxClockReads {
			lose(t, "the wait slept %d times and never ended: a loop lost the condition that stops it", calls)
		}
		*at = at.Add(d + extra)
	}
}

// boundedRealNow is the real clock, and fails the test when more than limit of
// real time has passed since it was made: a wait with a timeout of seconds that
// is still polling then has lost the comparison that ends it.
func boundedRealNow(t *testing.T, limit time.Duration) func() time.Time {
	t.Helper()
	born := time.Now()
	return func() time.Time {
		n := time.Now()
		if loopLost.Load() || n.Sub(born) > limit {
			lose(t, "the wait was still polling after %s of real time: a loop lost the condition that stops it", limit)
		}
		return n
	}
}

// boundedRealSleep is a no-op sleep for a real-clock waiter, failing the test
// when more than limit of real time has passed since it was made.
func boundedRealSleep(t *testing.T, limit time.Duration) func(time.Duration) {
	t.Helper()
	born := time.Now()
	return func(time.Duration) {
		if loopLost.Load() || time.Since(born) > limit {
			lose(t, "the wait was still sleeping after %s of real time: a loop lost the condition that stops it", limit)
		}
	}
}
