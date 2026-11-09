package main

import (
	"strings"
	"testing"
)

// THE DEFECT, STATED AS A TEST: go-tex/go-tex.github.io#239, head 769bfee,
// 2026-11-09. prwait v0.5.0 printed `PASS SUCCESS:3` and exited 0 while two
// of the four workflow runs for that SHA had conclusion=failure.
//
// It could not see them because a check run is created per JOB: `pages`
// failed having created NO job and `playground` created one of its three, so
// neither contributed anything red, anything pending, or anything to count.
// Every property the check list can express was true — not empty, none
// pending, none failed — which is why #1's rule was not enough.
func TestACompleteGreenCheckListWithARedRunIsNotAPass(t *testing.T) {
	p := pr{
		Mergeable:  "MERGEABLE",
		HeadRefOid: "769bfee014e24a9070938cbea578565b1dc68add",
		Rollup: []entry{
			{Typename: "CheckRun", Name: "bricolint", Status: "COMPLETED", Conclusion: "SUCCESS"},
			{Typename: "CheckRun", Name: "every link and image resolves", Status: "COMPLETED", Conclusion: "SUCCESS"},
			{Typename: "CheckRun", Name: "current / the landing names what the organisation has", Status: "COMPLETED", Conclusion: "SUCCESS"},
		},
		Runs: []wfrun{
			{Name: "docs", Status: "completed", Conclusion: "success"},
			{Name: "links", Status: "completed", Conclusion: "success"},
			{Name: "pages", Status: "completed", Conclusion: "failure"},
			{Name: "playground", Status: "completed", Conclusion: "failure"},
		},
	}
	v, why := assess(p, 1)
	if v != failed {
		t.Fatalf("three green checks beside two failed runs gave %v (%s), want failed", v, why)
	}
	// Both are named: "one run failed" on a repository with four workflows
	// sends the reader back to the browser to find out which.
	for _, want := range []string{"pages (workflow run)=FAILURE", "playground (workflow run)=FAILURE"} {
		if !strings.Contains(why, want) {
			t.Errorf("the reason does not name %q: %q", want, why)
		}
	}
}

