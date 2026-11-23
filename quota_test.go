package main

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// The messages gh actually hands over. This tool never sees a status code:
// gh writes to stderr and prwait wraps that text, so these strings are the
// whole of the evidence any classification here can work from.
const (
	primaryMsg   = "gh: a/b#1: gh: API rate limit exceeded for user ID 11405852. (HTTP 403)"
	secondaryMsg = "gh: a/b#1: You have exceeded a secondary rate limit. Please wait a few minutes before you try again. (HTTP 403)"
	abuseMsg     = "gh: a/b#1: You have triggered an abuse detection mechanism. (HTTP 403)"
	tooManyMsg   = "gh: a/b#1: HTTP 429: Too Many Requests"
	graphqlMsg   = "gh: a/b@main: API rate limit exceeded (HTTP 403)"
)

func fixedBudget(b budget) budgetReader {
	return func() (budget, error) { return b, nil }
}

func brokenBudget(err error) budgetReader {
	return func() (budget, error) { return budget{}, err }
}

// ⛔ THE CLASSIFICATION IS THE WHOLE SAFETY PROPERTY: a quota that reads as a
// red check is a false verdict, and a red check that reads as a quota would be
// worse. The first case below is the one that keeps the rest honest.
func TestWhatAFailedQueryIsClassifiedAs(t *testing.T) {
	reset := time.Unix(1795289130, 0)
	spentREST := budget{
		Core:    resource{Limit: 5000, Remaining: 0, Reset: reset},
		GraphQL: resource{Limit: 5000, Remaining: 4900, Reset: reset},
	}
	spentGraphQL := budget{
		Core:    resource{Limit: 5000, Remaining: 3748, Reset: reset},
		GraphQL: resource{Limit: 5000, Remaining: 0, Reset: reset},
	}
	roomy := budget{
		Core:    resource{Limit: 5000, Remaining: 3748, Reset: reset},
		GraphQL: resource{Limit: 5000, Remaining: 4900, Reset: reset},
	}

	cases := []struct {
		name          string
		err           error
		read          budgetReader
		wantKind      quotaKind
		wantWhich     string
		wantReset     time.Time
		wantElim      bool
		wantBudgetErr bool
	}{{
		// The case that must never widen. Everything prwait exists to say
		// about a pull request arrives through this path.
		name:     "an ordinary failure is not a quota",
		err:      errors.New("gh: a/b#1: gh: not authenticated"),
		read:     fixedBudget(roomy),
		wantKind: notQuota,
	}, {
		name:     "a conflict is not a quota either",
		err:      errors.New("gh: a/b#1: GraphQL: Could not resolve to a PullRequest"),
		read:     fixedBudget(roomy),
		wantKind: notQuota,
	}, {
		name:      "the primary REST limit, confirmed by the instrument",
		err:       errors.New(primaryMsg),
		read:      fixedBudget(spentREST),
		wantKind:  primaryQuota,
		wantWhich: "REST",
		wantReset: reset,
	}, {
		// ⛔ REST AND GRAPHQL ARE SEPARATE BUDGETS. A branch target is read
		// with `gh api graphql`; if only that budget is spent, naming REST
		// would send the reader to a number that is fine.
		name:      "the primary GraphQL limit, with REST still roomy",
		err:       errors.New(graphqlMsg),
		read:      fixedBudget(spentGraphQL),
		wantKind:  primaryQuota,
		wantWhich: "GraphQL",
		wantReset: reset,
	}, {
		// GitHub named it, so nothing is deduced — and NO reset is claimed,
		// because none exists for this limit.
		name:      "a secondary limit, named by GitHub",
		err:       errors.New(secondaryMsg),
		read:      fixedBudget(roomy),
		wantKind:  secondaryQuota,
		wantWhich: "REST",
	}, {
		name:      "the older abuse-detection wording is the secondary limit",
		err:       errors.New(abuseMsg),
		read:      fixedBudget(roomy),
		wantKind:  secondaryQuota,
		wantWhich: "REST",
	}, {
		// ⛔ THE CASE MEASURED 2026-10-10: 403 on every call while
		// /rate_limit read 5000 of 5000. The kind is deduced, and the report
		// has to admit that it was deduced.
		name:      "a rate limit the instrument cannot see is secondary by elimination",
		err:       errors.New(primaryMsg),
		read:      fixedBudget(roomy),
		wantKind:  secondaryQuota,
		wantWhich: "REST",
		wantElim:  true,
	}, {
		// GitHub documents BOTH statuses for a rate limit: "you will receive
		// a 403 or 429 response". gh renders the 429 with different words, so
		// the wording is matched, not the code.
		name:      "429 is a quota",
		err:       errors.New(tooManyMsg),
		read:      fixedBudget(spentREST),
		wantKind:  primaryQuota,
		wantWhich: "REST",
		wantReset: reset,
	}, {
		// An unreadable instrument says nothing about the quota. It must not
		// become a reset time, and it must not become a verdict.
		name:          "the instrument itself unreadable",
		err:           errors.New(primaryMsg),
		read:          brokenBudget(errors.New("gh: rate_limit: network is unreachable")),
		wantKind:      primaryQuota,
		wantBudgetErr: true,
	}, {
		// ⛔ THE SAME BRANCH, WITH GitHub HAVING NAMED THE LIMIT. Found by
		// mutsweep: forcing this `named` to false changed no test, because
		// every unreadable-instrument case used an UNNAMED message. One
		// branch of one guard, reachable in production the first time a
		// secondary limit and a network blip arrive together.
		name:          "a named secondary limit with the instrument unreadable",
		err:           errors.New(secondaryMsg),
		read:          brokenBudget(errors.New("gh: rate_limit: network is unreachable")),
		wantKind:      secondaryQuota,
		wantBudgetErr: true,
	}, {
		name:     "no instrument at all still classifies from the message",
		err:      errors.New(secondaryMsg),
		read:     nil,
		wantKind: secondaryQuota,
	}, {
		name:     "no error is no quota",
		err:      nil,
		read:     fixedBudget(spentREST),
		wantKind: notQuota,
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyQuota(c.err, c.read)
			if got.kind != c.wantKind {
				t.Fatalf("kind = %v, want %v", got.kind, c.wantKind)
			}
			if c.wantWhich != "" && got.which != c.wantWhich {
				t.Errorf("which = %q, want %q", got.which, c.wantWhich)
			}
			if !got.reset.Equal(c.wantReset) {
				t.Errorf("reset = %v, want %v", got.reset, c.wantReset)
			}
			if got.byElimination != c.wantElim {
				t.Errorf("byElimination = %v, want %v", got.byElimination, c.wantElim)
			}
			if (got.budgetErr != nil) != c.wantBudgetErr {
				t.Errorf("budgetErr = %v, want set: %v", got.budgetErr, c.wantBudgetErr)
			}
		})
	}
}

