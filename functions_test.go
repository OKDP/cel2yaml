package cel2yaml

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
)

// bigValue: a map of one top-level key of at least n bytes: entries of a
// number and a text of 1000 characters, as a values file that holds a
// large configuration.
func bigValue(n int) M {
	text := strings.Repeat("x", 1000)
	var items []any
	for size := 0; size < n; size += len(text) {
		items = append(items, M{"a": float64(len(items)), "b": text})
	}
	return M{"big": M{"items": items}}
}

// TestHostCostBytes: a host function pays the bytes of its argument, not
// its entries (hostCost): sha256 of a value of a mebibyte under one key
// costs about a mebibyte, as str, toJson, toYaml and quote do; an
// expression that hashes it 300 times passes a limit of 50000000, where it
// once cost under 20000.
func TestHostCostBytes(t *testing.T) {
	const mib = 1 << 20
	scope := M{"values": bigValue(mib)}
	for _, src := range []string{"sha256(values)", "str(values)", "toJson(values)", "toYaml(values)", "quote(values)", "first([values])", "json.encode(values)"} {
		s := NewSession(context.Background(), 1<<40)
		if _, _, err := evalIn(s, "a: ${{ "+src+" }}\n", scope); err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if s.used < mib || s.used > 4*mib {
			t.Errorf("%s: cost %d, want about %d", src, s.used, mib)
		}
	}
	const limit = 50_000_000
	_, _, err := evalIn(NewSession(context.Background(), limit), "n: ${{ lists.range(300).map(i, sha256(values)).size() }}\n", scope)
	if err == nil || !strings.HasSuffix(err.Error(), fmt.Sprintf("exceed its CEL cost limit (%d)", limit)) {
		t.Errorf("300 hashes of a mebibyte: got %v, want the limit exceeded", err)
	}
}

// TestCosted: a caller's host function, Costed, pays the bytes of its
// argument as the language's own do: one that reads a long text and returns
// its length exhausts a small limit.
func TestCosted(t *testing.T) {
	fn := Costed(cel.Function("count", cel.Overload("count_dyn", []*cel.Type{cel.DynType}, cel.IntType,
		Unary(func(v any) (int, error) { return len(fmt.Sprint(v)), nil }))), "count_dyn")
	scope := M{"values": M{"big": strings.Repeat("x", 10000)}}
	const limit = 5000
	_, _, err := evalIn(NewSession(context.Background(), limit, fn), "a: ${{ count(values.big) }}\n", scope)
	if err == nil || !strings.HasSuffix(err.Error(), fmt.Sprintf("exceed its CEL cost limit (%d)", limit)) {
		t.Errorf("got %v, want the limit exceeded", err)
	}
	s := NewSession(context.Background(), limit, fn)
	if _, _, err := evalIn(s, "a: ${{ count('x') }}\n", scope); err != nil || s.used == 0 || s.used > 10 {
		t.Errorf("a small argument: %v, cost %d", err, s.used)
	}
}

// FuzzHostCost: the bytes byteSize counts of a value, any nesting of the
// values' model (JSON data), are at least the bytes of its keys and texts,
// which any serialisation writes, and at most 8 times its JSON, a number
// counted 8 bytes; hostCost of a call is 1 more than its argument's,
// plus its result's length when a string, for each argument.
func FuzzHostCost(f *testing.F) {
	for _, seed := range []string{
		`null`, `""`, `0`, `true`, `[]`, `{}`,
		`{"a": 1, "b": [true, null, "x"], "c": {"d": {"e": []}}}`,
		`[[[[[[[[[["deep"]]]]]]]]]]`,
		`{"k": {"k": {"k": {"k": "` + strings.Repeat("v", 300) + `"}}}}`,
		`[1e300, -0.5, 12345678901234567890, "é\u0000", {"": ""}]`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, src string) {
		var d any
		if json.Unmarshal([]byte(src), &d) != nil {
			return
		}
		j, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		v := types.DefaultTypeAdapter.NativeToValue(d)
		n := byteSize(v)
		if lowest := textBytes(d); n < lowest {
			t.Errorf("%s: byteSize %d, under its %d bytes of keys and texts", j, n, lowest)
		}
		if highest := 8 * uint64(len(j)); n > highest {
			t.Errorf("%s: byteSize %d, over 8 times its JSON (%d)", j, n, highest)
		}
		if c := *hostCost([]ref.Val{v}, types.String("abc")); c != n+4 {
			t.Errorf("%s: cost %d with a result of 3 bytes, want %d", j, c, n+4)
		}
		if _, ok := v.(types.String); !ok {
			if c := *hostCost([]ref.Val{v, v}, v); c != 2*n+1 {
				t.Errorf("%s: cost %d of two arguments, want %d", j, c, 2*n+1)
			}
		}
	})
}

// textBytes: the bytes of the keys and the texts of d.
func textBytes(d any) uint64 {
	switch t := d.(type) {
	case string:
		return uint64(len(t))
	case M:
		var n uint64
		for k, e := range t {
			n += uint64(len(k)) + textBytes(e)
		}
		return n
	case []any:
		var n uint64
		for _, e := range t {
			n += textBytes(e)
		}
		return n
	}
	return 0
}

// hostCount: a host function, count, the identity, that counts its calls in
// n.
func hostCount(n *int) cel.EnvOption {
	return cel.Function("count", cel.Overload("count_dyn", []*cel.Type{cel.DynType}, cel.DynType,
		cel.UnaryBinding(func(v ref.Val) ref.Val { *n++; return v })))
}

// TestHostCost: what a host function builds counts in the session's CEL
// cost (hostCost): an expression that writes a large string from a large
// value, the value given, exhausts a small limit in one call, as CEL's
// own join does; a small output stays within it; without a limit,
// nothing is counted.
func TestHostCost(t *testing.T) {
	big := strings.Repeat("x", 10000)
	list := make([]any, 1000)
	for i := range list {
		list[i] = "0123456789"
	}
	scope := M{"values": M{"big": big, "list": list, "small": "x"}}
	const limit = 5000
	for _, src := range []string{"quote(values.big)", "str(values.big)", "toJson(values.list)", "toPrettyJson(values.list)", "toYaml(values.list)", "values.list.join(',')"} {
		doc := "a: ${{ " + src + " }}\n"
		_, _, err := evalIn(NewSession(context.Background(), limit), doc, scope)
		if err == nil || !strings.HasSuffix(err.Error(), fmt.Sprintf("exceed its CEL cost limit (%d)", limit)) {
			t.Errorf("%s: got %v, want the limit exceeded", src, err)
		}
		s := NewSession(context.Background(), 0)
		if _, _, err := evalIn(s, doc, scope); err != nil || s.used != 0 {
			t.Errorf("%s without a limit: %v, %d counted", src, err, s.used)
		}
	}
	s := NewSession(context.Background(), limit)
	if _, _, err := evalIn(s, "a: ${{ quote(values.small) }}\n", scope); err != nil || s.used == 0 || s.used > 10 {
		t.Errorf("a small output: %v, cost %d", err, s.used)
	}
}