// ⛔ THE MIRROR DEFECT, which the run target already carries a warning about:
// REST answers "completed"/"success" in lower case while entry.done() tests
// the GraphQL spelling COMPLETED. Taken as REST spells it, a finished run
// would never look done and every wait would run to its timeout.
func TestARestSpelledRunIsReadAsFinished(t *testing.T) {
	p := pr{
		Mergeable:  "MERGEABLE",
		HeadRefOid: "cafe000",
		Rollup:     []entry{{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"}},
		Runs:       []wfrun{{Name: "ci", Status: "completed", Conclusion: "success"}},
	}
	v, why := assess(p, 1)
	if v != passed {
		t.Fatalf("a finished, green run gave %v (%s), want passed", v, why)
	}
	if !strings.Contains(why, "SUCCESS:2") {
		t.Errorf("the tally does not count the run: %q", why)
	}
}

// A run still going keeps the wait going, exactly as a pending check does.
// This is the case the old code could not even reach: a workflow queued
// behind concurrency publishes no check run until it starts, so a wait that
// read checks alone would have concluded on whatever was already green.
func TestARunStillGoingKeepsWaiting(t *testing.T) {
	for _, st := range []string{"queued", "in_progress", "waiting", "requested", "pending"} {
		p := pr{
			Mergeable:  "MERGEABLE",
			HeadRefOid: "cafe000",
			Rollup:     []entry{{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"}},
			Runs:       []wfrun{{Name: "ci", Status: st}},
		}
		v, why := assess(p, 1)
		if v != waiting {
			t.Errorf("a %s run gave %v (%s), want waiting", st, v, why)
		}
		if !strings.Contains(why, "ci (workflow run)") {
			t.Errorf("the %s run is not named as what is being waited for: %q", st, why)
		}
	}
}

// ⛔ A RUN IS NOT A CHECK, so it does not satisfy -min-checks. That threshold
// asks whether the suite has reported at all; answering it with a workflow
// run would let a repository whose checks never appeared pass on the mere
// existence of a run — the absence this tool exists to refuse, one level up.
func TestRunsDoNotCountTowardsMinChecks(t *testing.T) {
	p := pr{
		Mergeable:  "MERGEABLE",
		HeadRefOid: "cafe000",
		Runs:       []wfrun{{Name: "ci", Status: "completed", Conclusion: "success"}},
	}
	if v, why := assess(p, 1); v != waiting {
		t.Fatalf("a green run with no checks gave %v (%s), want waiting", v, why)
	}
}

// The conclusions a RUN may carry are judged by the same table as a check's,
// including the one only a run produces: startup_failure, which is precisely
// a run that never created a job.
func TestWhichRunConclusionsCount(t *testing.T) {
	green := entry{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"}
	for _, c := range []string{"success", "skipped", "neutral"} {
		p := pr{Mergeable: "MERGEABLE", HeadRefOid: "cafe000", Rollup: []entry{green},
			Runs: []wfrun{{Name: "ci", Status: "completed", Conclusion: c}}}
		if v, why := assess(p, 1); v != passed {
			t.Errorf("a %s run = %v (%s), want passed", c, v, why)
		}
	}
	for _, c := range []string{"failure", "startup_failure", "cancelled", "timed_out", "action_required", "stale", ""} {
		p := pr{Mergeable: "MERGEABLE", HeadRefOid: "cafe000", Rollup: []entry{green},
			Runs: []wfrun{{Name: "ci", Status: "completed", Conclusion: c}}}
		if v, _ := assess(p, 1); v != failed {
			t.Errorf("a %q run = %v, want failed", c, v)
		}
	}
}

// An unnamed run still has to be reportable: a run the API gives no name for
// would otherwise be judged red and reported as "=FAILURE" with nothing
// before the sign.
func TestAnUnnamedRunIsStillNamed(t *testing.T) {
	es := runEntries([]wfrun{{Status: "completed", Conclusion: "failure"}})
	if len(es) != 1 || !strings.HasPrefix(es[0].label(), "(unnamed)") {
		t.Fatalf("an unnamed run renders as %+v", es)
	}
}

// ⛔ AN ABBREVIATED COMMIT ANSWERS [] AND SAYS NOTHING. Measured 2026-11-09:
// head_sha=e18cc9b returned an empty workflow_runs list for a commit that had
// a run in progress; all 40 characters found it. An empty list is a
// legitimate state — a repository with no workflows — so an abbreviation
// would pass as an absence instead of failing as an error.
func TestAnAbbreviatedCommitIsRefusedNotQueried(t *testing.T) {
	for _, s := range []string{"", "e18cc9b", "e18cc9b7e50ba9a0cf57e8484ecdaa21beb26de", "e18cc9b7e50ba9a0cf57e8484ecdaa21beb26de1x", "z18cc9b7e50ba9a0cf57e8484ecdaa21beb26de1"} {
		if fullSHA(s) {
			t.Errorf("%q passed for a full commit", s)
		}
	}
	if !fullSHA("e18cc9b7e50ba9a0cf57e8484ecdaa21beb26de1") {
		t.Error("a real 40-character commit was refused")
	}
	// And through the function that queries, so the guard is not merely
	// defined somewhere: gh is never reached, which is the point.
	if _, err := fetchRuns("/nonexistent-gh", pr{Repo: "a/b", Number: 1}, "e18cc9b"); err == nil {
		t.Fatal("an abbreviated commit was queried anyway")
	} else if !strings.Contains(err.Error(), "40-character") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
}

// ⛔ The query must be bound to the SHA. A branch name or a pull request
// number moves while a wait is running, and the runs for "main" an hour
// later are not the runs for the commit that was measured.
func TestTheBranchQueryStillCarriesTheHeadCommit(t *testing.T) {
	if !strings.Contains(branchQuery, "oid") {
		t.Error("the branch query no longer selects oid")
	}
	if !strings.Contains(branchJQ, "headRefOid") {
		t.Error("branchJQ drops the head commit, so the branch form cannot name what it judged")
	}
}
