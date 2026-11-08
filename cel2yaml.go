// Package cel2yaml compiles a YAML document with CEL expressions to a YAML
// document: YAML shaped like the document it computes, where ${{ <CEL> }}
// computes a value. The whole file is the output; its keys are keys, none
// is reserved but those starting with $.
//
//	key: ${{ expr }}              the whole scalar: the value keeps the expression's type
//	key: text ${{ expr }} text    interpolation: the value is a string
//	$${{                          a literal "${{"
//	$$key: ...                    a key starting with $, $key
//	key: {$if: ${{ cond }}, ...}  the key is left out when cond is false
//	key: {$if: ..., $value: x}    same, for a value that is not a map
//	key: {$if: ..., $value: x, $else: y}   x when cond is true, else y
//	key: {$for: c in <expr>, $key: ..., $value: ...}   a map built from a list
//	{$merge: ${{ map }}}          merged into the enclosing map
//	- {$for: c in <expr>, $each: item or [items]}     list items repeated per element
//	$vars: {name: ..., ...}       with $for: bindings per element, in order
//	- {$spread: ${{ list }}}      list items spliced in
//	- {$if: ${{ cond }}, ...}     the item is left out when cond is false
//
// The expressions see the variables of the scope the document is compiled
// on, and the functions of the language (functions.go) and the caller's
// (NewSession). A document is checked as a whole before anything is
// evaluated: a misspelled or misplaced directive, an expression that does
// not compile, fail in every branch, those no evaluation takes included. An
// error names the path of the expression in the document, a list position
// following its key without a separator: app: env[2]: value: ${{ ... }}:
// <error>.
//
// Render does it in one call; Parse, Session.Template and Template.Eval are
// its steps, for a caller that compiles several documents, or the parts of
// one, together: all of them compiled before any is evaluated.
package cel2yaml

import (
	"context"
	"maps"
	"slices"

	"go.yaml.in/yaml/v3"
	sigsyaml "sigs.k8s.io/yaml"
)

// DefaultCostLimit: the CEL cost the expressions of a compile may use
// together, in Render and in the command: about a second of work.
const DefaultCostLimit = 1_000_000

// Scope: the variables the expressions of a document see, by name: values
// as YAML and JSON hold them (maps with string keys, []any lists, float64
// numbers, strings, booleans, null).
type Scope = map[string]any

// Render: the document src compiled on the scope in a session of its own,
// cancelled with ctx and limited to DefaultCostLimit, written as YAML:
// block style, the keys of each map sorted, deterministic. A document a
// false $if leaves out, or an empty one, writes nothing: nil.
func Render(ctx context.Context, src []byte, scope Scope) ([]byte, error) {
	return NewSession(ctx, DefaultCostLimit).Render(src, scope)
}

// Render: src compiled on the scope in the session, as Render does, its
// cost counted with the session's. The document is one document of the
// session (Document).
func (s *Session) Render(src []byte, scope Scope) ([]byte, error) {
	n, err := Parse(src)
	if err != nil || n == nil {
		return nil, err
	}
	t, err := s.Template(n, slices.Sorted(maps.Keys(scope)))
	if err != nil {
		return nil, err
	}
	var v any
	var keep bool
	if err := s.Document(func() (err error) { v, keep, err = t.Eval(scope); return err }); err != nil || !keep {
		return nil, err
	}
	return sigsyaml.Marshal(v)
}

// Template: a node compiled once, in the environment of the names in scope
// where it stands, to the tree of nodes its evaluations run.
type Template struct {
	s    *Session
	root node
}

// Template: the node n (a nil node is null), compiled in the session where
// the names, the variables of the scope it is evaluated on, sorted, are in
// scope. Every directive and every expression is checked, in every branch,
// and every expression compiled, now: an error names the path of the
// node's content, without the node's own. Compile everything that is
// evaluated together before evaluating any of it.
func (s *Session) Template(n *yaml.Node, names []string) (*Template, error) {
	var v any
	if n != nil {
		var err error
		if v, err = fromNode(n); err != nil {
			return nil, err
		}
	}
	root, err := builder{s}.node(v, slices.Compact(slices.Sorted(slices.Values(names))))
	if err != nil {
		return nil, err
	}
	return &Template{s, root}, nil
}

// Eval: the template evaluated on scope, which holds the names it was
// compiled with, in its session: its value as data, keep false when it is
// left out ($if false, the value then null). The evaluation counts in the
// session's cost, and in the document being evaluated (Session.Document).
// An error names the path of the failing expression in the node.
func (t *Template) Eval(scope Scope) (v any, keep bool, err error) {
	return t.root.eval(t.s, scope)
}
