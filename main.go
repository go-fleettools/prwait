// prwait waits until a pull request's checks are finished, and refuses to
// call an empty check list a pass.
//
// # WHY IT EXISTS
//
// The obvious waiter asks "is anything still pending?" and stops when the
// answer is no. That waiter reported SUCCESS on four pull requests that had
// never been built:
//
//   - A CONFLICTING pull request gets no merge ref, so no workflow is
//     dispatched for it, so its check list stays EMPTY — and an empty list
//     has nothing pending. Worse, a pull request that conflicts AFTER a
//     green run keeps the green ticks it earned on the old base, so the
//     summary looks like a pass twice over.
//   - A check list is also empty for the first seconds after a push, and
//     for the whole life of a pull request whose workflow fails to PARSE:
//     an invalid workflow reports nothing at all, which reads exactly like
//     a check that has not started.
//
// So the rule here is the inverse of the obvious one: a pull request passes
// only when at least -min-checks checks exist AND every one of them has
// finished AND every conclusion is acceptable. Absence is never evidence.
//
// ⛔ AND A COMPLETE, ALL-GREEN CHECK LIST IS NOT A PASS EITHER. A check run
// is created per JOB, so a workflow run that fails BEFORE its jobs are
// created leaves no check run at all: nothing red, nothing pending, nothing
// to count. Measured 2026-11-09 on go-tex/go-tex.github.io#239, head
// 769bfee: this tool printed `PASS SUCCESS:3` while two of the four workflow
// runs for that very SHA had `conclusion: failure` — `pages` with ZERO jobs
// created and `playground` with one of its three. Every property the check
// list can express was true. So the runs are read as well, from
// actions/runs?head_sha=<oid>, and judged by the same rules; the check list
// is the wrong noun on its own.
//
// # WHAT IT DOES NOT DO
//
// It does not merge, and it never sees a credential. It shells out to gh,
// which holds the authentication; no token is read, printed, or passed on a
// command line.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/go-fleettools/prwait/judge"
)

const usage = `prwait — wait for a pull request's or a branch's checks, and refuse to call an empty list a pass

usage:
  prwait [-repo OWNER/REPO] [flags] <target>...

a <target> is
  82                 a pull request (with -repo)
  OWNER/REPO#82      a pull request, spelled whole
  @main              a BRANCH (with -repo)
  OWNER/REPO@main    a branch, spelled whole
  !40237685388       a workflow RUN (with -repo)
  OWNER/REPO!402376  a run, spelled whole

Each kind carries its own sigil, so a mistyped number stays an error instead
of becoming a wait on something nobody named. They answer different
questions: a pull request asks "may this land", a branch asks "is what landed
green" — which is what you want before tagging a release — and a run asks
"did that dispatch finish, and how".

A run has no pull request and no checks, so neither of the other forms can
see one, and "gh run watch" is what people reach for instead. Measured
2026-10-25: it returned three times while the run was still in progress, and
each time the conclusion read next did not exist yet. Here a run is one
check, judged by the same rules as any other: not finished is never a pass.

⛔ A pull request and a branch are judged on their head commit's WORKFLOW
RUNS as well as its checks. A check run exists per JOB, so a run that fails
before any job is created leaves nothing red and nothing pending in the check
list: measured 2026-11-09, three green checks and a PASS while two of the
four runs for that SHA had conclusion=failure, one of them with no jobs at
all. Both are read for the same commit, and both must be green.

⛔ A head that MOVES while the wait runs is refused, not followed:

  OWNER/REPO#752 MOVED @e87aa0a -> @2baff07 — the head changed while waiting;
  this wait judged neither, re-run against @2baff07

Continuing to poll would silently re-point the wait at work nobody asked
about and print a true statement about the wrong commit. Neither commit was
judged as a whole, so neither gets a verdict, and the exit status is 1. A SHA
one poll could not read is NOT a move.

flags:
  -repo OWNER/REPO   repository for bare targets
  -interval 45s×N    how often to ask: 45s for one target, 45s per target up to
                     a 5m cap, because a poll reads EVERY target and the REST
                     budget is per user — the targets of the OTHER live waits of
                     this user count too, read from <user cache>/prwait/waits.json.
                     Given explicitly, it is used as typed.
  -timeout 30m       give up after this, per run
  -min-checks 1      how many checks must EXIST before a pass is possible
  -gh gh             the gh binary to ask

⛔ A QUOTA IS NOT A VERDICT. When GitHub refuses a call for the rate limit,
nothing about the target was measured, so no verdict is printed and the exit
status is 3 — never 1, which means "measured, and not green". A quota has an
advertised reset, so it is held for and then retried when the reset falls
inside the timeout, and refused early when it does not. A measured red still
outranks it: if anything was read as failing, the status stays 1.

exit status:
  0  every target finished, with checks, all green
  1  one finished red, or conflicts, or ran out of time
  2  refused: bad usage, gh missing, or an answer that could not be read
  3  the API quota stopped the wait: nothing was measured, nothing is claimed
`

// The judge itself lives in package judge (one implementation, shared with
// every other program that asks "is this commit green"); the names below are
// this program's spelling of it.
//
// A rollup entry is a CheckRun (status + conclusion) or a StatusContext
// (state): see judge.Entry. A wfrun is one workflow run for the head commit,
// NOT a check run and absent from a check rollup: see the third mechanism in
// this file's doc comment.
type (
	entry = judge.Entry
	wfrun = judge.Run
)

