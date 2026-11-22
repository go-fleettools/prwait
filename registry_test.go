package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var regNow = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func always(int) bool { return true }
func never(int) bool  { return false }

// seed writes a registry holding ws, as another process would have left it.
func seed(t *testing.T, ws ...waitEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "waits.json")
	if err := writeWaits(path, ws); err != nil {
		t.Fatal(err)
	}
	return path
}

func read(t *testing.T, path string) []waitEntry {
	t.Helper()
	ws, err := readWaits(path)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func other(targets ...string) waitEntry {
	return waitEntry{PID: 4711, Started: regNow.Add(-3 * time.Minute), Deadline: regNow.Add(27 * time.Minute), Targets: targets}
}

func self(targets ...string) waitEntry {
	return waitEntry{PID: os.Getpid(), Started: regNow, Deadline: regNow.Add(32 * time.Minute), Targets: targets}
}

// ⛔ THE POINT OF THE WHOLE FILE: four waits of one target each must not cost
// four times what one wait of four targets costs. The quota is per user.
//
// `mutsweep -files registry.go,interval.go,alive_unix.go` reports 20 mutations,
// 20 caught, 0 survived (2026-11-22) — and it reaches only SOME `if`
// conditions, so what it does not reach was mutated BY HAND. Re-apply all three
// after any change here, and expect the named test to go red:
//
// ⛔ MEASURED 2026-11-22, AFTER THE WINDOWS FAILURE BELOW: a sweep over
// registry.go + alive_unix.go reported "17 mutations, 17 caught, 0 survived"
// and mutated NOTHING IN prune() — not its deadline test, not its pid test, not
// its alive() test. A green sweep is a count of what it reached; over this
// function it reached nothing at all, so its silence must never be read as
// cover. Same class as go-fleettools/fleettools#391.
//
//	defaultInterval(mine + elsewhere) -> defaultInterval(mine)
//	    TestASecondWaitIsPacedForEveryTargetOfThisUser: "interval 45s, want
//	    3m0s" and TestPastTheCap…: "11 targets gave 1m30s" (verified
//	    2026-11-22). This is the whole change: without the sum, every wait
//	    paces itself perfectly and the user's budget still goes to zero.
//	same(): drop `&& e.Started.Equal(o.Started)`
//	    TestLeaveRemovesThisWaitAndNobodyElses: "an entry sharing this PID was
//	    taken for this wait" (verified 2026-11-22). PIDs are reused.
//	prune(): `if w.PID <= 0` -> `if false && w.PID <= 0`
//	    TestWhichEntriesAreCountedAndWhichAreDropped: "pid zero, liveness says
//	    keep: kept=true, want false" AND the negative-pid row (verified
//	    2026-11-22, both red). ⛔ This rule lives here and NOT in alive(),
//	    because windows-latest went red on 556b67c proving the difference:
//	    "pid 0 and -1 are not processes to signal" (registry_test.go:167) held
//	    on unix and could not hold on a platform whose honest answer to every
//	    pid is "cannot tell". A non-positive pid is a malformed ENTRY, not a
//	    dead process.
func TestASecondWaitIsPacedForEveryTargetOfThisUser(t *testing.T) {
	path := seed(t, other("go-x/y#78", "go-x/y#79", "go-x/y#80"))
	reg := enter(path, self("go-x/y#82"), regNow, always)
	defer reg.Leave()
	if len(reg.Notes) != 0 {
		t.Fatalf("notes on a clean registry: %q", reg.Notes)
	}
	if len(reg.Others) != 1 || reg.targets() != 3 {
		t.Fatalf("%d other wait(s), %d target(s): %+v", len(reg.Others), reg.targets(), reg.Others)
	}
	d, lines := machineInterval(false, baseInterval, 1, reg.Others, regNow)
	if want := 4 * baseInterval; d != want {
		t.Fatalf("interval %s, want %s: one target here and three elsewhere is four targets of one budget", d, want)
	}
	joined := strings.Join(lines, "\n")
	for _, w := range []string{"pid 4711", "3m0s ago", "go-x/y#78 go-x/y#79 go-x/y#80", "per USER", "4 target(s) in all"} {
		if !strings.Contains(joined, w) {
			t.Errorf("the explanation never says %q:\n%s", w, joined)
		}
	}
	// And this wait is now in the file for the next one to find.
	if ws := read(t, path); len(ws) != 2 {
		t.Fatalf("%d entr(ies) recorded, want 2: %+v", len(ws), ws)
	}
}

// A single wait keeps every property #25 established: that is the control for
// the test above, since counting zero other targets must change nothing.
func TestAloneAWaitIsPacedExactlyAsBefore(t *testing.T) {
	path := seed(t)
	reg := enter(path, self("go-x/y#82"), regNow, always)
	defer reg.Leave()
	if len(reg.Others) != 0 {
		t.Fatalf("others in an empty registry: %+v", reg.Others)
	}
	d, lines := machineInterval(false, baseInterval, 1, reg.Others, regNow)
	if d != baseInterval || len(lines) != 0 {
		t.Fatalf("one target alone: %s, %q — want %s and no explanation", d, lines, baseInterval)
	}
	d, lines = machineInterval(false, baseInterval, 4, reg.Others, regNow)
	if want := 4 * baseInterval; d != want || len(lines) != 1 {
		t.Fatalf("four targets alone: %s, %d line(s) — want %s and the #25 explanation", d, len(lines), want)
	}
}

func TestWhichEntriesAreCountedAndWhichAreDropped(t *testing.T) {
	for _, c := range []struct {
		name  string
		entry waitEntry
		alive func(int) bool
		kept  bool
	}{
		{"live, deadline ahead", other("a"), always, true},
		{"deadline passed", waitEntry{PID: 4711, Started: regNow.Add(-time.Hour), Deadline: regNow.Add(-time.Second), Targets: []string{"a"}}, always, false},
		{"deadline exactly now", waitEntry{PID: 4711, Started: regNow.Add(-time.Hour), Deadline: regNow, Targets: []string{"a"}}, always, false},
		{"process gone", other("a"), never, false},
		// ⛔ pid 0 and -1 with `always`: the entry is dropped by the STRUCTURAL
		// rule, and `always` is what proves it — liveness is answering "keep
		// this" for both. Asserted through prune rather than through alive(),
		// because the !unix fallback must answer "cannot tell" for every pid;
		// phrasing this as liveness is what reddened windows-latest on 556b67c
		// ("pid 0 and -1 are not processes to signal", registry_test.go:167).
		{"pid zero, liveness says keep", waitEntry{PID: 0, Started: regNow, Deadline: regNow.Add(time.Hour), Targets: []string{"a"}}, always, false},
		{"negative pid, liveness says keep", waitEntry{PID: -1, Started: regNow, Deadline: regNow.Add(time.Hour), Targets: []string{"a"}}, always, false},
	} {
		got := prune([]waitEntry{c.entry}, regNow, c.alive)
		if kept := len(got) == 1; kept != c.kept {
			t.Errorf("%s: kept=%v, want %v", c.name, kept, c.kept)
		}
	}
	// ⛔ And through enter, where the consequence is the one that matters: an
	// entry that is not counted is a wait whose calls nobody paced for, and an
	// entry wrongly counted paces this wait for a budget nobody is spending.
	dead := waitEntry{PID: 4711, Started: regNow.Add(-time.Hour), Deadline: regNow.Add(-time.Second), Targets: []string{"a", "b"}}
	path := seed(t, dead)
	reg := enter(path, self("c"), regNow, always)
	defer reg.Leave()
	if len(reg.Others) != 0 || reg.targets() != 0 {
		t.Fatalf("an expired entry was counted: %+v", reg.Others)
	}
	if ws := read(t, path); len(ws) != 1 || ws[0].PID != os.Getpid() {
		t.Fatalf("the expired entry was left in the file: %+v", ws)
	}
}

// alive is the only part of this that cannot be faked, so it is measured
// against a process that certainly exists and one that certainly does not.
func TestAliveAnswersForARealProcessAndAReapedOne(t *testing.T) {
	if !alive(os.Getpid()) {
		t.Fatal("this very process reads as dead")
	}
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "PRWAIT_HELPER=exitnow")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the helper did not exit cleanly: %v", err)
	}
	switch got := alive(pid); {
	case !canTellProcessLiveness:
		// Documented in alive_other.go: this platform cannot tell, and says so
		// by keeping the entry. The assertion is here so that an implementation
		// which starts guessing "dead" is caught on its own platform.
		if !got {
			t.Fatalf("on %s alive must answer true whenever it cannot tell", runtime.GOOS)
		}
	case got:
		t.Fatalf("pid %d was started, waited for and reaped, and still reads as alive", pid)
	}
	// ⛔ UNIX ONLY, AND NOT BECAUSE WINDOWS IS EXCUSED. Dropping a non-positive
	// pid is a structural rule, held for every platform in prune() and asserted
	// in TestWhichEntriesAreCountedAndWhichAreDropped. What is asserted HERE is
	// narrower: that the unix implementation never hands 0 or -1 to
	// syscall.Kill, which address the process GROUP and every process this user
	// may signal. A platform that cannot tell liveness must answer true for
	// these as for any other pid, so asking it otherwise tests nothing it is
	// allowed to know.
	if canTellProcessLiveness && (alive(0) || alive(-1)) {
		t.Fatal("pid 0 and -1 must never reach syscall.Kill, which would address a process group")
	}
}

