package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ⛔ THE BUDGET IS PER USER, AND THE PACING WAS PER PROCESS.
//
// #25 made one wait's interval grow with its targets, so that
// `prwait -repo O/R 78 79 80 82` holds its REST cost at roughly what one target
// costs. It did nothing about the shape the incident in #20 actually had: FOUR
// prwait PROCESSES, one per pull request, each correctly pacing itself at the
// one-target default, together spending four times what any of them believed it
// was spending. The quota they drained is per USER (account 11405852), not per
// process and not per token, and on 2026-11-22 it reached 0 of 5000 again with
// about twenty-five sessions sharing this machine.
//
// So a wait has to know about the others. This file is that: a registry in the
// user's cache directory — per user by construction, which is exactly the
// quota's own granularity — holding one entry per live wait.
//
// ⛔ IT WARNS AND PACES, IT DOES NOT REFUSE. #20 offered "refuse a second
// concurrent prwait of the same user, or warn about it". A refusal was the wrong
// half of that choice here: with ~25 sessions on this machine, one 30-minute
// wait would lock every other session out of the only tool they are told to wait
// with, and the first thing that happens to a gate nobody can keep green is that
// it gets switched off — by a flag, by an environment variable, or by going back
// to `gh pr checks --watch`, which is the defect prwait exists to prevent. What
// a second wait gets instead is the first one's targets counted into its own
// pacing, and a line naming the wait it is sharing the budget with.
//
// ⛔ NOTHING HERE MAY FAIL A WAIT. Every error in this file becomes a note
// printed beside the verdict, never an exit status: a tool that refused to
// report on a pull request because it could not write its own bookkeeping file
// would be a worse failure than the one it is preventing.
//
// ⛔ IT RECORDS NO INTERVAL. The obvious field to add is the one this wait
// settled on, and it cannot be written truthfully: the interval is decided FROM
// the other entries, so an entry would have to be written before its own value
// is known and left stating the flag's default. A field that is right at first
// write and wrong forever after is worse than an absent one, and nothing here
// decides anything from it — the pacing counts targets.
type waitEntry struct {
	PID      int       `json:"pid"`
	Started  time.Time `json:"started"`
	Deadline time.Time `json:"deadline"`
	Targets  []string  `json:"targets"`
}

// same identifies an entry. A PID alone would not: PIDs are reused, and the
// entry of a dead wait whose number has come round again must not be mistaken
// for this one's.
func (e waitEntry) same(o waitEntry) bool {
	return e.PID == o.PID && e.Started.Equal(o.Started)
}

// describe names a wait in a message: what it is polling, and since when.
//
// ⛔ THE FILE IS PARSED AND THEN PRINTED TO A TERMINAL, so the strings in it are
// treated as data and never as output. Whoever can write this file already has
// the account, which is why this is a bound and not a defence: a target carrying
// ESC sequences would otherwise clear the screen, move the cursor over the
// verdict above it or rewrite the line that says what failed — a tool whose
// whole purpose is to report cannot hand another writer control of its report.
// Each target is also bounded in length and in number, because the one thing a
// corrupt file can always do is be enormous.
func (e waitEntry) describe(now time.Time) string {
	age := now.Sub(e.Started).Round(time.Second)
	shown := e.Targets
	more := 0
	if len(shown) > maxTargetsNamed {
		more = len(shown) - maxTargetsNamed
		shown = shown[:maxTargetsNamed]
	}
	safe := make([]string, 0, len(shown)+1)
	for _, t := range shown {
		safe = append(safe, printable(t))
	}
	if more > 0 {
		safe = append(safe, fmt.Sprintf("and %d more", more))
	}
	ts := strings.Join(safe, " ")
	if ts == "" {
		ts = "no target recorded"
	}
	return fmt.Sprintf("pid %d, started %s ago, %d target(s): %s", e.PID, age, len(e.Targets), ts)
}

