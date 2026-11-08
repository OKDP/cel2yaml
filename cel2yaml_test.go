package cel2yaml

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/okdp/cel2yaml/internal/value"
)

// mapCtx: a small scope for document tests.
func mapCtx() M {
	return M{
		"release": M{"name": "demo-x", "namespace": "demo"},
		"values": M{
			"items": []any{M{"name": "a", "n": float64(1)}, M{"name": "b", "n": float64(2)}},
			"on":    true,
			"off":   false,
			"extra": M{"k": "v", "x": float64(2)},
		},
	}
}

// evalIn: the document src compiled and evaluated on scope in the session
// s, as data; keep false when it is left out.
func evalIn(s *Session, src string, scope M) (v any, keep bool, err error) {
	n, err := Parse([]byte(src))
	if err != nil {
		return nil, false, err
	}
	t, err := s.Template(n, slices.Sorted(maps.Keys(scope)))
	if err != nil {
		return nil, false, err
	}
	err = s.Document(func() (err error) { v, keep, err = t.Eval(scope); return err })
	return v, keep, err
}

// run: the document src on mapCtx, in a session without a limit, as data.
func run(src string) (any, error) {
	v, _, err := evalIn(NewSession(context.Background(), 0), src, mapCtx())
	return v, err
}

// runMap: run, the document a map.
func runMap(src string) (M, error) {
	v, err := run(src)
	return value.AsMap(v), err
}

// evalExpr: one CEL expression, compiled and evaluated in scope.
func evalExpr(s *Session, src string, scope M) (any, error) {
	e, err := s.compile(src, slices.Sorted(maps.Keys(scope)))
	if err != nil {
		return nil, err
	}
	return s.run(e, scope)
}