func TestLeaveRemovesThisWaitAndNobodyElses(t *testing.T) {
	keep := other("go-x/y#78")
	path := seed(t, keep)
	mine := self("go-x/y#82")
	reg := enter(path, mine, regNow, always)
	if ws := read(t, path); len(ws) != 2 {
		t.Fatalf("%d entr(ies) after enter: %+v", len(ws), ws)
	}
	reg.Leave()
	ws := read(t, path)
	if len(ws) != 1 || !ws[0].same(keep) {
		t.Fatalf("Leave did not leave exactly the other wait: %+v", ws)
	}
	// Called twice, or with nothing recorded, it is still a no-op rather than a
	// panic: it runs on the exit path of a tool whose job is to report.
	reg.Leave()
	registration{}.Leave()
	if ws := read(t, path); len(ws) != 1 {
		t.Fatalf("a second Leave changed the file: %+v", ws)
	}
	// ⛔ A PID ALONE DOES NOT IDENTIFY A WAIT. An entry of this same PID from an
	// earlier, finished wait must not be removed by this one's Leave, and must
	// not be taken for this one either.
	older := waitEntry{PID: os.Getpid(), Started: regNow.Add(-time.Hour), Deadline: regNow.Add(time.Hour), Targets: []string{"go-x/y#1"}}
	path = seed(t, older)
	reg = enter(path, mine, regNow, always)
	if len(reg.Others) != 1 {
		t.Fatalf("an entry sharing this PID was taken for this wait: %+v", reg.Others)
	}
	reg.Leave()
	if ws := read(t, path); len(ws) != 1 || !ws[0].same(older) {
		t.Fatalf("Leave removed an entry that only shared the PID: %+v", ws)
	}
}