// printable makes one string from the registry safe to print: no control
// characters, and bounded in length.
func printable(s string) string {
	if len(s) > maxTargetLen {
		s = s[:maxTargetLen] + "…"
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}

type registryFileContents struct {
	Waits []waitEntry `json:"waits"`
}

const (
	registryDirName = "prwait"
	registryBase    = "waits.json"
	// registryGrace is how long past its own timeout an entry is still believed
	// to be polling, because an entry dropped too early is a wait that stops
	// being counted while it is still spending calls.
	//
	// ⛔ ONE WHOLE maxInterval, MEASURED AND NOT GUESSED. The first value here
	// was two minutes, from the assumption that a wait ends near its timeout.
	// Running the built binary settled it instead:
	// `prwait -gh /usr/bin/false -timeout 2s -repo go-x/y 78 79 80 82` did not
	// return for about three minutes, because the wait's clock is read when an
	// ANSWER arrives and the sleep before the next poll is a whole interval —
	// which #25's scaling made as long as maxInterval. So a wait can outlive
	// its own -timeout by one poll, and a grace shorter than that drops live
	// waits out of the registry.
	registryGrace = maxInterval + time.Minute
	// lockStale is when a lock directory is taken for the leftover of a wait
	// that was killed between taking it and releasing it. Everything done under
	// the lock is two file operations on a file of a few hundred bytes.
	lockStale = 30 * time.Second
	lockWait  = 2 * time.Second
	lockPoll  = 20 * time.Millisecond
	// maxRegistryBytes bounds what is parsed. A plausible registry is a few
	// hundred bytes per wait and this machine runs about twenty-five sessions;
	// a megabyte is four orders of magnitude of headroom and still a bound,
	// which an unbounded ReadFile of a file another process writes is not.
	// Past it the file is not parsed and the next write replaces it.
	maxRegistryBytes = 1 << 20
	// maxTargetsNamed and maxTargetLen bound the MESSAGE, which is the other
	// thing a corrupt file can make unusable.
	maxTargetsNamed = 3
	maxTargetLen    = 80
	// maxWaitsNamed bounds how many other waits a single line names.
	maxWaitsNamed = 3
)

// registryPath is where the live waits of THIS user are recorded.
func registryPath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, registryDirName, registryBase), nil
}

// prune drops the entries that cannot be polling any more.
//
// Two reasons, and they are not interchangeable:
//
//   - the DEADLINE has passed: the wait's own timeout plus a grace, which is
//     the only bound that holds on every platform. This is the primary test.
//   - the PROCESS is gone: a wait killed before its timeout. ⛔ This one may
//     only ever REMOVE an entry the deadline would have kept, and `alive`
//     answers true whenever it cannot tell, so a platform that cannot answer
//     keeps the entry until its deadline rather than silently emptying the
//     registry and restoring the unpaced behaviour this file exists to fix.
func prune(ws []waitEntry, now time.Time, alive func(int) bool) []waitEntry {
	kept := make([]waitEntry, 0, len(ws))
	for _, w := range ws {
		if !w.Deadline.After(now) {
			continue
		}
		// ⛔ A NON-POSITIVE PID IS STRUCTURAL, NOT A LIVENESS MEASUREMENT. No
		// platform has a process 0 or -1 to poll, and os.Getpid() never returns
		// one, so such an entry is malformed or hand-written rather than a wait
		// somebody is running — dropping it is not the asymmetric "it is gone"
		// judgement that alive() is forbidden to guess at. Keeping it here and
		// not in alive() is what windows-latest caught: the !unix fallback
		// answers "cannot tell" for EVERY pid, as it must, so a rule phrased as
		// liveness held on unix and nowhere else.
		//
		// ⛔ It also means syscall.Kill is never handed 0 or -1, which on unix
		// address the process GROUP and every process the user may signal. With
		// signal 0 nothing is delivered either way, so this is not the only
		// thing standing between us and that call — alive_unix.go refuses them
		// too — but a guard against signalling a group belongs on both sides.
		if w.PID <= 0 {
			continue
		}
		if !alive(w.PID) {
			continue
		}
		kept = append(kept, w)
	}
	return kept
}

