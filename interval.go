package main

import (
	"fmt"
	"time"
)

// ⛔ A WAIT'S COST GROWS WITH ITS TARGETS, AND THE DEFAULT DID NOT.
//
// Measured 2026-11-15 (#20): one session ran four waits at once, one per pull
// request, each at the 45 s default — and the per-user REST budget, which every
// session on this machine shares with Renovate, reached zero twice in one day.
// A single `prwait -repo O/R 78 79 80 82` has exactly the same cost as those
// four processes, because the interval is per POLL and a poll reads every
// target. So the flag that looks like "how often to ask" is really "how much of
// a shared budget to spend", and 45 s meant one thing for one target and four
// times that for four.
//
// What one poll of one target spends, counted from ghFetcher and fetchRuns
// rather than assumed:
//
//   - a pull request or a branch: ONE GraphQL call for the check list
//     (`gh pr view --json statusCheckRollup`, `gh api graphql` — statusCheckRollup
//     exists only in GitHub's GraphQL schema) and ONE REST call for the head
//     commit's workflow runs (`actions/runs?head_sha=`);
//   - a workflow run (`!<id>`): ONE REST call, and no runs read, since a run
//     target is already a run.
//
// ⚠ THE TWO BUDGETS ARE SEPARATE and are never summed: 5000 REST calls per hour
// and 5000 GraphQL points, one of which can be spent while the other is
// untouched. #20 priced a poll at two REST calls; it is one, and the other half
// of the cost lands somewhere else entirely. The scaling below is set by the
// REST half because that is the one that was measured at zero.
const (
	// baseInterval is what ONE target gets: 80 REST calls an hour, 1.6% of the
	// shared budget.
	baseInterval = 45 * time.Second
	// maxInterval caps the scaling. Linear growth is right for the budget and
	// wrong for a human past a few minutes: at 20 targets it would put 15
	// minutes between polls, so a verdict that was ready at minute 1 arrives at
	// minute 15 and the wait reads as a hang. Five minutes is the spacing the
	// second incident in #20 was already using by hand.
	maxInterval = 5 * time.Minute
)

// defaultInterval is the interval for n targets when the caller did not say.
//
// It holds the REST cost of a wait at roughly what one target costs, instead of
// multiplying it by the number of targets. The explanation is returned with it
// because a default that silently differs from the documented 45 s is a tool
// lying quietly: the number, the reason and the override are printed.
func defaultInterval(n int) (time.Duration, string) {
	if n < 1 {
		// Not reachable from main, which refuses an empty target list — and
		// returned rather than panicked for the same reason the rest of this
		// package refuses: a cost model that divides by zero targets would be
		// a crash in a tool whose job is to report.
		return baseInterval, ""
	}
	d := time.Duration(n) * baseInterval
	capped := false
	if d > maxInterval {
		d, capped = maxInterval, true
	}
	if n == 1 {
		return d, ""
	}
	line := fmt.Sprintf("prwait: %d targets, so the default interval is %s rather than %s — one poll spends one REST call per target (the head commit's workflow runs), and that budget is per USER, shared by every session here, Renovate included. -interval overrides this.",
		n, d, baseInterval)
	if capped {
		line = fmt.Sprintf("prwait: %d targets: the default interval is capped at %s (%d × %s would be %s, which reads as a hang) — so this wait spends about %.0f REST calls an hour. -interval overrides this.",
			n, maxInterval, n, baseInterval, time.Duration(n)*baseInterval,
			float64(n)*float64(time.Hour)/float64(maxInterval))
	}
	return d, line
}

// chosenInterval decides between what the caller asked for and the default.
//
// ⛔ AN EXPLICIT -interval IS NEVER SCALED. A caller who names an interval has
// said what they want of the budget, and silently multiplying it would make the
// flag mean something different from what it reads — the same defect as the
// unscaled default, with the sign reversed. set comes from flag.FlagSet.Visit,
// which reports the flags actually given, so `-interval 45s` typed by hand is
// honoured as typed even though it equals the one-target default.
func chosenInterval(set bool, asked time.Duration, targets int) (time.Duration, string) {
	if set {
		return asked, ""
	}
	return defaultInterval(targets)
}