// ⛔ A CLEANUP THAT CANNOT READ THE FILE MUST NOT WRITE IT. "Every entry except
// mine", computed over a file that could not be read, is an EMPTY registry:
// every other session's wait stops being counted and the next wait paces itself
// alone, which is the failure this whole file exists to prevent — caused by its
// own cleanup, on the exit path, where nothing is watching.
func TestLeaveLeavesAnUnreadableRegistryAloneRatherThanEmptyingIt(t *testing.T) {
	path := seed(t, other("go-x/y#78"))
	reg := enter(path, self("go-x/y#82"), regNow, always)
	broken := []byte(`{"waits": [ truncated`)
	if err := os.WriteFile(path, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	reg.Leave()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(broken) {
		t.Fatalf("Leave rewrote a registry it could not read:\n%s", got)
	}
	// The control: when it CAN read the file, it must still remove its own
	// entry — otherwise this test would pass on a Leave that does nothing at
	// all.
	path = seed(t, other("go-x/y#78"))
	reg = enter(path, self("go-x/y#82"), regNow, always)
	reg.Leave()
	if ws := read(t, path); len(ws) != 1 || ws[0].PID != 4711 {
		t.Fatalf("%+v", ws)
	}
}

// ⛔ "COULD NOT READ" MUST NOT ARRIVE AS "NOBODY ELSE IS WAITING". That
// substitution is the whole failure mode of an advisory registry: it is silent,
// it looks exactly like the healthy case, and its effect is the unpaced
// behaviour this file exists to stop.
func TestAnUnreadableRegistryIsSaidOutLoudAndStillRecordsThisWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), "waits.json")
	if err := os.WriteFile(path, []byte("}{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := enter(path, self("go-x/y#82"), regNow, always)
	defer reg.Leave()
	if len(reg.Notes) == 0 {
		t.Fatal("an unreadable registry produced no note at all")
	}
	if !strings.Contains(strings.Join(reg.Notes, "\n"), "not readable JSON") {
		t.Fatalf("the note does not say what was wrong: %q", reg.Notes)
	}
	if len(reg.Others) != 0 {
		t.Fatalf("others read out of unreadable JSON: %+v", reg.Others)
	}
	if ws := read(t, path); len(ws) != 1 || ws[0].PID != os.Getpid() {
		t.Fatalf("this wait was not recorded over the broken file: %+v", ws)
	}
}

func TestNoRegistryAtAllIsANoteAndNotAFailure(t *testing.T) {
	reg := enter("", self("go-x/y#82"), regNow, always)
	if len(reg.Notes) != 1 || !strings.Contains(reg.Notes[0], "cache directory") {
		t.Fatalf("notes %q", reg.Notes)
	}
	if len(reg.Others) != 0 {
		t.Fatalf("others %+v", reg.Others)
	}
	reg.Leave()
	// An unwritable directory is the same class: a note, and a wait that still
	// runs.
	//
	// ⛔ THE NOTE IS ASSERTED BY ITS TEXT, not by its existence. With "is there
	// a note?" as the assertion, this test passed while the MkdirAll check was
	// mutated away — the failure merely moved one call down and produced a
	// different note, which the weaker assertion accepted. `mutsweep` named
	// that: registry.go's MkdirAll branch survived against this very test.
	dir := t.TempDir()
	file := filepath.Join(dir, "notadir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	reg = enter(filepath.Join(file, "waits.json"), self("go-x/y#82"), regNow, always)
	defer reg.Leave()
	if !strings.Contains(strings.Join(reg.Notes, "\n"), "cannot create "+file) {
		t.Fatalf("the note does not name the directory it could not create: %q", reg.Notes)
	}
}

// The common case on a machine where prwait has never run, and the one a
// mutated `os.IsNotExist` turns into a complaint: a missing registry is an
// empty registry, not an unreadable one.
func TestTheFirstWaitOnAMachineFindsNoFileAndSaysNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "waits.json")
	reg := enter(path, self("go-x/y#82"), regNow, always)
	defer reg.Leave()
	if len(reg.Notes) != 0 {
		t.Fatalf("the first wait ever was given notes: %q", reg.Notes)
	}
	if ws := read(t, path); len(ws) != 1 {
		t.Fatalf("%d entr(ies): %+v", len(ws), ws)
	}
	// A file of zero bytes is the same: it is what a crash between create and
	// write leaves, and it says nothing is waiting, not that nothing can be
	// read.
	path = filepath.Join(t.TempDir(), "waits.json")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	reg2 := enter(path, self("go-x/y#82"), regNow, always)
	defer reg2.Leave()
	if len(reg2.Notes) != 0 {
		t.Fatalf("an empty registry file was given notes: %q", reg2.Notes)
	}
	if ws := read(t, path); len(ws) != 1 {
		t.Fatalf("%d entr(ies) over the empty file: %+v", len(ws), ws)
	}
}

