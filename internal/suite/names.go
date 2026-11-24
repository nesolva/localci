package suite

import "slices"

// Names lists the configured suites, sorted.
func (r *Runner) Names() []string {
	names := make([]string, 0, len(r.suites))
	for n := range r.suites {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}
