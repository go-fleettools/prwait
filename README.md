# prwait

Wait until a pull request's checks are finished — and **refuse to call an empty check list a pass**, or a complete and entirely green one, when a workflow run for the same commit is red.

```console
$ prwait -repo go-pkgx/pkgx 82 83
go-pkgx/pkgx#82 @4f21ab9 PASS SUCCESS:24
go-pkgx/pkgx#83 @c0de17e FAIL test (windows-latest)=FAILURE

$ prwait go-pkgx/bk#311 go-pkgx/mirror#28
go-pkgx/bk#311 FAIL CONFLICTING — rebase it; any green ticks it shows are from the old base
go-pkgx/mirror#28 @9a0b113 PASS SUCCESS:20 SKIPPED:1
```

⛔ **Every verdict names the commit it is about.** `#82 PASS` is true of *some*
commit; a wait that straddles a push — start it, push again, read the result —
cannot be acted on from that line alone, because the head it began with and the
head it ended with are different pieces of work. Measured on exactly that
sequence; the verdict happened to be right, and *"it was fine"* is not the same
as *"I knew it was fine"*. A target with no commit to name (a conflicted pull
request has no head to judge) prints none rather than an empty `@`.

⛔ **A red that can no longer be re-run says so.** Past GitHub's 30-day
re-run window, `gh run rerun` answers `403 Unable to retry this workflow run
because it was created over a month ago` — so the red is permanent where it
stands, and the only way to a fresh verdict is a ref you control.

```console
$ prwait -repo go-ansible/playbook 197
go-ansible/playbook#197 @c5dd86d FAIL test (macos-latest)=FAILURE
  ⛔ ci (pull_request run #412) was created 2026-10-12, 37 days ago: `gh run rerun` will answer 403 (GitHub allows a re-run for 30 days).
     This red cannot be refreshed where it stands. Reproduce the lane locally if it names your platform, or put the commit on a ref you control — `git cherry-pick` onto a branch in this repository — and let CI judge it there.
```

That line exists because the question went unasked for ten days on that very
pull request. It was handed back twice as *"blocked: three routes tried, all
closed"* — and all three routes were ways to make **that** run go green, so
they shared one premise and tested it once. Cherry-picked onto a branch in the
target repository, the same commit came back **9/9 green, `macos-latest`
included**: the red belonged to the stale base, not to the change. Nothing
is claimed about logs that are merely old; past the 90-day default retention
the line says so, and otherwise it reports the age and lets you conclude.

⛔ **And a head that moves mid-wait is refused, not followed.** Naming the
commit is necessary and not sufficient: a wait that silently re-points at a
push the caller never asked about ends by printing a true statement about the
wrong work.

```console
$ prwait -repo go-tex/engine 752
go-tex/engine#752 MOVED @e87aa0a -> @2baff07 — the head changed while waiting; this wait judged neither, re-run against @2baff07
```

Neither commit was judged as a whole, so neither is reported as a verdict, and
the exit status is `1`. It holds for a branch (`@main`) and a run as much as for
a pull request — a release gate waiting on the default branch must not answer
about a commit that arrived after the one it was asked about, or the tag lands
on something nobody judged. A SHA that one poll could not read is **not** a
move: a blank answer is weather, and the retry loop already covers it.

⛔ **And a rate limit is not a verdict.** When GitHub refuses the call for the
quota, nothing about the target was measured, so nothing is claimed about it:

```console
$ prwait -repo go-embedded-julia/julia 78 79 80 82
go-embedded-julia/julia#78 QUOTA — not a verdict: nothing about this target was measured.
  ⛔ the per-user REST quota is spent (0 of 5000 left). It resets at 20:45 CET, in 38m12s.
     The quota is per USER, not per token, so every session on this machine shares it — Renovate included.
⛔ 4 target(s) got no verdict at all: …#78, …#79, …#80, …#82 — the quota stopped the wait before they were read.
```

Measured 2026-11-15: the account's REST quota hit zero twice in one day, and
both times this tool printed the raw `gh: API rate limit exceeded … (HTTP 403)`
*where its verdict goes*, under **exit 1 — the status of a red check**. A
caller reading only the status took "nothing could be measured" for "the pull
request is red", which is this tool's own defect wearing its least
recognisable face: an **unread** answer presenting itself as a **negative**
one.

The retry made it worse. A failed query is weather here, so a quota was asked
again three times at the poll interval — three more refusals, each spending a
call against the very limit that was refusing, each printed as `asking again`.
**Weather has no reset time; a quota has one, advertised.** So a quota now gets
its own classification, its own hold, and its own exit status:

- **Held for, when the reset fits.** The wait sleeps to the advertised reset
  and resumes. Costs nothing, and keeps the verdict.
