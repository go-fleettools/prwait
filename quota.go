package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ⛔⛔ A QUOTA IS NOT A VERDICT, AND THIS FILE EXISTS BECAUSE IT WAS READ AS ONE.
//
// Measured 2026-11-15: the account's REST quota reached remaining:0 twice in
// one day, and both times prwait printed
//
//	gh: API rate limit exceeded for user ID 11405852. (HTTP 403)
//
// where its verdict goes, under exit 1 — the same status as a red check. A
// caller reading only the status took "nothing could be measured" for "the
// pull request is red". That is the defect this whole tool is built against,
// wearing its least recognisable face: an UNREAD answer presenting itself as
// a NEGATIVE one.
//
// It was made worse by the retry. A failed query is weather here (see
// fetchRetries), so a quota 403 was asked again three times at the poll
// interval — three more refusals, each one spending a call against the very
// limit that refused it, and each printed as "asking again". Weather has no
// reset time. A quota has one, advertised, and GET /rate_limit does not count
// against the quota it reports, so there is no excuse for guessing.
//
// So a quota gets: its own classification, its own hold (until the advertised
// reset, when that falls inside the timeout), and its own exit status.
//
// Refs go-fleettools/prwait#20.

// quotaStatus is the exit status for a wait that was stopped by the API quota
// rather than by anything it was asked about.
//
// ⛔ It must differ from 1. Exit 1 means "measured, and it is not green"; this
// means "not measured". Sharing a status is what let a quota read as a red
// check for the whole of 2026-11-15.
const quotaStatus = 3

// secondaryBackoff is the FIRST hold for a SECONDARY rate limit, doubled on
// each consecutive refusal of the same target up to secondaryBackoffCap.
//
// ⛔ ONE MINUTE BECAUSE THAT IS WHAT THE DOCUMENTATION SAYS, and the first
// draft of this file said "a few minutes" — a plausible sentence, attributed
// to GitHub, that GitHub does not contain. Read 2026-11-21 at
// docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api:
//
//	"If the retry-after response header is present, you should not retry
//	your request until after that many seconds has elapsed. If the
//	x-ratelimit-remaining header is 0, you should not retry your request
//	until after the time, in UTC epoch seconds, specified by the
//	x-ratelimit-reset header. Otherwise, wait for at least one minute
//	before retrying."
//
// The same page then says to increase the wait on repeated failures, which is
// the doubling below. The headers it names are not visible here — gh gives
// this tool a message on stderr, not a response — so the third branch is the
// one that applies, and the reset for the PRIMARY limit comes from
// /rate_limit instead.
const secondaryBackoff = time.Minute

// secondaryBackoffCap bounds the doubling, so a long wait holds at a sensible
// cadence instead of sleeping past its own deadline in one step.
const secondaryBackoffCap = 8 * time.Minute

// resetMargin is added to an advertised reset before asking again. The reset
// is a second-granularity timestamp from a clock that is not this machine's;
// arriving one second early spends a call to be told the same thing.
const resetMargin = 5 * time.Second

// resource is one quota as GET /rate_limit reports it.
type resource struct {
	Limit     int
	Remaining int
	Reset     time.Time
}

// budget is what GET /rate_limit says about the two budgets this tool spends.
//
// ⛔ REST AND GRAPHQL ARE SEPARATE BUDGETS, and the targets here do not spend
// the same one: a branch is read with `gh api graphql`, the workflow runs with
// a REST path. Treating them as one number reports a quota that is fine as
// spent, or the reverse. They are carried apart for that reason and never
// summed.
type budget struct {
	Core    resource
	GraphQL resource
}

// budgetReader reads the budget. It is a parameter, like fetcher, so every
// rule below can be tested without a network and without gh.
type budgetReader func() (budget, error)

type rateResource struct {
	Limit     int   `json:"limit"`
	Remaining int   `json:"remaining"`
	Reset     int64 `json:"reset"`
}

type rateReply struct {
	Resources struct {
		Core    rateResource `json:"core"`
		GraphQL rateResource `json:"graphql"`
	} `json:"resources"`
}