var (
	// latestRuns keeps the most recent run of each (workflow, event).
	latestRuns = judge.LatestRuns
	// runEntries renders workflow runs as check entries.
	runEntries = judge.RunEntries
	// ok reports whether a finished check's answer is one to accept.
	ok = judge.Ok
	// trim keeps the first n elements and marks the cut.
	trim = judge.Trim
	// tally counts the conclusions.
	tally = judge.Tally
)

// runsReply is the shape of actions/runs.
type runsReply struct {
	Runs []wfrun `json:"workflow_runs"`
}

// rerunWindow is how long GitHub lets a workflow run be re-run. Past it,
// `gh run rerun` answers 403 "Unable to retry this workflow run because it
// was created over a month ago".
const rerunWindow = 30 * 24 * time.Hour

// logRetention is GitHub's DEFAULT retention for a run's logs. A repository
// can shorten it, and an enterprise can lengthen it, which is why the advice
// below says "kept 90 days by default" rather than claiming they are gone.
const logRetention = 90 * 24 * time.Hour

// redRunAdvice says, for each FAILING run, whether its verdict can still be
// refreshed where it stands.
//
// ⛔ IT EXISTS BECAUSE THE QUESTION WENT UNASKED FOR TEN DAYS.
// go-ansible/playbook#197 sat red on one lane, `test (macos-latest)`, and was
// handed back twice as "blocked: three routes tried, all closed" — all three
// routes being ways to make THAT run go green. Nobody asked whether that run
// could be re-run at all (it could not: created 2026-10-12, over the window)
// or whether its logs could still be read (they could not). The change itself
// was fine: cherry-picked onto a branch in the target repository it came back
// 9/9 green, `macos-latest` included.
//
// Three failures that share one premise are not three tests — they are one
// untested premise, counted three times. So this prints the premise.
//
// It costs NO extra request: `created_at` already arrives with every run.
// Nothing is asserted about logs that are merely old; the age and the two
// documented limits are printed, and the reader concludes.
func redRunAdvice(runs []wfrun, now time.Time) []string {
	var out []string
	for _, r := range runs {
		if !strings.EqualFold(r.Conclusion, "failure") {
			continue
		}
		created, err := time.Parse(time.RFC3339, r.Created)
		if err != nil {
			// No opinion rather than a guess: absence is never evidence.
			continue
		}
		age := now.Sub(created)
		if age < rerunWindow {
			continue
		}
		name := r.Name
		if name == "" {
			name = "(unnamed)"
		}
		line := fmt.Sprintf(
			"  ⛔ %s (%s run #%d) was created %s, %d days ago: `gh run rerun` will answer 403 (GitHub allows a re-run for %d days)",
			name, r.Event, r.Number, created.Format("2006-01-02"), int(age.Hours()/24), int(rerunWindow.Hours()/24))
		if age >= logRetention {
			line += fmt.Sprintf(", and its logs are past the %d-day default retention", int(logRetention.Hours()/24))
		}
		line += ".\n     This red cannot be refreshed where it stands. Reproduce the lane locally if it names your platform, or put the commit on a ref you control — `git cherry-pick` onto a branch in this repository — and let CI judge it there."
		out = append(out, line)
	}
	return out
}

type pr struct {
	Repo      string
	Number    int
	Mergeable string  `json:"mergeable"`
	State     string  `json:"state"`
	Rollup    []entry `json:"statusCheckRollup"`
	// HeadRefOid is the head commit the checks and the runs are bound to.
	// Every verdict is about a SHA, never about a branch name or a number:
	// those move while the wait is running.
	HeadRefOid string `json:"headRefOid"`
	// Runs are the workflow runs for HeadRefOid. They are judged beside the
	// rollup but are NOT counted towards -min-checks: that threshold is
	// about checks existing, and a run is a different kind of evidence.
	Runs []wfrun `json:"-"`
	// Branch is set instead of Number when the target is a branch.
	Branch string
	// Assoc are the pull requests GitHub associates with HeadRefOid, asked for
	// only in branch mode. They are the raw answer; `branchMergeable` decides
	// which of them is this branch's.
	Assoc []assocPR `json:"associatedPullRequests"`
	// AssocPR is the open pull request whose head this branch is, once one has
	// been found. It is for the MESSAGE only: a reader told "CONFLICTING" about
	// a BRANCH has no way to know where that came from, and the number is the
	// thing they would go and look at.
	//
	// ⛔ DELIBERATELY NOT `Number`: that field selects the query shape, so
	// writing a number into it would turn the next poll into a pull-request
	// lookup and silently change what the wait is about.
	AssocPR int
	// Run is set instead of either when the target is a workflow RUN.
	//
	// ⛔ A DISPATCHED RUN HAS NO PULL REQUEST AND NO CHECKS, so neither of
	// the other two forms can see it, and `gh run watch` is what one
	// reaches for instead. Measured 2026-10-25: it returned three times
	// while the run was still `in_progress`, and each time the next thing
	// read was a conclusion that did not exist yet — "the build failed"
	// about a build still compiling. An answer not yet given must not read
	// as a negative.
	Run int64
}

