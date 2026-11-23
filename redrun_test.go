package main

import (
	"strings"
	"testing"
	"time"
)

// now is the instant every case below is measured against, so a test never
// drifts with the calendar.
var now = time.Date(2026, 11, 18, 22, 0, 0, 0, time.UTC)

func ago(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }

// TestTheRunThatWentUnaskedAboutForTenDays is the case this function exists
// for: go-ansible/playbook#197's macos-latest lane, created 2026-10-12 and
// reported as "blocked" twice without anyone asking whether it could be
// re-run at all.
func TestTheRunThatWentUnaskedAboutForTenDays(t *testing.T) {
	runs := []wfrun{{
		Name:       "ci",
		Event:      "pull_request",
		Number:     412,
		Conclusion: "failure",
		Created:    time.Date(2026, 10, 12, 15, 52, 0, 0, time.UTC).Format(time.RFC3339),
	}}
	lines := redRunAdvice(runs, now)
	if len(lines) != 1 {
		t.Fatalf("got %d line(s), want 1: %v", len(lines), lines)
	}
	for _, want := range []string{
		"ci (pull_request run #412)",
		"2026-10-12",
		"403",
		"30 days",
		"cherry-pick",
		"ref you control",
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the advice does not mention %q:\n%s", want, lines[0])
		}
	}
	// 37 days on 2026-11-18, which is past the window but inside the
	// 90-day log retention — so it must NOT claim the logs are gone.
	if strings.Contains(lines[0], "retention") {
		t.Errorf("claimed the logs are past retention at 37 days:\n%s", lines[0])
	}
}

func TestSaysNothingWhileTheRunCanStillBeRerun(t *testing.T) {
	for _, age := range []time.Duration{
		0,
		time.Hour,
		29 * 24 * time.Hour,
		rerunWindow - time.Minute,
	} {
		runs := []wfrun{{Name: "ci", Event: "pull_request", Conclusion: "failure", Created: ago(age)}}
		if lines := redRunAdvice(runs, now); len(lines) != 0 {
			t.Errorf("age %s: advised %v, but `gh run rerun` still works here", age, lines)
		}
	}
}

func TestNamesTheLogRetentionOnlyOncePast(t *testing.T) {
	runs := []wfrun{{Name: "ci", Event: "pull_request", Conclusion: "failure", Created: ago(100 * 24 * time.Hour)}}
	lines := redRunAdvice(runs, now)
	if len(lines) != 1 {
		t.Fatalf("got %d line(s), want 1", len(lines))
	}
	if !strings.Contains(lines[0], "90-day default retention") {
		t.Errorf("a 100-day-old run's advice does not mention retention:\n%s", lines[0])
	}
}

// TestOnlyFailingRunsAreAdvisedAbout keeps this from lecturing about runs
// that are not the problem — an old GREEN run needs no re-run.
func TestOnlyFailingRunsAreAdvisedAbout(t *testing.T) {
	old := ago(200 * 24 * time.Hour)
	for _, conclusion := range []string{"success", "SUCCESS", "skipped", "neutral", "cancelled", ""} {
		runs := []wfrun{{Name: "ci", Event: "push", Conclusion: conclusion, Created: old}}
		if lines := redRunAdvice(runs, now); len(lines) != 0 {
			t.Errorf("conclusion %q: advised %v", conclusion, lines)
		}
	}
	// And the failing one IS advised about, so the filter above is not
	// simply refusing everything.
	runs := []wfrun{{Name: "ci", Event: "push", Conclusion: "FAILURE", Created: old}}
	if lines := redRunAdvice(runs, now); len(lines) != 1 {
		t.Errorf("a failing run of the same age got %d line(s), want 1", len(lines))
	}
}

func TestAnUnparseableTimestampGetsNoOpinion(t *testing.T) {
	for _, created := range []string{"", "yesterday", "2026-10-12", "not a date"} {
		runs := []wfrun{{Name: "ci", Event: "pull_request", Conclusion: "failure", Created: created}}
		if lines := redRunAdvice(runs, now); len(lines) != 0 {
			t.Errorf("created %q: invented an age and advised %v", created, lines)
		}
	}
}

