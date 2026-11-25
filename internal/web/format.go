package web

import (
	"fmt"
	"html/template"
	"strings"
	"time"
)

var funcs = template.FuncMap{
	"dur":   fmtMS,
	"short": shortID,
	"pct":   func(f float64) string { return fmt.Sprintf("%.3f%%", f) },
	"ago":   func(ms int64) string { return agoAt(time.Now(), ms) },
	"clock": func(ms int64) string { return time.UnixMilli(ms).Format("15:04:05") },
	"who":   who,
}

// fmtMS renders a duration the way people read one: 840 ms, 4.2 s, 1 m 24 s, 1 h 02 m.
func fmtMS(ms int64) string {
	switch {
	case ms < 0:
		return "0 ms"
	case ms < 1000:
		return fmt.Sprintf("%d ms", ms)
	case ms < 10_000:
		return fmt.Sprintf("%.1f s", float64(ms)/1000)
	case ms < 60_000:
		return fmt.Sprintf("%d s", ms/1000)
	case ms < 3_600_000:
		return fmt.Sprintf("%d m %02d s", ms/60_000, ms/1000%60)
	}
	return fmt.Sprintf("%d h %02d m", ms/3_600_000, ms/60_000%60)
}

func agoAt(now time.Time, ms int64) string {
	d := now.Sub(time.UnixMilli(ms))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d d ago", int(d.Hours()/24))
}

// niceStep picks a round tick spacing (ms) giving about 5 ticks over span, and never more than 6 however long the
// span: past five hours the spacing is a whole number of hours.
func niceStep(span int64) int64 {
	if span <= 0 {
		return 1000
	}
	raw := float64(span) / 5
	steps := []int64{100, 200, 500, 1000, 2000, 5000, 10_000, 15_000, 30_000, 60_000, 120_000, 300_000, 600_000, 1_800_000, 3_600_000}
	for _, s := range steps {
		if float64(s) >= raw {
			return s
		}
	}
	hour := steps[len(steps)-1]
	return (span/5/hour + 1) * hour
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func isDone(status string) bool {
	switch strings.ToLower(status) {
	case "passed", "failed", "timedout", "skipped", "interrupted":
		return true
	}
	return false
}

func isFailed(status string) bool {
	switch strings.ToLower(status) {
	case "failed", "timedout", "interrupted":
		return true
	}
	return false
}

func gib(b uint64) string {
	if b == 0 {
		return ""
	}
	return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
}