// ⛔ A SECONDARY LIMIT HAS NO RESET, AND THE OTHER BUDGET'S RESET IS NOT A
// SUBSTITUTE. Filling it in would print a promise about a limit that did not
// refuse the call — the shape of an annotation overruling an instrument.
func TestNoResetIsInventedForASecondaryLimit(t *testing.T) {
	reset := time.Unix(1795289130, 0)
	b := budget{
		Core:    resource{Limit: 5000, Remaining: 4000, Reset: reset},
		GraphQL: resource{Limit: 5000, Remaining: 4900, Reset: reset},
	}
	for _, msg := range []string{secondaryMsg, abuseMsg, primaryMsg} {
		q := classifyQuota(errors.New(msg), fixedBudget(b))
		if q.kind != secondaryQuota {
			t.Fatalf("%q classified as %v", msg, q.kind)
		}
		if !q.reset.IsZero() {
			t.Errorf("%q: a reset of %v was invented for a limit that advertises none", msg, q.reset)
		}
		lines := strings.Join(q.lines("a/b#1", reset.Add(-time.Hour)), "\n")
		if strings.Contains(lines, "resets at") {
			t.Errorf("%q: the report promises a reset:\n%s", msg, lines)
		}
	}
}

// ⚠ A reset must be rendered as something a reader can act on, and an
// implausible one must be rendered as implausible. Driven from the fake-gh
// fixture that printed "in 56864h13m47s" with a straight face.
func TestHowAResetIsRendered(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name string
		at   time.Time
		want string
	}{
		{"an ordinary reset", now.Add(38*time.Minute + 12*time.Second), "38m12s"},
		{"one already passed", now.Add(-time.Second), "already reset"},
		{"one no clock could mean", now.Add(56864 * time.Hour), "suspect clock"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := until(c.at, now); !strings.Contains(got, c.want) {
				t.Errorf("until = %q, want it to contain %q", got, c.want)
			}
		})
	}
}