// TestDirectives: each directive of the language, on one key x.
func TestDirectives(t *testing.T) {
	for name, tc := range map[string]struct {
		src  string // the value of x
		want M
	}{
		"lone expression keeps its type": {
			`{num: '${{ values.items[1].n }}', b: '${{ values.on }}', l: '${{ [1, 2] }}'}`,
			M{"num": float64(2), "b": true, "l": []any{float64(1), float64(2)}},
		},
		"interpolation is text": {
			`{s: 'n=${{ values.items[1].n }} on=${{ values.on }}'}`,
			M{"s": "n=2 on=true"},
		},
		"escaped expression": {
			`{s: '$${{ not evaluated }}'}`,
			M{"s": "${{ not evaluated }}"},
		},
		"braces in a string literal": {
			`{s: '${{ {"k": "}}"}.k }}'}`,
			M{"s": "}}"},
		},
		"functions str and first": {
			`{s: '${{ str(1.5) }}', f: '${{ first(["", "y", "z"]) }}', e: '${{ first(["", ""]) }}', l: '${{ first("x") }}'}`,
			M{"s": "1.5", "f": "y", "e": "", "l": ""},
		},
		"$if on a key": {
			`{dropped: {$if: '${{ values.off }}', a: 1}, kept: {$if: '${{ values.on }}', a: 1}}`,
			M{"kept": M{"a": float64(1)}},
		},
		"$if with $value": {
			`{dropped: {$if: '${{ values.off }}', $value: 1}, kept: {$if: '${{ values.on }}', $value: [1]}}`,
			M{"kept": []any{float64(1)}},
		},
		"$else when the condition is not true": {
			`{a: {$if: '${{ values.on }}', $value: 1, $else: 2}, b: {$if: '${{ values.off }}', $value: 1, $else: {c: '${{ values.items[0].name }}'}}}`,
			M{"a": float64(1), "b": M{"c": "a"}},
		},
		"$else not evaluated when the condition is true": {
			`{a: {$if: '${{ values.on }}', $value: 1, $else: '${{ values.nope }}'}}`,
			M{"a": float64(1)},
		},
		"$else list is one item": {
			`{l: [0, {$if: '${{ values.off }}', $value: 1, $else: [2, 3]}]}`,
			M{"l": []any{float64(0), []any{float64(2), float64(3)}}},
		},
		"$else without $if is not read": {
			`{a: {$value: 1, $else: '${{ nope }}'}}`,
			M{"a": float64(1)},
		},
		"$else nested, an else if": {
			`{a: {$if: '${{ values.off }}', $value: 1, $else: {$if: '${{ values.off }}', $value: 2, $else: 3}}, b: {$if: '${{ values.off }}', $value: 1, $else: {$if: '${{ values.off }}', $value: 2}}}`,
			M{"a": float64(3)},
		},
		"$if on a list item": {
			`{l: [{$if: '${{ values.off }}', a: 1}, {b: 2}]}`,
			M{"l": []any{M{"b": float64(2)}}},
		},
		"$for in a map": {
			`{m: {$for: c in values.items, $key: '${{ c.name }}', $value: '${{ c.n }}'}}`,
			M{"m": M{"a": float64(1), "b": float64(2)}},
		},
		"$for in a map, guarded by $if": {
			`{m: {$if: '${{ values.off }}', $for: c in values.items, $key: '${{ c.name }}', $value: 1}}`,
			M{},
		},
		"$for in a list, one item each": {
			`{l: [0, {$for: c in values.items, $each: '${{ c.name }}'}]}`,
			M{"l": []any{float64(0), "a", "b"}},
		},
		"$for in a list, several items each": {
			`{l: [{$for: c in values.items, $each: ['${{ c.name }}', '${{ c.name }}2']}]}`,
			M{"l": []any{"a", "a2", "b", "b2"}},
		},
		"$vars bindings in order": {
			`{l: [{$for: c in values.items, $vars: {d: '${{ c.n * 2.0 }}', e: '${{ d + 1.0 }}'}, $each: '${{ e }}'}]}`,
			M{"l": []any{float64(3), float64(5)}},
		},
		"$spread": {
			`{l: [0, {$spread: '${{ [1, 2] }}'}, 3]}`,
			M{"l": []any{float64(0), float64(1), float64(2), float64(3)}},
		},
		"$merge on top of the keys": {
			`{m: {x: 1, w: 1, $merge: '${{ values.extra }}'}}`,
			M{"m": M{"x": float64(2), "w": float64(1), "k": "v"}},
		},
		"$$ writes a key starting with $": {
			`{$$name: 1, m: {$$if: a, $$$x: b}}`,
			M{"$name": float64(1), "m": M{"$if": "a", "$$x": "b"}},
		},
		"directives of any node": {
			`{a: {$if: false, b: 1}, c: {$if: 'x${{ false }}', $value: 1}, m: {x: 1, $merge: {z: 2}}, l: [{$spread: [1, '${{ 2 }}']}]}`,
			M{"c": float64(1), "m": M{"x": float64(1), "z": float64(2)}, "l": []any{float64(1), float64(2)}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := runMap("x: " + tc.src + "\n")
			if err != nil {
				t.Fatal(err)
			}
			if got := value.AsMap(out["x"]); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

// TestRoot: the root of a document is a node as any other: a map with
// directives, a list, a scalar; a key named vars or errors is a key.
func TestRoot(t *testing.T) {
	for name, tc := range map[string]struct {
		src  string
		want any
		keep bool
	}{
		"a map":               {"a: 1\nvars: {b: '${{ values.on }}'}\nerrors: [x]\n", M{"a": float64(1), "vars": M{"b": true}, "errors": []any{"x"}}, true},
		"a list":              {"- 1\n- '${{ values.items[1].name }}'\n", []any{float64(1), "b"}, true},
		"a scalar":            {"'${{ values.items.size() }}'\n", float64(2), true},
		"$if true":            {"$if: ${{ values.on }}\na: 1\n", M{"a": float64(1)}, true},
		"$if false":           {"$if: ${{ values.off }}\na: 1\n", nil, false},
		"$value":              {"$if: ${{ values.on }}\n$value: [1]\n", []any{float64(1)}, true},
		"$for":                {"$for: c in values.items\n$key: ${{ c.name }}\n$value: ${{ c.n }}\n", M{"a": float64(1), "b": float64(2)}, true},
		"$merge at the root":  {"$merge: ${{ values.extra }}\nx: 1\n", M{"x": float64(2), "k": "v"}, true},
		"$$ at the root":      {"$$if: a\n", M{"$if": "a"}, true},
		"null":                {"~\n", nil, true},
		"an empty map":        {"{}\n", M{}, true},
		"a second document":   {"a: 1\n---\na: 2\n", M{"a": float64(1)}, true},
		"a block scalar":      {"|\n  text", "text\n", true},
		"a folded scalar":     {">\n  a\n  b", "a b\n", true},
		"a key written twice": {"a: 1\na: 2\n", M{"a": float64(2)}, true},
	} {
		t.Run(name, func(t *testing.T) {
			got, keep, err := evalIn(NewSession(context.Background(), 0), tc.src, mapCtx())
			if err != nil || keep != tc.keep || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %#v, keep %v (%v), want %#v, keep %v", got, keep, err, tc.want, tc.keep)
			}
		})
	}
}

// TestRender: the document written as YAML, the keys of each map sorted,
// whatever the order written; the same bytes each time; a document a false
// $if leaves out, or an empty one, writes nothing.
func TestRender(t *testing.T) {
	src := []byte(`
z: 1
b:
  - name: ${{ release.name }}
    ports: [{port: 80}, {port: 443}]
  - $for: c in values.items
    $each: {item: '${{ c.name }}', n: '${{ c.n }}'}
a: |
  two
  lines
on: yes
"y": ${{ values.on }}
`)
	// Sorted keys; the words YAML 1.1 reads as booleans or numbers are
	// quoted, as sigs.k8s.io/yaml writes them.
	want := `a: |
  two
  lines
b:
- name: demo-x
  ports:
  - port: 80
  - port: 443
- item: a
  "n": 1
- item: b
  "n": 2
"on": "yes"
"y": true
z: 1
`
	for range 3 {
		got, err := Render(context.Background(), src, mapCtx())
		if err != nil || string(got) != want {
			t.Fatalf("got %q (%v)\nwant %q", got, err, want)
		}
	}
	for _, src := range []string{"$if: ${{ values.off }}\na: 1\n", "", "# only a comment\n"} {
		if got, err := Render(context.Background(), []byte(src), mapCtx()); err != nil || got != nil {
			t.Errorf("%q: got %q (%v), want nothing", src, got, err)
		}
	}
	if got, err := Render(context.Background(), []byte("~\n"), nil); err != nil || string(got) != "null\n" {
		t.Errorf("a null document: got %q (%v)", got, err)
	}
	if _, err := Render(context.Background(), []byte("a: ${{ nope }}\n"), mapCtx()); err == nil || !strings.HasPrefix(err.Error(), "a: ${{ nope }}: ERROR: <input>:1:2: undeclared reference to 'nope'") {
		t.Errorf("an error at its path: got %v", err)
	}
	if _, err := Render(context.Background(), []byte("a: ${{ nope }}\n"), M{"nope": 1}); err != nil {
		t.Errorf("a name of the scope: %v", err)
	}
}

// TestRenderScope: the scope is a map of named objects, a Go value as the
// caller built it: the document sees them as variables.
func TestRenderScope(t *testing.T) {
	src := []byte("a: ${{ data.test }}\nb: ${{ data.list.map(i, str(i)) }}\nc: ${{ data.name + '!' }}\n")
	for name, scope := range map[string]Scope{
		"ints":    {"data": map[string]any{"test": 1, "list": []int{1, 2}, "name": "x"}},
		"floats":  {"data": M{"test": float64(1), "list": []any{float64(1), float64(2)}, "name": "x"}},
		"strings": {"data": map[string]any{"test": 1, "list": []any{1, 2}, "name": "x"}},
	} {
		got, err := Render(context.Background(), src, scope)
		if want := "a: 1\nb:\n- \"1\"\n- \"2\"\nc: x!\n"; err != nil || string(got) != want {
			t.Errorf("%s: got %q (%v), want %q", name, got, err, want)
		}
	}
}

// TestRenderLimit: Render's session is limited to DefaultCostLimit; a
// session of the caller's counts its documents together (Session.Render).
func TestRenderLimit(t *testing.T) {
	scope := M{"big": make([]any, 5000)}
	src := []byte("a: ${{ big.map(i, big.size()).size() }}\n")
	if _, err := Render(context.Background(), src, scope); err != nil {
		t.Fatalf("within the limit: %v", err)
	}
	s := NewSession(context.Background(), 5)
	_, err := s.Render([]byte("a: ${{ [1, 2, 3].map(i, i * 2) }}\nb: ${{ [1, 2, 3].map(i, i * 2) }}\n"), nil)
	if want := "exceed its CEL cost limit (5)"; err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Errorf("past the limit: got %v, want %q", err, want)
	}
}

// TestDocument: the evaluations of one document counted together against
// the size a document may build, from 0 in each; a document evaluated
// inside another counted in the other's too.
func TestDocument(t *testing.T) {
	scope := M{"text": strings.Repeat("x", 20<<20)}
	// Two templates that build 20 MiB each: together past 32 MiB, one by
	// one within.
	templates := func(s *Session) (a, b *Template) {
		n, err := Parse([]byte("v: ${{ text }}\n"))
		if err != nil {
			t.Fatal(err)
		}
		if a, err = s.Template(n, []string{"text"}); err != nil {
			t.Fatal(err)
		}
		if b, err = s.Template(n, []string{"text"}); err != nil {
			t.Fatal(err)
		}
		return a, b
	}
	eval := func(tpl *Template) func() error {
		return func() error { _, _, err := tpl.Eval(scope); return err }
	}
	const want = "the values the document builds exceed 32 MiB"
	s := NewSession(context.Background(), 0)
	a, b := templates(s)
	err := s.Document(func() error { return errors.Join(eval(a)(), eval(b)()) })
	if err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Errorf("together: got %v, want %q", err, want)
	}
	for i, tpl := range []*Template{a, b} {
		if err := s.Document(eval(tpl)); err != nil {
			t.Errorf("one by one, %d: %v", i, err)
		}
	}
	// A document inside another, in the call of a host function: its size
	// counted from 0, then added to the outer's, which passes 32 MiB.
	s = NewSession(context.Background(), 0)
	a, b = templates(s)
	err = s.Document(func() error {
		if err := eval(a)(); err != nil {
			return err
		}
		if err := s.Document(eval(b)); err != nil {
			t.Errorf("the inner document alone is within the size: %v", err)
		}
		return eval(a)()
	})
	if err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Errorf("inside another: got %v, want %q", err, want)
	}
	// And the outer document alone is within it.
	if err := s.Document(eval(a)); err != nil {
		t.Errorf("a new document starts from 0: %v", err)
	}
}