// ghBudget reads GET /rate_limit through gh.
//
// ⚠ IT IS FREE OF THE PRIMARY LIMIT ONLY, which is less than this file first
// claimed. GitHub's own words, read 2026-11-21:
//
//	"Calling this endpoint does not count against your primary rate limit,
//	but it can count against your secondary rate limit."
//
// So it may be asked at the very moment the primary quota is spent — that is
// what makes the reset readable at all — but it is NOT something to poll. It
// is read once before the wait starts and once per refusal, never per poll:
// on a secondary limit, a call spent asking about the limit is a call spent
// against it.
func ghBudget(bin string) budgetReader {
	return func() (budget, error) {
		cmd := exec.Command(bin, "api", "rate_limit")
		var errb strings.Builder
		cmd.Stderr = &errb
		out, err := cmd.Output()
		if err != nil {
			msg := strings.TrimSpace(errb.String())
			if msg == "" {
				msg = err.Error()
			}
			return budget{}, fmt.Errorf("gh: rate_limit: %s", msg)
		}
		return decodeBudget(out)
	}
}

func decodeBudget(out []byte) (budget, error) {
	var r rateReply
	if err := json.Unmarshal(out, &r); err != nil {
		return budget{}, fmt.Errorf("cannot read gh's rate_limit answer: %w", err)
	}
	conv := func(x rateResource) resource {
		res := resource{Limit: x.Limit, Remaining: x.Remaining}
		if x.Reset > 0 {
			res.Reset = time.Unix(x.Reset, 0)
		}
		return res
	}
	return budget{Core: conv(r.Resources.Core), GraphQL: conv(r.Resources.GraphQL)}, nil
}

// quotaKind is which of the two refusals that look alike was hit. They arrive
// as the same HTTP 403 and the same words "rate limit", and they behave
// differently: one has an advertised reset, the other has none and is
// invisible to the instrument.
type quotaKind int

const (
	notQuota quotaKind = iota
	primaryQuota
	secondaryQuota
)

// quotaHit is what was learned about a refusal. Every field is something that
// was READ; nothing here is inferred except byElimination, which says so.
type quotaHit struct {
	kind quotaKind
	// which names the budget that was found spent, for the report.
	which string
	// reset is the advertised reset, or the zero time when nothing
	// authoritative advertised one.
	//
	// ⛔ ZERO MEANS UNKNOWN AND IS NEVER FILLED IN FROM THE OTHER BUDGET'S
	// RESET. A secondary limit has no reset; printing core's would be a
	// promise about a limit that did not refuse the call.
	reset     time.Time
	remaining int
	limit     int
	// byElimination records that the kind was deduced rather than read: gh
	// said "rate limit", the instrument reported room, and a secondary limit
	// is the only thing that fits. The report says so in those words.
	byElimination bool
	// budgetErr is set when the instrument itself could not be read, so the
	// report can admit that instead of claiming a reset.
	budgetErr error
}

// looksLikeQuota reports whether gh's complaint is a rate limit at all.
//
// It matches on gh's message because gh is the seam: this tool never sees a
// status code, only the text gh wrote to stderr. The three wordings below are
// the ones GitHub uses — the primary limit, the secondary limit, and the
// older "abuse detection mechanism" spelling, which is still what some
// endpoints answer.
func looksLikeQuota(msg string) bool {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "rate limit"):
		return true
	case strings.Contains(m, "too many requests"):
		return true
	case strings.Contains(m, "abuse detection"):
		return true
	}
	return false
}

// looksSecondary reports whether GitHub NAMED the secondary limit. Its own
// words are definitive; everything else is deduced.
func looksSecondary(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "secondary rate limit") || strings.Contains(m, "abuse detection")
}

