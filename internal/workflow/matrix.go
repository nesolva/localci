package workflow

import (
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// combo is one matrix cell: the values in the order GitHub shows them.
type combo struct {
	keys  []string
	vals  map[string]string
	typed map[string]val // the YAML type of each value, for expressions
}

func newCombo() *combo { return &combo{vals: map[string]string{}, typed: map[string]val{}} }

func (c *combo) set(k, v string, t val) {
	if _, ok := c.vals[k]; !ok {
		c.keys = append(c.keys, k)
	}
	c.vals[k] = v
	c.typed[k] = t
}

func (c *combo) clone() *combo {
	n := newCombo()
	for _, k := range c.keys {
		n.set(k, c.vals[k], c.typed[k])
	}
	return n
}

func (c *combo) matches(filter map[string]string) bool {
	for k, v := range filter {
		if cv, ok := c.vals[k]; !ok || cv != v {
			return false
		}
	}
	return true
}

// scalar renders a YAML node as the text a workflow author wrote ("3.10"
// stays "3.10"); collections become compact JSON.
func scalar(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	if n.Kind == yaml.AliasNode {
		return scalar(n.Alias)
	}
	if n.Kind == yaml.ScalarNode {
		if n.Tag == "!!null" {
			return ""
		}
		return n.Value
	}
	var v any
	if err := n.Decode(&v); err != nil {
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func mapPairs(n *yaml.Node) (keys []string, vals map[string]*yaml.Node) {
	if n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	vals = map[string]*yaml.Node{}
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, vals
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i].Value
		keys = append(keys, k)
		vals[k] = n.Content[i+1]
	}
	return keys, vals
}

// expandMatrix expands strategy.matrix into its cells. A job without a matrix
// yields one empty cell. The second result explains matrices it cannot expand.
func expandMatrix(n *yaml.Node) ([]*combo, string) {
	if n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n == nil || n.Kind == 0 {
		return []*combo{newCombo()}, ""
	}
	if n.Kind != yaml.MappingNode {
		return []*combo{newCombo()}, "strategy.matrix is computed at run time (" + scalar(n) + ")"
	}
	keys, vals := mapPairs(n)

	var axes []string
	for _, k := range keys {
		if k != "include" && k != "exclude" {
			axes = append(axes, k)
		}
	}

	var combos []*combo
	if len(axes) > 0 {
		combos = []*combo{newCombo()}
		for _, ax := range axes {
			an := vals[ax]
			if an.Kind == yaml.AliasNode {
				an = an.Alias
			}
			if an.Kind != yaml.SequenceNode {
				return []*combo{newCombo()}, fmt.Sprintf("strategy.matrix.%s is computed at run time", ax)
			}
			var next []*combo
			for _, c := range combos {
				for _, item := range an.Content {
					nc := c.clone()
					nc.set(ax, scalar(item), typedVal(item))
					next = append(next, nc)
				}
			}
			combos = next
		}
	}

	if ex, ok := vals["exclude"]; ok {
		for _, e := range seqMaps(ex) {
			var kept []*combo
			for _, c := range combos {
				if !c.matches(e) {
					kept = append(kept, c)
				}
			}
			combos = kept
		}
	}

	if inc, ok := vals["include"]; ok {
		orig := map[string]bool{}
		for _, a := range axes {
			orig[a] = true
		}
		for _, entry := range seqMapsOrdered(inc) {
			filter := map[string]string{}
			for _, kv := range entry {
				if orig[kv.k] {
					filter[kv.k] = kv.v
				}
			}
			added := false
			for _, c := range combos {
				if len(axes) > 0 && c.matches(filter) {
					for _, kv := range entry {
						if !orig[kv.k] {
							c.set(kv.k, kv.v, kv.t)
						}
					}
					added = true
				}
			}
			if !added {
				nc := newCombo()
				for _, kv := range entry {
					nc.set(kv.k, kv.v, kv.t)
				}
				combos = append(combos, nc)
			}
		}
	}
	if len(combos) == 0 {
		return nil, ""
	}
	return combos, ""
}

func seqMaps(n *yaml.Node) []map[string]string {
	var out []map[string]string
	for _, entry := range seqMapsOrdered(n) {
		m := map[string]string{}
		for _, kv := range entry {
			m[kv.k] = kv.v
		}
		out = append(out, m)
	}
	return out
}

type mapEntry struct {
	k, v string
	t    val
}

// seqMapsOrdered reads a list of maps as ordered key/value pairs.
func seqMapsOrdered(n *yaml.Node) [][]mapEntry {
	if n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	var out [][]mapEntry
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	for _, item := range n.Content {
		ks, vs := mapPairs(item)
		var entry []mapEntry
		for _, k := range ks {
			entry = append(entry, mapEntry{k, scalar(vs[k]), typedVal(vs[k])})
		}
		out = append(out, entry)
	}
	return out
}

// matrixSuffix is the " (a, b)" GitHub appends to a matrix job's name.
func (c *combo) suffix() string {
	if len(c.keys) == 0 {
		return ""
	}
	vs := make([]string, len(c.keys))
	for i, k := range c.keys {
		vs[i] = c.vals[k]
	}
	return " (" + strings.Join(vs, ", ") + ")"
}