// registration is what a wait learns by announcing itself.
type registration struct {
	// Others are the live waits of this user that are NOT this one.
	Others []waitEntry
	// Notes is what the bookkeeping could not do, to be printed rather than
	// returned as an error.
	Notes []string
	leave func()
}

// Leave removes this wait's entry. It is always safe to call, including when
// nothing was ever written.
func (r registration) Leave() {
	if r.leave != nil {
		r.leave()
	}
}

// targets counts the targets the other waits are polling.
func (r registration) targets() int {
	n := 0
	for _, o := range r.Others {
		n += len(o.Targets)
	}
	return n
}

// enter records self in the registry and reports the other live waits.
//
// It never returns an error: see the contract at the top of this file.
func enter(path string, self waitEntry, now time.Time, alive func(int) bool) registration {
	reg := registration{leave: func() {}}
	if path == "" {
		reg.Notes = append(reg.Notes, "prwait: no user cache directory, so this wait cannot be counted with the others of this user; its pacing is its own targets only.")
		return reg
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		reg.Notes = append(reg.Notes, fmt.Sprintf("prwait: cannot create %s (%v), so this wait is paced on its own targets only.", filepath.Dir(path), err))
		return reg
	}
	notes := withLock(path, func() error {
		ws, err := readWaits(path)
		if err != nil {
			// A registry that cannot be read is a registry with no other waits
			// in it as far as this process can tell, and saying so is the whole
			// point: "could not read" must not arrive as "nobody else is
			// waiting".
			reg.Notes = append(reg.Notes, fmt.Sprintf("prwait: %v — this wait is paced on its own targets only, and is still recorded for the next one.", err))
			ws = nil
		}
		ws = prune(ws, now, alive)
		for _, w := range ws {
			if !w.same(self) {
				reg.Others = append(reg.Others, w)
			}
		}
		return writeWaits(path, append(ws, self))
	})
	reg.Notes = append(reg.Notes, notes...)
	reg.leave = func() {
		_ = withLock(path, func() error {
			ws, err := readWaits(path)
			if err != nil {
				// ⛔ LEAVE WITHOUT A READ WOULD CLOBBER THE WHOLE REGISTRY.
				// Writing "every entry except mine" when the file could not be
				// read means writing an EMPTY registry: every other session's
				// wait would stop being counted, and the next wait would pace
				// itself alone — which is the defect this file exists to
				// prevent, caused by its own cleanup.
				return nil
			}
			kept := make([]waitEntry, 0, len(ws))
			for _, w := range ws {
				if !w.same(self) {
					kept = append(kept, w)
				}
			}
			return writeWaits(path, kept)
		})
	}
	return reg
}

func readWaits(path string) ([]waitEntry, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		// ⛔ "OPEN" AND "READ" ARE NAMED APART, and not for prettiness: with one
		// wording for both, this branch had NO test. Skipping it leaves a nil
		// file whose Read answers ErrInvalid, the next branch reports that under
		// the same sentence, and every assertion still passed — `mutsweep`
		// reported the condition as unheld twice before the two messages
		// differed. A diagnosis that cannot tell two causes apart cannot be
		// held to either.
		return nil, fmt.Errorf("cannot open the wait registry %s: %w", path, err)
	}
	defer f.Close()
	// ⛔ BOUNDED, and the bound is read by asking for ONE BYTE MORE than it
	// allows: a LimitReader that stops exactly at the limit returns a full
	// buffer for a file of that size and for a file of a gigabyte, so there is
	// nothing left in hand to tell "complete" from "truncated".
	b, err := io.ReadAll(io.LimitReader(f, maxRegistryBytes+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read the wait registry %s: %w", path, err)
	}
	if len(b) > maxRegistryBytes {
		return nil, fmt.Errorf("the wait registry %s is larger than %d bytes, which no plausible number of waits is: it is not parsed, and the next wait to finish replaces it", path, maxRegistryBytes)
	}
	if len(b) == 0 {
		return nil, nil
	}
	var c registryFileContents
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("the wait registry %s is not readable JSON: %w", path, err)
	}
	return c.Waits, nil
}