// classifyQuota decides what a failed query was.
//
// ⛔ IT IS DELIBERATELY UNWILLING TO INVENT A RESET. /rate_limit cannot see the
// secondary limit — measured 2026-10-10, 403 on every call while rate_limit
// read 5000 of 5000 — so a check can only ever confirm the primary limit. When
// the instrument reports room and gh still refused, the kind is deduced and the
// report says it was deduced.
func classifyQuota(err error, read budgetReader) quotaHit {
	if err == nil {
		return quotaHit{}
	}
	msg := err.Error()
	if !looksLikeQuota(msg) {
		return quotaHit{}
	}
	named := looksSecondary(msg)
	if read == nil {
		if named {
			return quotaHit{kind: secondaryQuota}
		}
		return quotaHit{kind: primaryQuota}
	}
	b, berr := read()
	if berr != nil {
		// The instrument is unreadable. That is reported as what it is; a
		// refusal is not a verdict, and neither is an unread answer.
		if named {
			return quotaHit{kind: secondaryQuota, budgetErr: berr}
		}
		return quotaHit{kind: primaryQuota, budgetErr: berr}
	}
	if named {
		// GitHub named it. The primary numbers are still reported, because
		// "3748 of 5000 left" is what tells the reader this is NOT the limit
		// they would have guessed — but no reset is claimed for it.
		return quotaHit{
			kind:      secondaryQuota,
			which:     "REST",
			remaining: b.Core.Remaining,
			limit:     b.Core.Limit,
		}
	}
	switch {
	case b.Core.Remaining == 0:
		return quotaHit{kind: primaryQuota, which: "REST", reset: b.Core.Reset, remaining: 0, limit: b.Core.Limit}
	case b.GraphQL.Remaining == 0:
		return quotaHit{kind: primaryQuota, which: "GraphQL", reset: b.GraphQL.Reset, remaining: 0, limit: b.GraphQL.Limit}
	}
	return quotaHit{
		kind:          secondaryQuota,
		which:         "REST",
		remaining:     b.Core.Remaining,
		limit:         b.Core.Limit,
		byElimination: true,
	}
}

// lines is what the reader is told. It names the target, says plainly that
// nothing about it was measured, and gives the reset when one was read.
func (q quotaHit) lines(target string, now time.Time) []string {
	switch q.kind {
	case primaryQuota:
		head := fmt.Sprintf("%s QUOTA — not a verdict: nothing about this target was measured.", target)
		var detail string
		switch {
		case q.budgetErr != nil:
			detail = fmt.Sprintf("  ⛔ GitHub refused the call for the rate limit, and /rate_limit could not be read either (%v), so no reset can be quoted here.", q.budgetErr)
		case !q.reset.IsZero():
			detail = fmt.Sprintf("  ⛔ the per-user %s quota is spent (%d of %d left). It resets at %s, in %s.",
				q.which, q.remaining, q.limit, q.reset.Local().Format("15:04 MST"), until(q.reset, now))
		default:
			detail = fmt.Sprintf("  ⛔ the per-user %s quota is spent (%d of %d left), and no reset was advertised.", q.which, q.remaining, q.limit)
		}
		return []string{head, detail, "     The quota is per USER, not per token, so every session on this machine shares it — Renovate included."}
	case secondaryQuota:
		head := fmt.Sprintf("%s QUOTA — not a verdict: nothing about this target was measured.", target)
		var detail string
		switch {
		case q.byElimination:
			detail = fmt.Sprintf("  ⛔ GitHub refused the call for a rate limit while /rate_limit reports %d of %d %s calls left, so the primary quota is not the one that refused: a SECONDARY limit is the only thing that fits. Deduced, not read.",
				q.remaining, q.limit, q.which)
		case q.budgetErr != nil:
			detail = fmt.Sprintf("  ⛔ GitHub named a SECONDARY rate limit. /rate_limit could not be read (%v), and it could not see this limit anyway.", q.budgetErr)
		case q.limit > 0:
			detail = fmt.Sprintf("  ⛔ GitHub named a SECONDARY rate limit. /rate_limit cannot see it: it reports %d of %d %s calls still left.",
				q.remaining, q.limit, q.which)
		default:
			detail = "  ⛔ GitHub named a SECONDARY rate limit, which /rate_limit cannot see."
		}
		return []string{head, detail, "     GitHub advertises no reset for it — \"There is not a way to check the status of your secondary rate limit\" — so the hold starts at the documented minute and doubles."}
	}
	return nil
}

// until renders how far off a reset is, for a reader deciding whether to wait.
//
// ⚠ A RESET MORE THAN AN HOUR OUT IS NOT A RESET. GitHub's primary window is
// an hour, so a larger figure is a clock or a fixture, not a quota — and
// "in 56864h13m47s" is a line no reader can act on. Printed as the suspect
// measurement it is, rather than dressed up as a schedule.
func until(reset, now time.Time) string {
	d := reset.Sub(now)
	switch {
	case d < 0:
		return "the past — it has already reset"
	case d > resetWindowCeiling:
		return fmt.Sprintf("%s, which is longer than any GitHub reset window — read that as a suspect clock, not a wait", d.Round(time.Minute))
	}
	return d.Round(time.Second).String()
}

// resetWindowCeiling is the longest a reported reset can be and still be
// credible. GitHub's primary limit is per hour; double it, so a sane reset is
// never called suspect.
const resetWindowCeiling = 2 * time.Hour

