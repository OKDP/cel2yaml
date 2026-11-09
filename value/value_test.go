package value

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestNormalize: Go values of any type as Helm values after their JSON
// round trip: M maps, []any lists, float64 numbers, at any depth.
func TestNormalize(t *testing.T) {
	type labels map[string]string
	n, three := 7, int8(3)
	for name, tc := range map[string]struct{ in, want any }{
		"map[any]any":      {map[any]any{"a": 1, 2: []any{int64(3), uint64(4), float32(0.5)}}, M{"a": 1.0, "2": []any{3.0, 4.0, 0.5}}},
		"typed maps":       {M{"conn": map[string]string{"host": "h"}, "env": map[string][]string{"A": {"x"}}, "labels": labels{"tier": "ha"}}, M{"conn": M{"host": "h"}, "env": M{"A": []any{"x"}}, "labels": M{"tier": "ha"}}},
		"lists":            {M{"tags": []string{"a", "b"}, "ports": [2]uint16{80, 443}, "deep": [][]int{{1}}}, M{"tags": []any{"a", "b"}, "ports": []any{80.0, 443.0}, "deep": []any{[]any{1.0}}}},
		"numbers":          {[]any{three, uint32(4), int64(1)<<53 + 1, float32(0.1), json.Number("1e3")}, []any{3.0, 4.0, 9007199254740992.0, 0.1, 1000.0}},
		"integer keys":     {map[int]bool{10: true}, M{"10": true}},
		"pointers":         {M{"n": &n, "nil": (*int)(nil), "any": any(nil)}, M{"n": 7.0, "nil": nil, "any": nil}},
		"nil map and list": {M{"m": map[string]string(nil), "l": []string(nil)}, M{"m": M{}, "l": []any{}}},
		"as encoding/json": {
			M{"s": struct {
				Name string `json:"name"`
				Skip int    `json:"-"`
			}{"x", 1}, "t": time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "raw": json.RawMessage(`{"a": [1]}`), "b": []byte("hi")},
			M{"s": M{"name": "x"}, "t": "2026-01-02T03:04:05Z", "raw": M{"a": []any{1.0}}, "b": "aGk="},
		},
	} {
		got, err := Normalize(tc.in)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %#v, %v, want %#v", name, got, err, tc.want)
		}
	}
	// A copy: the input is not changed, nor shared.
	in := M{"m": M{"l": []any{"x"}}}
	got, _ := Normalize(in)
	AsMap(AsMap(got)["m"])["l"].([]any)[0] = "y"
	if !reflect.DeepEqual(in, M{"m": M{"l": []any{"x"}}}) {
		t.Errorf("the input changed: %#v", in)
	}
}

// TestNormalizeErrors: what values cannot hold fails, at its JSON pointer,
// in one wording.
func TestNormalizeErrors(t *testing.T) {
	cycle := M{}
	cycle["self"] = []any{cycle}
	for _, tc := range []struct {
		in   any
		want string
	}{
		{M{"b": func() {}, "a": make(chan int)}, `json: cannot marshal from Go chan int within "/a"`},
		{M{"hooks": []any{1, func() {}}}, `json: cannot marshal from Go func() within "/hooks/1"`},
		{M{"x": M{"y": math.NaN()}}, `json: cannot marshal from Go float64 within "/x/y": unsupported value: NaN`},
		{M{"n": json.Number("1e400")}, `json: cannot unmarshal JSON number 1e400 into Go float64 within "/n": value out of range`},
		{M{"s": "\xff"}, `jsontext: invalid UTF-8 within "/s" after offset 5`},
		{M{"m": map[any]any{true: "a"}}, `json: cannot marshal from Go interface {}: object member name must be a string within "/m" after offset 6`},
		{M{"m": map[any]any{1: "a", "1": "b"}}, `jsontext: duplicate object member name "1" within "/m"`},
	} {
		if _, err := Normalize(tc.in); err == nil || err.Error() != tc.want {
			t.Errorf("got %v, want %s", err, tc.want)
		}
	}
	if _, err := Normalize(cycle); err == nil || !strings.HasSuffix(err.Error(), "encountered a cycle") {
		t.Errorf("a cycle: got %v", err)
	}
}

// TestTruthyStr: values as Helm conditions and Go templates see them.
func TestTruthyStr(t *testing.T) {
	for _, tc := range []struct {
		v      any
		truthy bool
		str    string
	}{
		{nil, false, ""},
		{false, false, "false"},
		{true, true, "true"},
		{"", false, ""},
		{"x", true, "x"},
		{float64(0), false, "0"},
		{float64(3), true, "3"},
		{0.5, true, "0.5"},
		{M{}, false, "map[]"},
		{M{"a": 1}, true, "map[a:1]"},
		{[]any{}, false, "[]"},
		{[]any{1}, true, "[1]"},
	} {
		if got := Truthy(tc.v); got != tc.truthy {
			t.Errorf("Truthy(%#v) = %v", tc.v, got)
		}
		if got := Str(tc.v); got != tc.str {
			t.Errorf("Str(%#v) = %q, want %q", tc.v, got, tc.str)
		}
	}
}

// TestMerge: Helm's merge of -f files: maps merge, lists and scalars
// replace, null sets null; src is copied.
func TestMerge(t *testing.T) {
	src := M{"a": nil, "b": M{"c": float64(2)}, "l": []any{float64(2)}}
	got := Merge(
		M{"a": float64(1), "b": M{"c": float64(1), "d": float64(1)}, "l": []any{float64(1)}},
		src,
	)
	want := M{"a": nil, "b": M{"c": float64(2), "d": float64(1)}, "l": []any{float64(2)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
	got["l"].([]any)[0] = "changed"
	if src["l"].([]any)[0] != float64(2) {
		t.Error("src shares a list with the result")
	}
}

// TestCopy: the copy shares no map or list with the original.
func TestCopy(t *testing.T) {
	orig := M{"m": M{"l": []any{"x"}}}
	cp := AsMap(Copy(orig))
	AsMap(cp["m"])["l"].([]any)[0] = "y"
	AsMap(cp["m"])["k"] = "z"
	if !reflect.DeepEqual(orig, M{"m": M{"l": []any{"x"}}}) {
		t.Errorf("the original changed: %#v", orig)
	}
}

// TestShort: a value for a message, as %#v prints it, cut at 160
// characters.
func TestShort(t *testing.T) {
	if got, want := Short(M{"a": []any{1.0, "x"}}), `map[string]interface {}{"a":[]interface {}{1, "x"}}`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	if got := Short(strings.Repeat("x", 300)); len(got) != 163 || !strings.HasSuffix(got, `xxx...`) {
		t.Errorf("a long value: got %d bytes %s", len(got), got)
	}
}
