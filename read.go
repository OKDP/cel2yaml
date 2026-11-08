package cel2yaml

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/okdp/cel2yaml/internal/value"
)

// M: a values map (value.M).
type M = value.M

// omap: an ordered YAML map: its entries in the order written, their keys
// as written, and an index of its keys (the last of a key written twice).
type omap struct {
	entries []entry
	index   map[string]int // key -> its entry
}

// entry: a key of a map and its value.
type entry struct {
	key string
	val any
}

// lookup: the value of the key k, and whether m has it.
func (m *omap) lookup(k string) (any, bool) {
	i, ok := m.index[k]
	if !ok {
		return nil, false
	}
	return m.entries[i].val, true
}

// get: the value of the key k, null when m does not have it.
func (m *omap) get(k string) any {
	v, _ := m.lookup(k)
	return v
}

// fields: the entries of m, in order.
func (m *omap) fields() []entry { return m.entries }

// fromNode: a YAML node as data, maps as omap to keep their key order,
// scalars as yaml.v3 decodes them (YAML 1.2: yes and on are text), then as
// values hold them: every number a float64, text UTF-8 (value.Normalize).
// A timestamp stays text, as in Helm; an alias is refused.
func fromNode(n *yaml.Node) (any, error) {
	switch n.Kind {
	case yaml.DocumentNode:
		return fromNode(n.Content[0])
	case yaml.AliasNode:
		return nil, fmt.Errorf("line %d: aliases are not read", n.Line)
	case yaml.MappingNode:
		m := &omap{index: map[string]int{}}
		for i := 0; i < len(n.Content); i += 2 {
			v, err := fromNode(n.Content[i+1])
			if err != nil {
				return nil, err
			}
			m.index[n.Content[i].Value] = len(m.entries)
			m.entries = append(m.entries, entry{n.Content[i].Value, v})
		}
		return m, nil
	case yaml.SequenceNode:
		out := []any{}
		for _, c := range n.Content {
			v, err := fromNode(c)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	if n.ShortTag() == "!!timestamp" {
		return n.Value, nil
	}
	var v any
	err := n.Decode(&v)
	if err == nil {
		v, err = value.Normalize(v)
	}
	if err != nil {
		return nil, fmt.Errorf("line %d: %w", n.Line, err)
	}
	return v, nil
}

// Parse: a YAML document with expressions as a node, the first YAML
// document of b, nil when b has none (empty, or only comments). As in a
// values file Helm reads, its last line ends with a line break: a | or >
// block scalar ending the input keeps its newline. The document is read as
// Template reads it, so that what it refuses is refused here: an alias, a
// scalar that has no data form (.nan, !!binary of bytes that are not UTF-8,
// a tag the scalar does not meet), the error starting with its line.
func Parse(b []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(strings.TrimSuffix(string(b), "\n")+"\n"), &doc); err != nil || doc.Kind == 0 {
		return nil, err
	}
	if _, err := fromNode(&doc); err != nil {
		return nil, err
	}
	return doc.Content[0], nil
}

// Data: n as plain data, no expression evaluated: maps with string keys (the
// later of a key written twice), []any lists, float64 numbers, strings,
// booleans, null; nil for a nil node. What Template reads of n, scalars as
// yaml.v3 decodes them (YAML 1.2: yes and on are text), a timestamp left as
// text; an alias, or a scalar that has no data form, fails with its line.
func Data(n *yaml.Node) (any, error) {
	if n == nil {
		return nil, nil
	}
	v, err := fromNode(n)
	return plain(v), err
}

// plain: a value fromNode read, its maps as values maps.
func plain(v any) any {
	switch t := v.(type) {
	case *omap:
		m := M{}
		for _, f := range t.fields() {
			m[f.key] = plain(f.val)
		}
		return m
	case []any:
		l := make([]any, len(t))
		for i, x := range t {
			l[i] = plain(x)
		}
		return l
	}
	return v
}
