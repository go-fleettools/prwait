package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// ⛔⛔ THE SAFEST INVOCATION WAS THE ONE WHERE THE CONFLICTING REFUSAL COULD
// NEVER FIRE.
//
// `assess` has checked CONFLICTING before counting checks since #1. But
// `branchJQ` supplies a fabricated `mergeable:"MERGEABLE"` — correct for a bare
// branch, which has nothing to merge into — and the @branch form is the one the
// fleet's own rule steers every caller to, because guessing a pull request
// number is the worse hazard. So the fabricated positive overrode the real
// answer and the refusal was unreachable from the form people use.
//
// Measured 2026-11-18 on go-zpkg/zpkb#505: `prwait @branch` reported
// "TIMEOUT after 30m0s — 0 checks, want at least 1", three retriggers were
// spent on a dropped-dispatch theory (Actions enabled, workflows active, repo
// public, githubstatus operational, a sibling repository running 34 minutes
// earlier), and `gh pr view --json mergeable` answered CONFLICTING in ONE call.
// GitHub runs `pull_request` workflows against `refs/pull/N/merge`, which does
// not exist while the merge cannot be computed — so a conflicted pull request
// has no runs, by construction, and "0 checks" is a VERDICT there rather than
// a wait.
func TestABranchThatIsAConflictedPullRequestsHeadIsRefused(t *testing.T) {
	p := pr{
		Repo:   "o/r",
		Branch: "feature",
		Assoc: []assocPR{
			{Number: 505, State: "OPEN", Mergeable: "CONFLICTING", HeadRefName: "feature"},
		},
	}
	p.Mergeable, p.AssocPR = branchMergeable(p.Branch, p.Assoc)

	v, why := assess(p, 1)
	if v != failed {
		t.Fatalf("verdict = %v, %q; want failed", v, why)
	}
	// ⚠ The number has to be in the message: told "CONFLICTING" about a
	// BRANCH, a reader has no way to know which pull request said so.
	if !strings.Contains(why, "#505") {
		t.Errorf("why = %q; want it to name the pull request", why)
	}
	if !strings.Contains(why, "rebase") {
		t.Errorf("why = %q; want it to say what to do", why)
	}
}

// ⭐ THE CONTROL, AND IT IS THE REASON THE FABRICATED VALUE WAS THERE: a bare
// branch — one no open pull request has as its head — must behave exactly as
// before. It has nothing to merge into, so asking about mergeability is a
// question with no subject, and the wait must ask only about the checks.
func TestABareBranchStillJudgesOnlyItsChecks(t *testing.T) {
	for _, name := range []string{"no association at all", "associated, but not as head"} {
		assoc := []assocPR(nil)
		if name != "no association at all" {
			// A commit is associated with every pull request that CONTAINS it,
			// so a branch cut from main is associated with whatever merged into
			// main. Those say nothing about what THIS branch would merge.
			assoc = []assocPR{
				{Number: 400, State: "OPEN", Mergeable: "CONFLICTING", HeadRefName: "somebody-else"},
				{Number: 399, State: "MERGED", Mergeable: "CONFLICTING", HeadRefName: "feature"},
			}
		}
		got, num := branchMergeable("feature", assoc)
		if got != "MERGEABLE" || num != 0 {
			t.Errorf("%s: branchMergeable = %q, %d; want MERGEABLE, 0", name, got, num)
		}
	}
}