func TestHowLongAQuotaIsHeldFor(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name     string
		hit      quotaHit
		deadline time.Time
		attempt  int
		want     time.Duration
		wantOK   bool
	}{{
		name:     "a primary reset inside the deadline is waited for",
		hit:      quotaHit{kind: primaryQuota, reset: now.Add(10 * time.Minute)},
		deadline: now.Add(30 * time.Minute),
		attempt:  1,
		want:     10*time.Minute + resetMargin,
		wantOK:   true,
	}, {
		// ⛔ REFUSED EARLY, NOT RETRIED LATE. Spending the rest of the
		// timeout asking a limit that resets after it is over is how four
		// calls were spent to learn one thing.
		name:     "a primary reset past the deadline is refused now",
		hit:      quotaHit{kind: primaryQuota, reset: now.Add(50 * time.Minute)},
		deadline: now.Add(30 * time.Minute),
		attempt:  1,
	}, {
		name:     "a reset already in the past still holds the margin",
		hit:      quotaHit{kind: primaryQuota, reset: now.Add(-time.Minute)},
		deadline: now.Add(30 * time.Minute),
		attempt:  1,
		want:     resetMargin,
		wantOK:   true,
	}, {
		name:     "no advertised reset is not held for blind",
		hit:      quotaHit{kind: primaryQuota},
		deadline: now.Add(30 * time.Minute),
		attempt:  1,
	}, {
		name:     "a secondary limit holds the documented minute",
		hit:      quotaHit{kind: secondaryQuota},
		deadline: now.Add(30 * time.Minute),
		attempt:  1,
		want:     time.Minute,
		wantOK:   true,
	}, {
		name:     "and doubles on the next consecutive refusal",
		hit:      quotaHit{kind: secondaryQuota},
		deadline: now.Add(30 * time.Minute),
		attempt:  3,
		want:     4 * time.Minute,
		wantOK:   true,
	}, {
		name:     "a hold that would outlast the deadline is refused",
		hit:      quotaHit{kind: secondaryQuota},
		deadline: now.Add(30 * time.Second),
		attempt:  1,
	}, {
		name:     "something that is not a quota is never held for",
		hit:      quotaHit{},
		deadline: now.Add(30 * time.Minute),
		attempt:  1,
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := c.hit.hold(now, c.deadline, c.attempt)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (d = %s)", ok, c.wantOK, got)
			}
			if ok && got != c.want {
				t.Errorf("hold = %s, want %s", got, c.want)
			}
		})
	}
}

// The doubling is bounded, and the floor is the documented one.
func TestTheSecondaryBackoffDoublesAndStops(t *testing.T) {
	want := []time.Duration{
		time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		8 * time.Minute, 8 * time.Minute,
	}
	for i, w := range want {
		if got := backoffFor(i + 1); got != w {
			t.Errorf("backoffFor(%d) = %s, want %s", i+1, got, w)
		}
	}
	if got := backoffFor(0); got != time.Minute {
		t.Errorf("backoffFor(0) = %s, want the one-minute floor", got)
	}
}

