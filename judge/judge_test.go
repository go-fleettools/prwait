package judge

import (
	"reflect"
	"testing"
)

func n(i int) *int { return &i }

func TestEntryLabelResultDone(t *testing.T) {
	if got := (Entry{Name: "a", Context: "b"}).Label(); got != "a" {
		t.Errorf("name must outrank context, got %q", got)
	}
	if got := (Entry{Context: "b"}).Label(); got != "b" {
		t.Errorf("got %q", got)
	}
	if got := (Entry{}).Label(); got != "(unnamed)" {
		t.Errorf("got %q", got)
	}
	if got := (Entry{Conclusion: "FAILURE", State: "SUCCESS"}).Result(); got != "FAILURE" {
		t.Errorf("conclusion must outrank state, got %q", got)
	}
	if got := (Entry{State: "ERROR"}).Result(); got != "ERROR" {
		t.Errorf("got %q", got)
	}
	cases := []struct {
		e    Entry
		want bool
	}{
		{Entry{Status: "COMPLETED"}, true},
		{Entry{Status: "IN_PROGRESS"}, false},
		{Entry{Status: "QUEUED", State: "SUCCESS"}, false},
		{Entry{Status: "COMPLETED", State: "PENDING"}, true},
		{Entry{State: "SUCCESS"}, true},
		{Entry{State: "FAILURE"}, true},
		{Entry{State: "PENDING"}, false},
		{Entry{State: "EXPECTED"}, false},
		{Entry{}, false},
	}
	for _, c := range cases {
		if got := c.e.Done(); got != c.want {
			t.Errorf("%+v: Done = %v, want %v", c.e, got, c.want)
		}
	}
}

func TestOk(t *testing.T) {
	for _, s := range []string{"SUCCESS", "NEUTRAL", "SKIPPED"} {
		if !Ok(s) {
			t.Errorf("%s must pass", s)
		}
	}
	for _, s := range []string{"CANCELLED", "FAILURE", "TIMED_OUT", "STALE", "", "success"} {
		if Ok(s) {
			t.Errorf("%q must not pass", s)
		}
	}
}

func TestTrimAndTally(t *testing.T) {
	if got := Trim([]string{"a", "b"}, 2); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("got %v", got)
	}
	if got := Trim([]string{"a", "b", "c"}, 2); !reflect.DeepEqual(got, []string{"a", "b", "…"}) {
		t.Errorf("got %v", got)
	}
	es := []Entry{{Conclusion: "SUCCESS"}, {Conclusion: "SUCCESS"}, {State: "FAILURE"}, {Conclusion: "SKIPPED"}}
	if got := Tally(es); got != "FAILURE:1 SKIPPED:1 SUCCESS:2" {
		t.Errorf("got %q", got)
	}
	if got := Tally(nil); got != "" {
		t.Errorf("got %q", got)
	}
}

// A retrigger replaces the red run it superseded.
func TestLatestRunsKeepsTheRetrigger(t *testing.T) {
	got := LatestRuns([]Run{
		{ID: 1, Name: "ci", WorkflowID: 7, Event: "pull_request", Number: 1, Conclusion: "failure"},
		{ID: 2, Name: "ci", WorkflowID: 7, Event: "pull_request", Number: 2, Conclusion: "success"},
		{ID: 3, Name: "ci", WorkflowID: 7, Event: "pull_request", Number: 0, Conclusion: "cancelled"},
	})
	if len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("got %+v, want the run numbered 2", got)
	}
}

// Same number: the later created_at wins, in either input order.
func TestLatestRunsBreaksANumberTieByCreation(t *testing.T) {
	a := Run{ID: 1, WorkflowID: 7, Event: "push", Number: 3, Created: "2026-01-01T00:00:00Z"}
	b := Run{ID: 2, WorkflowID: 7, Event: "push", Number: 3, Created: "2026-01-02T00:00:00Z"}
	for _, in := range [][]Run{{a, b}, {b, a}} {
		got := LatestRuns(in)
		if len(got) != 1 || got[0].ID != 2 {
			t.Fatalf("got %+v, want ID 2", got)
		}
	}
}

// A dispatch never masks a red pull_request run of the same workflow.
func TestLatestRunsKeysOnTheEvent(t *testing.T) {
	got := LatestRuns([]Run{
		{ID: 1, Name: "pg", WorkflowID: 7, Event: "pull_request", Number: 1, Conclusion: "failure"},
		{ID: 2, Name: "pg", WorkflowID: 7, Event: "workflow_dispatch", Number: 2, Conclusion: "success"},
	})
	if len(got) != 2 {
		t.Fatalf("got %+v, want both kinds kept", got)
	}
}

// Two different workflows with the same name and event stay apart.
func TestLatestRunsKeysOnTheWorkflow(t *testing.T) {
	got := LatestRuns([]Run{
		{ID: 1, Name: "ci", WorkflowID: 7, Event: "push", Number: 1},
		{ID: 2, Name: "ci", WorkflowID: 8, Event: "push", Number: 1},
	})
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("got %+v, want both, by workflow id", got)
	}
}

