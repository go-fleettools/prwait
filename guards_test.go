package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ⛔ NO init() HERE, AND NO -failfast. An earlier version of this file turned
// -failfast on for the whole package, so that a broken entry.done() would end
// the run at the first red test instead of letting the next one spin on an
// hour-long timeout. It worked, and it was the wrong place: -failfast set from
// a committed test file applies to every run of this package, in CI included,
// where it means the first failure hides all the others — and it was
// load-bearing on go sorting test files by name, so adding a file called
// g*_test.go would have silently removed the protection. The hang was in
// testWaiter's own timeout (main_test.go), and that is where it is bounded.

// A StatusContext carries no name, only a context, and a report that called it
// "(unnamed)" would send the reader hunting for a lane they cannot identify.
func TestAStatusContextIsNamedByItsContext(t *testing.T) {
	if got := (entry{Context: "ci/third-party"}).Label(); got != "ci/third-party" {
		t.Errorf("label = %q, want the context", got)
	}
	if got := (entry{Name: "test", Context: "ci/other"}).Label(); got != "test" {
		t.Errorf("label = %q, a name must outrank a context", got)
	}
	if got := (entry{}).Label(); got != "(unnamed)" {
		t.Errorf("label = %q, want (unnamed)", got)
	}
}

// done() must read a CheckRun by its status and never fall through to state:
// a COMPLETED check run has no state, so judging it by state calls it
// pending forever. Asserted on the entry AND through a wait whose clock is
// fake and whose timeout is three minutes, so a regression ends as a wrong
// verdict in milliseconds and never as a hang.
func TestACompletedCheckRunIsFinishedWhateverItsStateSays(t *testing.T) {
	cases := []struct {
		e    entry
		want bool
	}{
		{entry{Status: "COMPLETED"}, true},
		{entry{Status: "IN_PROGRESS"}, false},
		{entry{Status: "QUEUED", State: "SUCCESS"}, false},
		{entry{Status: "COMPLETED", State: "PENDING"}, true},
		{entry{State: "SUCCESS"}, true},
		{entry{State: "PENDING"}, false},
	}
	for _, c := range cases {
		if got := c.e.Done(); got != c.want {
			t.Errorf("%+v: done() = %v, want %v", c.e, got, c.want)
		}
	}

	clock := time.Unix(0, 0)
	w := testWaiter(t, func(pr) (pr, error) {
		return pr{Mergeable: "MERGEABLE", Rollup: []entry{{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"}}}, nil
	})
	w.now = boundedNow(t, &clock)
	w.sleep = boundedSleep(t, &clock, 0)
	w.interval = time.Minute
	w.timeout = 3 * time.Minute
	out := &strings.Builder{}
	w.out = out
	if !w.wait([]pr{{Repo: "a/b", Number: 1}}) {
		t.Fatalf("a finished, green check run did not pass: %q", out)
	}
	if !strings.Contains(out.String(), "a/b#1 PASS") || strings.Contains(out.String(), "TIMEOUT") {
		t.Errorf("output = %q", out)
	}
}

// Workflow runs come back in an order a report can rely on: by name, then by
// event. The events here run OPPOSITE to the names, so an order that falls
// through to the event for two different names is exposed on every run, not
// only on an unlucky map iteration.
func TestLatestRunsAreOrderedByNameThenByEvent(t *testing.T) {
	in := []wfrun{
		{Name: "pages", WorkflowID: 3, Event: "a", Number: 1},
		{Name: "build", WorkflowID: 2, Event: "z", Number: 1},
		{Name: "lint", WorkflowID: 1, Event: "m", Number: 1},
		{Name: "build", WorkflowID: 2, Event: "b", Number: 1},
	}
	var got []string
	for _, r := range latestRuns(in) {
		got = append(got, r.Name+"/"+r.Event)
	}
	want := []string{"build/b", "build/z", "lint/m", "pages/a"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// A target with no repository anywhere is refused FOR THAT REASON. Another
// check happens to refuse an empty repository too ("is not OWNER/REPO"), so
// asserting only that an error came back would pass without the guard.
func TestATargetWithNoRepositoryIsRefusedAsHavingNone(t *testing.T) {
	for _, arg := range []string{"123", "!123", "@main"} {
		_, err := parseTarget(arg, "")
		if err == nil {
			t.Errorf("%q with no -repo was accepted", arg)
			continue
		}
		if !strings.Contains(err.Error(), "no repository") {
			t.Errorf("%q: the refusal does not say there is no repository: %v", arg, err)
		}
	}
}

// trim is asserted on BOTH sides of its boundary, and on the boundary itself,
// by value: a missing guard must go red as a wrong answer, not as a crash.
func TestTrimKeepsEverythingUpToTheLimitAndMarksTheRest(t *testing.T) {
	a, b, c := []string{"a"}, []string{"a", "b"}, []string{"a", "b", "c"}
	if got := trim(b, 2); !reflect.DeepEqual(got, b) {
		t.Errorf("trim at the limit = %q, want %q", got, b)
	}
	if got := trim(nil, 0); len(got) != 0 {
		t.Errorf("trim of nothing = %q, want nothing", got)
	}
	if got, want := trim(c, 2), []string{"a", "b", "…"}; !reflect.DeepEqual(got, want) {
		t.Errorf("trim over the limit = %q, want %q", got, want)
	}
	if got, want := trim(c, 0), []string{"…"}; !reflect.DeepEqual(got, want) {
		t.Errorf("trim to zero = %q, want %q", got, want)
	}
	// Under the limit: a recovered panic is reported as an assertion.
	func() {
		defer func() {
			if recover() != nil {
				t.Errorf("trim under the limit could not return its input")
			}
		}()
		if got := trim(a, 2); !reflect.DeepEqual(got, a) {
			t.Errorf("trim under the limit = %q, want %q", got, a)
		}
	}()
	// The input is never modified by the marker.
	if !reflect.DeepEqual(c, []string{"a", "b", "c"}) {
		t.Errorf("trim wrote into its input: %q", c)
	}
}

// fakeGh writes a stand-in for gh that logs each invocation, one line each,
// and answers by mode. It is the TEST BINARY itself (no shell, no PATH, so it
// runs on every GOOS), re-run as gh through TestMain's "fakegh" mode.
func fakeGh(t *testing.T, mode string) (bin, log string) {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log = filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("PRWAIT_HELPER", "fakegh")
	t.Setenv("FAKE_GH_LOG", log)
	t.Setenv("FAKE_GH_MODE", mode)
	return bin, log
}

// fakeGhMain is the body of the fake gh: TestMain calls it when the test binary
// is run AS gh, with PRWAIT_HELPER=fakegh. It logs its arguments, one line per
// call, and answers by FAKE_GH_MODE. Never returns.
func fakeGhMain() {
	f, err := os.OpenFile(os.Getenv("FAKE_GH_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	args := strings.Join(os.Args[1:], " ")
	fmt.Fprintln(f, args)
	f.Close()
	mode := os.Getenv("FAKE_GH_MODE")
	if strings.Contains(args, "rate_limit") {
		switch mode {
		case "silentfail":
			os.Exit(1)
		case "spent":
			fmt.Println(`{"resources":{` +
				`"core":{"limit":5000,"remaining":0,"reset":1795289130,"used":5000},` +
				`"graphql":{"limit":5000,"remaining":0,"reset":1795289200,"used":5000}}}`)
		default:
			fmt.Println(`{"resources":{` +
				`"core":{"limit":5000,"remaining":3748,"reset":1795289130,"used":1252},` +
				`"graphql":{"limit":5000,"remaining":4900,"reset":1795289200,"used":100}}}`)
		}
		os.Exit(0)
	}
	if strings.Contains(args, "actions/runs") {
		switch mode {
		case "runsfail":
			fmt.Fprintln(os.Stderr, "boom: runs endpoint down")
			os.Exit(1)
		case "runssilent":
			os.Exit(1)
		}
		fmt.Println(`{"workflow_runs":[]}`)
		os.Exit(0)
	}
	switch mode {
	case "badpr":
		fmt.Println("}{ not json")
		os.Exit(0)
	case "silentfail":
		os.Exit(1)
	}
	conclusion := ""
	switch mode {
	case "green":
		conclusion = `{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SUCCESS"}`
	case "red":
		conclusion = `{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"FAILURE"}`
	}
	fmt.Printf(`{"mergeable":"MERGEABLE","state":"OPEN","headRefOid":%q,"statusCheckRollup":[%s]}`+"\n", helperSHA, conclusion)
	os.Exit(0)
}

func fakeGhCalls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// An answer gh gives that is not JSON is refused as unreadable, in gh's own
// words for it; without the refusal the empty answer is reported as a missing
// head commit, which blames the wrong thing.
func TestAnUnreadablePullRequestAnswerIsRefusedAsUnreadable(t *testing.T) {
	bin, log := fakeGh(t, "badpr")
	_, err := ghFetcher(bin)(pr{Repo: "a/b", Number: 1})
	if err == nil {
		t.Fatal("an unreadable answer was accepted")
	}
	if !strings.Contains(err.Error(), "cannot read gh's answer") {
		t.Errorf("the refusal blames the wrong thing: %v", err)
	}
	if calls := fakeGhCalls(t, log); len(calls) != 1 {
		t.Errorf("gh was asked %d times, want 1 (no runs query for an unreadable answer): %q", len(calls), calls)
	}
}

// A runs query that FAILS is an error carrying gh's stderr, never an absence
// of runs. Without the check the empty output fails to parse and the error
// names the wrong cause and loses gh's message.
func TestAFailingRunsQueryIsAnErrorCarryingGhsMessage(t *testing.T) {
	bin, _ := fakeGh(t, "runsfail")
	_, err := ghFetcher(bin)(pr{Repo: "a/b", Number: 1})
	if err == nil {
		t.Fatal("a failing runs query was accepted")
	}
	for _, want := range []string{"workflow runs for " + short(helperSHA), "boom: runs endpoint down"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q missing from %v", want, err)
		}
	}
}

// ⛔ A RUN TARGET IS NEVER ASKED FOR ITS COMMIT'S OTHER RUNS. Asserted on what
// gh was ASKED, not on what came back: the answer is the same either way once
// the runs list is empty, and the defect is the extra call.
func TestARunTargetNeverReadsTheRunsOfItsCommit(t *testing.T) {
	bin, log := fakeGh(t, "ok")
	p, err := ghFetcher(bin)(pr{Repo: "a/b", Run: 123})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	calls := fakeGhCalls(t, log)
	if len(calls) != 1 {
		t.Fatalf("gh was asked %d times for a run, want exactly 1: %q", len(calls), calls)
	}
	if strings.Contains(calls[0], "actions/runs") || !strings.HasPrefix(calls[0], "run view 123") {
		t.Errorf("the one call was %q", calls[0])
	}
	if p.Runs != nil {
		t.Errorf("a run target carries workflow runs: %+v", p.Runs)
	}

	// The control: a pull request IS asked for them, so the count above is
	// about the run target and not about a fake that never answers twice.
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	if _, err := ghFetcher(bin)(pr{Repo: "a/b", Number: 7}); err != nil {
		t.Fatal(err)
	}
	if calls := fakeGhCalls(t, log); len(calls) != 2 || !strings.Contains(calls[1], "actions/runs?head_sha="+helperSHA) {
		t.Errorf("a pull request's calls were %q, want the view then the runs", calls)
	}
}