// TestTemplate: a node compiled once and evaluated on several scopes; its
// names are the variables of the scopes; all the templates of a document
// compile before any is evaluated.
func TestTemplate(t *testing.T) {
	var calls int
	s := NewSession(context.Background(), 0, hostCount(&calls))
	good, err := Parse([]byte("a: ${{ count(x) }}\nb: {$if: '${{ x > 1 }}', c: '${{ x }}'}\n"))
	if err != nil {
		t.Fatal(err)
	}
	bad, err := Parse([]byte("a: ${{ y }}\n"))
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := s.Template(good, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Template(bad, []string{"x"})
	if err == nil || !strings.HasPrefix(err.Error(), "a: ${{ y }}: ERROR: <input>:1:2: undeclared reference to 'y'") {
		t.Errorf("an expression that does not compile: got %v", err)
	}
	if calls != 0 {
		t.Errorf("%d calls before any evaluation", calls)
	}
	for _, tc := range []struct {
		x    float64
		want any
	}{
		{1, M{"a": float64(1)}},
		{2, M{"a": float64(2), "b": M{"c": float64(2)}}},
	} {
		got, keep, err := tpl.Eval(M{"x": tc.x})
		if err != nil || !keep || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("x = %v: got %#v, keep %v (%v), want %#v", tc.x, got, keep, err, tc.want)
		}
	}
	if calls != 2 {
		t.Errorf("%d calls of 2", calls)
	}
	// Names in any order, repeated: the scope has them all.
	n, _ := Parse([]byte("'${{ a + b }}'\n"))
	tpl, err = s.Template(n, []string{"b", "a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _, err := tpl.Eval(M{"a": "x", "b": "y"}); err != nil || got != "xy" {
		t.Errorf("unsorted names: got %#v (%v)", got, err)
	}
	// A nil node is null; a left out node is not kept.
	tpl, err = s.Template(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, keep, err := tpl.Eval(nil); err != nil || !keep || got != nil {
		t.Errorf("a nil node: got %#v, keep %v (%v)", got, keep, err)
	}
	n, _ = Parse([]byte("{$if: false, a: 1}\n"))
	tpl, _ = s.Template(n, nil)
	if got, keep, err := tpl.Eval(nil); err != nil || keep || got != nil {
		t.Errorf("left out: got %#v, keep %v (%v)", got, keep, err)
	}
}

// TestExpressionValues: the value of an expression, and a host function's
// argument, is data, as cel-go converts it to JSON, wherever its optionals,
// numbers, bytes, timestamps are; what has no data form fails.
func TestExpressionValues(t *testing.T) {
	s := NewSession(context.Background(), 0)
	for expr, want := range map[string]any{
		"{'o': values.?extra.k, 'l': [1, 2u]}":                M{"o": "v", "l": []any{float64(1), float64(2)}},
		"[9007199254740993, 0.0/0.0, -1.0/0.0]":               []any{"9007199254740993", "NaN", "-Infinity"},
		"b'abc'":                                              "YWJj",
		"[duration('1h'), timestamp('2024-01-02T03:04:05Z')]": []any{"3600s", "2024-01-02T03:04:05Z"},
		"first([0, {'a': values.?extra.k}])":                  M{"a": "v"},
		"[str(b'abc'), str(0.0/0.0), toJson(b'abc')]":         []any{"YWJj", "NaN", `"YWJj"`},
	} {
		if got, err := evalExpr(s, expr, mapCtx()); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %#v (%v), want %#v", expr, got, err, want)
		}
	}
	for expr, want := range map[string]string{
		"[{1: 2}]":        "${{ [{1: 2}] }}: unsupported type conversion from 'int' to string",
		"optional.none()": "${{ optional.none() }}: optional.none() dereference",
		"type(1)":         "${{ type(1) }}: type conversion not supported for 'type'",
		"str(type(1))":    "${{ str(type(1)) }}: type conversion not supported for 'type'",
	} {
		if _, err := evalExpr(s, expr, mapCtx()); err == nil || err.Error() != want {
			t.Errorf("%s: got %v, want %q", expr, err, want)
		}
	}
	if _, err := run("x: {l: [1, '${{ type(1) }}']}\n"); err == nil || err.Error() != "x: l[1]: ${{ type(1) }}: type conversion not supported for 'type'" {
		t.Errorf("an error at its path: got %v", err)
	}
}

// TestLibraries: the CEL extensions at their pinned versions (Session.compile):
// base64.encode (encoders 0), format's precision at most 100 (strings 5),
// last (optional types 2); and json.encode, a host function, keys sorted.
func TestLibraries(t *testing.T) {
	s := NewSession(context.Background(), 0)
	for expr, want := range map[string]any{
		"json.encode({'b': [1, 'x'], 'a': 2.5})": `{"a":2.5,"b":[1,"x"]}`,
		"base64.encode(b'a')":                    "YQ==",
		"'%.2f'.format([1.0])":                   "1.00",
		"[1, 2].last()":                          float64(2),
	} {
		if got, err := evalExpr(s, expr, mapCtx()); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %#v (%v), want %#v", expr, got, err, want)
		}
	}
	if _, err := evalExpr(s, "'%.101f'.format([1.0])", mapCtx()); err == nil || !strings.Contains(err.Error(), "precision") {
		t.Errorf("a precision over 100: got %v", err)
	}
}

