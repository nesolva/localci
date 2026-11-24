package config

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Line is one effective value for display.
type Line struct {
	Key    string
	Value  string
	Source Source
}

// Lines returns every key in schema order, human-formatted, with its source.
func (e *Effective) Lines() []Line {
	c := e.Config
	lines := make([]Line, 0, len(Fields))
	for i := range Fields {
		f := &Fields[i]
		lines = append(lines, Line{Key: f.Key, Value: display(f.ref(&c)), Source: e.Sources[f.Key]})
	}
	return lines
}

func display(v any) string {
	switch p := v.(type) {
	case *time.Duration:
		return formatDuration(*p)
	case *uint64:
		if *p == 0 {
			return "none"
		}
		return FormatSize(*p)
	case *[]string:
		return strings.Join(*p, ", ")
	case *[]Repo:
		if len(*p) == 0 {
			return "(none)"
		}
		parts := make([]string, len(*p))
		for i, r := range *p {
			parts[i] = r.Name
			if !r.Checks {
				parts[i] += " (no checks)"
			}
			if len(r.SkipWorkflows) > 0 {
				parts[i] += " (skip " + strings.Join(r.SkipWorkflows, ", ") + ")"
			}
		}
		return strings.Join(parts, ", ")
	case *map[string]any:
		if len(*p) == 0 {
			return "{}"
		}
		b, _ := json.Marshal(*p)
		return string(b)
	case *int:
		return fmt.Sprint(*p)
	case *bool:
		return fmt.Sprint(*p)
	case *string:
		return *p
	}
	return fmt.Sprint(v)
}

func jsonValue(v any) any {
	switch p := v.(type) {
	case *time.Duration:
		return formatDuration(*p)
	case *uint64:
		return *p
	case *[]string:
		if *p == nil {
			return []string{}
		}
		return *p
	case *[]Repo:
		if *p == nil {
			return []Repo{}
		}
		return *p
	case *map[string]any:
		return *p
	case *int:
		return *p
	case *bool:
		return *p
	case *string:
		return *p
	}
	return v
}

// JSON renders the effective config as nested objects, each leaf being
// {"value": ..., "source": ...}; sizes are bytes and durations like "60s".
func (e *Effective) JSON() ([]byte, error) {
	c := e.Config
	out := map[string]any{"config_file": e.File, "config_file_found": e.FileFound}
	for i := range Fields {
		f := &Fields[i]
		leaf := map[string]any{"value": jsonValue(f.ref(&c)), "source": e.Sources[f.Key]}
		if sec, name, ok := strings.Cut(f.Key, "."); ok {
			m, _ := out[sec].(map[string]any)
			if m == nil {
				m = map[string]any{}
				out[sec] = m
			}
			m[name] = leaf
		} else {
			out[f.Key] = leaf
		}
	}
	return json.MarshalIndent(out, "", "  ")
}
