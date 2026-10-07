package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// THE DEFECT THIS TOOL EXISTS FOR, stated as a test.
//
// A conflicted pull request gets no merge ref, so no workflow runs for it
// and its check list stays empty — and an empty list has nothing pending.
// The waiter this replaces asked "is anything pending?", got "no", and
// announced success on four pull requests that had never been built.
func TestAnEmptyCheckListIsNeverAPass(t *testing.T) {
	v, why := assess(pr{Mergeable: "MERGEABLE"}, 1)
	if v != waiting {
		t.Fatalf("an empty rollup gave %v (%s), want waiting", v, why)
	}
}

// And the same list is empty while a workflow that fails to PARSE reports
// nothing at all — indistinguishable, from here, from one not yet started.
// Both must keep the waiter waiting rather than let it conclude.
func TestMinChecksHoldsOutForTheWholeSuite(t *testing.T) {
	p := pr{Mergeable: "MERGEABLE", Rollup: []entry{
		{Name: "actionlint", Status: "COMPLETED", Conclusion: "SUCCESS"},
	}}
	if v, _ := assess(p, 1); v != passed {
		t.Errorf("one green check with -min-checks 1 = %v, want passed", v)
	}
	if v, why := assess(p, 9); v != waiting {
		t.Errorf("one green check with -min-checks 9 = %v (%s), want waiting", v, why)
	}
}