- **Refused early, when it does not.** Spending the rest of the timeout to
  learn what `/rate_limit` already said is how four calls bought one fact.
- **A secondary limit is reported as a secondary limit.** `/rate_limit` cannot
  see it — *"There is not a way to check the status of your secondary rate
  limit"* — so when GitHub refuses for a rate limit while the instrument
  reports room, that is said in those words, and marked as **deduced**. No
  reset is ever borrowed from the primary limit to stand in for one that does
  not exist.
- **A measured red still outranks it.** If anything was actually read as
  failing, the status stays `1`.

⚠ `GET /rate_limit` is free of the *primary* limit only — *"Calling this
endpoint does not count against your primary rate limit, but it can count
against your secondary rate limit"* — so it is read once before the wait and
once per refusal, never per poll.

⛔ **And the default interval now grows with the number of targets**, because a
poll reads *every* target: `-interval` reads as "how often to ask" and is really
"how much of a shared budget to spend", so one value cannot mean the same thing
for one target and for four. `prwait -repo O/R 78 79 80 82` cost exactly what the
four separate processes in the incident above cost. The default is **45 s for one
target and 45 s per target** beyond that, capped at **5 minutes** — linear growth
is right for the budget and wrong for a human past a few minutes, since at twenty
targets it would put a quarter of an hour between polls and a verdict ready at
minute 1 would arrive at minute 15. The chosen interval, the reason and the
override are printed; an explicitly given `-interval` is used **as typed**, since
a caller who names one has said what they want of the budget.

⛔ **And the count that paces a wait is the user's, not the process's.** Scaling
one wait by its own targets is right and was not enough: the incident above was
**four processes**, each pacing itself correctly for the one target it knew
about, and on 2026-11-22 the same quota reached 0 of 5000 again with about
twenty-five sessions on the machine. So every wait now records itself in
`<user cache>/prwait/waits.json` — per user by construction, which is the
quota's own granularity — and counts the targets the other live waits are
already polling into its own interval:

```text
prwait: 1 other wait(s) of this user are already polling 3 target(s) (pid 4711, started 3m0s ago, 3 target(s): go-x/y#78 go-x/y#79 go-x/y#80) — the REST budget is per USER, shared with every session here and with Renovate, so one wait can take every target: `prwait -repo O/R 78 79 80 82` costs what this wait alone costs and reports them together.
prwait: pacing this wait for 4 target(s) in all (1 here, 3 elsewhere), so the interval is 3m0s rather than 45s. -interval overrides this.
```

⛔ **It warns and paces; it does not refuse.** A second wait could have been
rejected outright, and with ~25 sessions sharing this machine one 30-minute wait
would then lock every other session out of the only tool they are told to wait
with — and the first thing that happens to a gate nobody can keep green is that
it gets switched off, by a flag or by going back to `gh pr checks --watch`, which
is the defect this tool exists to prevent.