// A registry that cannot be read for a reason OTHER than absence, which is the
// branch between those two: a directory where the file should be.
func TestADirectoryWhereTheRegistryShouldBeIsReportedAsUnreadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "waits.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	reg := enter(path, self("go-x/y#82"), regNow, always)
	defer reg.Leave()
	joined := strings.Join(reg.Notes, "\n")
	if !strings.Contains(joined, "cannot read the wait registry") {
		t.Fatalf("notes: %q", reg.Notes)
	}
	// And the write that cannot replace a directory is reported too, instead of
	// leaving the caller believing it was recorded.
	if !strings.Contains(joined, "cannot replace the wait registry") {
		t.Fatalf("the failed write was not reported: %q", reg.Notes)
	}
	if len(reg.Others) != 0 {
		t.Fatalf("others %+v", reg.Others)
	}
	// ⛔ AND A FILE THAT CANNOT BE OPENED AT ALL is a third case, not a repeat
	// of the two above: a directory OPENS successfully on unix and fails when
	// read, so the open's own failure had no test until this one. Mode 0000 is
	// what a half-migrated cache directory leaves behind.
	if runtime.GOOS == "windows" {
		t.Skip("file modes do not deny a read here")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file")
	}
	path = filepath.Join(t.TempDir(), "waits.json")
	if err := os.WriteFile(path, []byte(`{"waits":[]}`), 0o000); err != nil {
		t.Fatal(err)
	}
	reg2 := enter(path, self("go-x/y#82"), regNow, always)
	defer reg2.Leave()
	if !strings.Contains(strings.Join(reg2.Notes, "\n"), "cannot open the wait registry") {
		t.Fatalf("an unopenable registry gave %q", reg2.Notes)
	}
	if ws := read(t, path); len(ws) != 1 {
		t.Fatalf("it was not replaced: %+v", ws)
	}
}

