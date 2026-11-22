//go:build !unix

package main

// alive cannot be answered here with the standard library alone. On Windows
// os.Process.Signal is unsupported and os.FindProcess succeeds for a handle
// that may already be a terminated process; plan9 and js have no signal 0
// either. Guessing from any of that would produce exactly the wrong error — a
// "false" that empties the registry and restores the per-process pacing — so
// every process reads as "cannot tell", and an entry lives until its own
// deadline: its wait's timeout plus the grace in registry.go.
//
// The cost of that is a wait killed early still being counted for up to its
// remaining timeout, which paces the next wait of this user more conservatively
// than it needs to be. The cost of the other answer was the quota at zero.
//
// ⛔ The build tag is `!unix`, not `windows`: this file is the fallback for
// every platform without signal 0, and a tag naming one of them would leave the
// others with no definition at all.
const canTellProcessLiveness = false

func alive(int) bool { return true }
