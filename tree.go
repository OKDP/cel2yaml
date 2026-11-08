// Tree: a document compiled once, when it is loaded (Session.Template), into a
// tree of typed nodes: each map by the directives that give its form, each
// string scanned, each expression compiled in the environment of the names
// in scope at its place. Building the tree is the check of the language: a
// misspelled or misplaced directive, an expression that does not compile,
// fail in every branch, those no evaluation takes included. Evaluation then
// runs the tree, a method per node type, without scanning or compiling.

package cel2yaml

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/okdp/cel2yaml/internal/value"
)

// node: a node of the tree, evaluated on a scope holding the names it was
// built with: its value, keep false when it is left out ($if false).
type node interface {
	eval(s *Session, scope M) (v any, keep bool, err error)
}

// item: a list item, evaluated: the values it gives, none ($if false),
// one, or several ($for, $spread).
type item interface {
	items(s *Session, scope M) ([]any, error)
}

// constant: a scalar without expression: a number, a boolean, null, or a
// string, $${{ read as ${{.
type constant struct{ v any }

// eval: the scalar, its size counted (Session.grow): the values hold it
// once more.
func (c constant) eval(s *Session, _ M) (any, bool, error) {
	n := uint64(scalarMem)
	if str, ok := c.v.(string); ok {
		n += uint64(len(str))
	}
	return c.v, true, s.grow(n)
}

// eval: a lone expression, the whole scalar: its value, of its type.
func (e *expr) eval(s *Session, scope M) (any, bool, error) {
	v, err := s.run(e, scope)
	return v, true, err
}

// text: a string of literal parts and expressions, interpolated: each
// expression's value written as text (value.Str).
type text []part

// part: a literal text, or an expression (e).
type part struct {
	lit string
	e   *expr
}

// eval: the text, its expressions evaluated in order.
func (t text) eval(s *Session, scope M) (any, bool, error) {
	var b strings.Builder
	for _, p := range t {
		if p.e == nil {
			b.WriteString(p.lit)
			continue
		}
		v, err := s.run(p.e, scope)
		if err != nil {
			return nil, false, err
		}
		b.WriteString(value.Str(v))
	}
	return b.String(), true, s.grow(scalarMem + uint64(b.Len()))
}

// list: a YAML list, its items in order; an error at the position of the
// item as written.
type list []item

// eval: the values of the items, spliced, their slots counted
// (Session.grow), their values by the nodes that give them.
func (l list) eval(s *Session, scope M) (any, bool, error) {
	out := []any{}
	for i, it := range l {
		vs, err := it.items(s, scope)
		if err == nil {
			err = s.grow(slotMem * uint64(len(vs)))
		}
		if err != nil {
			return nil, false, atIndex(i, err)
		}
		out = append(out, vs...)
	}
	return out, true, s.grow(containerMem)
}

// one: a list item that is one node: its value, none when it is left out.
type one struct{ n node }

// items: the node's value, kept.
func (o one) items(s *Session, scope M) ([]any, error) {
	v, keep, err := o.n.eval(s, scope)
	if err != nil || !keep {
		return nil, err
	}
	return []any{v}, nil
}

// spread: a list item {$spread: <list>}: the items of the list, none when
// the value is not a list.
type spread struct{ n node }

// items: the list's items.
func (sp spread) items(s *Session, scope M) ([]any, error) {
	v, _, err := sp.n.eval(s, scope)
	if err != nil {
		return nil, err
	}
	return value.AsList(v), nil
}

// plainMap: a map of keys, each evaluated in order, then the map of its
// $merge, when it has one, merged over them.
type plainMap struct {
	fields []field
	merge  node // nil: no $merge
}

// field: a key of a plain map: as read (key, which errors name) and as
// written to the values (out: a key starting with $$ has one $ less), and
// its node.
type field struct {
	key, out string
	n        node
}

