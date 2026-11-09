package cel2yaml

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/okdp/cel2yaml/value"
)

// doubled: nested cel.bind doubling a shared list n times, the body using
// the last, a0 being one short string: a value of 2^n elements that costs
// about n to build.
func doubled(n int, body string) string {
	var b strings.Builder
	b.WriteString(`cel.bind(a0, ["xxxxxxxxxxxxxxxx"], `)
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, `cel.bind(a%d, [a%d, a%d], `, i, i-1, i-1)
	}
	b.WriteString(strings.ReplaceAll(body, "$L", fmt.Sprintf("a%d", n)))
	b.WriteString(strings.Repeat(")", n+1))
	return b.String()
}

// mapped: the same doubling, written with n maps, no cel.bind.
func mapped(n int, op string) string {
	return `[["xxxxxxxxxxxxxxxx"]]` + strings.Repeat(`.map(a, `+op+`)`, n)
}

// TestSharedValues: CEL-01 and CEL-02: an expression building a value
// that holds a shared part many times, its logical size far larger than
// its cost, fails as it is built, on a size error, whatever reads it next:
// a host function, flatten, format, distinct, an equality, whether the
// value is built by cel.bind, map or concatenation, under a limit or
// without one. The session's context stops any of them otherwise.
func TestSharedValues(t *testing.T) {
	for _, tc := range []struct {
		name, expr string
		limit      uint64
	}{
		{"toJson", doubled(27, "size(toJson($L))"), DefaultCostLimit},
		{"str", doubled(27, "size(str($L))"), DefaultCostLimit},
		{"first", doubled(27, "first($L) == null"), DefaultCostLimit},
		{"flatten", doubled(27, "size($L.flatten(27))"), DefaultCostLimit},
		{"format", doubled(27, "size('%s'.format([$L]))"), DefaultCostLimit},
		{"distinct", doubled(27, "size($L.distinct())"), DefaultCostLimit},
		{"result", doubled(27, "$L"), DefaultCostLimit},
		{"no limit", doubled(40, "size(toJson($L))"), 0},
		{"equal", "cel.bind(l, " + doubled(40, "$L") + ", cel.bind(r, " + doubled(40, "$L") + ", l == r))", DefaultCostLimit},
		{"map", "size(" + mapped(40, "[a, a]") + ")", DefaultCostLimit},
		{"concatenation", "size(" + mapped(40, "a + a") + ")", 0},
		{"map of maps", "size(" + mapped(40, "{'x':a,'y':a}") + ")", DefaultCostLimit},
		{"transformMap", "size(cel.bind(b, lists.range(1000), lists.range(100000).transformMap(i, x, b)))", DefaultCostLimit},
		{"string", doublestr(40), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			s := NewSession(tc.limit)
			_, _, err := evalIn(ctx, s, "x: ${{ "+tc.expr+" }}\n", M{})
			if want := s.sizeError().Error(); err == nil || !strings.HasSuffix(err.Error(), want) {
				t.Errorf("got %.200v, want a failure ending with %q", err, want)
			}
		})
	}
}

// doublestr: a string doubled n times.
func doublestr(n int) string {
	var b strings.Builder
	b.WriteString("cel.bind(s0, 'xxxxxxxx', ")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "cel.bind(s%d, s%d + s%d, ", i, i-1, i-1)
	}
	fmt.Fprintf(&b, "size(s%d)", n)
	b.WriteString(strings.Repeat(")", n+1))
	return b.String()
}

// TestUnaryPreflight: a host function's argument larger than the size
// bound once converted fails before its conversion, the data the scope
// holds included.
func TestUnaryPreflight(t *testing.T) {
	big := strings.Repeat("x", maxDocSize+1)
	_, _, err := evalIn(context.Background(), NewSession(0), "x: ${{ toJson(big) }}\n", M{"big": big})
	if want := "a value larger than 32 MiB"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("got %.200v, want a failure with %q", err, want)
	}
	// Within the bound, the argument converts.
	v, _, err := evalIn(context.Background(), NewSession(0), "x: ${{ size(toJson(big)) }}\n", M{"big": strings.Repeat("x", 100)})
	if err != nil || v.(M)["x"] != float64(102) {
		t.Errorf("got %v, %v", v, err)
	}
}

