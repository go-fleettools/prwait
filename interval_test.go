package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// restCallsPerHour is what one wait of n targets spends at interval d, from the
// cost model in interval.go: one REST call per target per poll.
//
// ⛔ THE TEST COMPUTES THIS, rather than comparing against numbers copied out of
// the implementation. A test that spells the implementation's constants tests
// the constants: it goes red on any legitimate change of them and can never
// catch what it exists for, which here is a wait whose cost grows with its
// targets.
func restCallsPerHour(targets int, d time.Duration) float64 {
	return float64(targets) * float64(time.Hour) / float64(d)
}

func TestOneTargetKeepsTheDocumentedInterval(t *testing.T) {
	d, line := defaultInterval(1)
	if d != baseInterval {
		t.Fatalf("one target got %s, want the documented %s", d, baseInterval)
	}
	if line != "" {
		t.Fatalf("one target explained itself (%q); there is nothing to explain when the default is the documented one", line)
	}
}

// TestTheCostOfAWaitDoesNotGrowWithItsTargets is the property the change exists
// for: four targets used to cost four times one target, which is how the shared
// budget reached zero.
func TestTheCostOfAWaitDoesNotGrowWithItsTargets(t *testing.T) {
	one, _ := defaultInterval(1)
	budget := restCallsPerHour(1, one)
	for n := 1; n <= int(maxInterval/baseInterval); n++ {
		d, _ := defaultInterval(n)
		if got := restCallsPerHour(n, d); got > budget+1 {
			t.Errorf("%d targets spend %.0f REST calls an hour at %s; one target spends %.0f, and the default must not multiply that",
				n, got, d, budget)
		}
	}
}

// TestPastTheCapTheCostIsStatedInsteadOfHeld says what the cap costs. The cap is
// a deliberate choice of a human-usable interval over a constant budget, so the
// property above stops holding — and a trade-off that is not printed is one
// nobody can act on.
func TestPastTheCapTheCostIsStatedInsteadOfHeld(t *testing.T) {
	n := int(maxInterval/baseInterval) + 3
	d, line := defaultInterval(n)
	if d != maxInterval {
		t.Fatalf("%d targets got %s, want the cap %s", n, d, maxInterval)
	}
	want := []string{"capped", "reads as a hang"}
	for _, w := range want {
		if !strings.Contains(line, w) {
			t.Errorf("the capped explanation does not say %q:\n%s", w, line)
		}
	}
	// The hourly cost it admits to must be the one it actually spends.
	spent := restCallsPerHour(n, d)
	if !strings.Contains(line, "REST calls an hour") {
		t.Fatalf("the capped explanation does not state a cost:\n%s", line)
	}
	// ⛔ The STRING and the NUMBER are checked in the same test, with the same
	// formatting the message uses: an assertion that rounds differently from
	// the sentence it checks passes while the published figure is wrong.
	if !strings.Contains(line, fmt.Sprintf("%.0f", spent)) {
		t.Errorf("the explanation does not hold the %.0f REST calls an hour it spends:\n%s", spent, line)
	}
}

func TestTheExplanationNamesTheTargetsTheIntervalAndTheOverride(t *testing.T) {
	d, line := defaultInterval(3)
	for _, w := range []string{"3 targets", d.String(), baseInterval.String(), "-interval overrides this"} {
		if !strings.Contains(line, w) {
			t.Errorf("the explanation for 3 targets does not say %q:\n%s", w, line)
		}
	}
}

// TestAnExplicitIntervalIsNeverScaled holds the flag's meaning. Without this,
// scaling a value the caller typed would be the same defect with the sign
// reversed: a flag that does not mean what it reads.
func TestAnExplicitIntervalIsNeverScaled(t *testing.T) {
	asked := 10 * time.Second
	d, line := chosenInterval(true, asked, 9)
	if d != asked {
		t.Fatalf("an explicit -interval %s became %s for 9 targets", asked, d)
	}
	if line != "" {
		t.Fatalf("an explicit -interval was explained at the caller (%q); they said what they wanted", line)
	}
}

// TestTheDefaultIsUsedWhenTheFlagWasNotGiven is the other side of the same
// switch: a control, because a chosenInterval that always returned what it was
// handed would pass the test above.
func TestTheDefaultIsUsedWhenTheFlagWasNotGiven(t *testing.T) {
	scaled, line := defaultInterval(4)
	d, got := chosenInterval(false, baseInterval, 4)
	if d != scaled {
		t.Fatalf("with no -interval and 4 targets the wait used %s, want the scaled %s", d, scaled)
	}
	if got != line {
		t.Fatalf("the explanation differs between defaultInterval and chosenInterval:\n%q\n%q", line, got)
	}
}

// TestAnEmptyTargetListCannotDivideTheCost holds the guard on a count main
// cannot produce (it refuses an empty list first). It is here because the
// function is a cost model: a later caller that computes targets instead of
// counting them would reach zero, and a report tool must not crash.
func TestAnEmptyTargetListCannotDivideTheCost(t *testing.T) {
	d, line := defaultInterval(0)
	if d != baseInterval {
		t.Fatalf("zero targets gave %s, want the base %s", d, baseInterval)
	}
	if line != "" {
		t.Fatalf("zero targets explained itself: %q", line)
	}
}