// assocPR is one pull request GitHub associates with a commit.
type assocPR struct {
	Number      int    `json:"number"`
	State       string `json:"state"`
	Mergeable   string `json:"mergeable"`
	HeadRefName string `json:"headRefName"`
}

// branchMergeable answers "can what this branch would merge, merge?" from the
// pull requests associated with its head commit.
//
// ⛔⛔ THE SELECTION IS BY HEAD, NOT BY ASSOCIATION. GitHub associates a commit
// with every pull request that CONTAINS it, so a branch cut from main is
// associated with whatever merged into main. Only a pull request whose
// headRefName IS this branch says anything about what this branch would merge;
// the rest are other people's.
//
// ⛔ AND AN UNANSWERED MERGEABILITY IS NOT "MERGEABLE". GitHub computes it
// lazily and says UNKNOWN for a few seconds after a push, and an empty string
// is what arrives if the field is ever absent. Both are carried through as
// UNKNOWN, which `assess` already treats as "keep asking" — turning either into
// MERGEABLE is the fabricated-positive this whole change exists to remove.
//
// Several open pull requests can share one head (same branch, different bases).
// CONFLICTING wins, then UNKNOWN: a conflict against any base is worth stopping
// for, and a wait that stops early costs a rebase while one that runs on costs
// the timeout and a wrong theory.
func branchMergeable(branch string, assoc []assocPR) (mergeable string, number int) {
	mergeable, number = "MERGEABLE", 0
	for _, a := range assoc {
		if a.State != "OPEN" || a.HeadRefName != branch {
			continue
		}
		m := a.Mergeable
		if m == "" {
			m = "UNKNOWN"
		}
		switch {
		case m == "CONFLICTING":
			// Decided: nothing a later entry says can soften it.
			return m, a.Number
		case mergeable != "UNKNOWN":
			mergeable, number = m, a.Number
		}
	}
	return mergeable, number
}

func (p pr) String() string {
	switch {
	case p.Branch != "":
		return p.Repo + "@" + p.Branch
	case p.Run != 0:
		return fmt.Sprintf("%s!%d", p.Repo, p.Run)
	}
	return fmt.Sprintf("%s#%d", p.Repo, p.Number)
}

type verdict int

const (
	waiting verdict = iota
	passed
	failed
)

// assess decides, from one observation, whether to stop and with what.
//
// It never networks and never sleeps, so every rule below is testable
// against a literal.
func assess(p pr, minChecks int) (verdict, string) {
	switch p.State {
	case "MERGED":
		return passed, "already merged"
	case "CLOSED":
		return failed, "closed without merging"
	}
	// CONFLICTING FIRST, before the checks are even counted: a conflicted
	// pull request keeps whatever ticks it earned on the old base, so its
	// rollup can be complete and entirely green while nothing has been
	// built against what would actually be merged.
	if p.Mergeable == "CONFLICTING" {
		// ⚠ The number is named when the target was a BRANCH, because a reader
		// told "CONFLICTING" about a branch has no way to know which pull
		// request said so — and that number is the thing they would open.
		if p.AssocPR != 0 {
			return failed, fmt.Sprintf("CONFLICTING (#%d) — rebase it; any green ticks it shows are from the old base", p.AssocPR)
		}
		return failed, "CONFLICTING — rebase it; any green ticks it shows are from the old base"
	}
	if len(p.Rollup) < minChecks {
		// AND SAY WHEN WAITING IS HOPELESS WITHOUT GIVING UP ON IT.
		//
		// -min-checks is one number for a whole run, so a single command
		// covering repositories with different suites gets the biggest
		// repository's threshold applied to the smallest. Observed: a run
		// over four Go repositories (20+ lanes each) and one docs
		// repository (2) with -min-checks 20. The docs pull request was
		// finished and green and the waiter sat on it until the timeout,
		// repeating "2 checks, want at least 20" without ever saying that
		// those 2 were DONE.
		//
		// It still waits — a threshold that gave up would be a threshold
		// that passes an incomplete list, which is the whole defect this
		// tool exists for. But the reason now distinguishes "more are
		// coming" from "this is all there will ever be", and names the
		// flag, because the fix is to split the run and only the caller
		// can do that.
		done := 0
		for _, e := range p.Rollup {
			if e.Done() {
				done++
			}
		}
		if len(p.Rollup) > 0 && done == len(p.Rollup) {
			return waiting, fmt.Sprintf("%d checks, all finished, want at least %d — is -min-checks meant for this repository?",
				len(p.Rollup), minChecks)
		}
		return waiting, fmt.Sprintf("%d checks, want at least %d", len(p.Rollup), minChecks)
	}
	// FROM HERE ON, THE RUNS ARE JUDGED BESIDE THE CHECKS. The threshold
	// above counted checks alone on purpose; everything below asks "has it
	// finished" and "did it pass", which a workflow run answers in its own
	// right — and answers in the one case the check list cannot express at
	// all, a run that failed before creating any job.
	judged := make([]entry, 0, len(p.Rollup)+len(p.Runs))
	judged = append(judged, p.Rollup...)
	judged = append(judged, runEntries(p.Runs)...)

	var pending []string
	for _, e := range judged {
		if !e.Done() {
			pending = append(pending, e.Label())
		}
	}
	if len(pending) > 0 {
		return waiting, fmt.Sprintf("%d of %d running (%s)", len(pending), len(judged), strings.Join(trim(pending, 3), ", "))
	}
	var bad []string
	for _, e := range judged {
		if !ok(e.Result()) {
			bad = append(bad, fmt.Sprintf("%s=%s", e.Label(), e.Result()))
		}
	}
	if len(bad) > 0 {
		return failed, strings.Join(bad, " ")
	}
	// MERGEABLE is computed lazily by GitHub and is UNKNOWN for a few
	// seconds after any push. Green checks plus UNKNOWN is not yet an
	// answer about whether it can land, so keep asking.
	if p.Mergeable == "UNKNOWN" {
		return waiting, "checks green, mergeability not computed yet"
	}
	return passed, tally(judged)
}