// The fixture is the answer this machine actually gave at 20:06 CET on
// 2026-11-21, so the field names are GitHub's and not a guess at them.
func TestTheBudgetIsReadFromGhsOwnAnswer(t *testing.T) {
	const body = `{"resources":{` +
		`"core":{"limit":5000,"remaining":3748,"reset":1795289130,"used":1252},` +
		`"graphql":{"limit":5000,"remaining":4900,"reset":1795289200,"used":100}}}`
	b, err := decodeBudget([]byte(body))
	if err != nil {
		t.Fatalf("decodeBudget: %v", err)
	}
	if b.Core.Remaining != 3748 || b.Core.Limit != 5000 {
		t.Errorf("core = %+v", b.Core)
	}
	if b.GraphQL.Remaining != 4900 {
		t.Errorf("graphql = %+v", b.GraphQL)
	}
	if want := time.Unix(1795289130, 0); !b.Core.Reset.Equal(want) {
		t.Errorf("core reset = %v, want %v", b.Core.Reset, want)
	}
	// ⚠ A zero reset must stay the zero TIME, not the epoch: "resets at
	// 01:00 CET 1970" is a sentence no reader can act on.
	empty, err := decodeBudget([]byte(`{"resources":{"core":{"limit":5000,"remaining":5000}}}`))
	if err != nil {
		t.Fatalf("decodeBudget: %v", err)
	}
	if !empty.Core.Reset.IsZero() {
		t.Errorf("a missing reset became %v", empty.Core.Reset)
	}
	if _, err := decodeBudget([]byte(`not json`)); err == nil {
		t.Error("an unreadable answer was accepted")
	}
}

// The exec seam, because decodeBudget being right proves nothing about the
// command that feeds it. Driven through the test binary itself, which is this
// package's established stand-in for gh.
func TestTheBudgetIsReadThroughGh(t *testing.T) {
	t.Setenv("PRWAIT_HELPER", "ratelimit")
	b, err := ghBudget(os.Args[0])()
	if err != nil {
		t.Fatalf("ghBudget: %v", err)
	}
	if b.Core.Remaining != 3748 || b.GraphQL.Remaining != 4900 {
		t.Errorf("budget = %+v", b)
	}
}

// ⛔ AND A gh THAT FAILS IS AN ERROR, NOT AN EMPTY BUDGET. Found by mutsweep:
// nothing exercised this guard, so removing it returned budget{} — every
// field zero, which classifyQuota would read as "both budgets spent" and
// report as a confirmed primary quota. An unreadable instrument inventing a
// measurement is the worst of the available outcomes.
func TestAghThatFailsIsNotAnEmptyBudget(t *testing.T) {
	t.Setenv("PRWAIT_HELPER", "quotafail")
	b, err := ghBudget(os.Args[0])()
	if err == nil {
		t.Fatalf("a gh that exited 1 was read as a budget: %+v", b)
	}
	// gh's own words, so the reader can tell a quota from a missing token.
	if !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("gh's message was lost: %v", err)
	}
	// And the classification must then admit it could not look, rather than
	// treating the zeroes as a reading.
	q := classifyQuota(errors.New(primaryMsg), ghBudget(os.Args[0]))
	if q.budgetErr == nil {
		t.Error("the unreadable instrument was not recorded")
	}
	if !q.reset.IsZero() {
		t.Errorf("a reset of %v came from a budget that could not be read", q.reset)
	}
}

