package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ⛔ ON WINDOWS A LOCK DIRECTORY THAT IS BEING DELETED ANSWERS Mkdir WITH
// "ACCESS DENIED", NOT "EXISTS". Measured on windows-latest before the fix: 51 of
// 1800 rounds of 8 concurrent enters lost an entry, every one of them with
// `mkdir …waits.json.lock: Access is denied.`, because that error was read as
// "cannot lock at all" and the body never ran. Win32 RemoveDirectory only marks
// a directory for deletion on close, so the name stays taken until the last
// handle goes.

// swapMkdir installs a fake lock-directory creator for one test.
func swapMkdir(t *testing.T, f func(string, os.FileMode) error) {
	t.Helper()
	old := mkdirLock
	mkdirLock = f
	t.Cleanup(func() { mkdirLock = old })
}

func TestAPermissionErrorOnTheLockIsContentionAndIsRetried(t *testing.T) {
	var calls atomic.Int32
	swapMkdir(t, func(p string, m os.FileMode) error {
		if calls.Add(1) <= 3 {
			return &fs.PathError{Op: "mkdir", Path: p, Err: fs.ErrPermission}
		}
		return os.Mkdir(p, m)
	})
	path := filepath.Join(t.TempDir(), "waits.json")
	ran := false
	notes := withLock(path, func() error { ran = true; return nil })
	if !ran {
		t.Fatalf("the body never ran: a delete-pending lock was read as unlockable (notes %q)", notes)
	}
	if len(notes) != 0 {
		t.Fatalf("a transient contention left notes: %q", notes)
	}
	if calls.Load() != 4 {
		t.Fatalf("%d mkdir calls, want 4 (three refusals, then the lock)", calls.Load())
	}
}

func TestAPermissionErrorThatNeverClearsEndsInANoteNamingIt(t *testing.T) {
	swapMkdir(t, func(p string, _ os.FileMode) error {
		return &fs.PathError{Op: "mkdir", Path: p, Err: fs.ErrPermission}
	})
	ran := false
	start := time.Now()
	notes := withLock(filepath.Join(t.TempDir(), "waits.json"), func() error { ran = true; return nil })
	if ran {
		t.Fatal("the body ran without the lock")
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "stayed locked") || !strings.Contains(joined, fs.ErrPermission.Error()) {
		t.Fatalf("notes %q do not say it stayed locked and why", notes)
	}
	if waited := time.Since(start); waited > lockWait+time.Second {
		t.Fatalf("waited %s, more than the lockWait bound", waited)
	}
}

func TestAnotherErrorOnTheLockIsStillNotRetried(t *testing.T) {
	boom := errors.New("disk on fire")
	swapMkdir(t, func(string, os.FileMode) error { return boom })
	start := time.Now()
	notes := withLock(filepath.Join(t.TempDir(), "waits.json"), func() error { return nil })
	if !strings.Contains(strings.Join(notes, "\n"), "cannot lock the wait registry") {
		t.Fatalf("notes: %q", notes)
	}
	if waited := time.Since(start); waited >= lockWait {
		t.Fatalf("waited %s for an error that cannot clear", waited)
	}
}

// The stress half: many rounds of the concurrent-enter case, so that a lock
// that loses one round in fifty cannot pass by luck. On windows-latest the
// unfixed lock lost 51 rounds of 1800 (2.8 %), i.e. this fails with
// probability 1-(0.972)^100 ≈ 94 % per run there.
func TestConcurrentEntriesAreNotLostOverManyRounds(t *testing.T) {
	rounds := 100
	if testing.Short() {
		rounds = 10
	}
	const n = 8
	for r := 0; r < rounds; r++ {
		path := filepath.Join(t.TempDir(), "waits.json")
		var wg sync.WaitGroup
		regs := make([]registration, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				regs[i] = enter(path, waitEntry{PID: 10000 + i, Started: regNow, Deadline: regNow.Add(time.Hour), Targets: []string{"x"}}, regNow, always)
			}(i)
		}
		wg.Wait()
		if ws := read(t, path); len(ws) != n {
			var notes []string
			for _, g := range regs {
				notes = append(notes, g.Notes...)
			}
			t.Fatalf("round %d: %d of %d entries survived; notes: %q", r, len(ws), n, notes)
		}
	}
}