func TestWritingTheRegistryFailsLoudlyAndLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	// An entry no JSON can hold: time.Time refuses a year outside [0,9999], and
	// an encoding failure must be reported rather than replacing the registry
	// with an empty one.
	bad := waitEntry{PID: 1, Started: time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC), Deadline: regNow, Targets: []string{"a"}}
	if err := writeWaits(filepath.Join(dir, "waits.json"), []waitEntry{bad}); err == nil {
		t.Fatal("an unencodable entry was written without complaint")
	}
	// A directory that does not exist. ⛔ The ERROR'S SUBJECT is asserted, not
	// just its existence: with the write's own branch mutated away, the rename
	// of a file that was never written fails too, so "an error came back" is
	// true either way and says nothing about which step failed.
	err := writeWaits(filepath.Join(dir, "nope", "waits.json"), nil)
	if err == nil {
		t.Fatal("a missing directory was written to without complaint")
	}
	if !strings.Contains(err.Error(), "cannot write the wait registry") {
		t.Fatalf("a failed write is reported as %v", err)
	}
	// A target that cannot be replaced by a rename.
	target := filepath.Join(dir, "waits.json")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "inside"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeWaits(target, nil); err == nil {
		t.Fatal("a non-empty directory was renamed over without complaint")
	} else if !strings.Contains(err.Error(), "cannot replace the wait registry") {
		t.Fatalf("a failed rename is reported as %v", err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 {
		t.Fatalf("a half-written file was left behind: %v", ents)
	}
}

// The lock's own failures, which are not "somebody holds it": a parent that is
// a file cannot hold a lock directory at all, and reading that as "held" would
// wait two seconds for nothing.
func TestALockThatCannotBeTakenAtAllIsNotReadAsHeld(t *testing.T) {
	file := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ran := false
	start := time.Now()
	notes := withLock(filepath.Join(file, "waits.json"), func() error { ran = true; return nil })
	if ran {
		t.Fatal("the body ran without the lock")
	}
	if !strings.Contains(strings.Join(notes, "\n"), "cannot lock the wait registry") {
		t.Fatalf("notes: %q", notes)
	}
	if waited := time.Since(start); waited > lockWait {
		t.Fatalf("waited %s for a lock that could never be taken", waited)
	}
}

func TestRegistryPathIsUnderTheUsersCacheDirectory(t *testing.T) {
	p, err := registryPath()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(p) != registryBase || filepath.Base(filepath.Dir(p)) != registryDirName {
		t.Fatalf("%s is not <cache>/%s/%s", p, registryDirName, registryBase)
	}
	if runtime.GOOS == "windows" {
		t.Skip("no HOME to unset: os.UserCacheDir reads %LocalAppData% here")
	}
	// ⛔ AND IT SAYS SO WHEN IT CANNOT ANSWER. Without this branch the path is
	// the empty string, enter() treats it as "no registry", and the wait is
	// paced alone — the exact failure this file exists to prevent, arriving
	// through a silent success.
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	if _, err := registryPath(); err == nil {
		t.Fatal("with no home and no XDG_CACHE_HOME, a path was returned anyway")
	}
}

// Two prwaits starting in the same second is the normal case on this machine,
// and whatever a race loses is another session's entry — which reads afterwards
// as "nobody else is waiting".
func TestConcurrentEntriesAreNotLost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "waits.json")
	const n = 8
	var wg sync.WaitGroup
	regs := make([]registration, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			regs[i] = enter(path, waitEntry{
				PID:      10000 + i,
				Started:  regNow,
				Deadline: regNow.Add(time.Hour),
				Targets:  []string{"go-x/y#" + string(rune('a'+i))},
			}, regNow, always)
		}(i)
	}
	wg.Wait()
	if ws := read(t, path); len(ws) != n {
		t.Fatalf("%d of %d entries survived the race: %+v", len(ws), n, ws)
	}
	for i := range regs {
		for _, note := range regs[i].Notes {
			if strings.Contains(note, "stayed locked") {
				t.Errorf("entry %d gave up on the lock: %s", i, note)
			}
		}
	}
	var wg2 sync.WaitGroup
	for i := range regs {
		wg2.Add(1)
		go func(i int) {
			defer wg2.Done()
			regs[i].Leave()
		}(i)
	}
	wg2.Wait()
	if ws := read(t, path); len(ws) != 0 {
		t.Fatalf("%d entr(ies) left after every Leave: %+v", len(ws), ws)
	}
}