// ⛔⛔ A WAIT MUST END, AND ONLY ONE CHECK MAKES IT END. mutsweep HUNG on the
// deadline test inside hold: nothing else in the loop stops a target that is
// refused over and over, so removing it runs forever. A hang is not a test
// result, so the sleep itself asserts the bound.
func TestRepeatedQuotasStillEndTheWaitWithinItsTimeout(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	now := start
	calls := 0
	w := testWaiter(t, func(pr) (pr, error) {
		calls++
		return pr{}, errors.New(secondaryMsg)
	})
	w.budget = fixedBudget(budget{
		Core:    resource{Limit: 5000, Remaining: 4000, Reset: start.Add(20 * time.Minute)},
		GraphQL: resource{Limit: 5000, Remaining: 4900, Reset: start.Add(20 * time.Minute)},
	})
	w.now = boundedNow(t, &now)
	w.timeout = 3 * time.Minute
	w.interval = 0
	w.sleep = func(d time.Duration) {
		now = now.Add(d)
		if now.Sub(start) > 2*w.timeout {
			t.Fatalf("the wait slept %s, past twice its own %s timeout", now.Sub(start), w.timeout)
		}
	}
	out := &strings.Builder{}
	w.out = out

	if got := w.run([]pr{{Repo: "a/b", Number: 1}}); got != quotaStopped {
		t.Fatalf("outcome = %v, want quotaStopped:\n%s", got, out)
	}
	// The documented minute, then two: the second hold would land exactly on
	// the 3m deadline, so it is refused instead of taken. That is the whole
	// bound, arrived at from the backoff and the timeout rather than copied
	// out of the implementation.
	if calls != 2 {
		t.Errorf("spent %d calls, want 2", calls)
	}
}

// ⛔ PREFLIGHT REFUSES ONLY WHAT IS CERTAIN. This fleet deleted a guard
// measured at 4137 refusals against two defects; an estimate that refuses a
// wait which would have worked is worse than no estimate.
func TestPreflightRefusesOnlyWhatCannotPossiblyWork(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	reset := now.Add(20 * time.Minute)
	cases := []struct {
		name       string
		read       budgetReader
		wantRefuse bool
		wantQuiet  bool
		want       string
	}{{
		name: "both budgets spent: nothing can be read, so it refuses",
		read: fixedBudget(budget{
			Core:    resource{Limit: 5000, Remaining: 0, Reset: reset},
			GraphQL: resource{Limit: 5000, Remaining: 0, Reset: reset},
		}),
		wantRefuse: true,
		want:       "refusing to start",
	}, {
		// Which budget a target spends is gh's business, not something
		// asserted here, so one zero is reported and the wait proceeds.
		name: "one budget spent: reported, not refused",
		read: fixedBudget(budget{
			Core:    resource{Limit: 5000, Remaining: 0, Reset: reset},
			GraphQL: resource{Limit: 5000, Remaining: 4900, Reset: reset},
		}),
		want: "REST budget is spent",
	}, {
		name: "room in both: nothing to say",
		read: fixedBudget(budget{
			Core:    resource{Limit: 5000, Remaining: 3748, Reset: reset},
			GraphQL: resource{Limit: 5000, Remaining: 4900, Reset: reset},
		}),
		wantQuiet: true,
	}, {
		// ⛔ "I could not look" must never become "you may not go".
		name: "an unreadable instrument is not a refusal",
		read: brokenBudget(errors.New("network is unreachable")),
		want: "could not be read",
	}, {
		name:      "no instrument at all is silent",
		read:      nil,
		wantQuiet: true,
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lines, refuse := preflight(c.read, now)
			if refuse != c.wantRefuse {
				t.Fatalf("refuse = %v, want %v (%q)", refuse, c.wantRefuse, lines)
			}
			joined := strings.Join(lines, "\n")
			if c.wantQuiet && len(lines) != 0 {
				t.Fatalf("expected silence, got %q", joined)
			}
			if c.want != "" && !strings.Contains(joined, c.want) {
				t.Errorf("%q missing from %q", c.want, joined)
			}
		})
	}
}

