package capacity

import (
	"bufio"
	"strings"
	"testing"
)

func TestSlots(t *testing.T) {
	cases := []struct {
		name               string
		cpus               int
		mem, docker        uint64
		wantCheck, wantJob int
	}{
		{"cpu bound", 4, 64 * gib, 0, 4, 2},
		{"memory bound", 16, 6 * gib, 0, 4, 2},
		{"docker bound", 16, 64 * gib, 8 * gib, 4, 2},
		{"unknown budgets do not limit, capped", 64, 0, 0, 32, 16},
		{"tiny machine still runs one", 1, gib / 2, gib / 2, 1, 1},
		{"no docker means no docker limit", 8, 32 * gib, 0, 8, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			check, job := Slots(c.cpus, c.mem, c.docker)
			if check != c.wantCheck || job != c.wantJob {
				t.Errorf("Slots(%d, %d, %d) = %d, %d; want %d, %d", c.cpus, c.mem, c.docker, check, job, c.wantCheck, c.wantJob)
			}
		})
	}
}

func TestParseMeminfo(t *testing.T) {
	withAvail := "MemTotal:       16000000 kB\nMemFree:          500000 kB\nMemAvailable:    8000000 kB\n"
	if got := parseMeminfo(bufio.NewScanner(strings.NewReader(withAvail))); got != 8000000*1024 {
		t.Errorf("MemAvailable must win: %d", got)
	}
	noAvail := "MemTotal:       16000000 kB\nMemFree:          500000 kB\n"
	if got := parseMeminfo(bufio.NewScanner(strings.NewReader(noAvail))); got != 500000*1024 {
		t.Errorf("falls back to MemFree: %d", got)
	}
}

func TestParseVMStat(t *testing.T) {
	out := `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                               1000.
Pages active:                            50000.
Pages inactive:                           2000.
Pages speculative:                         500.
`
	if got, want := parseVMStat(out), uint64(3500*16384); got != want {
		t.Errorf("parseVMStat = %d, want %d (free + inactive + speculative, not active)", got, want)
	}
}