// yamlDoc: a document of YAML 1.2 scalars and keys, for TestYAML12 and
// FuzzParse.
const yamlDoc = `
x:
  bools: [yes, no, on, off, y, N, "yes", true, False]
  numbers: [0777, 0o17, 0x1F, 1_000, 1.0, 012345678901234567890]
  strings: [2001-12-14, "0777", 1.2.3, !!str 1]
  keys: {on: a, 1.0: b, 0x10: c, "y": d, <<: e, ~: f, dup: 1, dup: 2}
  vars: [{$for: c in values.items, $vars: {n: 1.0, y: 2.0, y: 3.0, Y: '${{ c.n + n }}'}, $each: '${{ [y, Y] }}'}]
`

// readerErrors: the documents the reader refuses, and their errors, for
// TestYAML12 and FuzzParse.
var readerErrors = map[string]string{
	"x:\n  a: &a {k: 1}\n  b: *a\n": "line 3: aliases are not read",
	"x: {v: !!bool yes}\n":          "line 1: yaml: cannot decode !!str `yes` as a !!bool",
	"x: {v: !!binary gIA=}\n":       "line 1: jsontext: invalid UTF-8",
}

// TestYAML12: the document's scalars are read as yaml.v3 decodes them (YAML
// 1.2: yes and on are text), its keys as written, the later of a key
// written twice winning; a second document is ignored; aliases are refused.
func TestYAML12(t *testing.T) {
	out, err := runMap(yamlDoc)
	if err != nil {
		t.Fatal(err)
	}
	want := M{
		"bools":   []any{"yes", "no", "on", "off", "y", "N", "yes", true, false},
		"numbers": []any{float64(511), float64(15), float64(31), float64(1000), float64(1), float64(12345678901234567890)},
		"strings": []any{"2001-12-14", "0777", "1.2.3", "1"},
		"keys":    M{"on": "a", "1.0": "b", "0x10": "c", "y": "d", "<<": "e", "~": "f", "dup": float64(2)},
		"vars":    []any{float64(3), float64(2), float64(3), float64(3)},
	}
	if got := value.AsMap(out["x"]); !reflect.DeepEqual(got, want) {
		t.Errorf("got  %#v\nwant %#v", got, want)
	}
	// Found by FuzzParse: a block scalar ending the file keeps its newline,
	// as in Helm.
	if out, err := runMap("x:\n  v: >\n    text"); err != nil || value.AsMap(out["x"])["v"] != "text\n" {
		t.Errorf("a block scalar ending the file: got %#v (%v)", out, err)
	}
	if out, err := runMap("x: 1\n---\nx: 2\n"); err != nil || out["x"] != float64(1) {
		t.Errorf("a second document: got %#v (%v)", out, err)
	}
	for src, want := range readerErrors {
		if _, err := run(src); err == nil || err.Error() != want {
			t.Errorf("%q: got %v, want %q", src, err, want)
		}
	}
}

