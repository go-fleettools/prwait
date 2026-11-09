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
	"sort"
	"strconv"
	"strings"
	"time"
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

flags:
  -repo OWNER/REPO   repository for bare targets
  -interval 45s      how often to ask
  -timeout 30m       give up after this, per run
  -min-checks 1      how many checks must EXIST before a pass is possible
  -gh gh             the gh binary to ask

exit status:
  0  every target finished, with checks, all green
  1  one finished red, or conflicts, or ran out of time
  2  refused: bad usage, gh missing, or an answer that could not be read
`

// A rollup entry is one of two shapes and they do NOT share a field.
//
// A CheckRun has status + conclusion. A StatusContext — the old commit
// status API, still used by plenty of third-party services — has neither:
// it has state. A waiter that tests `status != "COMPLETED"` therefore finds
// every StatusContext pending FOREVER, and waits out its timeout on a pull
// request that is finished and green.
type entry struct {
	Typename   string `json:"__typename"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Context    string `json:"context"`
	State      string `json:"state"`
}

// label is what to call this check in a report.
func (e entry) label() string {
	if e.Name != "" {
		return e.Name
	}
	if e.Context != "" {
		return e.Context
	}
	return "(unnamed)"
}

// done reports whether this check has reached a final answer.
func (e entry) done() bool {
	if e.Status != "" {
		return e.Status == "COMPLETED"
	}
	switch e.State {
	case "", "PENDING", "EXPECTED":
		return false
	default:
		return true
	}
}

// result is the final answer, from whichever field carries it.
func (e entry) result() string {
	if e.Conclusion != "" {
		return e.Conclusion
	}
	return e.State
}

// wfrun is one workflow run for the head commit, as the REST actions API
// reports it. It is NOT a check run and does not appear in a check rollup:
// see the third mechanism in this file's doc comment.
type wfrun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"html_url"`
}

// runsReply is the shape of actions/runs.
type runsReply struct {
	Runs []wfrun `json:"workflow_runs"`
}

// runEntries renders workflow runs as check entries, so ONE set of rules
// judges both. The suffix keeps them apart in a report: "pages (workflow
// run)" and a check named "pages" are different objects and a reader chasing
// the red one needs to know which.
//
// ⛔ Upper-cased, because entry.done() tests the GraphQL spelling COMPLETED
// while REST answers "completed". Left as REST spells it, a finished run
// would never look done and the wait would run to its timeout — the mirror
// image of the defect this file exists for.
func runEntries(rs []wfrun) []entry {
	es := make([]entry, 0, len(rs))
	for _, r := range rs {
		name := r.Name
		if name == "" {
			name = "(unnamed)"
		}
		es = append(es, entry{
			Typename:   "WorkflowRun",
			Name:       name + " (workflow run)",
			Status:     strings.ToUpper(r.Status),
			Conclusion: strings.ToUpper(r.Conclusion),
		})
	}
	return es
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

// ok reports whether a finished check's answer is one to accept.
//
// NEUTRAL and SKIPPED are passes: a lane that excluded itself did its job.
// CANCELLED is NOT — a cancelled lane ran nothing, and in this fleet that
// happens through concurrency starvation, which is precisely the case where
// a green summary would be a lie.
func ok(result string) bool {
	switch result {
	case "SUCCESS", "NEUTRAL", "SKIPPED":
		return true
	default:
		return false
	}
}

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
			if e.done() {
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
		if !e.done() {
			pending = append(pending, e.label())
		}
	}
	if len(pending) > 0 {
		return waiting, fmt.Sprintf("%d of %d running (%s)", len(pending), len(judged), strings.Join(trim(pending, 3), ", "))
	}
	var bad []string
	for _, e := range judged {
		if !ok(e.result()) {
			bad = append(bad, fmt.Sprintf("%s=%s", e.label(), e.result()))
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

func trim(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(s[:n:n], "…")
}

// tally counts the conclusions, so a pass says what it passed on rather
// than merely saying "ok".
func tally(es []entry) string {
	n := map[string]int{}
	for _, e := range es {
		n[e.result()]++
	}
	keys := make([]string, 0, len(n))
	for k := range n {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s:%d", k, n[k]))
	}
	return strings.Join(parts, " ")
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
const branchQuery = `query($owner:String!,$name:String!,$ref:String!){
  repository(owner:$owner,name:$name){
    ref(qualifiedName:$ref){ target{ ... on Commit{ oid
      statusCheckRollup{ contexts(last:100){ nodes{
        __typename
        ... on CheckRun{ name status conclusion }
        ... on StatusContext{ context state }
      } } }
    } } }
  }
}`

// headRefOid is carried through under the SAME name `gh pr view` uses, so
// the one field serves both kinds of target and the runs lookup needs no
// special case. The query already selected `oid`; dropping it was what left
// the branch form unable to name the commit it was judging.
const branchJQ = `{mergeable:"MERGEABLE",state:"OPEN",` +
	`headRefOid:(.data.repository.ref.target.oid // ""),` +
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
const runJQ = `{mergeable:"MERGEABLE",state:"OPEN",statusCheckRollup:[{` +
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
				"--json", "name,status,conclusion",
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
	return reply.Runs, nil
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
	return p, nil
}

type waiter struct {
	fetch     fetcher
	interval  time.Duration
	timeout   time.Duration
	minChecks int
	out       io.Writer
	sleep     func(time.Duration)
	now       func() time.Time
}

// wait polls until every pull request has settled or the clock runs out.
// It returns true when all of them passed.
func (w waiter) wait(refs []pr) bool {
	start := w.now()
	left := make([]pr, len(refs))
	copy(left, refs)
	allOK := true
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
	for len(left) > 0 {
		var still []pr
		for _, p := range left {
			got, err := w.fetch(p)
			if err != nil {
				fails[p.String()]++
				if n := fails[p.String()]; n < fetchRetries && w.now().Sub(start) < w.timeout {
					fmt.Fprintf(w.out, "%s asking again (%d/%d): %v\n", p, n, fetchRetries, err)
					still = append(still, p)
					continue
				}
				fmt.Fprintf(w.out, "%s %v\n", p, err)
				allOK = false
				continue
			}
			fails[p.String()] = 0
			got.Repo, got.Number = p.Repo, p.Number
			switch v, why := assess(got, w.minChecks); v {
			case passed:
				fmt.Fprintf(w.out, "%s PASS %s\n", got, why)
			case failed:
				fmt.Fprintf(w.out, "%s FAIL %s\n", got, why)
				allOK = false
			default:
				if w.now().Sub(start) >= w.timeout {
					fmt.Fprintf(w.out, "%s TIMEOUT after %s — %s\n", got, w.timeout, why)
					allOK = false
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
	return allOK
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
	w := waiter{
		fetch:     ghFetcher(*ghBin),
		interval:  *interval,
		timeout:   *timeout,
		minChecks: *minChecks,
		out:       os.Stdout,
		sleep:     time.Sleep,
		now:       time.Now,
	}
	if !w.wait(refs) {
		os.Exit(1)
	}
}

// fetchRetries is how many times in a row a query may fail before the
// pull request is given up on. Three, at the poll interval, is about two
// minutes of weather — long enough for an API blip or a loaded machine,
// short enough that a genuinely broken setup is not waited out in silence.
const fetchRetries = 3