// eval: the map: its keys kept, then $merge, a copy of its map merged
// over them; its entries and the copy counted (Session.grow).
func (m plainMap) eval(s *Session, scope M) (any, bool, error) {
	out := M{}
	if err := s.grow(containerMem); err != nil {
		return nil, false, err
	}
	for _, f := range m.fields {
		v, keep, err := f.n.eval(s, scope)
		if err == nil && keep {
			err = s.grow(slotMem + uint64(len(f.out)))
		}
		if err != nil {
			return nil, false, atKey(f.key, err)
		}
		if keep {
			out[f.out] = v
		}
	}
	if m.merge != nil {
		v, _, err := m.merge.eval(s, scope)
		if err != nil {
			return nil, false, err
		}
		src := value.AsMap(v)
		n, ok := dataSize(src, maxDocSize-s.built)
		if !ok {
			return nil, false, s.sizeError()
		}
		if err := s.grow(n); err != nil {
			return nil, false, err
		}
		out = value.Merge(out, src)
	}
	return out, true, nil
}

// ifNode: a map with $if: the map in its form (plain, $for or $value) when
// its condition is true (as Helm tests a condition: empty is false); else
// its $else (a map with $value), or left out without one.
type ifNode struct {
	cond      node
	then, els node // els nil: no $else
}

// eval: the condition, then the branch it takes.
func (n ifNode) eval(s *Session, scope M) (any, bool, error) {
	c, _, err := n.cond.eval(s, scope)
	switch {
	case err != nil:
		return nil, false, err
	case value.Truthy(c):
		return n.then.eval(s, scope)
	case n.els != nil:
		return n.els.eval(s, scope)
	}
	return nil, false, nil
}

// loop: a $for: its variable, the list it runs over (the expression after
// " in "), the spec as written (for messages), and the $vars bindings of
// each element.
type loop struct {
	name, spec string
	list       node
	vars       bindings
}

// run: f on the scope of each element of the list, in order: scope, the
// variable bound to the element, then the $vars bindings. A null list has
// no element; a value that is not a list fails. Each element counts 1 in
// the CEL cost, the context checked (Session.charge), and what its body
// builds counts too (Session.grow): a $for repeats its body without an
// evaluation of CEL's own, three nested over a list of 150 elements
// building 3.4 million items from an expression of cost 150.
func (l *loop) run(s *Session, scope M, f func(local M) error) error {
	v, _, err := l.list.eval(s, scope)
	if err != nil {
		return err
	}
	elems, ok := v.([]any)
	if !ok && v != nil {
		return fmt.Errorf("$for %q: not a list", l.spec)
	}
	if len(elems) == 0 {
		return nil
	}
	// One copy of scope for the elements, its variable and $vars bound
	// again for each: an evaluation keeps no scope, its value being data.
	local := make(M, len(scope)+1+len(l.vars))
	maps.Copy(local, scope)
	s.loops++
	defer func() { s.loops-- }()
	for _, x := range elems {
		if err := s.charge(1); err != nil {
			return fmt.Errorf("$for %q: %w", l.spec, err)
		}
		local[l.name] = x
		if err := l.vars.bind(s, local); err != nil {
			return err
		}
		if err := f(local); err != nil {
			return err
		}
	}
	return nil
}

// forMap: a map with $for: one key per element, $key as text, its value
// $value (null when left out); a later element of the same key wins.
type forMap struct {
	loop
	key, val node
}

// eval: the map of the elements.
func (m *forMap) eval(s *Session, scope M) (any, bool, error) {
	out := M{}
	err := m.run(s, scope, func(local M) error {
		k, _, err := m.key.eval(s, local)
		if err != nil {
			return err
		}
		v, _, err := m.val.eval(s, local)
		if err != nil {
			return err
		}
		key := value.Str(k)
		out[key] = v
		return s.grow(slotMem + uint64(len(key)))
	})
	if err != nil {
		return nil, false, err
	}
	return out, true, s.grow(containerMem)
}

// forItem: a list item with $for: the items $each gives per element, a
// list spliced, a value left out by $if none.
type forItem struct {
	loop
	each node
}

