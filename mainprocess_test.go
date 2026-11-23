package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests run main() ITSELF, as a child process: the test binary re-run
// through TestMain's "main" mode, with the test binary again as its gh.
//
// ⛔ Until this file, nothing executed main(). `mutsweep -sites all` (the
// [branch] sites, v0.17.0) found nine guards in it that no test held: the
// usage refusal, -h, a bad flag, -min-checks, a bad target, the interval flag
// being honoured, the missing cache directory, the budget refusal and the exit
// status. Each test below names the mutation it kills.

// mainHelper is the body of the "main" mode: PRWAIT_MAIN_ARGS (newline
// separated) become os.Args, the gh the child will spawn is chosen by
// PRWAIT_GH_HELPER, and main() runs. A main that returns has succeeded, which
// is exit 0, so that is what happens here. Never returns.
func mainHelper() {
	os.Args = []string{"prwait"}
	if a := os.Getenv("PRWAIT_MAIN_ARGS"); a != "" {
		os.Args = append(os.Args, strings.Split(a, "\n")...)
	}
	os.Setenv("PRWAIT_HELPER", os.Getenv("PRWAIT_GH_HELPER"))
	main()
	os.Exit(0)
}

type mainRun struct {
	code           int
	stdout, stderr string
}

// runMain runs prwait with args against the fake gh in mode, with a private
// cache directory so the registry of the machine is neither read nor written.
// extraEnv wins over the defaults.
func runMain(t *testing.T, mode string, extraEnv []string, args ...string) mainRun {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	cmd := exec.Command(exe)
	cmd.Env = append(append(os.Environ(),
		"PRWAIT_HELPER=main",
		"PRWAIT_GH_HELPER=fakegh",
		"FAKE_GH_MODE="+mode,
		"FAKE_GH_LOG="+filepath.Join(t.TempDir(), "calls.log"),
		"PRWAIT_MAIN_ARGS="+strings.Join(args, "\n"),
		"HOME="+cache, "XDG_CACHE_HOME="+cache, "LocalAppData="+cache),
		extraEnv...)
	var so, se strings.Builder
	cmd.Stdout, cmd.Stderr = &so, &se
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("prwait %q did not finish in 60s; stdout %q stderr %q", args, so.String(), se.String())
	}
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatal(err)
		}
		code = ee.ExitCode()
	}
	return mainRun{code: code, stdout: so.String(), stderr: se.String()}
}

func ghFlag(t *testing.T) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return []string{"-gh", exe}
}

// Kills `fs.NArg() == 0` -> false: with no target the run is an empty one, and
// an empty list of targets is judged green, exit 0.
func TestNoTargetIsRefusedWithTheUsage(t *testing.T) {
	r := runMain(t, "green", nil)
	if r.code != 2 {
		t.Fatalf("exit %d, want 2; stderr %q", r.code, r.stderr)
	}
	if r.stderr != usage {
		t.Errorf("stderr is not the usage text: %q", r.stderr)
	}
}

// Kills `errors.Is(err, flag.ErrHelp)` -> false: asking for help is not an
// error, and was answered with the exit status of one.
func TestHelpExitsZero(t *testing.T) {
	r := runMain(t, "green", nil, "-h")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0; stderr %q", r.code, r.stderr)
	}
	if !strings.Contains(r.stderr, "prwait") {
		t.Errorf("no usage was printed: %q", r.stderr)
	}
}

// Kills `err != nil` after Parse -> false: the flag error would fall through
// and the usage would be printed a second time by the missing-target branch.
func TestAnUnknownFlagIsRefusedOnceWithExitTwo(t *testing.T) {
	r := runMain(t, "green", nil, "-nonsense")
	if r.code != 2 {
		t.Fatalf("exit %d, want 2; stderr %q", r.code, r.stderr)
	}
	if !strings.Contains(r.stderr, "-nonsense") {
		t.Errorf("the flag package's own complaint is missing: %q", r.stderr)
	}
	if n := strings.Count(r.stderr, usage); n != 1 {
		t.Errorf("the usage was printed %d times, want 1: %q", n, r.stderr)
	}
}