func TestALockLeftBehindIsTakenOverAndSaidSoLoud(t *testing.T) {
	path := filepath.Join(t.TempDir(), "waits.json")
	lock := path + ".lock"
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-lockStale - time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	reg := enter(path, self("go-x/y#82"), regNow, always)
	defer reg.Leave()
	if !strings.Contains(strings.Join(reg.Notes, "\n"), "left behind") {
		t.Fatalf("a stale lock was taken over in silence: %q", reg.Notes)
	}
	if ws := read(t, path); len(ws) != 1 {
		t.Fatalf("the wait was not recorded: %+v", ws)
	}
	// A FRESH lock is the control: it is waited for, not taken over, and after
	// lockWait the wait proceeds unpaced with a note that says exactly that.
	path = filepath.Join(t.TempDir(), "waits.json")
	if err := os.Mkdir(path+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	reg2 := enter(path, self("go-x/y#82"), regNow, always)
	// Released here rather than deferred: Leave takes the same lock, and
	// waiting lockWait for it a second time only lengthens the suite.
	os.RemoveAll(path + ".lock")
	defer reg2.Leave()
	if waited := time.Since(start); waited < lockPoll {
		t.Fatalf("a held lock was not waited for at all (%s)", waited)
	}
	if !strings.Contains(strings.Join(reg2.Notes, "\n"), "stayed locked") {
		t.Fatalf("a held lock produced %q", reg2.Notes)
	}
}

// #25's rule, now that there is a second thing that could override it: a caller
// who names an interval has said what they want of the budget.
func TestAnExplicitIntervalIsNotScaledButTheOtherWaitIsStillNamed(t *testing.T) {
	others := []waitEntry{other("go-x/y#78", "go-x/y#79", "go-x/y#80")}
	d, lines := machineInterval(true, 90*time.Second, 1, others, regNow)
	if d != 90*time.Second {
		t.Fatalf("an explicit -interval became %s", d)
	}
	joined := strings.Join(lines, "\n")
	for _, w := range []string{"pid 4711", "honoured as typed", "NOT scaled"} {
		if !strings.Contains(joined, w) {
			t.Errorf("%q missing from:\n%s", w, joined)
		}
	}
}

func TestPastTheCapTheSharedCostIsStatedRatherThanHeld(t *testing.T) {
	var many []string
	for i := 0; i < 9; i++ {
		many = append(many, "go-x/y#"+string(rune('a'+i)))
	}
	d, lines := machineInterval(false, baseInterval, 2, []waitEntry{other(many...)}, regNow)
	if d != maxInterval {
		t.Fatalf("11 targets gave %s, want the cap %s", d, maxInterval)
	}
	joined := strings.Join(lines, "\n")
	for _, w := range []string{"capped at 5m0s", "reads as a hang", "and the waits above spend theirs too"} {
		if !strings.Contains(joined, w) {
			t.Errorf("%q missing from:\n%s", w, joined)
		}
	}
}

// The file another process reads must be the file this one wrote: a shape
// nobody can decode is the same as an empty registry, and this is the only
// assertion that holds the two ends together.
func TestTheRecordedShapeIsTheOneTheNextWaitDecodes(t *testing.T) {
	path := seed(t, other("go-x/y#78"))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c registryFileContents
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Waits) != 1 || c.Waits[0].PID != 4711 || len(c.Waits[0].Targets) != 1 {
		t.Fatalf("%s", b)
	}
	if !strings.Contains(string(b), `"pid"`) || !strings.Contains(string(b), `"deadline"`) {
		t.Fatalf("the field names another version would read are not in the file: %s", b)
	}
	if ents, err := os.ReadDir(filepath.Dir(path)); err != nil || len(ents) != 1 {
		t.Fatalf("%d entr(ies) in the directory (a temporary file was left behind?): %v", len(ents), err)
	}
}