// items: the items of the elements.
func (it *forItem) items(s *Session, scope M) ([]any, error) {
	var out []any
	err := it.run(s, scope, func(local M) error {
		v, keep, err := it.each.eval(s, local)
		if err != nil {
			return err
		}
		if l, ok := v.([]any); ok {
			out = append(out, l...)
		} else if keep {
			out = append(out, v)
		}
		return nil
	})
	return out, err
}

// bindings: $vars: named nodes, each built with the names before it in
// scope.
type bindings []binding

// binding: a name, as written, and its node; at, its path in errors
// ("$vars <name>").
type binding struct {
	name, at string
	n        node
}

// bind: the bindings evaluated in order, each added to scope, in place, as
// soon as it is evaluated: a value left out by $if binds null.
func (bs bindings) bind(s *Session, scope M) error {
	for _, b := range bs {
		v, _, err := b.n.eval(s, scope)
		if err != nil {
			return atKey(b.at, err)
		}
		scope[b.name] = v
	}
	return nil
}

// exprError: an error of an expression found when the tree is built, where
// evaluation found it before: a string that does not scan, an expression
// that does not compile, a $for that is not "<name> in <expression>". It
// is reported at the path of evaluation's errors (under).
type exprError struct{ err error }

// Error: the error.
func (e *exprError) Error() string { return e.err.Error() }

// Unwrap: the error.
func (e *exprError) Unwrap() error { return e.err }

// under: err, an error of the node of a directive d ($key, $value, $each):
// a mistake of the language under d; an expression's error (exprError) at
// the map or item holding d, where its evaluation fails.
func under(d string, err error) error {
	if err == nil || errors.As(err, new(*exprError)) {
		return err
	}
	return atKey(d, err)
}

// builder: builds the tree of a document in a session, whose environments
// compile its expressions.
type builder struct{ s *Session }

// node: the node of n, a value of the document, where names are in scope:
// a string scanned and compiled (scalar), a list, a map in the form its
// directives give it, another scalar as it is.
func (b builder) node(n any, names []string) (node, error) {
	switch t := n.(type) {
	case string:
		return b.scalar(t, names)
	case []any:
		return b.list(t, names)
	case *omap:
		return b.mapNode(t, names)
	}
	return constant{n}, nil
}

// mapNode: the node of a map, in the form its directives give it: $for,
// else $value, else plain.
func (b builder) mapNode(m *omap, names []string) (node, error) {
	switch {
	case has(m, "$for"):
		return b.forMap(m, names)
	case has(m, "$value"):
		return b.valueMap(m, names)
	}
	return b.plainMap(m, names)
}

// list: the node of a list, its items built in order; an error at the
// position of its item.
func (b builder) list(l []any, names []string) (list, error) {
	out := make(list, 0, len(l))
	for i, x := range l {
		it, err := b.item(x, names)
		if err != nil {
			return nil, atIndex(i, err)
		}
		out = append(out, it)
	}
	return out, nil
}

// item: a list item: a map with $for, or $spread, gives items; any other
// node is one.
func (b builder) item(x any, names []string) (item, error) {
	switch m, ok := x.(*omap); {
	case ok && has(m, "$for"):
		return b.forItem(m, names)
	case ok && has(m, "$spread"):
		return b.spread(m, names)
	}
	n, err := b.node(x, names)
	if err != nil {
		return nil, err
	}
	return one{n}, nil
}