// A CONFLICTING pull request keeps the ticks it earned on the OLD base, so
// it can look complete and entirely green while nothing has been built
// against what would actually land. Conflict is read BEFORE the checks, and
// it is a failure, not something to wait out.
func TestAConflictedPullRequestIsNotAPassEvenWhenGreen(t *testing.T) {
	p := pr{Mergeable: "CONFLICTING", Rollup: []entry{
		{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"},
		{Name: "vet", Status: "COMPLETED", Conclusion: "SUCCESS"},
	}}
	v, why := assess(p, 1)
	if v != failed {
		t.Fatalf("conflicting + all green = %v, want failed", v)
	}
	if !strings.Contains(why, "rebase") {
		t.Errorf("the reason does not say what to do: %q", why)
	}
}

// A StatusContext — the old commit status API, still used by third-party
// services — carries NEITHER status NOR conclusion; it has state. A waiter
// testing `status != "COMPLETED"` finds it pending forever and waits out its
// timeout on a pull request that is finished and green.
func TestAStatusContextIsNotPendingForever(t *testing.T) {
	p := pr{Mergeable: "MERGEABLE", Rollup: []entry{
		{Typename: "StatusContext", Context: "ci/external", State: "SUCCESS"},
		{Typename: "CheckRun", Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"},
	}}
	v, why := assess(p, 1)
	if v != passed {
		t.Fatalf("a finished StatusContext gave %v (%s), want passed", v, why)
	}
	if !strings.Contains(why, "SUCCESS:2") {
		t.Errorf("the tally lost the context: %q", why)
	}
}

func TestAPendingStatusContextStillWaits(t *testing.T) {
	p := pr{Mergeable: "MERGEABLE", Rollup: []entry{
		{Typename: "StatusContext", Context: "ci/external", State: "PENDING"},
	}}
	if v, _ := assess(p, 1); v != waiting {
		t.Errorf("a PENDING context = %v, want waiting", v)
	}
}

// CANCELLED IS NOT A PASS. In this fleet a lane is cancelled by concurrency
// starvation, having executed zero steps — exactly the case where a green
// summary would be a lie. NEUTRAL and SKIPPED are passes: a lane that
// excluded itself did its job.
func TestWhichConclusionsCount(t *testing.T) {
	for _, c := range []string{"SUCCESS", "NEUTRAL", "SKIPPED"} {
		p := pr{Mergeable: "MERGEABLE", Rollup: []entry{{Name: "x", Status: "COMPLETED", Conclusion: c}}}
		if v, why := assess(p, 1); v != passed {
			t.Errorf("%s = %v (%s), want passed", c, v, why)
		}
	}
	for _, c := range []string{"FAILURE", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE", "STALE", "ERROR", ""} {
		p := pr{Mergeable: "MERGEABLE", Rollup: []entry{{Name: "x", Status: "COMPLETED", Conclusion: c}}}
		if v, _ := assess(p, 1); v != failed {
			t.Errorf("%q = %v, want failed", c, v)
		}
	}
}

// A red check is named, because "one check failed" on a 25-lane matrix
// sends the reader back to the browser.
func TestAFailureNamesTheLane(t *testing.T) {
	p := pr{Mergeable: "MERGEABLE", Rollup: []entry{
		{Name: "test (ubuntu-latest)", Status: "COMPLETED", Conclusion: "SUCCESS"},
		{Name: "test (windows-latest)", Status: "COMPLETED", Conclusion: "FAILURE"},
	}}
	v, why := assess(p, 1)
	if v != failed {
		t.Fatalf("got %v, want failed", v)
	}
	if !strings.Contains(why, "test (windows-latest)=FAILURE") {
		t.Errorf("the failing lane is not named: %q", why)
	}
}

// Mergeability is computed lazily, and is UNKNOWN for seconds after a push.
// Green checks plus UNKNOWN is not yet an answer about whether it can land.
func TestUnknownMergeabilityKeepsWaiting(t *testing.T) {
	p := pr{Mergeable: "UNKNOWN", Rollup: []entry{
		{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"},
	}}
	if v, why := assess(p, 1); v != waiting {
		t.Errorf("UNKNOWN mergeability = %v (%s), want waiting", v, why)
	}
}

// An already merged pull request is not something to wait for, and a closed
// one will never go green. Neither should burn a timeout.
func TestAClosedOrMergedPullRequestSettlesAtOnce(t *testing.T) {
	if v, _ := assess(pr{State: "MERGED"}, 1); v != passed {
		t.Errorf("MERGED = %v, want passed", v)
	}
	if v, _ := assess(pr{State: "CLOSED"}, 1); v != failed {
		t.Errorf("CLOSED = %v, want failed", v)
	}
}

func TestParseRef(t *testing.T) {
	for _, c := range []struct {
		arg, def, repo string
		num            int
		bad            bool
	}{
		{arg: "82", def: "go-pkgx/pkgx", repo: "go-pkgx/pkgx", num: 82},
		{arg: "go-pkgx/bk#311", def: "", repo: "go-pkgx/bk", num: 311},
		{arg: "go-pkgx/bk#311", def: "other/other", repo: "go-pkgx/bk", num: 311},
		{arg: "82", def: "", bad: true},
		{arg: "notanumber", def: "a/b", bad: true},
		{arg: "0", def: "a/b", bad: true},
		{arg: "-3", def: "a/b", bad: true},
		{arg: "a/b/c#1", def: "", bad: true},
		{arg: "b#1", def: "", bad: true},
	} {
		repo, num, err := parseRef(c.arg, c.def)
		if c.bad {
			if err == nil {
				t.Errorf("parseRef(%q, %q) = %s#%d, want an error", c.arg, c.def, repo, num)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseRef(%q, %q): %v", c.arg, c.def, err)
			continue
		}
		if repo != c.repo || num != c.num {
			t.Errorf("parseRef(%q, %q) = %s#%d, want %s#%d", c.arg, c.def, repo, num, c.repo, c.num)
		}
	}
}

// The loop itself: it must keep asking while a pull request is unsettled,
// and stop the moment it is.
func TestTheWaiterPollsUntilItSettles(t *testing.T) {
	calls := 0
	w := testWaiter(func(repo string, num int) (pr, error) {
		calls++
		if calls < 3 {
			return pr{Mergeable: "MERGEABLE", Rollup: []entry{{Name: "test", Status: "IN_PROGRESS"}}}, nil
		}
		return pr{Mergeable: "MERGEABLE", Rollup: []entry{{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"}}}, nil
	})
	out := &strings.Builder{}
	w.out = out
	if !w.wait([]pr{{Repo: "a/b", Number: 1}}) {
		t.Fatalf("wait said no: %s", out)
	}
	if calls != 3 {
		t.Errorf("asked %d times, want 3", calls)
	}
	if !strings.Contains(out.String(), "a/b#1 PASS") {
		t.Errorf("output = %q", out)
	}
}

// A pull request that never settles must end as a FAILURE, not as a pass
// and not as a hang: the timeout is an answer about the pull request.
func TestATimeoutIsAFailure(t *testing.T) {
	now := time.Unix(0, 0)
	w := testWaiter(func(string, int) (pr, error) {
		return pr{Mergeable: "MERGEABLE", Rollup: []entry{{Name: "test", Status: "QUEUED"}}}, nil
	})
	w.now = func() time.Time { return now }
	w.sleep = func(d time.Duration) { now = now.Add(d) }
	w.interval = time.Minute
	w.timeout = 3 * time.Minute
	out := &strings.Builder{}
	w.out = out
	if w.wait([]pr{{Repo: "a/b", Number: 1}}) {
		t.Fatal("a pull request that never finished was reported as a pass")
	}
	if !strings.Contains(out.String(), "TIMEOUT") {
		t.Errorf("output = %q", out)
	}
}

// One red pull request among several must sink the whole run, and the
// others must still be reported rather than abandoned.
func TestOneFailureSinksTheRunAndTheRestAreStillReported(t *testing.T) {
	w := testWaiter(func(repo string, num int) (pr, error) {
		c := "SUCCESS"
		if num == 2 {
			c = "FAILURE"
		}
		return pr{Mergeable: "MERGEABLE", Rollup: []entry{{Name: "test", Status: "COMPLETED", Conclusion: c}}}, nil
	})
	out := &strings.Builder{}
	w.out = out
	if w.wait([]pr{{Repo: "a/b", Number: 1}, {Repo: "a/b", Number: 2}, {Repo: "a/b", Number: 3}}) {
		t.Fatal("a run containing a red pull request was reported as a pass")
	}
	for _, want := range []string{"a/b#1 PASS", "a/b#2 FAIL", "a/b#3 PASS"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("%q missing from %q", want, out)
		}
	}
}

// AN ERROR FROM gh IS NOT A PASS EITHER — not a wait, and not silence.
func TestAFetchErrorIsReportedAndFails(t *testing.T) {
	w := testWaiter(func(string, int) (pr, error) { return pr{}, errors.New("gh: not authenticated") })
	out := &strings.Builder{}
	w.out = out
	if w.wait([]pr{{Repo: "a/b", Number: 1}}) {
		t.Fatal("a failed query was reported as a pass")
	}
	if !strings.Contains(out.String(), "not authenticated") {
		t.Errorf("the reason was swallowed: %q", out)
	}
}

func testWaiter(f fetcher) waiter {
	return waiter{
		fetch:     f,
		interval:  0,
		timeout:   time.Hour,
		minChecks: 1,
		out:       os.Stdout,
		sleep:     func(time.Duration) {},
		now:       time.Now,
	}
}

func TestDecodeReadsGhsShape(t *testing.T) {
	// Trimmed from a real `gh pr view --json mergeable,state,statusCheckRollup`.
	const body = `{"mergeable":"MERGEABLE","state":"OPEN","statusCheckRollup":[
	  {"__typename":"CheckRun","name":"build + vet + test (ubuntu-latest)","status":"COMPLETED","conclusion":"SUCCESS","workflowName":"ci"},
	  {"__typename":"StatusContext","context":"license/cla","state":"SUCCESS"}]}`
	p, err := decode([]byte(body), "a/b", 7)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Repo != "a/b" || p.Number != 7 {
		t.Errorf("the reference was lost: %s", p)
	}
	if v, why := assess(p, 2); v != passed {
		t.Errorf("a real answer gave %v (%s)", v, why)
	}
}

func TestDecodeRefusesNonsense(t *testing.T) {
	if _, err := decode([]byte("not json"), "a/b", 7); err == nil {
		t.Fatal("garbage decoded without complaint")
	}
}

// THE EXEC PATH, end to end, without a network and without gh: the helper
// process stands in for the binary. This is what catches a wrong argument
// list, which no amount of testing assess() can see.
//
// It is a HELPER PROCESS rather than a shell script because a script cannot
// be executed on Windows, and this tool is expected to run on every lane.
func TestGhFetcherRunsTheBinaryAndReadsItsOutput(t *testing.T) {
	f := ghFetcher(os.Args[0])
	t.Setenv("PRWAIT_HELPER", "1")
	p, err := f("go-pkgx/pkgx", 82)
	if err != nil {
		t.Fatalf("ghFetcher: %v", err)
	}
	if p.Mergeable != "MERGEABLE" || len(p.Rollup) != 1 {
		t.Fatalf("got %+v", p)
	}
	// The helper echoes its own arguments into the check name, which is how
	// a wrong flag or a lost repository shows up here.
	want := "pr view 82 --repo go-pkgx/pkgx --json mergeable,state,statusCheckRollup"
	if got := p.Rollup[0].Name; got != want {
		t.Errorf("gh was called as\n  %s\nwant\n  %s", got, want)
	}
}

func TestGhFetcherReportsAFailingBinary(t *testing.T) {
	f := ghFetcher(os.Args[0])
	t.Setenv("PRWAIT_HELPER", "fail")
	_, err := f("a/b", 1)
	if err == nil {
		t.Fatal("a gh that exited non-zero was not reported")
	}
	// gh's own words, not just an exit status: "no pull requests found" and
	// "not authenticated" need different actions from the reader.
	if !strings.Contains(err.Error(), "could not resolve to a PullRequest") {
		t.Errorf("gh's message was lost: %v", err)
	}
}

func TestMain(m *testing.M) {
	switch os.Getenv("PRWAIT_HELPER") {
	case "":
		os.Exit(m.Run())
	case "fail":
		fmt.Fprintln(os.Stderr, "gh: could not resolve to a PullRequest")
		os.Exit(1)
	default:
		args := strings.Join(os.Args[1:], " ")
		body, err := jsonName(args)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(body)
		os.Exit(0)
	}
}

// jsonName renders an answer whose single check is NAMED after the
// arguments the helper was given.
func jsonName(args string) (string, error) {
	type e struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
	type doc struct {
		Mergeable string `json:"mergeable"`
		State     string `json:"state"`
		Rollup    []e    `json:"statusCheckRollup"`
	}
	b, err := json.Marshal(doc{
		Mergeable: "MERGEABLE",
		State:     "OPEN",
		Rollup:    []e{{Name: args, Status: "COMPLETED", Conclusion: "SUCCESS"}},
	})
	return string(b), err
}

// A THRESHOLD MEANT FOR ANOTHER REPOSITORY, which is what one -min-checks
// across a whole run produces. Observed: four Go repositories with 20+
// lanes each and one docs repository with 2, under -min-checks 20. The docs
// pull request was finished and green, and the waiter sat on it to the
// timeout repeating "2 checks, want at least 20" without ever saying those
// 2 were DONE.
//
// It must still WAIT — a threshold that gave up would be a threshold that
// passes an incomplete list, which is the defect this tool exists for. What
// changes is the reason: "more are coming" and "this is all there will ever
// be" are different situations and only the caller can act on the second.
func TestAThresholdMeantForAnotherRepositoryIsSaidOutLoud(t *testing.T) {
	complete := pr{Mergeable: "MERGEABLE", Rollup: []entry{
		{Name: "docs", Status: "COMPLETED", Conclusion: "SUCCESS"},
		{Name: "links", Status: "COMPLETED", Conclusion: "SKIPPED"},
	}}
	v, why := assess(complete, 20)
	if v != waiting {
		t.Fatalf("a short-but-complete list gave %v, want waiting — the threshold must hold", v)
	}
	if !strings.Contains(why, "all finished") || !strings.Contains(why, "-min-checks") {
		t.Errorf("the reason does not distinguish a finished list from a growing one: %q", why)
	}

	// AND THE ORDINARY CASE KEEPS THE ORDINARY REASON: with something still
	// running, more really are coming and the hint would be wrong.
	growing := pr{Mergeable: "MERGEABLE", Rollup: []entry{
		{Name: "docs", Status: "COMPLETED", Conclusion: "SUCCESS"},
		{Name: "build", Status: "IN_PROGRESS"},
	}}
	if _, why := assess(growing, 20); strings.Contains(why, "all finished") {
		t.Errorf("a list with a running check was called finished: %q", why)
	}
}
