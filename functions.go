// Functions: the host functions every document may call besides CEL's own,
// each deterministic and hermetic: str and first, and sha256, toJson,
// toPrettyJson, toYaml and quote, which reproduce the Helm template
// functions of the same names. A caller adds its own (NewSession). Each
// counts in the session's CEL cost, under a limit, the bytes of what it
// reads and of what it builds (hostCost).

package cel2yaml

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/cost"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/okdp/cel2yaml/internal/value"
)

// Unary: the binding of a host function of one argument: f of the argument
// as data (toData), its result converted back to CEL. An argument without
// a data form, or f's error, fails the expression.
func Unary[T any](f func(any) (T, error)) cel.OverloadOpt {
	return cel.UnaryBinding(func(v ref.Val) ref.Val {
		d, err := toData(v)
		var r T
		if err == nil {
			r, err = f(d)
		}
		if err != nil {
			return types.WrapErr(err)
		}
		return types.DefaultTypeAdapter.NativeToValue(r)
	})
}

// Func: the host function name, of one argument of any type, returning a
// value of the type out: f of the argument (Unary), its overload
// <name>_dyn; its cost, under a limit, hostCost.
func Func[T any](name string, out *cel.Type, f func(any) (T, error)) cel.EnvOption {
	id := name + "_dyn"
	return Costed(cel.Function(name, cel.Overload(id, []*cel.Type{cel.DynType}, out, Unary(f))), id)
}

// Costed: the host function fn, a cel.Function of a caller, the calls of
// its overloads ids counted with hostCost under a limit, as Func's: a
// caller's function bound with Unary converts its whole argument too.
func Costed(fn cel.EnvOption, ids ...string) cel.EnvOption {
	return cel.Lib(hostLib{fn: fn, ids: ids})
}

// hostLib: a host function, the ids of its overloads, as a CEL library:
// the function, and the cost tracker its programs count its calls with.
type hostLib struct {
	fn  cel.EnvOption
	ids []string
}

// CompileOptions: the function.
func (l hostLib) CompileOptions() []cel.EnvOption { return []cel.EnvOption{l.fn} }

// ProgramOptions: its calls counted with hostCost, when the program
// tracks its cost (a session under a limit, NewSession).
func (l hostLib) ProgramOptions() []cel.ProgramOption {
	opts := make([]cost.TrackerOption, len(l.ids))
	for i, id := range l.ids {
		opts[i] = cost.OverloadTracker(id, hostCost)
	}
	return []cel.ProgramOption{cel.CostTrackerOptions(opts...)}
}

// hostCost: the CEL cost of a call of a host function: 1, the call, plus
// the bytes of its arguments, about as many as their serialisation
// (byteSize), plus the bytes of its result when a string (what it builds:
// toJson, toYaml, quote of a large value). A host function converts its
// whole argument to data (Unary, toData) and most serialise it (str,
// sha256, toJson, toYaml, quote): its work is linear in the argument's
// bytes, which a map's or a list's entry count does not bound (a map of
// one key, a string of a megabyte). It pays a unit a byte, ten times what
// CEL's own string functions pay a character (ext.Strings' join,
// replace), whose work is a scan, not a conversion.
func hostCost(args []ref.Val, result ref.Val) *uint64 {
	total := uint64(1)
	for _, a := range args {
		total = addSat(total, byteSize(a))
	}
	if s, ok := result.(types.String); ok {
		total = addSat(total, uint64(len(s)))
	}
	return &total
}

// scalarSize: what byteSize counts for a value that is neither a string,
// bytes, a map nor a list (a number, a boolean, null, a timestamp): about
// the bytes of a number's text.
const scalarSize = 8

// byteSize: about the bytes of v serialised, walked once: a string's or
// bytes' length, a map's keys and values and 2 an entry (": ", ", "), a
// list's elements and 1 each, 2 a map's or a list's brackets, scalarSize
// for any other value.
func byteSize(v ref.Val) uint64 {
	switch t := v.(type) {
	case types.String:
		return uint64(len(t))
	case types.Bytes:
		return uint64(len(t))
	case traits.Mapper:
		n := uint64(2)
		for it := t.Iterator(); it.HasNext() == types.True; {
			k := it.Next()
			n = addSat(n, addSat(byteSize(k), 2))
			if e, ok := t.Find(k); ok {
				n = addSat(n, byteSize(e))
			}
		}
		return n
	case traits.Lister:
		n := uint64(2)
		for it := t.Iterator(); it.HasNext() == types.True; {
			n = addSat(n, addSat(byteSize(it.Next()), 1))
		}
		return n
	}
	return scalarSize
}

// addSat: a + b, at most the largest uint64.
func addSat(a, b uint64) uint64 {
	if s := a + b; s >= a {
		return s
	}
	return math.MaxUint64
}

// hostFunctions: str (value.Str), first (the first item of a list that is
// not empty, "" without one or for a value that is not a list), sha256
// (Helm's sha256sum of str(v), null being ""), toJson (Go's encoding/json,
// as Helm: keys sorted, <, >, & escaped), toPrettyJson (indented by two
// spaces, as sprig), toYaml (sigs.k8s.io/yaml without the last newline, as
// Helm) and quote (Go's %q; a null gives "", as in sprig).
func hostFunctions() []cel.EnvOption {
	toJSON := func(v any) (string, error) {
		b, err := json.Marshal(v)
		return string(b), err
	}
	return []cel.EnvOption{
		Func("str", cel.StringType, func(v any) (string, error) { return value.Str(v), nil }),
		Func("first", cel.DynType, func(v any) (any, error) {
			for _, x := range value.AsList(v) {
				if value.Truthy(x) {
					return x, nil
				}
			}
			return "", nil
		}),
		Func("sha256", cel.StringType, func(v any) (string, error) { return fmt.Sprintf("%x", sha256.Sum256([]byte(value.Str(v)))), nil }),
		Func("toJson", cel.StringType, toJSON),
		// json.encode, not ext.Encoders' (its version 1): that one writes a
		// map in no fixed order and costs more than any limit admits.
		Func("json.encode", cel.StringType, toJSON),
		Func("toPrettyJson", cel.StringType, func(v any) (string, error) {
			b, err := json.MarshalIndent(v, "", "  ")
			return string(b), err
		}),
		Func("toYaml", cel.StringType, func(v any) (string, error) {
			b, err := sigsyaml.Marshal(v)
			return string(bytes.TrimSuffix(b, []byte("\n"))), err
		}),
		Func("quote", cel.StringType, func(v any) (string, error) {
			if v == nil {
				return "", nil
			}
			return fmt.Sprintf("%q", value.Str(v)), nil
		}),
	}
}