// scalar: the node of a string: its expressions scanned once and compiled;
// a lone one is the whole scalar; without any (one piece of text at most,
// as scan joins the text between expressions), the text it reads.
func (b builder) scalar(str string, names []string) (node, error) {
	pieces, err := scan(str)
	switch {
	case err != nil:
		return nil, &exprError{err}
	case len(pieces) == 0:
		return constant{""}, nil
	case len(pieces) == 1 && !pieces[0].expr:
		return constant{pieces[0].text}, nil
	case len(pieces) == 1:
		e, err := b.compile(pieces[0].text, names)
		if err != nil {
			return nil, err
		}
		return e, nil
	}
	t := make(text, len(pieces))
	for i, p := range pieces {
		if !p.expr {
			t[i].lit = p.text
		} else if t[i].e, err = b.compile(p.text, names); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// compile: the expression src, compiled in the environment of names; its
// error is the expression's (exprError).
func (b builder) compile(src string, names []string) (*expr, error) {
	e, err := b.s.compile(src, names)
	if err != nil {
		return nil, &exprError{err}
	}
	return e, nil
}

// plainMap: a map of keys, a map value or a list item: its keys built in
// order, with $if and $merge.
func (b builder) plainMap(m *omap, names []string) (node, error) {
	if err := check(m, true, "$if", "$merge"); err != nil {
		return nil, err
	}
	cond, err := b.cond(m, names)
	if err != nil {
		return nil, err
	}
	var out plainMap
	if has(m, "$merge") {
		if out.merge, err = b.node(m.get("$merge"), names); err != nil {
			return nil, under("$merge", err)
		}
	}
	for _, f := range m.fields() {
		if !literal(f.key) {
			continue
		}
		n, err := b.node(f.val, names)
		if err != nil {
			return nil, atKey(f.key, err)
		}
		out.fields = append(out.fields, field{f.key, strings.TrimPrefix(f.key, "$"), n})
	}
	return guard(cond, out), nil
}

// valueMap: a map with $value: the node of $value, with $if, and the node
// of $else, its value when the condition is not true (without $if, $else
// is not read).
func (b builder) valueMap(m *omap, names []string) (node, error) {
	if err := check(m, false, "$if", "$value", "$else"); err != nil {
		return nil, err
	}
	cond, err := b.cond(m, names)
	if err != nil {
		return nil, err
	}
	n, err := b.node(m.get("$value"), names)
	if err != nil {
		return nil, under("$value", err)
	}
	if cond == nil || !has(m, "$else") {
		return guard(cond, n), nil
	}
	els, err := b.node(m.get("$else"), names)
	if err != nil {
		return nil, under("$else", err)
	}
	return ifNode{cond, n, els}, nil
}

// forMap: a map with $for: its $key and $value built in the scope of an
// element, with $if.
func (b builder) forMap(m *omap, names []string) (node, error) {
	if err := check(m, false, "$if", "$for", "$key", "$value", "$vars"); err != nil {
		return nil, err
	}
	if !has(m, "$key") || !has(m, "$value") {
		return nil, atKey("$for", fmt.Errorf("in a map, needs $key and $value"))
	}
	cond, err := b.cond(m, names)
	if err != nil {
		return nil, err
	}
	l, local, err := b.loop(m, names)
	if err != nil {
		return nil, err
	}
	out := &forMap{loop: l}
	if out.key, err = b.node(m.get("$key"), local); err != nil {
		return nil, under("$key", err)
	}
	if out.val, err = b.node(m.get("$value"), local); err != nil {
		return nil, under("$value", err)
	}
	return guard(cond, out), nil
}

// forItem: a list item with $for: its $each built in the scope of an
// element.
func (b builder) forItem(m *omap, names []string) (item, error) {
	if err := check(m, false, "$for", "$each", "$vars"); err != nil {
		return nil, err
	}
	if !has(m, "$each") {
		return nil, atKey("$for", fmt.Errorf("in a list item, needs $each"))
	}
	l, local, err := b.loop(m, names)
	if err != nil {
		return nil, err
	}
	out := &forItem{loop: l}
	if out.each, err = b.node(m.get("$each"), local); err != nil {
		return nil, under("$each", err)
	}
	return out, nil
}

// spread: a list item with $spread.
func (b builder) spread(m *omap, names []string) (item, error) {
	if err := check(m, false, "$spread"); err != nil {
		return nil, err
	}
	n, err := b.node(m.get("$spread"), names)
	if err != nil {
		return nil, under("$spread", err)
	}
	return spread{n}, nil
}

// loop: the $for of m, "<name> in <expression>": the expression built
// where names are in scope, the $vars bindings after the variable; and the
// names in scope of an element.
func (b builder) loop(m *omap, names []string) (loop, []string, error) {
	spec := value.Str(m.get("$for"))
	name, src, ok := strings.Cut(spec, " in ")
	if !ok {
		return loop{}, nil, &exprError{fmt.Errorf("$for: %q is not \"<name> in <expression>\"", spec)}
	}
	l := loop{name: strings.TrimSpace(name), spec: spec}
	var err error
	if l.list, err = b.scalar("${{"+src+"}}", names); err != nil {
		return loop{}, nil, err
	}
	local := withName(names, l.name)
	if l.vars, local, err = b.bindings(m.get("$vars"), local); err != nil {
		return loop{}, nil, err
	}
	return l, local, nil
}

// cond: the condition of m's $if, nil without one.
func (b builder) cond(m *omap, names []string) (node, error) {
	if !has(m, "$if") {
		return nil, nil
	}
	n, err := b.node(m.get("$if"), names)
	return n, under("$if", err)
}

// guard: n, left out when cond is not true; n itself without a condition.
func guard(cond, n node) node {
	if cond == nil {
		return n
	}
	return ifNode{cond: cond, then: n}
}

// bindings: a $vars map, built: its names as written (a name bound twice,
// y and "y", the later wins), each value built with the names before it in
// scope. Null has none. The names in scope after them are returned too.
func (b builder) bindings(v any, names []string) (bindings, []string, error) {
	if v == nil {
		return nil, names, nil
	}
	m, ok := v.(*omap)
	if !ok {
		return nil, nil, atKey("$vars", fmt.Errorf("must be a map of bindings, got %s", shape(v)))
	}
	out := make(bindings, 0, len(m.entries))
	for _, e := range m.entries {
		at := "$vars " + e.key
		n, err := b.node(e.val, names)
		if err != nil {
			return nil, nil, atKey(at, err)
		}
		out = append(out, binding{e.key, at, n})
		names = withName(names, e.key)
	}
	return out, names, nil
}

// withName: names, sorted, with n.
func withName(names []string, n string) []string {
	i, found := slices.BinarySearch(names, n)
	if found {
		return names
	}
	return slices.Insert(slices.Clip(names), i, n)
}

// check: every key of m is one of the directives dirs of its form, or a
// key when keys is set.
func check(m *omap, keys bool, dirs ...string) error {
	for _, f := range m.fields() {
		if k := f.key; (!keys || !literal(k)) && !slices.Contains(dirs, k) {
			return atKey(k, fmt.Errorf("not allowed here (allowed: %s)", strings.Join(dirs, ", ")))
		}
	}
	return nil
}

// literal: k is a key, not a directive: it does not start with $, or it
// starts with $$ (the key is written with one $ less).
func literal(k string) bool { return !strings.HasPrefix(k, "$") || strings.HasPrefix(k, "$$") }

// has: m holds the key k.
func has(m *omap, k string) bool {
	_, ok := m.index[k]
	return ok
}

// shape: a node for a message: a map or a list by its kind, null, else the
// scalar as Go's %#v prints it.
func shape(n any) string {
	switch n.(type) {
	case *omap:
		return "a map"
	case []any:
		return "a list"
	case nil:
		return "null"
	}
	return value.Short(n)
}

// atKey: err, under the key k (a map key, "$vars <name>"): its path in the
// document, down to the expression (app: env[2]: value: ...); a list
// position follows the key before it without a separator.
func atKey(k string, err error) error {
	if strings.HasPrefix(err.Error(), "[") {
		return fmt.Errorf("%s%w", k, err)
	}
	return fmt.Errorf("%s: %w", k, err)
}

// atIndex: err, at the position i of a list.
func atIndex(i int, err error) error { return atKey(fmt.Sprintf("[%d]", i), err) }