// ⛔⛔ THE INTEGRATION TEST, AND THE ONE THAT WOULD HAVE CAUGHT 2026-11-15.
//
// Every rule above can be right while the wait still prints a quota as a red
// check and exits 1 — a correct function beside a silent tool. Only driving
// the loop can say otherwise, so this asserts the three things a caller
// actually reads: the status, the absence of a verdict, and the number of
// calls spent.
func TestAQuotaIsNotAVerdictAndNotAFail(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	calls := 0
	w := testWaiter(t, func(pr) (pr, error) {
		calls++
		return pr{}, errors.New(primaryMsg)
	})
	// The reset falls outside the wait's own deadline, so there is nothing
	// to hold for.
	w.budget = fixedBudget(budget{
		Core:    resource{Limit: 5000, Remaining: 0, Reset: now.Add(50 * time.Minute)},
		GraphQL: resource{Limit: 5000, Remaining: 4900, Reset: now.Add(50 * time.Minute)},
	})
	w.now = boundedNow(t, &now)
	w.sleep = boundedSleep(t, &now, 0)
	w.interval = 45 * time.Second
	w.timeout = 30 * time.Minute
	out := &strings.Builder{}
	w.out = out

	got := w.run([]pr{{Repo: "a/b", Number: 1}})

	if got != quotaStopped {
		t.Errorf("outcome = %v, want quotaStopped", got)
	}
	if got.status() != quotaStatus {
		t.Errorf("exit status = %d, want %d", got.status(), quotaStatus)
	}
	if got.status() == 1 {
		t.Error("a quota exited 1, which is the status of a measured red check")
	}
	// ⛔ NO VERDICT. "FAIL" here would be a statement about a pull request
	// nothing was read about.
	if strings.Contains(out.String(), "FAIL") {
		t.Errorf("a quota printed a verdict:\n%s", out)
	}
	for _, want := range []string{"QUOTA", "not a verdict", "nothing about this target was measured", "resets at"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("%q missing from:\n%s", want, out)
		}
	}
	// ⛔ ONE CALL, NOT fetchRetries OF THEM. A quota is not weather: each
	// retry was a call spent against the limit that was already refusing,
	// and each printed "asking again".
	if calls != 1 {
		t.Errorf("spent %d calls on a quota, want 1", calls)
	}
	if strings.Contains(out.String(), "asking again") {
		t.Errorf("the quota was retried as weather:\n%s", out)
	}
}

