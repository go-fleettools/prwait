package main

import (
	"testing"
	"time"
)

// ⛔ A TEST OF A LOOP MUST FAIL WHEN THE LOOP NEVER ENDS, NOT HANG.
//
// `mutsweep -sites all` could only answer HUNG for three mutations of run()
// (the quota-stop `err != nil`, the timeout comparison and the `len(left) > 0`
// before the sleep), and a HUNG is unproven: removing any one of them leaves
// the wait with nothing that stops it, and every test then ran until the
// 10-minute go test timeout. These two clocks bound the reads, so the same
// mutation turns the test red in seconds, naming what happened.

// boundedNow is a simulated clock that reads *at, and fails the test when it
// has been read more than maxClockReads times. A wait whose clock only moves in
// sleep, and which has lost the call to sleep, reads it forever.
const maxClockReads = 100000

func boundedNow(t *testing.T, at *time.Time) func() time.Time {
	t.Helper()
	reads := 0
	return func() time.Time {
		reads++
		if reads > maxClockReads {
			t.Fatalf("the wait read its clock %d times and never ended: a loop lost the condition that stops it", reads)
		}
		return *at
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
		if n.Sub(born) > limit {
			t.Fatalf("the wait was still polling after %s of real time: a loop lost the condition that stops it", limit)
		}
		return n
	}
}