func TestLatestRunsOrderIsByNameThenEvent(t *testing.T) {
	got := LatestRuns([]Run{
		{ID: 1, Name: "b", WorkflowID: 1, Event: "push"},
		{ID: 2, Name: "a", WorkflowID: 2, Event: "push"},
		{ID: 3, Name: "a", WorkflowID: 3, Event: "dispatch"},
	})
	ids := []int64{got[0].ID, got[1].ID, got[2].ID}
	if !reflect.DeepEqual(ids, []int64{3, 2, 1}) {
		t.Fatalf("order %v, want [3 2 1]", ids)
	}
}

// The order is a total order over (name, event, workflow id), whatever order
// the map iterates in: a full grid, shuffled by the map, repeated.
func TestLatestRunsOrderIsTotal(t *testing.T) {
	var in, want []Run
	id := int64(0)
	for _, name := range []string{"a", "b", "c"} {
		for _, ev := range []string{"pull_request", "push", "schedule"} {
			for wf := int64(1); wf <= 3; wf++ {
				id++
				r := Run{ID: id, Name: name, Event: ev, WorkflowID: id*10 + wf}
				want = append(want, r)
				in = append(in, r)
			}
		}
	}
	// A distinct workflow per run, so nothing is superseded; the grid's own
	// order is (name, event, workflow id) already when ids ascend with wf.
	for i := 0; i < 40; i++ {
		got := LatestRuns(in)
		if len(got) != len(want) {
			t.Fatalf("got %d runs, want %d", len(got), len(want))
		}
		for j := 1; j < len(got); j++ {
			a, b := got[j-1], got[j]
			less := a.Name < b.Name ||
				(a.Name == b.Name && a.Event < b.Event) ||
				(a.Name == b.Name && a.Event == b.Event && a.WorkflowID < b.WorkflowID)
			if !less {
				t.Fatalf("round %d: %+v must not come before %+v", i, a, b)
			}
		}
	}
}

func TestLatestRunsOfNothingIsEmpty(t *testing.T) {
	if got := LatestRuns(nil); got == nil || len(got) != 0 {
		t.Fatalf("got %#v", got)
	}
}

func TestRunEntriesUpperCaseAndNameTheEvent(t *testing.T) {
	es := RunEntries([]Run{
		{Name: "pages", Event: "pull_request", Status: "completed", Conclusion: "success"},
		{Event: "", Status: "in_progress"},
	})
	want := []Entry{
		{Typename: "WorkflowRun", Name: "pages (pull_request run)", Status: "COMPLETED", Conclusion: "SUCCESS"},
		{Typename: "WorkflowRun", Name: "(unnamed) (workflow run)", Status: "IN_PROGRESS"},
	}
	if !reflect.DeepEqual(es, want) {
		t.Fatalf("got %+v", es)
	}
	if !es[0].Done() || es[1].Done() {
		t.Fatal("REST spelling must still read as done / not done")
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		e      Entry
		state  State
		detail string
	}{
		{Entry{Status: "IN_PROGRESS"}, Pending, "status in_progress"},
		{Entry{State: "PENDING"}, Pending, "status pending"},
		{Entry{}, Pending, "status (none)"},
		{Entry{Status: "COMPLETED"}, Pending, "completed with no conclusion"},
		{Entry{Status: "COMPLETED", Conclusion: "FAILURE"}, Red, "conclusion failure"},
		{Entry{Status: "COMPLETED", Conclusion: "CANCELLED"}, Red, "conclusion cancelled"},
		{Entry{Status: "COMPLETED", Conclusion: "SUCCESS"}, Green, ""},
		{Entry{Status: "COMPLETED", Conclusion: "NEUTRAL"}, Green, ""},
		{Entry{Status: "COMPLETED", Conclusion: "SKIPPED"}, Green, ""},
		{Entry{State: "SUCCESS"}, Green, ""},
		{Entry{State: "ERROR"}, Red, "conclusion error"},
	}
	for _, c := range cases {
		st, d := Classify(c.e)
		if st != c.state || d != c.detail {
			t.Errorf("%+v: got %s %q, want %s %q", c.e, st, d, c.state, c.detail)
		}
	}
}

func TestRunStateCountsJobs(t *testing.T) {
	cases := []struct {
		name   string
		r      Run
		state  State
		detail string
	}{
		{"zero jobs is red even when success", Run{Status: "completed", Conclusion: "success", Jobs: n(0)}, Red, "ran with zero jobs: it never started"},
		{"unread job count is pending", Run{Status: "completed", Conclusion: "success"}, Pending, "job count not read"},
		{"one job is green", Run{Status: "completed", Conclusion: "success", Jobs: n(1)}, Green, ""},
		{"skipped with jobs is green", Run{Status: "completed", Conclusion: "skipped", Jobs: n(2)}, Green, ""},
		{"failure is red whatever the jobs", Run{Status: "completed", Conclusion: "failure", Jobs: n(3)}, Red, "conclusion failure"},
		{"null conclusion is pending", Run{Status: "completed", Jobs: n(3)}, Pending, "completed with no conclusion"},
		{"running is pending", Run{Status: "in_progress", Jobs: n(3)}, Pending, "status in_progress"},
		{"queued with zero jobs is pending, not red", Run{Status: "queued", Jobs: n(0)}, Pending, "status queued"},
	}
	for _, c := range cases {
		st, d := RunState(c.r)
		if st != c.state || d != c.detail {
			t.Errorf("%s: got %s %q, want %s %q", c.name, st, d, c.state, c.detail)
		}
	}
}