// Kills `*minChecks < 1` -> false: zero checks would make an empty list pass.
func TestMinChecksBelowOneIsRefused(t *testing.T) {
	args := append(ghFlag(t), "-min-checks", "0", "a/b#1")
	r := runMain(t, "green", nil, args...)
	if r.code != 2 {
		t.Fatalf("exit %d, want 2; stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stderr, "-min-checks must be at least 1") {
		t.Errorf("the reason is missing: %q", r.stderr)
	}
	if r.stdout != "" {
		t.Errorf("a wait ran anyway: %q", r.stdout)
	}
}

// Kills the `err != nil` after parseTarget -> false: a target that is not one
// must stop the run with exit 2, not be judged as an empty one.
func TestABadTargetIsRefusedWithExitTwo(t *testing.T) {
	args := append(ghFlag(t), "banana")
	r := runMain(t, "green", nil, args...)
	if r.code != 2 {
		t.Fatalf("exit %d, want 2; stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	if !strings.HasPrefix(r.stderr, "prwait: ") {
		t.Errorf("the refusal is not prwait's: %q", r.stderr)
	}
	if r.stdout != "" {
		t.Errorf("a wait ran anyway: %q", r.stdout)
	}
}

// Kills `f.Name == "interval"` -> false: an interval typed by hand would be
// scaled by the target count like a default, which is the defect #25 removed.
func TestAGivenIntervalIsHonouredAndADefaultOneIsExplained(t *testing.T) {
	two := []string{"a/b#1", "a/b#2"}
	given := runMain(t, "green", nil, append(append(ghFlag(t), "-interval", "1ms"), two...)...)
	if given.code != 0 {
		t.Fatalf("exit %d; stdout %q stderr %q", given.code, given.stdout, given.stderr)
	}
	if strings.Contains(given.stderr, "default interval") {
		t.Errorf("a given -interval was treated as the default: %q", given.stderr)
	}
	// The control: without the flag the same two targets ARE scaled and said so,
	// so the assertion above is about the flag and not about a silent run.
	dflt := runMain(t, "green", nil, append(ghFlag(t), two...)...)
	if dflt.code != 0 {
		t.Fatalf("exit %d; stdout %q stderr %q", dflt.code, dflt.stdout, dflt.stderr)
	}
	if !strings.Contains(dflt.stderr, "default interval") {
		t.Errorf("the control did not explain its default interval: %q", dflt.stderr)
	}
}

// Kills `perr != nil` -> false: a machine with no cache directory cannot count
// its waits, and must say so rather than silently skip the count.
func TestNoCacheDirectoryIsSaidAndDoesNotChangeTheVerdict(t *testing.T) {
	r := runMain(t, "green", []string{"HOME=", "XDG_CACHE_HOME=", "LocalAppData="}, append(ghFlag(t), "a/b#1")...)
	if r.code != 0 {
		t.Fatalf("exit %d, want 0 (the registry is advisory); stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	// registry.go says it too, in its own words ("no user cache directory,"), so
	// the assertion names main's, which carries the cause in parentheses: with
	// the registry's note alone the reader is not told why.
	if !strings.Contains(r.stderr, "no user cache directory (") {
		t.Errorf("main did not say why the waits cannot be counted: %q", r.stderr)
	}
}

// Kills `refuse` -> false: with both budgets spent every call would be spent to
// be refused, and the wait would start anyway.
func TestSpentBudgetsRefuseToStartWithTheQuotaStatus(t *testing.T) {
	r := runMain(t, "spent", nil, append(ghFlag(t), "a/b#1")...)
	if r.code != quotaStatus {
		t.Fatalf("exit %d, want %d; stdout %q stderr %q", r.code, quotaStatus, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stderr, "refusing to start") {
		t.Errorf("no refusal was printed: %q", r.stderr)
	}
	if r.stdout != "" {
		t.Errorf("a wait ran anyway: %q", r.stdout)
	}
}

// Kills `code != 0` -> false: a red check must reach the exit status, which is
// all a script that merges on green can read.
func TestTheExitStatusFollowsTheVerdict(t *testing.T) {
	green := runMain(t, "green", nil, append(ghFlag(t), "a/b#1")...)
	if green.code != 0 || !strings.Contains(green.stdout, "PASS") {
		t.Errorf("green: exit %d, stdout %q stderr %q", green.code, green.stdout, green.stderr)
	}
	red := runMain(t, "red", nil, append(ghFlag(t), "a/b#1")...)
	if red.code != 1 || !strings.Contains(red.stdout, "FAIL") {
		t.Errorf("red: exit %d, want 1; stdout %q stderr %q", red.code, red.stdout, red.stderr)
	}
}

// ⛔ gh failing WITHOUT a word on stderr must still say something: the error
// then carries the exec error ("exit status 1"), not an empty message after the
// colon. Kills `msg == ""` -> false in ghFetcher (twice) and ghBudget.
func TestASilentGhFailureIsStillNamed(t *testing.T) {
	bin, _ := fakeGh(t, "silentfail")
	if _, err := ghFetcher(bin)(pr{Repo: "a/b", Number: 1}); err == nil || !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("a silent pr view failure read %v, want it to name the exit status", err)
	}
	if _, err := ghBudget(bin)(); err == nil || !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("a silent rate_limit failure read %v, want it to name the exit status", err)
	}
	bin, _ = fakeGh(t, "runssilent")
	if _, err := ghFetcher(bin)(pr{Repo: "a/b", Number: 1}); err == nil || !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("a silent runs query failure read %v, want it to name the exit status", err)
	}
}

// Kills `ts == ""` -> false in describe: a wait with no target is described
// as such, not with an empty list after the colon.
func TestADescriptionOfAWaitWithNoTargetSaysSo(t *testing.T) {
	got := waitEntry{PID: 7, Started: regNow}.describe(regNow)
	if !strings.HasSuffix(got, "0 target(s): no target recorded") {
		t.Errorf("describe = %q", got)
	}
}
