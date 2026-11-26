// Package judge decides whether the checks and workflow runs of ONE commit
// are finished and green. It is the judge of fleet tool prwait, extracted so
// that every program asking "is this commit green" shares one implementation.
//
// The rules, each learned from a defect that was measured:
//
//   - workflow runs are read per head SHA, never per branch or pull request;
//   - only the LATEST run per (workflow, event) is judged: a retrigger replaces
//     a red run, and a manual dispatch never masks a red pull_request run
//     ([LatestRuns]);
//   - a check or run with no final answer yet is pending, never a pass;
//   - NEUTRAL and SKIPPED pass, CANCELLED does not ([Ok]);
//   - a run that finished with ZERO jobs never started: [RunState] makes it
//     red, never green, even when GitHub says "success"; an unread job count is
//     pending ([Run.Jobs] nil). prwait itself does not count jobs, and says so
//     by leaving Jobs nil and calling [RunEntries], not [RunState].
//
// The package never networks and never sleeps.
package judge

import (
	"fmt"
	"sort"
	"strings"
)

// Entry is one rollup entry and has one of two shapes that do NOT share a
// field. A CheckRun has status + conclusion. A StatusContext - the old commit
// status API, still used by plenty of third-party services - has neither: it
// has state. A waiter that tests `status != "COMPLETED"` therefore finds every
// StatusContext pending FOREVER.
type Entry struct {
	Typename   string `json:"__typename"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Context    string `json:"context"`
	State      string `json:"state"`
}

// Label is what to call this check in a report.
func (e Entry) Label() string {
	if e.Name != "" {
		return e.Name
	}
	if e.Context != "" {
		return e.Context
	}
	return "(unnamed)"
}

// Done reports whether this check has reached a final answer.
func (e Entry) Done() bool {
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

// Result is the final answer, from whichever field carries it.
func (e Entry) Result() string {
	if e.Conclusion != "" {
		return e.Conclusion
	}
	return e.State
}

// Run is one workflow run for a commit, as the REST actions API reports it.
// It is NOT a check run and does not appear in a check rollup.
type Run struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	HeadSHA string `json:"head_sha"`
	// Conclusion is empty (JSON null) until the run has finished.
	Conclusion string `json:"conclusion"`
	URL        string `json:"html_url"`
	// WorkflowID, Event and Number identify WHICH attempt at a workflow this
	// is. A commit can carry several runs of the same workflow - a failed one
	// and the retrigger that replaced it - and judging both would make a
	// commit that is now green unpassable forever. See [LatestRuns].
	WorkflowID int64  `json:"workflow_id"`
	Event      string `json:"event"`
	Number     int    `json:"run_number"`
	Created    string `json:"created_at"`
	// Jobs is the number of jobs the run created. nil means NOT COUNTED: the
	// caller did not read it, and [RunState] then reports a finished, passing
	// run as pending rather than guessing. It is never part of the REST reply.
	Jobs *int `json:"-"`
}

// LatestRuns keeps only the most recent run of each workflow FOR EACH EVENT,
// and drops the ones it superseded.
//
// Measured: two `pull_request` runs on go-tex/go-tex.github.io#239 failed to
// start; closing and reopening the pull request produced fresh, green runs for
// the SAME commit, and actions/runs?head_sha returns both. Judging every run
// refused a commit that was by then green.
//
// The key includes the event: a `workflow_dispatch` run that was GREEN would
// otherwise mask a RED `pull_request` run of the same workflow. A run started
// to investigate a failure must never be able to answer for it.
func LatestRuns(rs []Run) []Run {
	type key struct {
		id    int64
		event string
	}
	best := map[key]Run{}
	for _, r := range rs {
		k := key{r.WorkflowID, r.Event}
		cur, seen := best[k]
		switch {
		case !seen:
		case r.Number > cur.Number:
		case r.Number == cur.Number && r.Created > cur.Created:
		default:
			continue
		}
		best[k] = r
	}
	out := make([]Run, 0, len(best))
	for _, r := range best {
		out = append(out, r)
	}
	// Deterministic order, so a report reads the same twice.
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.Name != b.Name:
			return a.Name < b.Name
		case a.Event != b.Event:
			return a.Event < b.Event
		}
		return a.WorkflowID < b.WorkflowID
	})
	return out
}

// RunEntries renders workflow runs as check entries, so ONE set of rules
// judges both. The suffix keeps them apart in a report: "pages (workflow
// run)" and a check named "pages" are different objects.
//
// Upper-cased, because [Entry.Done] tests the GraphQL spelling COMPLETED while
// REST answers "completed". Left as REST spells it, a finished run would never
// look done and a wait would run to its timeout.
func RunEntries(rs []Run) []Entry {
	es := make([]Entry, 0, len(rs))
	for _, r := range rs {
		es = append(es, runEntry(r))
	}
	return es
}

func runEntry(r Run) Entry {
	name := r.Name
	if name == "" {
		name = "(unnamed)"
	}
	kind := "workflow"
	if r.Event != "" {
		// The EVENT is part of the identity: two runs of one workflow on one
		// commit, from different events, are different answers.
		kind = r.Event
	}
	return Entry{
		Typename:   "WorkflowRun",
		Name:       name + " (" + kind + " run)",
		Status:     strings.ToUpper(r.Status),
		Conclusion: strings.ToUpper(r.Conclusion),
	}
}

// Ok reports whether a finished check's answer (upper case) is one to accept.
//
// NEUTRAL and SKIPPED are passes: a lane that excluded itself did its job.
// CANCELLED is NOT - a cancelled lane ran nothing, and in this fleet that
// happens through concurrency starvation, which is precisely the case where a
// green summary would be a lie.
func Ok(result string) bool {
	switch result {
	case "SUCCESS", "NEUTRAL", "SKIPPED":
		return true
	default:
		return false
	}
}

// Trim keeps the first n elements and marks the cut.
func Trim(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(s[:n:n], "…")
}

// Tally counts the conclusions, so a pass says what it passed on rather than
// merely saying "ok".
func Tally(es []Entry) string {
	n := map[string]int{}
	for _, e := range es {
		n[e.Result()]++
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

// State is the answer for one check or run.
type State string

// The three answers.
const (
	Pending State = "PENDING"
	Red     State = "RED"
	Green   State = "GREEN"
)

// Classify judges one entry: not finished is pending, finished with no answer
// is pending (absence is never evidence of a pass), a refused answer is red,
// an accepted one green. The detail says why.
func Classify(e Entry) (State, string) {
	if !e.Done() {
		s := e.Status
		if s == "" {
			s = e.State
		}
		if s == "" {
			s = "(none)"
		}
		return Pending, "status " + strings.ToLower(s)
	}
	res := e.Result()
	if res == "" {
		return Pending, "completed with no conclusion"
	}
	if !Ok(res) {
		return Red, "conclusion " + strings.ToLower(res)
	}
	return Green, ""
}

// RunState judges one workflow run, counting its jobs: a run that finished
// green with ZERO jobs never started and is red; one whose job count was not
// read (Jobs nil) is pending.
func RunState(r Run) (State, string) {
	st, detail := Classify(runEntry(r))
	if st != Green {
		return st, detail
	}
	switch {
	case r.Jobs == nil:
		return Pending, "job count not read"
	case *r.Jobs == 0:
		return Red, "ran with zero jobs: it never started"
	}
	return Green, ""
}
