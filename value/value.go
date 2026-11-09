// Package value is the data the engine shares: YAML and JSON values after
// their JSON round trip (maps with string keys, []any lists, float64
// numbers, strings, booleans, null), and the helpers that read them. A
// value computed by CEL and the same value read from a file are equal.
package value

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
)

// M: a values map (YAML or JSON object).
type M = map[string]any

// Normalize: v, a value a caller built in memory, as it is after its JSON
// round trip, in a copy: written by encoding/json/v2, map keys
// sorted, and read back. At any depth a map is an M, a slice or an array a
// []any, every number a float64, a nil map or slice empty. What values
// cannot hold fails, its path named as a JSON pointer: a func, a chan, a
// complex, a NaN or an infinity, text that is not UTF-8, a key that is not
// text or a number (a map[any]any's), two keys that read as the same, a
// value that contains itself.
func Normalize(v any) (any, error) {
	b, err := json.Marshal(v, json.Deterministic(true))
	var out any
	if err == nil {
		err = json.Unmarshal(b, &out)
	}
	if err != nil {
		// One wording: json/v2 picks "cannot" or "unable to" per process.
		return nil, errors.New(strings.Replace(err.Error(), "json: unable to ", "json: cannot ", 1))
	}
	return out, nil
}

// Copy: a copy of v that shares no map or list with it.
func Copy(v any) any {
	switch t := v.(type) {
	case M:
		out := M{}
		for k, x := range t {
			out[k] = Copy(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = Copy(x)
		}
		return out
	}
	return v
}

// AsMap: v as a map, or an empty map.
func AsMap(v any) M {
	if m, ok := v.(M); ok {
		return m
	}
	return M{}
}

// AsList: v as a list, or nil.
func AsList(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return nil
}

// Merge: the deep merge of two maps: maps merge, lists and scalars
// replace, a null replaces too and stays null. dst is changed in place and returned; src is
// copied.
func Merge(dst, src M) M {
	for k, sv := range src {
		if sm, ok := sv.(M); ok {
			if dm, ok := dst[k].(M); ok {
				dst[k] = Merge(dm, sm)
				continue
			}
		}
		dst[k] = Copy(sv)
	}
	return dst
}

// Truthy: v as a condition (empty is false: null, false, "", 0, an empty
// map or list).
func Truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case M:
		return len(t) > 0
	case []any:
		return len(t) > 0
	}
	return true
}

// Str: v as text, as Go templates print it.
func Str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	}
	return fmt.Sprint(v) // Go %v: float64(3) -> "3", 0.5 -> "0.5", true -> "true"
}

// Short: a value for a message, as Go's %#v prints it, cut at 160
// characters.
func Short(v any) string {
	s := fmt.Sprintf("%#v", v)
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}