// ref is OWNER/REPO#NUMBER, or a bare number resolved against -repo.
// parseTarget reads one command-line target: a pull request, or a BRANCH.
//
//	82                     a pull request, with -repo
//	go-pkgx/pkgx#82        a pull request, spelled whole
//	@main                  a branch, with -repo
//	go-pkgx/pkgx@main      a branch, spelled whole
//
// A branch ALWAYS carries the @, even as a bare argument. That is deliberate:
// treating any bare non-number as a branch would turn a mistyped pull request
// number ("8two") into a wait on a branch that does not exist, which reports
// "no checks" instead of "that is not a number". One character buys an
// unambiguous error.
//
// The two kinds answer different questions -- a pull request asks "may this
// land", a branch asks "is what landed green" -- and the second is what you
// need before tagging.
func parseTarget(arg, defRepo string) (pr, error) {
	repo, rest, isBranch, isRun := defRepo, arg, false, false
	if i := strings.LastIndex(arg, "@"); i >= 0 {
		repo, rest, isBranch = arg[:i], arg[i+1:], true
		if repo == "" {
			repo = defRepo // the bare "@branch" form
		}
	} else if i := strings.LastIndex(arg, "!"); i >= 0 {
		// ⛔ A RUN GETS ITS OWN SIGIL for the reason a branch does: a run
		// id and a pull request number are both bare digits, and the two
		// ask different questions of different objects. Without the `!` a
		// mistyped id becomes a wait on pull request 40237685388, which
		// does not exist — a refusal, luckily, but by accident.
		repo, rest, isRun = arg[:i], arg[i+1:], true
		if repo == "" {
			repo = defRepo // the bare "!123" form
		}
	} else if i := strings.LastIndex(arg, "#"); i >= 0 {
		repo, rest = arg[:i], arg[i+1:]
	}
	if repo == "" {
		return pr{}, fmt.Errorf("%s: no repository — pass -repo OWNER/REPO or write OWNER/REPO#%s", arg, arg)
	}
	if strings.Count(repo, "/") != 1 || strings.HasPrefix(repo, "/") || strings.HasSuffix(repo, "/") {
		return pr{}, fmt.Errorf("%s: %q is not OWNER/REPO", arg, repo)
	}
	if rest == "" {
		switch {
		case isBranch:
			return pr{}, fmt.Errorf("%s: nothing after the @", arg)
		case isRun:
			return pr{}, fmt.Errorf("%s: nothing after the !", arg)
		}
		return pr{}, fmt.Errorf("%s: nothing after the #", arg)
	}
	if isBranch {
		return pr{Repo: repo, Branch: rest}, nil
	}
	if isRun {
		// 64 bits: a run id is already eleven digits and climbing.
		id, err := strconv.ParseInt(rest, 10, 64)
		if err != nil || id <= 0 {
			return pr{}, fmt.Errorf("%s: %q is not a workflow run id", arg, rest)
		}
		return pr{Repo: repo, Run: id}, nil
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n <= 0 {
		return pr{}, fmt.Errorf("%s: %q is not a pull request number (a branch is written @%s, a run !%s)", arg, rest, rest, rest)
	}
	return pr{Repo: repo, Number: n}, nil
}

// fetcher asks for one observation. It is a parameter so the tests can run
// every rule without a network and without gh.
type fetcher func(t pr) (pr, error)

// branchQuery asks for a branch's check rollup and has gh reshape the answer
// into the SAME object `gh pr view` returns, so every rule in assess applies
// unchanged.
//
// state and mergeable are supplied, not read: a branch is neither open nor
// merged and has nothing to merge into, so "OPEN"/"MERGEABLE" are the values
// that make assess ask only about the checks. Writing them here rather than
// special-casing assess keeps one set of rules for both kinds of target.
// ⛔⛔ associatedPullRequests IS WHY THE "nothing to merge into" PREMISE ABOVE
// IS ONLY HALF TRUE. It is true of a bare branch and FALSE of a branch that is
// the head of an open pull request — and the house rule steers every caller to
// the @branch form, because guessing a pull request number is the worse hazard.
// So the safest invocation was the one where the CONFLICTING refusal in
// `assess` could never fire: the fabricated "MERGEABLE" below overrode the real
// answer, and a conflicted pull request reported "0 checks, want at least 1"
// until the timeout. Measured 2026-11-18 on go-zpkg/zpkb#505: thirty minutes of
// silence, then three retriggers spent on a dropped-dispatch theory, while
// `gh pr view --json mergeable` said CONFLICTING in one call.
const branchQuery = `query($owner:String!,$name:String!,$ref:String!){
  repository(owner:$owner,name:$name){
    ref(qualifiedName:$ref){ target{ ... on Commit{ oid
      statusCheckRollup{ contexts(last:100){ nodes{
        __typename
        ... on CheckRun{ name status conclusion }
        ... on StatusContext{ context state }
      } } }
      associatedPullRequests(first:10){ nodes{
        number state mergeable headRefName
      } }
    } } }
  }
}`

// headRefOid is carried through under the SAME name `gh pr view` uses, so
// the one field serves both kinds of target and the runs lookup needs no
// special case. The query already selected `oid`; dropping it was what left
// the branch form unable to name the commit it was judging.
//
// ⚠ "MERGEABLE" STAYS HERE AS THE BARE-BRANCH DEFAULT and is refined in
// `decode`, not in jq. The selection — which of the associated pull requests
// is actually this branch's — is a rule, and every rule in this file is in Go
// where a literal can test it. Interpolating a branch name into a jq program
// to do it would also be an injection site for an argument.
const branchJQ = `{mergeable:"MERGEABLE",state:"OPEN",` +
	`headRefOid:(.data.repository.ref.target.oid // ""),` +
	`associatedPullRequests:(.data.repository.ref.target.associatedPullRequests.nodes // []),` +
	`statusCheckRollup:(.data.repository.ref.target.statusCheckRollup.contexts.nodes // [])}`

// runJQ reshapes `gh run view` into the object every rule here already
// understands: one CheckRun whose name is the workflow's.
//
// ⛔ UPPERCASED, because entry.done() tests `COMPLETED` — the GraphQL
// spelling — while `gh run view --json status` answers `completed`. Left
// lowercase the run would never look done and prwait would wait out its
// timeout on a finished, green build: the failure this target exists to
// prevent, wearing the opposite sign.
//
// ⛔ `// ""` before the upcase, because a run still going has a NULL
// conclusion, and `null | ascii_upcase` is a jq error — which would
// arrive as "cannot read gh's answer" on every poll until the run ended.
// headRefOid is carried here too, so a run's verdict can name the commit it is
// about like every other target's. A run HAS a head commit; it simply was not
// asked for.
const runJQ = `{mergeable:"MERGEABLE",state:"OPEN",` +
	`headRefOid:(.headSha // ""),` +
	`statusCheckRollup:[{` +
	`__typename:"CheckRun",` +
	`name:((.name // "") + " (run)"),` +
	`status:((.status // "") | ascii_upcase),` +
	`conclusion:((.conclusion // "") | ascii_upcase)}]}`

func ghFetcher(bin string) fetcher {
	return func(t pr) (pr, error) {
		var cmd *exec.Cmd
		switch {
		case t.Run != 0:
			cmd = exec.Command(bin, "run", "view", strconv.FormatInt(t.Run, 10),
				"--repo", t.Repo,
				"--json", "name,status,conclusion,headSha",
				"--jq", runJQ)
		case t.Branch != "":
			owner, name, _ := strings.Cut(t.Repo, "/")
			cmd = exec.Command(bin, "api", "graphql",
				"-f", "query="+branchQuery,
				"-F", "owner="+owner,
				"-F", "name="+name,
				"-F", "ref=refs/heads/"+t.Branch,
				"--jq", branchJQ)
		default:
			cmd = exec.Command(bin, "pr", "view", strconv.Itoa(t.Number),
				"--repo", t.Repo,
				"--json", "headRefOid,mergeable,state,statusCheckRollup")
		}
		var errb strings.Builder
		cmd.Stderr = &errb
		out, err := cmd.Output()
		if err != nil {
			msg := strings.TrimSpace(errb.String())
			if msg == "" {
				msg = err.Error()
			}
			return pr{}, fmt.Errorf("gh: %s: %s", t, msg)
		}
		p, err := decode(out, t)
		if err != nil {
			return pr{}, err
		}
		// A RUN TARGET IS ALREADY A RUN: asking the actions API which runs
		// share its head commit would drag in every other workflow for that
		// SHA, which is not what `!<id>` was asked about.
		if t.Run != 0 {
			return p, nil
		}
		if p.HeadRefOid == "" {
			// Not an absence to pass over: without the SHA there is nothing
			// to bind a verdict to, and a verdict bound to a branch name or
			// a pull request number is about whatever moved there since.
			return pr{}, fmt.Errorf("%s: gh gave no head commit, so no verdict can be bound to one", t)
		}
		runs, err := fetchRuns(bin, t, p.HeadRefOid)
		if err != nil {
			return pr{}, err
		}
		p.Runs = runs
		return p, nil
	}
}

// fetchRuns reads the workflow runs for one commit.
//
// ⛔ head_sha, never the branch or the pull request: both move while a wait
// is running, and a verdict about a different commit than the one measured
// is the defect this tool is made of.
func fetchRuns(bin string, t pr, sha string) ([]wfrun, error) {
	// ⛔ A SHORT SHA ANSWERS [] AND SAYS NOTHING. Measured 2026-11-09:
	// head_sha=e18cc9b returned an empty workflow_runs list for a commit that
	// had a run in progress, while the same query with all 40 characters
	// found it. An empty list is indistinguishable from "no workflows here",
	// which is a legitimate state, so the abbreviation would pass as an
	// absence rather than fail as an error. gh gives the full oid; anything
	// else is a programming mistake and is refused as one.
	if !fullSHA(sha) {
		return nil, fmt.Errorf("%s: %q is not a full 40-character commit; head_sha answers an empty list for an abbreviation", t, sha)
	}
	cmd := exec.Command(bin, "api",
		fmt.Sprintf("repos/%s/actions/runs?head_sha=%s&per_page=100", t.Repo, sha))
	var errb strings.Builder
	cmd.Stderr = &errb
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("gh: %s: workflow runs for %s: %s", t, short(sha), msg)
	}
	var reply runsReply
	if err := json.Unmarshal(out, &reply); err != nil {
		return nil, fmt.Errorf("%s: cannot read the workflow runs for %s: %w", t, short(sha), err)
	}
	return latestRuns(reply.Runs), nil
}