// TestParse: the first document of the input as a node; none when there is
// none; the errors of the reader at once, with their line.
func TestParse(t *testing.T) {
	for _, src := range []string{"", "# only a comment\n"} {
		if n, err := Parse([]byte(src)); n != nil || err != nil {
			t.Errorf("%q: got %v (%v), want no node", src, n, err)
		}
	}
	for _, src := range []string{"---\n", "--- # a comment\n", "~\n"} {
		n, err := Parse([]byte(src))
		if err != nil || n == nil {
			t.Fatalf("%q: got %v (%v), want a null node", src, n, err)
		}
		if v, err := Data(n); err != nil || v != nil {
			t.Errorf("%q: got %#v (%v), want null", src, v, err)
		}
	}
	n, err := Parse([]byte("a: [1, {b: yes}]\nc: 2001-12-14\n---\nd: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := M{"a": []any{float64(1), M{"b": "yes"}}, "c": "2001-12-14"}
	if got, err := Data(n); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v (%v), want %#v", got, err, want)
	}
	for _, src := range []string{"a: [\n", "a: &x 1\nb: *x\n", "a: .nan\n"} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%q: want an error", src)
		}
	}
	if v, err := Data(nil); v != nil || err != nil {
		t.Errorf("a nil node: got %#v (%v)", v, err)
	}
}