// ⛔⛔ AN UNANSWERED MERGEABILITY IS NOT "MERGEABLE", and this is the half that
// would turn one fabricated positive into another. GitHub computes the field
// lazily and answers UNKNOWN for a few seconds after every push; an absent
// field arrives as the empty string. Both must reach `assess` as UNKNOWN,
// which it already treats as "keep asking" — because a wait that stops early
// on a green-but-uncomputed pull request would pass something it never
// established.
func TestAnUncomputedMergeabilityKeepsWaitingRatherThanPassing(t *testing.T) {
	for _, raw := range []string{"UNKNOWN", ""} {
		got, num := branchMergeable("feature", []assocPR{
			{Number: 7, State: "OPEN", Mergeable: raw, HeadRefName: "feature"},
		})
		if got != "UNKNOWN" || num != 7 {
			t.Fatalf("mergeable %q → %q, %d; want UNKNOWN, 7", raw, got, num)
		}
		// And end to end: all checks green, mergeability uncomputed, so the
		// verdict is still a wait.
		p := pr{Repo: "o/r", Branch: "feature", Mergeable: got, AssocPR: num,
			Rollup: []entry{{Typename: "CheckRun", Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"}}}
		if v, why := assess(p, 1); v != waiting {
			t.Errorf("mergeable %q: verdict = %v, %q; want waiting", raw, v, why)
		}
	}
}

// ⚠ SEVERAL OPEN PULL REQUESTS CAN SHARE ONE HEAD — the same branch against
// two bases. CONFLICTING wins, then UNKNOWN: a conflict against any base is
// worth stopping for, and stopping early costs a rebase while running on costs
// the whole timeout and a wrong theory.
func TestAmongSeveralHeadsTheWorstAnswerWins(t *testing.T) {
	for _, tc := range []struct {
		name  string
		assoc []assocPR
		want  string
	}{
		{"conflicting beside mergeable", []assocPR{
			{Number: 1, State: "OPEN", Mergeable: "MERGEABLE", HeadRefName: "f"},
			{Number: 2, State: "OPEN", Mergeable: "CONFLICTING", HeadRefName: "f"},
		}, "CONFLICTING"},
		{"conflicting FIRST is not softened by a later mergeable", []assocPR{
			{Number: 2, State: "OPEN", Mergeable: "CONFLICTING", HeadRefName: "f"},
			{Number: 1, State: "OPEN", Mergeable: "MERGEABLE", HeadRefName: "f"},
		}, "CONFLICTING"},
		{"unknown beside mergeable", []assocPR{
			{Number: 1, State: "OPEN", Mergeable: "MERGEABLE", HeadRefName: "f"},
			{Number: 2, State: "OPEN", Mergeable: "UNKNOWN", HeadRefName: "f"},
		}, "UNKNOWN"},
		{"unknown FIRST is not overwritten by a later mergeable", []assocPR{
			{Number: 2, State: "OPEN", Mergeable: "UNKNOWN", HeadRefName: "f"},
			{Number: 1, State: "OPEN", Mergeable: "MERGEABLE", HeadRefName: "f"},
		}, "UNKNOWN"},
	} {
		if got, _ := branchMergeable("f", tc.assoc); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// ⛔ AND THE WIRING IS ASSERTED, not only the rule. `decode` is the single
// place a branch answer becomes a `pr`; a rule that is right and unreached is
// the defect this change repairs, one layer over.
func TestDecodeAppliesTheRuleForABranchAndNotForAPullRequest(t *testing.T) {
	body := map[string]any{
		"mergeable":  "MERGEABLE", // what branchJQ fabricates
		"state":      "OPEN",
		"headRefOid": "deadbeef",
		"associatedPullRequests": []map[string]any{
			{"number": 505, "state": "OPEN", "mergeable": "CONFLICTING", "headRefName": "feature"},
		},
		"statusCheckRollup": []any{},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	got, err := decode(raw, pr{Repo: "o/r", Branch: "feature"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Mergeable != "CONFLICTING" || got.AssocPR != 505 {
		t.Errorf("branch: mergeable=%q assoc=%d; want CONFLICTING, 505", got.Mergeable, got.AssocPR)
	}

	// ⭐ A PULL-REQUEST target is left alone: `gh pr view` already answers
	// `mergeable` for real, and refining it from the association list would be
	// a second source for a field that has an authoritative one.
	got, err = decode(raw, pr{Repo: "o/r", Number: 505})
	if err != nil {
		t.Fatal(err)
	}
	if got.Mergeable != "MERGEABLE" || got.AssocPR != 0 {
		t.Errorf("pull request: mergeable=%q assoc=%d; want the field as given", got.Mergeable, got.AssocPR)
	}
}

// ⚠ The query must actually ASK for what the rule reads. A rule fed by a field
// the query never selected is the same defect as the fabricated constant: it
// would see an empty list and answer MERGEABLE forever.
func TestTheBranchQueryAsksForTheAssociatedPullRequests(t *testing.T) {
	for _, want := range []string{"associatedPullRequests", "headRefName", "mergeable", "state"} {
		if !strings.Contains(branchQuery, want) {
			t.Errorf("branchQuery does not select %q", want)
		}
	}
	if !strings.Contains(branchJQ, "associatedPullRequests") {
		t.Error("branchJQ does not carry associatedPullRequests through to decode")
	}
}
