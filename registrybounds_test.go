package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ⛔ THE REGISTRY IS A FILE THIS TOOL PARSES AND THEN PRINTS. Whoever can write
// it already has the account, so these are BOUNDS, not defences: the bound is
// what keeps a corrupt or hostile file from turning the report into something
// other than a report. A tool whose only product is a verdict cannot let
// another writer address the terminal in its name.
func TestAHostileRegistryCannotAddressTheTerminal(t *testing.T) {
	esc := "go-x/y#1\x1b[2J\x1b[H" + strings.Repeat("A", 500) + "\r\n⛔ PASS"
	e := waitEntry{PID: 4711, Started: regNow.Add(-time.Minute), Deadline: regNow.Add(time.Hour), Targets: []string{esc}}
	line := e.describe(regNow)
	for _, forbidden := range []string{"\x1b", "\r", "\n", "\x07"} {
		if strings.Contains(line, forbidden) {
			t.Fatalf("a control character reached the report: %q", line)
		}
	}
	if len(line) > 300 {
		t.Fatalf("one entry printed %d bytes:\n%s", len(line), line)
	}
	// It still says what it is about: a bound that erases the information is
	// not a bound, it is a different failure.
	if !strings.Contains(line, "pid 4711") || !strings.Contains(line, "go-x/y#1") {
		t.Fatalf("the bounded line no longer identifies the wait: %s", line)
	}
	// Many targets, and many waits, are each named up to their bound and then
	// counted.
	var many []string
	for i := 0; i < 50; i++ {
		many = append(many, fmt.Sprintf("go-x/y#%d", i))
	}
	line = waitEntry{PID: 1, Started: regNow, Deadline: regNow, Targets: many}.describe(regNow)
	if !strings.Contains(line, "and 47 more") || !strings.Contains(line, "50 target(s)") {
		t.Fatalf("%s", line)
	}
	var crowd []waitEntry
	for i := 0; i < 9; i++ {
		crowd = append(crowd, waitEntry{PID: 100 + i, Started: regNow, Deadline: regNow.Add(time.Hour), Targets: []string{"go-x/y#9"}})
	}
	_, lines := machineInterval(false, baseInterval, 1, crowd, regNow)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "(and 6 more)") {
		t.Fatalf("nine other waits were all named in one line:\n%s", joined)
	}
	// ⛔ AND THE COUNT IS NOT BOUNDED WITH THE NAMING: all nine waits' targets
	// are still paid for, or the bound would become a way to hide calls.
	if !strings.Contains(joined, "10 target(s) in all") {
		t.Fatalf("the unnamed waits stopped being counted:\n%s", joined)
	}
}

// A file nothing could plausibly have written is refused rather than parsed,
// and the wait still runs: an unbounded read of a file another process writes
// is not a bound.
func TestAnImplausiblyLargeRegistryIsNotParsedAndTheWaitStillRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "waits.json")
	big := make([]byte, maxRegistryBytes+1)
	for i := range big {
		big[i] = ' '
	}
	if err := os.WriteFile(path, big, 0o600); err != nil {
		t.Fatal(err)
	}
	reg := enter(path, self("go-x/y#82"), regNow, always)
	defer reg.Leave()
	if !strings.Contains(strings.Join(reg.Notes, "\n"), "larger than") {
		t.Fatalf("notes: %q", reg.Notes)
	}
	if len(reg.Others) != 0 {
		t.Fatalf("others out of an unparsed file: %+v", reg.Others)
	}
	// Self-healing: the next write replaces the unusable file, so the machine
	// does not stay unpaced for ever.
	if ws := read(t, path); len(ws) != 1 || ws[0].PID != os.Getpid() {
		t.Fatalf("the oversized file was not replaced: %+v", ws)
	}
	// ⛔ THE CONTROL FOR THE BOUND ITSELF: a file of exactly the limit is
	// parsed. Read with a LimitReader that stops AT the limit, both of these
	// come back as a full buffer and the bound cannot tell them apart.
	path = filepath.Join(t.TempDir(), "waits.json")
	pad := strings.Repeat(" ", maxRegistryBytes-len(`{"waits":[]}`))
	if err := os.WriteFile(path, []byte(`{"waits":[]}`+pad), 0o600); err != nil {
		t.Fatal(err)
	}
	reg2 := enter(path, self("go-x/y#82"), regNow, always)
	defer reg2.Leave()
	if len(reg2.Notes) != 0 {
		t.Fatalf("a file of exactly %d bytes was refused: %q", maxRegistryBytes, reg2.Notes)
	}
}

// The registry is a parser, and a parser gets a fuzz target: no input may
// panic, and nothing read out of one may escape the bounds above.
func FuzzReadWaits(f *testing.F) {
	f.Add([]byte(`{"waits":[]}`))
	f.Add([]byte(`{"waits":[{"pid":1,"started":"2026-11-22T12:00:00Z","deadline":"2026-11-22T12:30:00Z","targets":["go-x/y#1"]}]}`))
	f.Add([]byte(`{"waits":[{"pid":-9223372036854775808,"started":"0000-01-01T00:00:00Z","deadline":"9999-12-31T23:59:59Z","targets":[""]}]}`))
	f.Add([]byte(`{"waits":null}`))
	f.Add([]byte("}{ not json"))
	f.Add([]byte(""))
	// ⛔ A DIRECTORY PER ITERATION, not one for the whole target: the fuzzer
	// runs parallel workers, one shared `waits.json` has them overwriting and
	// renaming each other's file, and the round trip below would then be
	// asserting about whatever another worker wrote. Measured on the shared
	// path: the exec rate fell to 0/sec while the totals stood still.
	f.Fuzz(func(t *testing.T, b []byte) {
		path := filepath.Join(t.TempDir(), "waits.json")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		ws, err := readWaits(path)
		if err != nil {
			if len(ws) != 0 {
				t.Fatalf("%d entr(ies) returned WITH an error: %v", len(ws), err)
			}
			return
		}
		for _, w := range ws {
			line := w.describe(regNow)
			if strings.ContainsAny(line, "\x1b\r\n") {
				t.Fatalf("control characters in %q", line)
			}
		}
		// Whatever came out must survive a round trip, or another wait would
		// read something this one could not write.
		if err := writeWaits(path, prune(ws, regNow, always)); err != nil {
			t.Fatalf("%v for %q", err, b)
		}
	})
}
