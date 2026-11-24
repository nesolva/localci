package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var repoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// applyString parses raw (from a default, env var or flag) into the field.
// It reports whether the value was "auto".
func applyString(c *Config, f *Field, raw string) (auto bool, err error) {
	raw = strings.TrimSpace(raw)
	if f.Auto && strings.EqualFold(raw, "auto") {
		return true, nil
	}
	switch p := f.ref(c).(type) {
	case *int:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return false, fmt.Errorf("%q is not a whole number", raw)
		}
		if err := checkRange(f, n); err != nil {
			return false, err
		}
		*p = int(n)
	case *bool:
		switch strings.ToLower(raw) {
		case "true", "yes", "1":
			*p = true
		case "false", "no", "0":
			*p = false
		default:
			return false, fmt.Errorf("%q is not true or false", raw)
		}
	case *time.Duration:
		d, err := time.ParseDuration(raw)
		if err != nil {
			return false, fmt.Errorf("%q is not a duration (examples: 30s, 5m, 24h)", raw)
		}
		if f.Min > 0 && d < time.Duration(f.Min)*time.Second {
			return false, fmt.Errorf("%s is too short (minimum %s)", raw, formatDuration(time.Duration(f.Min)*time.Second))
		}
		*p = d
	case *string:
		if !contains(f.Enum, raw) {
			return false, enumError(raw, f)
		}
		*p = raw
	case *uint64:
		n, err := ParseSize(raw)
		if err != nil {
			return false, err
		}
		*p = n
	case *[]string:
		var items []string
		for _, s := range strings.Split(raw, ",") {
			if s = strings.TrimSpace(s); s != "" {
				items = append(items, s)
			}
		}
		for _, it := range items {
			if !contains(f.Enum, it) {
				return false, enumError(it, f)
			}
		}
		*p = items
	case *[]Repo:
		var repos []Repo
		for _, s := range strings.Split(raw, ",") {
			if s = strings.TrimSpace(s); s == "" {
				continue
			}
			if !repoName.MatchString(s) {
				return false, fmt.Errorf("%q is not a repository (expected owner/name)", s)
			}
			repos = append(repos, Repo{Name: s, Checks: true})
		}
		*p = repos
	default:
		return false, fmt.Errorf("cannot be set from a single value")
	}
	return false, nil
}

func checkRange(f *Field, n int64) error {
	if f.Min != 0 && n < f.Min || f.Max != 0 && n > f.Max {
		switch {
		case f.Min != 0 && f.Max != 0:
			return fmt.Errorf("%d is out of range (%d to %d)", n, f.Min, f.Max)
		case f.Min != 0:
			return fmt.Errorf("%d is too small (minimum %d)", n, f.Min)
		default:
			return fmt.Errorf("%d is too large (maximum %d)", n, f.Max)
		}
	}
	return nil
}

func enumError(got string, f *Field) error {
	opts := f.Enum
	if f.Auto {
		opts = append([]string{"auto"}, opts...)
	}
	msg := fmt.Sprintf("%q is not one of: %s", got, strings.Join(opts, ", "))
	if s := suggest(got, opts); s != "" {
		msg += fmt.Sprintf(" (did you mean %q?)", s)
	}
	return fmt.Errorf("%s", msg)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ParseSize reads "8GiB", "512MiB", "6g", "1.5G" or a plain byte count.
func ParseSize(s string) (uint64, error) {
	t := strings.TrimSpace(s)
	i := len(t)
	for i > 0 && (t[i-1] < '0' || t[i-1] > '9') && t[i-1] != '.' {
		i--
	}
	num, unit := strings.TrimSpace(t[:i]), strings.ToLower(strings.TrimSpace(t[i:]))
	n, err := strconv.ParseFloat(num, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%q is not a size (examples: 512MiB, 8GiB)", s)
	}
	mult := map[string]float64{
		"": 1, "b": 1,
		"k": 1 << 10, "kb": 1 << 10, "kib": 1 << 10,
		"m": 1 << 20, "mb": 1 << 20, "mib": 1 << 20,
		"g": 1 << 30, "gb": 1 << 30, "gib": 1 << 30,
		"t": 1 << 40, "tb": 1 << 40, "tib": 1 << 40,
	}
	m, ok := mult[unit]
	if !ok {
		return 0, fmt.Errorf("%q is not a size (unknown unit %q; use KiB, MiB, GiB or TiB)", s, t[i:])
	}
	return uint64(n * m), nil
}

// FormatSize renders bytes as a short binary size.
func FormatSize(b uint64) string {
	const unit = 1024.0
	v := float64(b)
	for _, u := range []string{"B", "KiB", "MiB", "GiB"} {
		if v < unit {
			return trimFloat(v) + " " + u
		}
		v /= unit
	}
	return trimFloat(v) + " TiB"
}

func trimFloat(v float64) string {
	return strings.TrimSuffix(strings.TrimSuffix(strconv.FormatFloat(v, 'f', 1, 64), "0"), ".")
}

// formatDuration prints 60s as "60s", 5m as "5m" and 24h as "24h": the form a user writes.
func formatDuration(d time.Duration) string {
	switch {
	case d == 0:
		return "0s"
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0 && d >= 2*time.Minute:
		return fmt.Sprintf("%dm", d/time.Minute)
	case d%time.Second == 0:
		return fmt.Sprintf("%ds", d/time.Second)
	}
	return d.String()
}