// TestEqualCost: CEL-02: an equality costs the size of the smaller side:
// the same expression comparing larger values costs more, and a
// comprehension repeating one stops at the limit, not after the walks.
func TestEqualCost(t *testing.T) {
	cost := func(n int, expr string) uint64 {
		t.Helper()
		s := NewSession(DefaultCostLimit)
		src := fmt.Sprintf("x: ${{ cel.bind(l, lists.range(%d).map(i, 'yyyyyyyyyyyyyyyy'), cel.bind(r, lists.range(%d).map(i, 'yyyyyyyyyyyyyyyy'), %s)) }}\n", n, n, expr)
		if _, _, err := evalIn(context.Background(), s, src, M{}); err != nil {
			t.Fatal(err)
		}
		return s.Used()
	}
	sizes := cost(2000, "size(l) == 0")
	eq := cost(2000, "l == r")
	if eq <= sizes+50 {
		t.Errorf("an equality of 2000 elements costs %d, the sizes alone %d", eq, sizes)
	}
	if ne := cost(2000, "l != r"); ne != eq {
		t.Errorf("l != r costs %d, l == r costs %d", ne, eq)
	}
	// Scalars compare at CEL's cost.
	if c := cost(10, "size(l) == 10 && true"); c > sizes {
		t.Errorf("a scalar comparison costs %d", c)
	}
	// A comprehension repeating the comparison stops on cost.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := NewSession(DefaultCostLimit)
	src := "x: ${{ cel.bind(l, lists.range(100000).map(i, 'yyyyyyyyyyyyyyyy'), lists.range(100000).exists(i, l != l)) }}\n"
	_, _, err := evalIn(ctx, s, src, M{})
	if want := s.costError().Error(); err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Errorf("got %.200v, want %q", err, want)
	}
	if ctx.Err() != nil {
		t.Error("stopped by the context, not the limit")
	}
}

// TestProgramRebuild: CEL-03: a program is built again only when the
// session has left less than half of its cost cap, not per evaluation.
func TestProgramRebuild(t *testing.T) {
	s := NewSession(2000)
	e, err := s.compile("[1, 2, 3].size()", nil)
	if err != nil {
		t.Fatal(err)
	}
	first := e.prg
	builds := 0
	for prg := first; s.left() > 0; {
		if _, err := s.exec(context.Background(), e, M{}); err != nil {
			break
		}
		if e.prg != prg {
			prg = e.prg
			builds++
		}
		if builds > 12 {
			break
		}
	}
	if builds == 0 || builds > 12 {
		t.Errorf("the program was built %d times over the session", builds)
	}
	if s.left() >= 1000 && e.prg != first {
		t.Error("the program was built again with more than half its cap left")
	}
}

// TestProgramRebuiltCap: a rebuilt program may cost the rest of the
// session, no more: its cap follows what is left, not the session's limit.
func TestProgramRebuiltCap(t *testing.T) {
	s := NewSession(100000)
	e, err := s.compile("lists.range(60000).size()", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.exec(context.Background(), e, M{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.exec(context.Background(), e, M{}); err == nil || !strings.Contains(err.Error(), "exceed its CEL cost limit") {
		t.Errorf("second run: got %v", err)
	}
}

// TestLoopVars: CEL-04: the $vars of an element are not in scope of the
// next, the outer value of a name read by each element, not its binding
// by the previous element.
func TestLoopVars(t *testing.T) {
	doc := "x:\n  - $for: i in [1.0, 2.0, 3.0]\n    $vars:\n      acc: ${{ acc + i }}\n    $each: ${{ acc }}\n"
	v, _, err := evalIn(context.Background(), NewSession(0), doc, M{"acc": float64(10)})
	if err != nil {
		t.Fatal(err)
	}
	got := v.(M)["x"]
	want := []any{float64(11), float64(12), float64(13)}
	if value.Short(got) != value.Short(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
