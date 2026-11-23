package main

import (
	"reflect"
	"testing"
)

// latestRuns builds its answer by ranging over a map, whose order Go
// randomises on every range, so its sort is what makes a report read the same
// twice. `mutsweep -sites all` found that the tie-break on Event was held by
// nothing: [return=true] and [return=false] on it both survived, because with
// two runs of one name the answer is merely RANDOM and a single call agrees
// with the expected order half the time. Repeating the call is what turns a
// coin that lands right into an assertion.
func TestRunsOfOneNameComeOutInEventOrderEveryTime(t *testing.T) {
	in := []wfrun{
		{Name: "pages", WorkflowID: 1, Event: "push", Number: 4},
		{Name: "pages", WorkflowID: 1, Event: "dynamic", Number: 3},
		{Name: "pages", WorkflowID: 1, Event: "pull_request", Number: 2},
		{Name: "pages", WorkflowID: 1, Event: "schedule", Number: 1},
		{Name: "build", WorkflowID: 2, Event: "push", Number: 9},
	}
	want := []string{"build/push", "pages/dynamic", "pages/pull_request", "pages/push", "pages/schedule"}
	for i := 0; i < 200; i++ {
		var got []string
		for _, r := range latestRuns(in) {
			got = append(got, r.Name+"/"+r.Event)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("call %d: order %q, want %q", i, got, want)
		}
	}
}