func TestEachFailingRunGetsItsOwnLine(t *testing.T) {
	old := ago(40 * 24 * time.Hour)
	runs := []wfrun{
		{Name: "ci", Event: "pull_request", Number: 1, Conclusion: "failure", Created: old},
		{Name: "pages", Event: "push", Number: 2, Conclusion: "failure", Created: old},
		{Name: "ci", Event: "push", Number: 3, Conclusion: "success", Created: old},
	}
	lines := redRunAdvice(runs, now)
	if len(lines) != 2 {
		t.Fatalf("got %d line(s), want 2: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "pull_request run #1") || !strings.Contains(lines[1], "push run #2") {
		t.Errorf("the lines do not name their own runs:\n%s\n%s", lines[0], lines[1])
	}
}

func TestAnUnnamedRunIsStillReported(t *testing.T) {
	runs := []wfrun{{Name: "", Event: "pull_request", Conclusion: "failure", Created: ago(40 * 24 * time.Hour)}}
	lines := redRunAdvice(runs, now)
	if len(lines) != 1 {
		t.Fatalf("got %d line(s), want 1", len(lines))
	}
	if !strings.Contains(lines[0], "(unnamed)") {
		t.Errorf("an unnamed run is reported without a placeholder:\n%s", lines[0])
	}
}

func TestNoRunsAtAllIsSilent(t *testing.T) {
	if lines := redRunAdvice(nil, now); len(lines) != 0 {
		t.Errorf("advised %v with no runs", lines)
	}
}

// TestTheWaitItselfPrintsTheAdvice is the witness that matters: the unit
// tests above prove the function is right, not that prwait SAYS it. The
// mistake this exists to stop happened while reading prwait's output, so the
// line has to be in that output.
func TestTheWaitItselfPrintsTheAdvice(t *testing.T) {
	red := pr{
		Mergeable:  "MERGEABLE",
		State:      "OPEN",
		HeadRefOid: "c5dd86dcfaeb4108aed533b00abe29d106fbab53",
		Rollup: []entry{
			{Typename: "CheckRun", Name: "test (ubuntu-latest)", Status: "COMPLETED", Conclusion: "SUCCESS"},
		},
		Runs: []wfrun{{
			Name:       "ci",
			Status:     "completed",
			Conclusion: "failure",
			Event:      "pull_request",
			Number:     412,
			Created:    time.Date(2026, 10, 12, 15, 52, 0, 0, time.UTC).Format(time.RFC3339),
		}},
	}
	w := testWaiter(t, func(p pr) (pr, error) {
		red.Repo, red.Number = p.Repo, p.Number
		return red, nil
	})
	out := &strings.Builder{}
	w.out = out
	// A clock that moves a second per reading, so a wait that never settles
	// reaches its timeout instead of spinning on a frozen one.
	clock := now
	w.now = func() time.Time { clock = clock.Add(time.Second); return clock }

	if w.wait([]pr{{Repo: "go-ansible/playbook", Number: 197}}) {
		t.Fatal("a failing run was reported as a pass")
	}
	got := out.String()
	if !strings.Contains(got, "FAIL") {
		t.Fatalf("no FAIL verdict in the output:\n%s", got)
	}
	// The advice must travel WITH the verdict, in the same output somebody
	// reads before deciding they are blocked.
	for _, want := range []string{
		"ci (pull_request run #412)",
		"403",
		"cannot be refreshed where it stands",
		"cherry-pick",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the FAIL output does not carry %q:\n%s", want, got)
		}
	}
}

// TestAFreshRedPrintsNoAdvice keeps the output quiet in the ordinary case:
// a red you can simply retrigger needs no paragraph about cherry-picking.
func TestAFreshRedPrintsNoAdvice(t *testing.T) {
	red := pr{
		Mergeable:  "MERGEABLE",
		State:      "OPEN",
		HeadRefOid: "c5dd86dcfaeb4108aed533b00abe29d106fbab53",
		Rollup:     []entry{{Typename: "CheckRun", Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"}},
		Runs: []wfrun{{
			Name: "ci", Status: "completed", Conclusion: "failure",
			Event: "pull_request", Number: 9, Created: ago(2 * time.Hour),
		}},
	}
	w := testWaiter(t, func(p pr) (pr, error) {
		red.Repo, red.Number = p.Repo, p.Number
		return red, nil
	})
	out := &strings.Builder{}
	w.out = out
	// A clock that moves a second per reading, so a wait that never settles
	// reaches its timeout instead of spinning on a frozen one.
	clock := now
	w.now = func() time.Time { clock = clock.Add(time.Second); return clock }

	if w.wait([]pr{{Repo: "a/b", Number: 1}}) {
		t.Fatal("a failing run was reported as a pass")
	}
	got := out.String()
	if !strings.Contains(got, "FAIL") {
		t.Fatalf("no FAIL verdict:\n%s", got)
	}
	if strings.Contains(got, "cherry-pick") {
		t.Errorf("advised cherry-picking for a two-hour-old red that `gh run rerun` would retrigger:\n%s", got)
	}
}
