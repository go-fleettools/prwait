# prwait

Wait until a pull request's checks are finished — and **refuse to call an empty check list a pass**.

```console
$ prwait -repo go-pkgx/pkgx 82 83
go-pkgx/pkgx#82 PASS SUCCESS:24
go-pkgx/pkgx#83 FAIL test (windows-latest)=FAILURE

$ prwait go-pkgx/bk#311 go-pkgx/mirror#28
go-pkgx/bk#311 FAIL CONFLICTING — rebase it; any green ticks it shows are from the old base
go-pkgx/mirror#28 PASS SUCCESS:20 SKIPPED:1
```

| exit | meaning |
|---|---|
| `0` | every pull request finished, with checks, and all of them acceptable |
| `1` | one finished red, conflicts, was closed, or ran out of time |
| `2` | refused: bad usage, or an answer that could not be read |

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

**A timeout is a failure, not a hang and not a pass.**

**An error from `gh` is reported, and sinks the run.** Not silence, not a retry forever.

## Install

```
go install github.com/go-fleettools/prwait@latest
```

It shells out to [`gh`](https://cli.github.com), which holds the authentication: prwait
never reads, prints or passes a token, and nothing secret ever reaches a command line.

## Usage

```
prwait [-repo OWNER/REPO] [flags] <pr>...
```

A `<pr>` is either a number (with `-repo`) or `OWNER/REPO#NUMBER`, so a single run can
span repositories — which is what a dependency bump across a family of modules needs.

| flag | default | |
|---|---|---|
| `-repo` | | repository for bare numbers |
| `-interval` | `45s` | how often to ask |
| `-timeout` | `30m` | give up after this, per run |
| `-min-checks` | `1` | how many checks must **exist** before a pass is possible |
| `-gh` | `gh` | the `gh` binary to ask |

`-min-checks` is worth setting when you know the suite: a repository with 25 lanes that
reports 1 has registered one workflow so far, and stopping there is the partial-list race.
`-min-checks 0` is refused, because it would restore the defect this tool exists for.

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