// at renders the commit a verdict is ABOUT, as " @b33f79d", or "" when there is
// none to name.
//
// ⛔ A VERDICT MUST SAY WHAT IT IS A VERDICT ABOUT. The line used to read
//
//	go-fleettools/fleettools#230 PASS SUCCESS:9
//
// which is true of SOME commit. A wait that straddles a push — start it, push
// again, read the result — cannot be acted on from that line alone: the head it
// began with and the head it ended with are different pieces of work, and
// nothing in the output says which was judged. Measured 2026-11-10 on exactly
// that sequence; the verdict happened to be right, and "it was fine" is not the
// same as "I knew it was fine", which is the whole value of a gate.
//
// This is the same family as #13, where a verdict did not say which SURFACES it
// covered. Both are a status detached from its subject, and both read as
// broader than they are.
//
// ⚠ It does NOT decide what should happen when the head moves mid-wait —
// whether to refuse ("the head moved from X to Y") or to follow it. Those are
// different products and the question is open on #16; naming the commit is
// useful under either, which is why it is here on its own.
func at(p pr) string {
	if !fullSHA(p.HeadRefOid) {
		return ""
	}
	return " @" + short(p.HeadRefOid)
}

// fullSHA reports whether s is a whole 40-character hexadecimal commit.
func fullSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// short abbreviates a commit for a message.
func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func decode(out []byte, t pr) (pr, error) {
	var p pr
	if err := json.Unmarshal(out, &p); err != nil {
		return pr{}, fmt.Errorf("%s: cannot read gh's answer: %w", t, err)
	}
	// Carried back, because gh's answer has none of them: without Run the
	// report would call a run "repo#0" and the next poll would ask about
	// pull request zero.
	p.Repo, p.Number, p.Branch, p.Run = t.Repo, t.Number, t.Branch, t.Run
	// ⛔⛔ BRANCH MODE ONLY, AND AFTER THE CARRY-BACK BECAUSE IT NEEDS p.Branch.
	// `branchJQ` supplies a fabricated "MERGEABLE" — right for a bare branch,
	// and a LIE when the branch is the head of an open pull request. Refined
	// here rather than in jq so the rule is testable against a literal.
	if p.Branch != "" {
		p.Mergeable, p.AssocPR = branchMergeable(p.Branch, p.Assoc)
	}
	return p, nil
}

