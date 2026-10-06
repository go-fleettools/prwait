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

const usage = `prwait — wait for a pull request's checks, and refuse to call an empty list a pass

usage:
  prwait [-repo OWNER/REPO] [flags] <pr>...

a <pr> is either a number (with -repo) or OWNER/REPO#NUMBER.

flags:
  -repo OWNER/REPO   repository for bare numbers
  -interval 45s      how often to ask
  -timeout 30m       give up after this, per run
  -min-checks 1      how many checks must EXIST before a pass is possible
  -gh gh             the gh binary to ask

exit status:
  0  every pull request finished, with checks, all green
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

type pr struct {
	Repo      string
	Number    int
	Mergeable string  `json:"mergeable"`
	State     string  `json:"state"`
	Rollup    []entry `json:"statusCheckRollup"`
}

func (p pr) String() string { return fmt.Sprintf("%s#%d", p.Repo, p.Number) }

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
		return waiting, fmt.Sprintf("%d checks, want at least %d", len(p.Rollup), minChecks)
	}
	var pending []string
	for _, e := range p.Rollup {
		if !e.done() {
			pending = append(pending, e.label())
		}
	}
	if len(pending) > 0 {
		return waiting, fmt.Sprintf("%d of %d running (%s)", len(pending), len(p.Rollup), strings.Join(trim(pending, 3), ", "))
	}
	var bad []string
	for _, e := range p.Rollup {
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
	return passed, tally(p.Rollup)
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
func parseRef(arg, defRepo string) (string, int, error) {
	repo, num := defRepo, arg
	if i := strings.LastIndex(arg, "#"); i >= 0 {
		repo, num = arg[:i], arg[i+1:]
	}
	if repo == "" {
		return "", 0, fmt.Errorf("%s: no repository — pass -repo OWNER/REPO or write OWNER/REPO#%s", arg, arg)
	}
	if strings.Count(repo, "/") != 1 || strings.HasPrefix(repo, "/") || strings.HasSuffix(repo, "/") {
		return "", 0, fmt.Errorf("%s: %q is not OWNER/REPO", arg, repo)
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return "", 0, fmt.Errorf("%s: %q is not a pull request number", arg, num)
	}
	return repo, n, nil
}

// fetcher asks for one observation. It is a parameter so the tests can run
// every rule without a network and without gh.
type fetcher func(repo string, num int) (pr, error)

func ghFetcher(bin string) fetcher {
	return func(repo string, num int) (pr, error) {
		cmd := exec.Command(bin, "pr", "view", strconv.Itoa(num),
			"--repo", repo,
			"--json", "mergeable,state,statusCheckRollup")
		var errb strings.Builder
		cmd.Stderr = &errb
		out, err := cmd.Output()
		if err != nil {
			msg := strings.TrimSpace(errb.String())
			if msg == "" {
				msg = err.Error()
			}
			return pr{}, fmt.Errorf("gh pr view %s#%d: %s", repo, num, msg)
		}
		return decode(out, repo, num)
	}
}

func decode(out []byte, repo string, num int) (pr, error) {
	var p pr
	if err := json.Unmarshal(out, &p); err != nil {
		return pr{}, fmt.Errorf("%s#%d: cannot read gh's answer: %w", repo, num, err)
	}
	p.Repo, p.Number = repo, num
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
	for len(left) > 0 {
		var still []pr
		for _, p := range left {
			got, err := w.fetch(p.Repo, p.Number)
			if err != nil {
				fmt.Fprintf(w.out, "%s %v\n", p, err)
				allOK = false
				continue
			}
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
		r, n, err := parseRef(arg, *repo)
		if err != nil {
			fmt.Fprintln(os.Stderr, "prwait:", err)
			os.Exit(2)
		}
		refs = append(refs, pr{Repo: r, Number: n})
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