// writeWaits replaces the registry, never truncating it in place: a reader that
// arrives mid-write must see either the old file or the new one.
//
// ⛔ ONE WRITE AND A RENAME, not CreateTemp/Write/Close/Rename. The four-call
// shape has four error branches, and two of them (a short Write, a failing
// Close) cannot be reached from any input a test can build — only from a fake
// file handle, which is how a fake that reported a failure WITHOUT doing the
// operation came to invent a Windows-only defect in another repository. These
// three branches are each reachable with a real condition, and the tests use
// those: an unencodable entry, a missing directory, a target that is a
// directory. The temporary name needs no randomness because every call happens
// under the lock.
func writeWaits(path string, ws []waitEntry) error {
	b, err := json.Marshal(registryFileContents{Waits: ws})
	if err != nil {
		// Reachable: time.Time refuses a year outside [0,9999], so one
		// unencodable entry is reported rather than silently replacing the
		// registry with nothing.
		return fmt.Errorf("cannot encode the wait registry: %w", err)
	}
	tmp := path + ".writing"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("cannot write the wait registry: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("cannot replace the wait registry: %w", err)
	}
	return nil
}

// mkdirLock creates the lock directory; a variable so a test can inject the
// error Windows gives for a directory that is delete-pending.
var mkdirLock = os.Mkdir

// withLock serialises the read-modify-write above between processes.
//
// ⛔ A DIRECTORY, because os.Mkdir is the one creation that is atomic and
// exclusive on every platform this tool is built for, Windows included — and
// two prwaits starting in the same second is the normal case here, not the
// exception: whatever is lost under a race is another session's entry, which
// reads afterwards as "nobody else is waiting".
func withLock(path string, f func() error) (notes []string) {
	lock := path + ".lock"
	for start := time.Now(); ; {
		err := mkdirLock(lock, 0o700)
		if err == nil {
			break
		}
		// ⛔ "ACCESS DENIED" IS CONTENTION ON WINDOWS. RemoveDirectory only MARKS
		// a directory for deletion on close ("the directory is not removed until
		// the last handle to the directory is closed", Win32 RemoveDirectory
		// remarks), and until then a Mkdir of the same name fails with
		// ERROR_ACCESS_DENIED — not "already exists". Reading that as "cannot
		// lock at all" skipped the body, so a wait's entry was never written:
		// measured on windows-latest, 51 of 1800 rounds of 8 concurrent enters
		// lost an entry, every one with `mkdir …waits.json.lock: Access is
		// denied.` A permission error is therefore retried like EEXIST until
		// lockWait, and a lock that really cannot be created (read-only parent)
		// still ends in a note, naming the error.
		contended := os.IsExist(err) || os.IsPermission(err)
		if !contended {
			return append(notes, fmt.Sprintf("prwait: cannot lock the wait registry (%v), so this wait is paced on its own targets only.", err))
		}
		if st, serr := os.Stat(lock); serr == nil && time.Since(st.ModTime()) > lockStale {
			// The holder died between Mkdir and RemoveAll. Taking it over is
			// said out loud: a lock removed in silence is indistinguishable
			// from one that was never contended.
			notes = append(notes, fmt.Sprintf("prwait: the wait registry's lock was left behind %s ago, taking it over.", time.Since(st.ModTime()).Round(time.Second)))
			os.RemoveAll(lock)
			continue
		}
		if time.Since(start) > lockWait {
			return append(notes, fmt.Sprintf("prwait: the wait registry stayed locked for %s (%v), so this wait is paced on its own targets only.", lockWait, err))
		}
		time.Sleep(lockPoll)
	}
	defer os.RemoveAll(lock)
	if err := f(); err != nil {
		notes = append(notes, fmt.Sprintf("prwait: %v — the pacing of the NEXT wait is what this costs, not this one's verdict.", err))
	}
	return notes
}