⛔ **Nothing about that bookkeeping can fail a wait.** No cache directory, an
unreadable file, a lock held by a wait that was killed: each is a line printed
beside the verdict, and the wait runs. An entry is dropped when its deadline
(its wait's `-timeout` plus one poll interval) has passed, or when its process is
gone — and *"the process is gone"* is only ever allowed to drop an entry the
deadline would have kept, because the platforms that cannot answer it answer
"cannot tell" rather than "dead".

What one poll of one target spends, counted from the code rather than assumed:

| target | GraphQL | REST |
| --- | --- | --- |
| a pull request or a branch | 1 — the check list (`statusCheckRollup` exists only in the GraphQL schema) | 1 — `actions/runs?head_sha=` |
| a workflow run (`!<id>`) | — | 1, and no runs read: a run target is already a run |

⚠ **The two budgets are separate and are never summed**: 5000 REST calls an hour
and 5000 GraphQL points, either of which can be spent while the other is
untouched. The scaling is set by the REST half, because that is the half that was
measured at zero.

| exit | meaning |
|---|---|
| `0` | every pull request finished, with checks, and all of them acceptable |
| `1` | one finished red, conflicts, was closed, or ran out of time |
| `2` | refused: bad usage, or an answer that could not be read |
| `3` | the API quota stopped the wait: nothing was measured, nothing is claimed |

## Why it exists

The obvious waiter asks *"is anything still pending?"* and stops when the answer is no.
That waiter reported success on four pull requests that **had never been built**.

A **conflicted** pull request gets no merge ref, so no workflow is dispatched for it, so
its check list stays **empty** — and an empty list has nothing pending. Worse: a pull
request that starts conflicting *after* a green run keeps the ticks it earned on the old
base, so the summary reads as a pass twice over, once from the ticks and once from the
silence.

The same list is empty in the first seconds after a push, and for the entire life of a
pull request whose workflow **fails to parse** — an invalid workflow reports nothing at
all, which looks exactly like a check that has not started yet.

So the rule here is the inverse of the obvious one:

> A pull request passes only when **at least `-min-checks` checks exist**, **every one of
> them has finished**, and **every conclusion is acceptable**. Absence is never evidence.

### And a complete, all-green check list is not a pass either

A check run is created **per job**. A workflow run that fails *before its jobs are
created* therefore leaves **no check run at all**: nothing red, nothing pending, nothing
to count.

Measured on `go-tex/go-tex.github.io#239`, head `769bfee`, 2026-11-09 — prwait v0.5.0
printed `PASS SUCCESS:3` and exited `0`:

| what was asked | what it said |
|---|---|
| `commits/769bfee/check-runs` | 3 check runs, **all `success`**, none pending |
| `actions/runs?head_sha=769bfee` | `docs` success, `links` success, **`pages` failure**, **`playground` failure** |

`pages` had created **zero** jobs and `playground` one of its three. Every property the
check list can express was true, which is why the rule above was not enough: the check
list is the wrong noun on its own. So a pull request and a branch are judged on their
head commit's **workflow runs** as well, read from `actions/runs?head_sha=<oid>` and put
through the same table — a run still going keeps the wait going, and
`startup_failure`, the conclusion only a run can carry, is a failure like any other.

⛔ The verdict is bound to the **SHA**, never to the branch name or the number: both move
while a wait is running. An answer carrying no head commit is refused rather than judged
on its checks alone.

⛔ Only the **latest run of each workflow, for each event**, is judged. A commit can carry
a failed run and the retrigger that replaced it — `actions/runs?head_sha` returns both —
and judging every run leaves a commit that is now green **unpassable forever**, which is a
gate people route around rather than a gate.

And the event is part of that key, which is the trap under the trap: on the commit that
exposed all this, a `workflow_dispatch` of the playground workflow was **green** while its
`pull_request` run was **red** — the dispatch being a run started by hand to find out
whether the branch was sound. Keyed on the workflow alone, a run started to investigate a
failure would have answered for it. The report names the event for the same reason:
`playground (pull_request run)=FAILURE`, not `playground (workflow run)=FAILURE` twice.

⛔ And the SHA must be **whole**. Measured the same day: `head_sha=e18cc9b` answered an
empty `workflow_runs` list for a commit that had a run *in progress*, while the same query
with all 40 characters found it. An empty list is a legitimate state — a repository with
no workflows — so an abbreviation would have passed as an absence instead of failing as an
error. Anything short of 40 hex characters is refused before `gh` is reached.

## What else it gets right

**A `StatusContext` is not a `CheckRun`.** GitHub's rollup mixes two shapes that share no
field: a check run has `status` + `conclusion`, while a commit status — still used by
plenty of third-party services — has only `state`. A waiter testing
`status != "COMPLETED"` finds every commit status pending **forever** and times out on a
pull request that is finished and green.

**`CANCELLED` is not a pass.** A cancelled lane executed zero steps. In a fleet where
lanes are cancelled by concurrency starvation, that is exactly the case where a green
summary would be a lie. `NEUTRAL` and `SKIPPED` *are* passes: a lane that excluded itself
did its job.

**`UNKNOWN` mergeability is not an answer.** GitHub computes mergeability lazily; green
checks plus `UNKNOWN` means it has not yet been decided whether the branch can land.

**A `@branch` whose head is a conflicted pull request is refused, and it was not.**
GitHub runs `pull_request` workflows against `refs/pull/N/merge`, which does not exist
while the merge cannot be computed — so a conflicted pull request has **no runs at all**,
by construction. `prwait` has refused `CONFLICTING` before counting checks since its first
release, but the branch form supplied a fabricated `mergeable: "MERGEABLE"` — right for a
bare branch, which has nothing to merge into, and a lie when the branch is a pull
request's head. So the refusal was unreachable from the form that avoids guessing a
number, which is the form the fleet's own rule prescribes.

Measured 2026-11-18 on `go-zpkg/zpkb#505`: `TIMEOUT after 30m0s — 0 checks, want at
least 1`, then three retriggers spent on a dropped-dispatch theory — Actions enabled,
all workflows `active`, repository public, githubstatus `operational`, a sibling
repository running 34 minutes earlier — while `gh pr view --json mergeable` answered
`CONFLICTING` in one call. It now says
`CONFLICTING (#505) — rebase it; …`, naming the pull request, because a reader told
"CONFLICTING" about a *branch* has no way to know which one said so.

⚠ The selection is by **head**, not by association: GitHub associates a commit with every
pull request that contains it, so a branch cut from `main` is associated with whatever
merged into `main`. Only a pull request whose `headRefName` is this branch says anything
about what this branch would merge. A bare branch still behaves exactly as before — that
is what the fabricated value was for, and a test holds it.

**A timeout is a failure, not a hang and not a pass.**

**An error from `gh` is reported, and sinks the run.** Not silence, not a retry forever.

## Install

```
go install github.com/go-fleettools/prwait@latest
```

It shells out to [`gh`](https://cli.github.com), which holds the authentication: prwait
never reads, prints or passes a token, and nothing secret ever reaches a command line.

## The judge as a library

The rules above (latest run per workflow and event, null conclusion pending,
CANCELLED not a pass, a green run with zero jobs never started) live in
`github.com/go-fleettools/prwait/judge`, so a program that asks "is this commit
green" imports them instead of copying them. It never networks and never
sleeps: `LatestRuns`, `RunEntries`, `Classify`, `RunState` (which counts jobs),
`Ok`, `Tally`. prwait itself uses it unchanged.

## Usage

```
prwait [-repo OWNER/REPO] [flags] <pr>...
```

A target is one of three things, each with its own sigil, and a single run can span
repositories — which is what a dependency bump across a family of modules needs.

| target | asks |
|---|---|
| `82` or `OWNER/REPO#82` | a **pull request**: may this land |
| `@main` or `OWNER/REPO@main` | a **branch**: is what landed green — what you want before tagging |
| `!40237685388` or `OWNER/REPO!402…` | a **workflow run**: did that dispatch finish, and how |

Each sigil is required, so a mistyped number stays an error instead of becoming a wait
on something nobody named: a run id and a pull request number are both bare digits.

### Why a run target, when `gh run watch` exists

A dispatched run has no pull request and no checks, so neither of the other forms can
see one. Measured 2026-10-25, chasing a build across three CI defects: `gh run watch`
**returned three times while the run was still in progress**, and each time the
conclusion read next did not exist yet — a build still compiling, reported as a build
that failed.

Here a run is simply one check, judged by the same rules as any other, so *not
finished* is never a pass:

```console
$ prwait -repo go-pkgx/packages '!40237685388'
go-pkgx/packages!40237685388 TIMEOUT after 40s — 1 of 1 running (build (run))
```

⛔ The reshape **uppercases**. `gh run view --json status` answers `completed` while the
rollup rules test the GraphQL spelling `COMPLETED`; left as it comes, a finished run
would look unfinished forever and prwait would wait out its timeout on a green build —
this target's failure mode with the sign reversed. A test pins both that and the `null`
conclusion a running job really sends.

| flag | default | |
|---|---|---|
| `-repo` | | repository for bare numbers |
| `-interval` | `45s` × targets, capped at `5m` | how often to ask. A poll reads every target, and the targets of this user's other live waits count too, so the default holds the USER's REST cost at about what one target costs; given explicitly it is used as typed |
| `-timeout` | `30m` | give up after this, per run |
| `-min-checks` | `1` | how many checks must **exist** before a pass is possible |
| `-gh` | `gh` | the `gh` binary to ask |

`-min-checks` is worth setting when you know the suite: a repository with 25 lanes that
reports 1 has registered one workflow so far, and stopping there is the partial-list race.
`-min-checks 0` is refused, because it would restore the defect this tool exists for.

**It is one number for the whole run.** A single command spanning repositories with
different suites applies the biggest one's threshold to the smallest — four Go repositories
at 20+ lanes and one docs repository at 2, under `-min-checks 20`, leaves the docs pull
request finished, green and waited on until the timeout. So **split the run** when the
suites differ. The waiter still refuses to pass a short list, but it now says which
situation you are in:

```console
go-pkgx/docs#23 … 2 checks, all finished, want at least 20 — is -min-checks meant for this repository?
```

"More are coming" and "this is all there will ever be" are different, and only the caller
can act on the second.

## Tests

Every rule is a pure function of one observation, so the suite runs without a network and
without `gh`. The exec path is covered by a **helper process** rather than a shell script,
because a script cannot be executed on Windows and this runs on every lane.

Each central guard has been proved breakable with
[`mutate`](https://github.com/go-fleettools/mutate):

| mutation | caught by |
|---|---|
| the empty-list guard | `TestAnEmptyCheckListIsNeverAPass` |
| the conflict guard | `TestAConflictedPullRequestIsNotAPassEvenWhenGreen` |
| a pending commit status becomes "done" | `TestAPendingStatusContextStillWaits` |
| `CANCELLED` counts as success | `TestWhichConclusionsCount` |
| the unknown-mergeability wait | `TestUnknownMergeabilityKeepsWaiting` |
| the workflow runs dropped from the judgement | `TestACompleteGreenCheckListWithARedRunIsNotAPass` |

That last row was measured by hand before the code was trusted: with the runs removed
from the judgement, the test reports `gave 1 (SUCCESS:3)` — **verbatim what prwait
printed on the pull request that exposed this**.
