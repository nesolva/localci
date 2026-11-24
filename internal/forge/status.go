package forge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxDescription is GitHub's limit for a status description, in characters.
const MaxDescription = 140

// Context is the status context for a suite: `localci/<suite>`.
func Context(suite string) string { return "localci/" + suite }

// Queued is the pending status of a run waiting behind `inLine` others.
func Queued(suite string, inLine int, url string) Status {
	return Status{Suite: suite, State: StatePending, TargetURL: url,
		Description: truncate(fmt.Sprintf("queued · %d in line", inLine))}
}

// Running is the pending status of a run with done of total tests finished.
func Running(suite string, done, total int, since time.Time, url string) Status {
	return Status{Suite: suite, State: StatePending, TargetURL: url,
		Description: truncate(fmt.Sprintf("running · %d/%d · since %s", done, total, since.Format("15:04")))}
}

// Passed is the success status of a finished run.
func Passed(suite string, passed int, took time.Duration, url string) Status {
	return Status{Suite: suite, State: StateSuccess, TargetURL: url,
		Description: truncate(fmt.Sprintf("%d passed · %s", passed, FormatDuration(took)))}
}

// Failed is the failure status of a finished run; firstTitle names the first
// failing test and may be empty.
func Failed(suite string, failed int, firstTitle, url string) Status {
	d := fmt.Sprintf("%d failed", failed)
	if t := strings.Join(strings.Fields(firstTitle), " "); t != "" {
		d += ": " + t
	}
	return Status{Suite: suite, State: StateFailure, TargetURL: url, Description: truncate(d)}
}

// FormatDuration renders 6m12s, 45s or 1h03m12s.
func FormatDuration(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	switch {
	case s >= 3600:
		return fmt.Sprintf("%dh%02dm%02ds", s/3600, s%3600/60, s%60)
	case s >= 60:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%ds", s)
}

// truncate cuts s to MaxDescription characters, ending in an ellipsis.
func truncate(s string) string {
	r := []rune(s)
	if len(r) <= MaxDescription {
		return s
	}
	return string(r[:MaxDescription-1]) + "…"
}

// SetStatuses posts st on every PR head of the set (heads on a default branch
// have no PR and are skipped). It tries all heads and joins the errors.
func SetStatuses(ctx context.Context, f Forge, heads []Head, st Status) error {
	var errs []error
	for _, h := range heads {
		if h.PR == 0 {
			continue
		}
		if err := f.SetStatus(ctx, h.Repo, h.SHA, st); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", h.Repo, err))
		}
	}
	return errors.Join(errs...)
}
