package capacity

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

func freeMemory(ctx context.Context) (uint64, error) {
	switch runtime.GOOS {
	case "linux":
		f, err := os.Open("/proc/meminfo")
		if err != nil {
			return 0, err
		}
		defer f.Close()
		return parseMeminfo(bufio.NewScanner(f)), nil
	case "darwin":
		out, err := exec.CommandContext(ctx, "vm_stat").Output()
		if err != nil {
			return 0, err
		}
		return parseVMStat(string(out)), nil
	}
	return 0, nil
}

// parseMeminfo returns MemAvailable (falling back to MemFree) in bytes.
func parseMeminfo(s *bufio.Scanner) uint64 {
	var avail, free uint64
	for s.Scan() {
		name, rest, ok := strings.Cut(s.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch name {
		case "MemAvailable":
			avail = kb * 1024
		case "MemFree":
			free = kb * 1024
		}
	}
	if avail > 0 {
		return avail
	}
	return free
}

// parseVMStat sums the pages macOS can hand out at once: free, inactive and
// speculative, using the page size printed in the header.
func parseVMStat(out string) uint64 {
	page := uint64(16384)
	var pages uint64
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Mach Virtual Memory Statistics") {
			if i := strings.Index(line, "page size of "); i >= 0 {
				if f := strings.Fields(line[i+len("page size of "):]); len(f) > 0 {
					if n, err := strconv.ParseUint(f[0], 10, 64); err == nil && n > 0 {
						page = n
					}
				}
			}
			continue
		}
		name, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch name {
		case "Pages free", "Pages inactive", "Pages speculative":
			n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(val), "."), 10, 64)
			if err == nil {
				pages += n
			}
		}
	}
	return pages * page
}
