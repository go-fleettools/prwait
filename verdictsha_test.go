// Copyright (c) 2026 the go-fleettools/prwait authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file at the root of this repository.

package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// ⛔ A VERDICT MUST SAY WHAT IT IS A VERDICT ABOUT.
//
// The line used to read `owner/repo#230 PASS SUCCESS:9`, which is true of SOME
// commit. A wait that straddles a push — start it, push again, read the result
// — cannot be acted on from that line alone: the head it began with and the
// head it ended with are different pieces of work, and nothing said which was
// judged. Measured 2026-11-10 on exactly that sequence (#16).
func TestTheVerdictNamesTheCommitItJudged(t *testing.T) {
	const sha = "b33f79daaedf5394d6ec826b745b049211b399d0"
	for _, c := range []struct {
		name string
		p    pr
		want string
	}{
		{
			name: "a pass",
			p: pr{Repo: "o/r", Number: 230, Mergeable: "MERGEABLE", HeadRefOid: sha,
				Rollup: []entry{{Name: "ci", Status: "COMPLETED", Conclusion: "SUCCESS"}}},
			want: "PASS",
		},
		{
			name: "a failure",
			p: pr{Repo: "o/r", Number: 230, Mergeable: "MERGEABLE", HeadRefOid: sha,
				Rollup: []entry{{Name: "ci", Status: "COMPLETED", Conclusion: "FAILURE"}}},
			want: "FAIL",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out strings.Builder
			w := waiter{
				fetch:     func(pr) (pr, error) { return c.p, nil },
				interval:  time.Millisecond,
				timeout:   time.Minute,
				minChecks: 1,
				out:       &out,
				sleep:     func(time.Duration) {},
				now:       time.Now,
			}
			w.wait([]pr{{Repo: "o/r", Number: 230}})
			got := out.String()
			if !strings.Contains(got, "@b33f79d") {
				t.Errorf("the verdict does not name the commit it judged:\n%s", got)
			}
			if !strings.Contains(got, c.want) {
				t.Errorf("want a %s line:\n%s", c.want, got)
			}
		})
	}
}

// A timeout names it too — that is the verdict most likely to be read later,
// by someone who no longer knows what the head was.
func TestATimeoutNamesTheCommit(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	var out strings.Builder
	w := waiter{
		fetch: func(pr) (pr, error) {
			return pr{Repo: "o/r", Number: 7, Mergeable: "MERGEABLE", HeadRefOid: sha,
				Rollup: []entry{{Name: "ci", Status: "IN_PROGRESS"}}}, nil
		},
		interval:  time.Millisecond,
		timeout:   -1, // already expired
		minChecks: 1,
		out:       &out,
		sleep:     func(time.Duration) {},
		now:       time.Now,
	}
	w.wait([]pr{{Repo: "o/r", Number: 7}})
	got := out.String()
	if !strings.Contains(got, "TIMEOUT") {
		t.Fatalf("expected a timeout:\n%s", got)
	}
	if !strings.Contains(got, "@0123456") { // short() keeps seven characters
		t.Errorf("the timeout does not name the commit:\n%s", got)
	}
}

// ⛔ And a target with NO commit to name says nothing rather than something
// wrong. An empty or malformed oid must not become "@" or "@       " — a line
// that looks like it named a commit and did not is worse than one that never
// claimed to.
func TestNoCommitIsNamedWhenThereIsNone(t *testing.T) {
	for _, oid := range []string{"", "b33f79d", "not-a-sha", strings.Repeat("z", 40)} {
		if got := at(pr{HeadRefOid: oid}); got != "" {
			t.Errorf("oid %q rendered as %q, want nothing", oid, got)
		}
	}
	if got := at(pr{HeadRefOid: "b33f79daaedf5394d6ec826b745b049211b399d0"}); got != " @b33f79d" {
		t.Errorf("a real commit rendered as %q", got)
	}
}

// The RUN form carries a commit too, and it had to be asked for: a run has a
// head SHA and `gh run view` was never told to return it.
func TestTheRunQueryAsksForTheHeadCommit(t *testing.T) {
	if !strings.Contains(runJQ, "headRefOid") {
		t.Error("runJQ no longer carries the head commit, so a run's verdict cannot name one")
	}
	f := ghFetcher(os.Args[0])
	t.Setenv("PRWAIT_HELPER", "1")
	p, err := f(pr{Repo: "o/r", Run: 42})
	if err != nil {
		t.Fatalf("ghFetcher: %v", err)
	}
	// The helper echoes its arguments into the check name, which is how a
	// dropped --json field shows up here rather than in production.
	if len(p.Rollup) == 0 || !strings.Contains(p.Rollup[0].Name, "headSha") {
		t.Errorf("gh was not asked for headSha: %+v", p.Rollup)
	}
}
