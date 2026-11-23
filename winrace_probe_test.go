package main

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TEMPORARY: counts, over many rounds, how often concurrent enters lose an entry and why.
func TestWinRaceProbe(t *testing.T) {
	rounds, _ := strconv.Atoi(os.Getenv("PROBE_ROUNDS"))
	if rounds == 0 {
		t.Skip("PROBE_ROUNDS unset")
	}
	const n = 8
	lost, noted := 0, 0
	notes := map[string]int{}
	for r := 0; r < rounds; r++ {
		path := filepath.Join(t.TempDir(), "waits.json")
		var wg sync.WaitGroup
		regs := make([]registration, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				regs[i] = enter(path, waitEntry{PID: 10000 + i, Started: regNow, Deadline: regNow.Add(time.Hour), Targets: []string{"x"}}, regNow, always)
			}(i)
		}
		wg.Wait()
		ws := read(t, path)
		if len(ws) != n {
			lost++
		}
		for _, g := range regs {
			for _, nt := range g.Notes {
				noted++
				if len(nt) > 160 {
					nt = nt[:160]
				}
				notes[nt]++
			}
		}
	}
	t.Logf("PROBE rounds=%d lost=%d notes=%d", rounds, lost, noted)
	for k, v := range notes {
		t.Logf("PROBE note x%d: %s", v, k)
	}
}
