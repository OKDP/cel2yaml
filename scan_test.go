package cel2yaml

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"cel.dev/cel-go/cel"
)

// scanCases: strings and their pieces, for TestScan and FuzzScan.
var scanCases = []struct {
	in   string
	want []piece
}{
	{"plain", []piece{{text: "plain"}}},
	{"${{ a }}", []piece{{text: " a ", expr: true}}},
	{`a ${{ {"k": "}}"}.k }} $${{ b`, []piece{{text: "a "}, {text: ` {"k": "}}"}.k `, expr: true}, {text: " ${{ b"}}},
	{`${{ 'it\'s }}' }}`, []piece{{text: ` 'it\'s }}' `, expr: true}}},
	{"${{ {'a': {'b': 1}} }}x", []piece{{text: " {'a': {'b': 1}} ", expr: true}, {text: "x"}}},
	{"${{ a // a note }}x", []piece{{text: " a // a note ", expr: true}, {text: "x"}}},
}

// unterminated: strings without the }} closing an expression, for TestScan
// and FuzzScan.
var unterminated = []string{"${{ a }", `${{ '\' }}`, "${{ a // b\n }"}

// TestScan: expressions, nested braces, strings, and the $${{ escape.
func TestScan(t *testing.T) {
	for _, tc := range scanCases {
		got, err := scan(tc.in)
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: got %#v, want %#v", tc.in, got, tc.want)
		}
	}
	for _, s := range unterminated {
		if _, err := scan(s); err == nil {
			t.Errorf("%q: an unterminated expression must fail", s)
		}
	}
}

// unscanned: what the scanner does not read as CEL does: a raw or
// triple-quoted string, a comment.
var unscanned = regexp.MustCompile(`[rR]['"]|'''|"""|//`)

// FuzzScan: scan gives a string back whole, its literal text ($${{ written
// again) and the text between each ${{ and the }} that closes it; an
// expression CEL parses is one expression, ${{ <it> }}, whatever its quoted
// strings and braces hold (but with a raw or triple-quoted string, or a
// comment: unscanned). Seeds: the strings of TestScan, and its expressions.
func FuzzScan(f *testing.F) {
	for _, tc := range scanCases {
		f.Add(tc.in)
		for _, p := range tc.want {
			if p.expr {
				f.Add(p.text)
			}
		}
	}
	for _, s := range unterminated {
		f.Add(s)
	}
	env, err := cel.NewEnv()
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if pieces, err := scan(s); err == nil {
			var b strings.Builder
			for _, p := range pieces {
				if p.expr {
					b.WriteString("${{" + p.text + "}}")
				} else {
					b.WriteString(strings.ReplaceAll(p.text, "${{", "$${{"))
				}
			}
			if b.String() != s {
				t.Errorf("%q: scanned as %#v, which reads %q", s, pieces, b.String())
			}
		}
		if _, iss := env.Parse(s); iss.Err() != nil || unscanned.MatchString(s) {
			return
		}
		got, err := scan("${{" + s + "}}")
		if want := []piece{{text: s, expr: true}}; err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("${{%s}}: got %#v (%v), want the expression %q", s, got, err, s)
		}
	})
}