// FuzzParse: the reader (Parse) returns on any document, with values as
// values hold them (value.Normalize keeps them as they are). Seeds: the
// documents of TestYAML12.
func FuzzParse(f *testing.F) {
	f.Add([]byte(yamlDoc))
	for src := range readerErrors {
		f.Add([]byte(src))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		n, err := Parse(b)
		if err != nil {
			return
		}
		v, err := Data(n)
		if err != nil {
			t.Fatalf("%q: parsed, then refused: %v", b, err)
		}
		if got, err := value.Normalize(v); err != nil || !reflect.DeepEqual(got, v) {
			t.Fatalf("%q: read as %#v, which values do not hold (%v)", b, v, err)
		}
	})
}

// TestFunctions: the host functions in a document.
func TestFunctions(t *testing.T) {
	scope := mapCtx()
	scope["config"] = M{"b": float64(1), "a": []any{"x"}}
	v, _, err := evalIn(NewSession(context.Background(), 0), `
x:
  checksum: ${{ sha256(toJson(config)) }}
  json: ${{ toJson(config) }}
  yaml: ${{ toYaml(config) }}
  quoted: ${{ quote(values.items[0].n) }}
`, scope)
	if err != nil {
		t.Fatal(err)
	}
	want := M{
		"checksum": fmt.Sprintf("%x", sha256.Sum256([]byte(`{"a":["x"],"b":1}`))),
		"json":     `{"a":["x"],"b":1}`,
		"yaml":     "a:\n- x\nb: 1",
		"quoted":   `"1"`,
	}
	if got := value.AsMap(value.AsMap(v)["x"]); !reflect.DeepEqual(got, want) {
		t.Errorf("got  %#v\nwant %#v", got, want)
	}
	if _, err := run(`x: {v: '${{ toYaml("\x01 \x7f") }}'}`); err == nil || !strings.Contains(err.Error(), `${{ toYaml("\x01 \x7f") }}: yaml: `) {
		t.Errorf("an encoding error: got %v", err)
	}
}