type waiter struct {
	fetch     fetcher
	budget    budgetReader
	interval  time.Duration
	timeout   time.Duration
	minChecks int
	out       io.Writer
	sleep     func(time.Duration)
	now       func() time.Time
}

// outcome is what a whole wait came to. It exists because "not green" and
// "not measured" are different facts and were sharing one exit status.
type outcome int

const (
	allGreen outcome = iota
	someRed
	// quotaStopped is a wait that ended without a verdict because the API
	// quota refused the calls. Nothing was measured, so nothing is claimed.
	quotaStopped
)

// status is the process exit status for an outcome.
func (o outcome) status() int {
	switch o {
	case allGreen:
		return 0
	case quotaStopped:
		return quotaStatus
	default:
		return 1
	}
}

// wait is the boolean question — "did everything pass?" — and is what the
// callers that only merge on green ask.
//
// ⚠ It cannot distinguish a red check from a quota, which is exactly why it
// is not what main reads. Use run for that.
func (w waiter) wait(refs []pr) bool {
	return w.run(refs) == allGreen
}

// run polls until every pull request has settled or the clock runs out.
func (w waiter) run(refs []pr) outcome {
	start := w.now()
	left := make([]pr, len(refs))
	copy(left, refs)
	allOK := true
	// measuredBad is set only by an answer that was actually read: a red
	// check, a conflict, a moved head, a timeout, or a query that failed for
	// something other than the quota.
	//
	// ⛔ IT OUTRANKS quotaStop. A red check is a fact and stays reported as
	// one even if the quota then stopped the rest of the wait; a quota only
	// decides the outcome when nothing was measured to be wrong. The reverse
	// would let a real red hide behind a rate limit.
	measuredBad := false
	quotaStop := false
	// A FETCH ERROR IS OFTEN WEATHER, NOT AN ANSWER.
	//
	// Observed in one 25-minute wait: GitHub answered HTTP 401 once, and
	// minutes later the machine could not fork at all —
	//
	//	gh pr view …: HTTP 401: Requires authentication
	//	fork/exec /usr/local/bin/gh: resource temporarily unavailable
	//
	// — the second because a peer session had the machine at load 576.
	// Neither says anything about the pull request, and both killed the
	// whole wait, which then had to be restarted by hand.
	//
	// So a failed query is RETRIED, up to a few times in a row, and only
	// then gives up on that pull request. It is still never a pass: the
	// run fails if the errors do not stop. Consecutive, not cumulative —
	// a blip an hour apart is weather, three in a row is a wall.
	fails := map[string]int{}
	// quotaHolds counts CONSECUTIVE quota refusals per target, for the
	// documented doubling of the secondary-limit wait. It is separate from
	// fails because a quota is not weather and must not consume the weather
	// budget — that conflation is what turned one 403 into four.
	quotaHolds := map[string]int{}
	// ⛔ A WAIT IS ABOUT A COMMIT, AND A MOVED HEAD IS A DIFFERENT COMMIT.
	//
	// #17 made the verdict NAME the commit it judged. Naming it is not enough
	// on its own: when the head moves mid-wait, the poll silently re-points at
	// work the caller never asked about, and the line that comes out is a true
	// statement about a commit nobody was waiting for.
	//
	// It is not hypothetical. On 2026-11-10 a wait was started on go-tex/engine
	// #752 at e87aa0a; a second commit was pushed to the same branch while it
	// ran, and the caller's correct move was to KILL the wait and start another
	// — which is this refusal, done by hand. Nothing here can do better than
	// that, because the new head has not been judged as a whole by anything;
	// what it can do is notice, and say which two commits are involved.
	//
	// It applies to a branch (@main) and a run as much as to a pull request: a
	// release gate waiting on main must not answer about a commit that arrived
	// after the one it was asked about. That is the whole of the risk.
	firstHead := map[string]string{}
	// unjudged collects the targets a quota stop left without a verdict, so
	// the report names them. Leaving them out would be the same defect in
	// miniature: silence about a target reads as consent.
	var unjudged []pr
	for len(left) > 0 && !quotaStop {
		var still []pr
		for i, p := range left {
			got, err := w.fetch(p)
			if err != nil {
				// ⛔ THE QUOTA IS TESTED BEFORE THE WEATHER, because it is
				// not weather: it has an advertised reset, and asking again
				// spends a call against the limit that is already refusing.
				// Left in the weather path it was retried three times and
				// then reported under the exit status of a red check.
				if q := classifyQuota(err, w.budget); q.kind != notQuota {
					for _, line := range q.lines(p.String(), w.now()) {
						fmt.Fprintln(w.out, line)
					}
					quotaHolds[p.String()]++
					// ⛔⛔ THE WAIT'S OWN CLOCK IS ASKED HERE TOO, not only
					// inside hold. Every other branch of this loop checks it,
					// and this one used to delegate its only bound to another
					// function: mutsweep HUNG on hold's deadline test —
					// removing that one comparison made the wait run forever,
					// because nothing else could stop it. A loop whose
					// termination rests on a single condition in a different
					// function terminates until the day that condition is
					// wrong.
					if w.now().Sub(start) < w.timeout {
						if d, ok := q.hold(w.now(), start.Add(w.timeout), quotaHolds[p.String()]); ok {
							fmt.Fprintf(w.out, "     holding %s for it, then asking again.\n", d.Round(time.Second))
							w.sleep(d)
							still = append(still, p)
							continue
						}
					}
					// Every target shares one quota, so there is nothing to
					// be learned by asking about the others: stop, and name
					// the ones that got no verdict rather than leaving them
					// out of the report.
					quotaStop = true
					allOK = false
					// This target and every one after it in this round, plus
					// the ones already pending, end with nothing measured.
					unjudged = append(unjudged, left[i:]...)
					unjudged = append(unjudged, still...)
					break
				}
				fails[p.String()]++
				if n := fails[p.String()]; n < fetchRetries && w.now().Sub(start) < w.timeout {
					fmt.Fprintf(w.out, "%s asking again (%d/%d): %v\n", p, n, fetchRetries, err)
					still = append(still, p)
					continue
				}
				fmt.Fprintf(w.out, "%s %v\n", p, err)
				allOK = false
				measuredBad = true
				continue
			}
			fails[p.String()] = 0
			quotaHolds[p.String()] = 0
			got.Repo, got.Number = p.Repo, p.Number
			if fullSHA(got.HeadRefOid) {
				key := p.String()
				switch was := firstHead[key]; {
				case was == "":
					firstHead[key] = got.HeadRefOid
				case was != got.HeadRefOid:
					fmt.Fprintf(w.out, "%s MOVED @%s -> @%s — the head changed while waiting; this wait judged neither, re-run against @%s\n",
						got, short(was), short(got.HeadRefOid), short(got.HeadRefOid))
					allOK = false
					measuredBad = true
					continue
				}
			}
			switch v, why := assess(got, w.minChecks); v {
			case passed:
				fmt.Fprintf(w.out, "%s%s PASS %s\n", got, at(got), why)
			case failed:
				fmt.Fprintf(w.out, "%s%s FAIL %s\n", got, at(got), why)
				// A red whose run is past the re-run window is a different
				// situation from a red you can retrigger, and saying so here
				// is the whole point: this is the moment somebody decides
				// whether they are blocked.
				for _, line := range redRunAdvice(got.Runs, w.now()) {
					fmt.Fprintln(w.out, line)
				}
				allOK = false
				measuredBad = true
			default:
				if w.now().Sub(start) >= w.timeout {
					fmt.Fprintf(w.out, "%s%s TIMEOUT after %s — %s\n", got, at(got), w.timeout, why)
					allOK = false
					measuredBad = true
					continue
				}
				still = append(still, p)
			}
		}
		left = still
		if len(left) > 0 {
			w.sleep(w.interval)
		}
	}
	if quotaStop && len(unjudged) > 0 {
		fmt.Fprintf(w.out, "⛔ %d target(s) got no verdict at all: %s — the quota stopped the wait before they were read.\n",
			len(unjudged), targetList(unjudged))
	}
	switch {
	case allOK:
		return allGreen
	case quotaStop && !measuredBad:
		return quotaStopped
	default:
		return someRed
	}
}