// hold says how long to wait before asking again, and whether waiting can
// work at all before the deadline.
//
// ⛔ A QUOTA IS REFUSED EARLY, NEVER RETRIED LATE. It has a known reset, so
// there is nothing to learn from asking again before it: every such call is
// spent to be told the same thing, against the limit that is already refusing.
// When the reset falls outside the wait's own deadline the answer is to stop
// and say so, not to spend the remaining timeout discovering it.
// attempt is how many times in a row this target has been refused for the
// quota, counted from 1, and only the secondary limit uses it: the primary
// limit advertises a reset, and doubling a wait for a clock you can read
// would be guesswork laid over a measurement.
func (q quotaHit) hold(now, deadline time.Time, attempt int) (time.Duration, bool) {
	var d time.Duration
	switch q.kind {
	case primaryQuota:
		if q.reset.IsZero() {
			// Nothing was advertised. Holding blind would be a guess
			// dressed as a schedule.
			return 0, false
		}
		d = q.reset.Sub(now) + resetMargin
		if d < resetMargin {
			d = resetMargin
		}
	case secondaryQuota:
		d = backoffFor(attempt)
	default:
		return 0, false
	}
	if !now.Add(d).Before(deadline) {
		return 0, false
	}
	return d, true
}

// backoffFor is the documented minute, doubled once per consecutive refusal
// and capped. GitHub's page says to wait at least a minute and to increase the
// wait if the failures continue; that is all it says, so that is all this does.
func backoffFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := secondaryBackoff
	for i := 1; i < attempt; i++ {
		if d >= secondaryBackoffCap {
			break
		}
		d *= 2
	}
	if d > secondaryBackoffCap {
		d = secondaryBackoffCap
	}
	return d
}

// preflight reports what the budget says before the first call is spent.
//
// ⛔ IT REFUSES ONLY WHAT IS CERTAIN. A wait cannot be promised: the secondary
// limit is invisible here, other sessions spend the same quota while this one
// runs, and a reset can arrive mid-wait. So the only refusal is the case where
// BOTH budgets are at zero, where no target of any kind can be read and every
// call would be spent to be refused. Anything short of that is reported and
// the wait proceeds — an estimate that refuses a wait which would have worked
// is a worse tool than no estimate, and this fleet has one such rule in its
// history, measured at 4137 refusals against two defects.
//
// ⚠ A ZERO in ONE budget is not a refusal either, because which budget a given
// target spends is gh's business, not something asserted here: a branch is read
// with `gh api graphql`, a run with a REST path, and a pull request through
// whichever gh chooses. It is reported, and the wait finds out.
func preflight(read budgetReader, now time.Time) (lines []string, refuse bool) {
	if read == nil {
		return nil, false
	}
	b, err := read()
	if err != nil {
		// Not a refusal: an unreadable instrument says nothing about the
		// quota, and this tool must not turn "I could not look" into "you
		// may not go".
		return []string{fmt.Sprintf("⚠ the API budget could not be read (%v) — starting anyway, and a quota will be reported as a quota if one is hit.", err)}, false
	}
	switch {
	case b.Core.Remaining == 0 && b.GraphQL.Remaining == 0:
		return []string{
			"⛔ prwait: refusing to start — both API budgets are spent, so nothing can be read and no verdict can exist.",
			fmt.Sprintf("     REST resets at %s (in %s); GraphQL at %s (in %s).",
				b.Core.Reset.Local().Format("15:04 MST"), until(b.Core.Reset, now),
				b.GraphQL.Reset.Local().Format("15:04 MST"), until(b.GraphQL.Reset, now)),
			"     The quota is per USER, so this is the whole machine, Renovate included. Wait for the reset, or give one wait all the targets at once.",
		}, true
	case b.Core.Remaining == 0:
		return []string{fmt.Sprintf("⚠ the REST budget is spent (0 of %d); it resets at %s, in %s. Targets that need it cannot be read until then.",
			b.Core.Limit, b.Core.Reset.Local().Format("15:04 MST"), until(b.Core.Reset, now))}, false
	case b.GraphQL.Remaining == 0:
		return []string{fmt.Sprintf("⚠ the GraphQL budget is spent (0 of %d); it resets at %s, in %s. Targets that need it cannot be read until then.",
			b.GraphQL.Limit, b.GraphQL.Reset.Local().Format("15:04 MST"), until(b.GraphQL.Reset, now))}, false
	}
	return nil, false
}