// TestErrors: invalid documents, and where their errors point.
func TestErrors(t *testing.T) {
	for name, tc := range map[string]struct{ src, want string }{
		"invalid YAML":       {"x: [\n", "yaml: line 1: did not find expected node content"},
		"unterminated":       {"x: {v: '${{ 1 + '}\n", "unterminated ${{"},
		"syntax error":       {"x: {v: '${{ 1 + }}'}\n", "${{ 1 + }}"},
		"unknown variable":   {"x: {v: '${{ nope }}'}\n", "undeclared reference"},
		"key path":           {"x: {a: {b: '${{ nope }}'}}\n", "x: a: b:"},
		"list positions":     {"x: {l: [a, {b: [1, '${{ nope }}']}]}\n", "x: l[1]: b[1]: ${{ nope }}:"},
		"list in a list":     {"x: {l: [[1, '${{ nope }}']]}\n", "x: l[0][1]: ${{ nope }}:"},
		"$for position":      {"x: {l: [0, {$for: c in values.items, $each: '${{ nope }}'}]}\n", "x: l[1]: ${{ nope }}:"},
		"$spread position":   {"x: {l: [0, {$spread: '${{ nope }}'}]}\n", "x: l[1]: ${{ nope }}:"},
		"$vars name":         {"x: {l: [{$for: c in values.items, $vars: {d: '${{ nope }}'}, $each: 1}]}\n", "x: l[0]: $vars d: ${{ nope }}:"},
		"$for syntax":        {"x: {m: {$for: c values.items, $key: a, $value: 1}}\n", `is not "<name> in <expression>"`},
		"$for not a list":    {"x: {m: {$for: c in values.extra, $key: a, $value: 1}}\n", "not a list"},
		"$else path":         {"x: {m: {$if: '${{ values.off }}', $value: 1, $else: {b: '${{ values.nope }}'}}}\n", "x: m: b: ${{ values.nope }}: no such key: nope"},
		"at the root":        {"${{ nope }}\n", "${{ nope }}: ERROR: <input>:1:2: undeclared reference to 'nope'"},
		"in a root list":     {"[a, '${{ nope }}']\n", "[1]: ${{ nope }}:"},
		"evaluated, a key":   {"x: {a: '${{ values.nope }}'}\n", "x: a: ${{ values.nope }}: no such key: nope"},
		"evaluated, in $for": {"x: [{$for: c in values.items, $each: '${{ c.nope }}'}]\n", "x[0]: ${{ c.nope }}: no such key: nope"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := run(tc.src)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// TestMistakes: mistakes in the language, and expressions that do not
// compile, fail when the document is loaded, at their path, in every
// branch, those no evaluation takes included.
func TestMistakes(t *testing.T) {
	for name, tc := range map[string]struct{ src, want string }{
		"untaken branch":       {"x: {m: {$if: '${{ values.off }}', a: '${{ nope }}'}}\n", "x: m: a: ${{ nope }}: ERROR: <input>:1:2: undeclared reference to 'nope'"},
		"under $each":          {"x: {l: [{$for: c in values.items, $each: {$iff: 1}}]}\n", "x: l[0]: $each: $iff: not allowed here (allowed: $if, $merge)"},
		"$for without $each":   {"x: {l: [{$for: c in values.items}]}\n", "x: l[0]: $for: in a list item, needs $each"},
		"$for without $key":    {"x: {m: {$for: c in values.items, $value: 1}}\n", "x: m: $for: in a map, needs $key and $value"},
		"unknown directive":    {"x: {m: {$iff: '${{ true }}', a: 1}}\n", "x: m: $iff: not allowed here (allowed: $if, $merge)"},
		"unknown in a $for":    {"x: {l: [{$for: c in values.items, $Each: 1}]}\n", "x: l[0]: $Each: not allowed here (allowed: $for, $each, $vars)"},
		"$each in a map":       {"x: {m: {$each: 1}}\n", "x: m: $each: not allowed here (allowed: $if, $merge)"},
		"$key in a list item":  {"x: {l: [{$key: 1}]}\n", "x: l[0]: $key: not allowed here (allowed: $if, $merge)"},
		"$if beside $for":      {"x: {l: [{$if: '${{ true }}', $for: c in values.items, $each: 1}]}\n", "x: l[0]: $if: not allowed here (allowed: $for, $each, $vars)"},
		"a key beside $spread": {"x: {l: [{$spread: '${{ [] }}', a: 1}]}\n", "x: l[0]: a: not allowed here (allowed: $spread)"},
		"a key beside $value":  {"x: {m: {$value: 1, $merge: '${{ {} }}'}}\n", "x: m: $merge: not allowed here (allowed: $if, $value, $else)"},
		"$else without $value": {"x: {l: [{$if: '${{ true }}', a: 1, $else: 2}]}\n", "x: l[0]: $else: not allowed here (allowed: $if, $merge)"},
		"$else beside $for":    {"x: {m: {$if: '${{ true }}', $for: c in values.items, $key: a, $value: 1, $else: 2}}\n", "x: m: $else: not allowed here (allowed: $if, $for, $key, $value, $vars)"},
		"under $else":          {"x: {m: {$if: '${{ true }}', $value: 1, $else: {$iff: 1}}}\n", "x: m: $else: $iff: not allowed here (allowed: $if, $merge)"},
		"untaken $else":        {"x: {m: {$if: '${{ true }}', $value: 1, $else: '${{ nope }}'}}\n", "x: m: ${{ nope }}: ERROR: <input>:1:2: undeclared reference to 'nope'"},
		"a key beside $for":    {"x: {m: {$for: c in values.items, $key: a, $value: 1, b: 2}}\n", "x: m: b: not allowed here (allowed: $if, $for, $key, $value, $vars)"},
		"under $merge":         {"x: {m: {$merge: {$iff: 1}}}\n", "x: m: $merge: $iff: not allowed here (allowed: $if, $merge)"},
		"not evaluated":        {"x: {m: {$if: '${{ false }}', w: [{$iff: 1}]}}\n", "x: m: w[0]: $iff: not allowed here (allowed: $if, $merge)"},
		"$vars not a map":      {"x: {l: [{$for: c in values.items, $vars: [a], $each: 1}]}\n", "x: l[0]: $vars: must be a map of bindings, got a list"},
		"$vars a scalar":       {"x: {l: [{$for: c in values.items, $vars: 5, $each: 1}]}\n", "x: l[0]: $vars: must be a map of bindings, got 5"},
		"$vars value":          {"x: {l: [{$for: c in values.items, $vars: {a: {$iff: 1}}, $each: 1}]}\n", "x: l[0]: $vars a: $iff: not allowed here (allowed: $if, $merge)"},
		"top-level directive":  {"$iff: '${{ false }}'\nx: {}\n", "$iff: not allowed here (allowed: $if, $merge)"},
		".nan":                 {"x: {v: .nan}\n", "line 1: json: cannot marshal from Go float64: unsupported value: NaN"},
		"-.inf":                {"x:\n  v: [1, -.Inf]\n", "line 2: json: cannot marshal from Go float64: unsupported value: -Inf"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := run(tc.src); err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Errorf("got %v, want %q", err, tc.want)
			}
		})
	}
}