// targetList names targets in a report.
func targetList(ps []pr) string {
	names := make([]string, 0, len(ps))
	for _, p := range ps {
		names = append(names, p.String())
	}
	return strings.Join(names, ", ")
}

func main() {
	fs := flag.NewFlagSet("prwait", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	repo := fs.String("repo", "", "repository for bare pull request numbers")
	interval := fs.Duration("interval", 45*time.Second, "how often to ask")
	timeout := fs.Duration("timeout", 30*time.Minute, "give up after this")
	minChecks := fs.Int("min-checks", 1, "how many checks must exist before a pass is possible")
	ghBin := fs.String("gh", "gh", "the gh binary to ask")
	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(2)
	}
	if fs.NArg() == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if *minChecks < 1 {
		// Zero would restore the very defect this tool exists for.
		fmt.Fprintln(os.Stderr, "prwait: -min-checks must be at least 1; an empty check list is not a pass")
		os.Exit(2)
	}
	var refs []pr
	for _, arg := range fs.Args() {
		t, err := parseTarget(arg, *repo)
		if err != nil {
			fmt.Fprintln(os.Stderr, "prwait:", err)
			os.Exit(2)
		}
		refs = append(refs, t)
	}
	// ⛔ The interval is decided from the TARGET COUNT, after the targets have
	// been parsed and before any call is spent. A poll reads every target, so
	// the flag that reads as "how often to ask" is really "how much of a shared
	// budget to spend", and one value cannot mean the same thing for one target
	// and for four. An explicitly given -interval is honoured as typed.
	intervalSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "interval" {
			intervalSet = true
		}
	})
	// ⛔ AND THE TARGET COUNT IS THE USER'S, NOT THIS PROCESS'S. A wait that
	// paces itself perfectly for its own targets still drained the shared quota
	// four times over, because four of them ran at once (#20). The registry is
	// advisory in both directions: it never refuses a wait, and no failure of it
	// ever reaches the exit status — see registry.go.
	startedAt := time.Now()
	path, perr := registryPath()
	if perr != nil {
		fmt.Fprintf(os.Stderr, "prwait: no user cache directory (%v), so this wait cannot be counted with the other waits of this user.\n", perr)
		path = ""
	}
	targets := make([]string, 0, len(refs))
	for _, p := range refs {
		targets = append(targets, p.String())
	}
	reg := enter(path, waitEntry{
		PID:      os.Getpid(),
		Started:  startedAt,
		Deadline: startedAt.Add(*timeout + registryGrace),
		Targets:  targets,
	}, startedAt, alive)
	// ⛔ NOT `defer`: every exit below is os.Exit, which runs no deferred call,
	// and an entry left behind paces every later wait of this user as though
	// this one were still polling — until its deadline, which is this wait's
	// whole timeout. The registry's own staleness rules are the net, not the
	// plan.
	for _, note := range reg.Notes {
		fmt.Fprintln(os.Stderr, note)
	}
	chosen, lines := machineInterval(intervalSet, *interval, len(refs), reg.Others, startedAt)
	for _, line := range lines {
		fmt.Fprintln(os.Stderr, line)
	}
	// The budget is read before the first call is spent, and again whenever a
	// query is refused, because GET /rate_limit is free and a quota that is
	// merely guessed at is how a rate limit came to be reported as a red
	// check.
	read := ghBudget(*ghBin)
	lines, refuse := preflight(read, time.Now())
	for _, line := range lines {
		fmt.Fprintln(os.Stderr, line)
	}
	if refuse {
		reg.Leave()
		os.Exit(quotaStatus)
	}
	w := waiter{
		fetch:     ghFetcher(*ghBin),
		budget:    read,
		interval:  chosen,
		timeout:   *timeout,
		minChecks: *minChecks,
		out:       os.Stdout,
		sleep:     time.Sleep,
		now:       time.Now,
	}
	// ⛔ run, not wait: the boolean cannot tell a red check from a quota, and
	// collapsing them is the defect this change removes.
	code := w.run(refs).status()
	reg.Leave()
	if code != 0 {
		os.Exit(code)
	}
}

// fetchRetries is how many times in a row a query may fail before the
// pull request is given up on. Three, at the poll interval, is about two
// minutes of weather — long enough for an API blip or a loaded machine,
// short enough that a genuinely broken setup is not waited out in silence.
const fetchRetries = 3
