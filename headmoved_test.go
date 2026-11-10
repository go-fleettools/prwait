// Copyright (c) the go-fleettools/prwait authors.
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// ⛔ A WAIT THAT STRADDLES A PUSH JUDGED NEITHER COMMIT.
//
// #17 made the verdict name the commit it judged, which is necessary and not
// sufficient: when the head moved mid-wait the loop went on polling the new
// one, and the line it printed was a true statement about work the caller had
// never asked about. The caller's only correct move was to notice by hand,
// kill the wait and start another — so that is what this does, and it says
// which two commits are involved.

// Full 40-character commits, built rather than typed: a SHA one character
// short is not a SHA to fullSHA, and the test would then be measuring its own
// typo instead of the behaviour.
var (
	shaOne = "e87aa0a" + strings.Repeat("1", 33)
	shaTwo = "2baff07" + strings.Repeat("2", 33)
)

// movingHead returns a fetcher that answers with the first SHA once, then with
// the second for ever — an unfinished check both times, so nothing but the move
// can end the wait.
func movingHead(p pr) func(pr) (pr, error) {
	calls := 0
	return func(pr) (pr, error) {
		calls++
		head := shaOne
		if calls > 1 {
			head = shaTwo
		}
		q := p
		q.HeadRefOid = head
		q.Mergeable = "MERGEABLE"
		q.Rollup = []entry{{Name: "ci", Status: "IN_PROGRESS"}}
		return q, nil
	}
}

func waiterOver(fetch func(pr) (pr, error), out *bytes.Buffer) waiter {
	return waiter{
		fetch:     fetch,
		interval:  time.Millisecond,
		timeout:   time.Minute,
		minChecks: 1,
		out:       out,
		sleep:     func(time.Duration) {},
		now:       time.Now,
	}
}

func TestAHeadThatMovesMidWaitIsRefusedNotFollowed(t *testing.T) {
	ref := pr{Repo: "o/r", Number: 752}
	var out bytes.Buffer
	ok := waiterOver(movingHead(ref), &out).wait([]pr{ref})

	if ok {
		t.Error("a wait whose subject changed under it reported success")
	}
	got := out.String()
	if !strings.Contains(got, "MOVED") {
		t.Errorf("the move is not reported at all:\n%s", got)
	}
	// BOTH commits, because "it moved" without saying from what leaves the
	// caller doing the same lookup by hand that this exists to spare them.
	for _, want := range []string{short(shaOne), short(shaTwo)} {
		if !strings.Contains(got, want) {
			t.Errorf("the report does not name %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, "PASS") {
		t.Errorf("a moved head must not also print a verdict:\n%s", got)
	}
}

// ⛔ The control. A head that does NOT move must still reach its verdict —
// without this, "refuse on a move" could be satisfied by refusing everything,
// and the suite would be green on a tool that never passes anything again.
func TestAStillHeadStillReachesItsVerdict(t *testing.T) {
	ref := pr{Repo: "o/r", Number: 750}
	var out bytes.Buffer
	ok := waiterOver(func(pr) (pr, error) {
		q := ref
		q.HeadRefOid = shaOne
		q.Mergeable = "MERGEABLE"
		q.Rollup = []entry{{Name: "ci", Status: "COMPLETED", Conclusion: "SUCCESS"}}
		return q, nil
	}, &out).wait([]pr{ref})

	if !ok {
		t.Errorf("a settled, green, unmoved head did not pass:\n%s", out.String())
	}
	if got := out.String(); !strings.Contains(got, "PASS") || strings.Contains(got, "MOVED") {
		t.Errorf("verdict on an unmoved head:\n%s", got)
	}
}

// A branch and a run are waited on the same way, and a release gate waiting on
// the default branch is exactly where answering about a LATER commit would do
// the most damage: the tag would land on something nobody judged.
func TestABranchHeadThatMovesIsRefusedToo(t *testing.T) {
	ref := pr{Repo: "o/r", Branch: "main"}
	var out bytes.Buffer
	ok := waiterOver(movingHead(ref), &out).wait([]pr{ref})

	if ok {
		t.Error("a branch that moved under the wait reported success")
	}
	if got := out.String(); !strings.Contains(got, "MOVED") {
		t.Errorf("a moved branch head is not reported:\n%s", got)
	}
}

// An empty or partial SHA is not a move. gh answers "" for a surface it cannot
// read, and treating that as a change would refuse every wait that hit one bad
// poll — turning weather into a verdict, which is the mistake the retry loop
// above exists to avoid.
func TestAnUnreadableHeadIsNotAMove(t *testing.T) {
	ref := pr{Repo: "o/r", Number: 9}
	calls := 0
	var out bytes.Buffer
	ok := waiterOver(func(pr) (pr, error) {
		calls++
		q := ref
		q.Mergeable = "MERGEABLE"
		switch calls {
		case 1:
			q.HeadRefOid = shaOne
			q.Rollup = []entry{{Name: "ci", Status: "IN_PROGRESS"}}
		case 2:
			q.HeadRefOid = "" // unreadable this time round
			q.Rollup = []entry{{Name: "ci", Status: "IN_PROGRESS"}}
		default:
			q.HeadRefOid = shaOne
			q.Rollup = []entry{{Name: "ci", Status: "COMPLETED", Conclusion: "SUCCESS"}}
		}
		return q, nil
	}, &out).wait([]pr{ref})

	if !ok {
		t.Errorf("a blank SHA on one poll was read as a moved head:\n%s", out.String())
	}
	if got := out.String(); strings.Contains(got, "MOVED") {
		t.Errorf("an unreadable head was reported as a move:\n%s", got)
	}
}
