//go:build unix

package main

import (
	"errors"
	"syscall"
)

// alive answers whether a process could still be polling.
//
// ⛔ IT ANSWERS TRUE WHENEVER IT CANNOT TELL. It is used only to drop an entry
// the deadline would have kept, so a wrong "false" silently empties the
// registry and restores the per-process pacing that drained the quota, while a
// wrong "true" only paces a wait more conservatively until the entry's own
// deadline passes. The two errors are not symmetrical, and the safe one is the
// one that keeps the entry.
//
// ⛔ syscall.Kill AND NOT os.FindProcess + Signal: on unix FindProcess never
// looks and never fails, so the `if err != nil` that shape forces on the caller
// is a branch no input can reach and no test can hold — which `mutsweep`
// reports, correctly, as a condition nothing holds. Signal 0 is the POSIX
// existence test: it validates the arguments and delivers nothing. EPERM means
// the process exists and belongs to somebody else — impossible between sibling
// sessions of one user, and still an existence proof, so it reads as alive.
// canTellProcessLiveness lets a test assert the platform's actual contract
// instead of listing operating systems: on a platform that cannot tell, "dead"
// is not a wrong answer to be fixed, it is an answer that must not be given.
const canTellProcessLiveness = true

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	switch err := syscall.Kill(pid, 0); {
	case err == nil:
		return true
	case errors.Is(err, syscall.ESRCH):
		return false
	default:
		return true
	}
}