// A quota whose reset arrives inside the deadline is WAITED for, and the wait
// then finishes normally. Holding costs nothing; giving up costs the verdict.
func TestAQuotaWhoseResetFitsIsHeldForAndTheWaitFinishes(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	calls := 0
	w := testWaiter(t, func(pr) (pr, error) {
		calls++
		if calls == 1 {
			return pr{}, errors.New(primaryMsg)
		}
		return pr{Mergeable: "MERGEABLE", Rollup: []entry{{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"}}}, nil
	})
	w.budget = fixedBudget(budget{
		Core:    resource{Limit: 5000, Remaining: 0, Reset: now.Add(2 * time.Minute)},
		GraphQL: resource{Limit: 5000, Remaining: 4900, Reset: now.Add(2 * time.Minute)},
	})
	w.now = boundedNow(t, &now)
	// Every sleep moves the clock at least a second: with interval 0 a sleep of
	// 0 leaves it frozen, and a wait that never settles then never times out.
	w.sleep = boundedSleep(t, &now, time.Second)
	w.interval = 0
	w.timeout = 30 * time.Minute
	out := &strings.Builder{}
	w.out = out

	if got := w.run([]pr{{Repo: "a/b", Number: 1}}); got != allGreen {
		t.Fatalf("outcome = %v, want allGreen:\n%s", got, out)
	}
	if !strings.Contains(out.String(), "holding 2m5s for it") {
		t.Errorf("the hold was not reported:\n%s", out)
	}
	if !strings.Contains(out.String(), "a/b#1 PASS") {
		t.Errorf("the verdict after the reset is missing:\n%s", out)
	}
	if calls != 2 {
		t.Errorf("spent %d calls, want 2", calls)
	}
}

// ⛔ A MEASURED RED OUTRANKS AN UNMEASURED QUOTA. The red is a fact; the quota
// is the absence of one. If the quota won, a genuinely failing pull request
// could exit 3 and be read as "come back later".
func TestAMeasuredRedOutranksAQuota(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	w := testWaiter(t, func(p pr) (pr, error) {
		if p.Number == 1 {
			return pr{Mergeable: "MERGEABLE", Rollup: []entry{{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"}}}, nil
		}
		return pr{}, errors.New(primaryMsg)
	})
	w.budget = fixedBudget(budget{
		Core:    resource{Limit: 5000, Remaining: 0, Reset: now.Add(50 * time.Minute)},
		GraphQL: resource{Limit: 5000, Remaining: 0, Reset: now.Add(50 * time.Minute)},
	})
	w.now = boundedNow(t, &now)
	w.sleep = boundedSleep(t, &now, 0)
	w.timeout = 30 * time.Minute
	out := &strings.Builder{}
	w.out = out

	got := w.run([]pr{{Repo: "a/b", Number: 1}, {Repo: "a/b", Number: 2}})
	if got != someRed {
		t.Errorf("outcome = %v, want someRed:\n%s", got, out)
	}
	if got.status() != 1 {
		t.Errorf("exit status = %d, want 1", got.status())
	}
	if !strings.Contains(out.String(), "a/b#1 FAIL") {
		t.Errorf("the red verdict was lost:\n%s", out)
	}
	if !strings.Contains(out.String(), "QUOTA") {
		t.Errorf("the quota was not reported:\n%s", out)
	}
}

// ⛔ THE TARGETS THAT GOT NO VERDICT ARE NAMED. Silence about a target reads
// as consent, which is the same defect in miniature: #13 was a verdict that
// did not say which surfaces it covered.
func TestTheTargetsAQuotaLeftUnjudgedAreNamed(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	w := testWaiter(t, func(pr) (pr, error) { return pr{}, errors.New(primaryMsg) })
	w.budget = fixedBudget(budget{
		Core:    resource{Limit: 5000, Remaining: 0, Reset: now.Add(50 * time.Minute)},
		GraphQL: resource{Limit: 5000, Remaining: 0, Reset: now.Add(50 * time.Minute)},
	})
	w.now = boundedNow(t, &now)
	w.sleep = boundedSleep(t, &now, 0)
	w.timeout = 30 * time.Minute
	out := &strings.Builder{}
	w.out = out

	if got := w.run([]pr{{Repo: "a/b", Number: 1}, {Repo: "a/b", Number: 2}, {Repo: "a/b", Number: 3}}); got != quotaStopped {
		t.Fatalf("outcome = %v, want quotaStopped", got)
	}
	for _, want := range []string{"3 target(s) got no verdict", "a/b#1", "a/b#2", "a/b#3"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("%q missing from:\n%s", want, out)
		}
	}
}

// The secondary limit, end to end: reported as the secondary limit, with the
// primary numbers quoted to show it is not the one the reader would guess,
// and held for the documented minute before the doubling.
func TestASecondaryLimitIsReportedAsOneAndHeldFor(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	calls := 0
	w := testWaiter(t, func(pr) (pr, error) {
		calls++
		if calls <= 2 {
			return pr{}, errors.New(secondaryMsg)
		}
		return pr{Mergeable: "MERGEABLE", Rollup: []entry{{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"}}}, nil
	})
	w.budget = fixedBudget(budget{
		Core:    resource{Limit: 5000, Remaining: 4000, Reset: now.Add(20 * time.Minute)},
		GraphQL: resource{Limit: 5000, Remaining: 4900, Reset: now.Add(20 * time.Minute)},
	})
	w.now = boundedNow(t, &now)
	// As above: a clock that always moves, so a broken wait times out.
	w.sleep = boundedSleep(t, &now, time.Second)
	w.interval = 0
	w.timeout = 30 * time.Minute
	out := &strings.Builder{}
	w.out = out

	if got := w.run([]pr{{Repo: "a/b", Number: 1}}); got != allGreen {
		t.Fatalf("outcome = %v, want allGreen:\n%s", got, out)
	}
	for _, want := range []string{
		"SECONDARY rate limit",
		"4000 of 5000 REST calls still left",
		"holding 1m0s for it",
		"holding 2m0s for it",
		"a/b#1 PASS",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("%q missing from:\n%s", want, out)
		}
	}
	// The primary reset must not be offered as this limit's reset.
	if strings.Contains(out.String(), "resets at") {
		t.Errorf("a reset was promised for the secondary limit:\n%s", out)
	}
}
