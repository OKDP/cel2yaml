package cel2yaml

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types/ref"

	"github.com/okdp/cel2yaml/internal/value"
)

// TestSession: the CEL cost of a compile, shared by the evaluations of its
// documents, and its cancellation, before and during an evaluation.
func TestSession(t *testing.T) {
	one := "a: ${{ values.items.map(i, i.n * 2.0) }}\n"
	two := one + "b: ${{ values.items.map(i, i.n * 2.0) }}\n"
	// The cost of one key, measured under a limit it cannot reach.
	s := NewSession(context.Background(), math.MaxUint64)
	if _, _, err := evalIn(s, one, mapCtx()); err != nil {
		t.Fatal(err)
	}
	cost := s.used
	if cost == 0 {
		t.Fatal("no cost counted")
	}
	// Enough for one key, not for two, the second passing the limit (the
	// total checked after each evaluation): in one document, or in two
	// documents of the session.
	limit := 2*cost - 1
	_, _, err := evalIn(NewSession(context.Background(), limit), two, mapCtx())
	if want := fmt.Sprintf("b: ${{ values.items.map(i, i.n * 2.0) }}: the expressions of the compile exceed its CEL cost limit (%d)", limit); err == nil || err.Error() != want {
		t.Errorf("past the limit: got %v, want %q", err, want)
	}
	s = NewSession(context.Background(), limit)
	if _, _, err := evalIn(s, one, mapCtx()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := evalIn(s, one, mapCtx()); err == nil || !strings.HasSuffix(err.Error(), fmt.Sprintf("exceed its CEL cost limit (%d)", limit)) {
		t.Errorf("past the limit, in a second document: got %v", err)
	}
	if _, _, err := evalIn(NewSession(context.Background(), 2*cost), two, mapCtx()); err != nil {
		t.Errorf("within the limit: %v", err)
	}
	// One evaluation past the limit alone: CEL stops it there.
	if _, _, err := evalIn(NewSession(context.Background(), cost/2), one, mapCtx()); err == nil || !strings.HasSuffix(err.Error(), fmt.Sprintf("exceed its CEL cost limit (%d)", cost/2)) {
		t.Errorf("past the limit in one evaluation: got %v", err)
	}
	// An evaluation costs what the session has left, not the limit: after
	// one key, the second stops where the rest runs out, not at the limit
	// again (it would have run to its end, which costs less than the limit).
	items := make([]any, 200)
	for i := range items {
		items[i] = M{"n": float64(i)}
	}
	var calls int
	tick := cel.Function("tick", cel.Overload("tick_dyn", []*cel.Type{cel.DynType}, cel.DynType,
		cel.UnaryBinding(func(v ref.Val) ref.Val { calls++; return v })))
	scope := M{"values": M{"items": items}}
	doc := "a: ${{ values.items.map(i, i.n * 2.0) }}\nb: ${{ values.items.map(i, tick(i.n * 2.0)) }}\n"
	s = NewSession(context.Background(), math.MaxUint64, tick)
	if _, _, err := evalIn(s, doc, scope); err != nil {
		t.Fatal(err)
	}
	both := s.Used()
	if calls != len(items) {
		t.Fatalf("%d calls of %d", calls, len(items))
	}
	calls = 0
	limit = both * 3 / 4
	_, _, err = evalIn(NewSession(context.Background(), limit, tick), doc, scope)
	if err == nil || !strings.HasSuffix(err.Error(), fmt.Sprintf("exceed its CEL cost limit (%d)", limit)) {
		t.Fatalf("past the limit: %v", err)
	}
	if calls >= len(items) {
		t.Errorf("the second key ran to its end (%d calls): the limit was not the rest of the session's", calls)
	}
	// Cancelled before an evaluation: the context's error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := evalIn(NewSession(ctx, 0), one, mapCtx()); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: got %v, want context.Canceled", err)
	}
	// Cancelled during one: a comprehension stops at its next check.
	items = make([]any, 2*interruptEvery)
	for i := range items {
		items[i] = float64(i)
	}
	// stop cancels the context at its first call, after the check before
	// the evaluation.
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	calls = 0
	stop := cel.Function("stop", cel.Overload("stop_dyn", []*cel.Type{cel.DynType}, cel.DynType,
		cel.UnaryBinding(func(v ref.Val) ref.Val { calls++; cancel(); return v })))
	_, _, err = evalIn(NewSession(ctx, 0, stop), "a: ${{ values.items.map(i, stop(i)) }}\n", M{"values": M{"items": items}})
	if !errors.Is(err, context.Canceled) || calls > interruptEvery {
		t.Errorf("interrupted: got %v after %d of %d items", err, calls, len(items))
	}
}

// TestLoopCost: what a $for repeats counts, its elements and what its body
// builds (Session.charge, Session.grow): three loops nested over 150
// elements, 3.4 million items of one constant map, pass a limit of
// 100000 at once; without a limit, the size of the document stops them,
// as it stops a list of references to a value bound before it, each copy
// counted; a cancelled context stops a loop of no expression.
func TestLoopCost(t *testing.T) {
	big := make([]any, 150)
	for i := range big {
		big[i] = float64(i)
	}
	scope := mapCtx()
	scope["big"] = big
	nested := "x:\n  - $for: a in big\n    $each:\n      - $for: b in big\n        $each:\n          - $for: c in big\n            $each: {k: v}\n"
	const limit = 100_000
	s := NewSession(context.Background(), limit)
	_, _, err := evalIn(s, nested, scope)
	if want := fmt.Sprintf("exceed its CEL cost limit (%d)", limit); err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Errorf("nested loops under a limit: got %v, want %q", err, want)
	}
	want := fmt.Sprintf("the values the document builds exceed %d MiB", maxDocSize>>20)
	if _, _, err := evalIn(NewSession(context.Background(), 0), nested, scope); err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Errorf("nested loops without a limit: got %v, want %q", err, want)
	}
	laughs := "x:\n  - $for: i in [0]\n    $vars:\n      a: " + strings.Repeat("x", 1000) + "\n"
	prev := "a"
	for _, n := range []string{"b", "c", "d", "e", "f", "g", "h"} {
		laughs += "      " + n + ": ${{ [" + strings.TrimSuffix(strings.Repeat(prev+",", 10), ",") + "] }}\n"
		prev = n
	}
	laughs += "    $each: 1\n"
	if _, _, err := evalIn(NewSession(context.Background(), 0), laughs, scope); err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Errorf("references to a value: got %v, want %q", err, want)
	}
	// A small loop: its elements and its items counted, within the limit.
	s = NewSession(context.Background(), limit)
	v, _, err := evalIn(s, "x:\n  - $for: i in values.items\n    $each: {k: v}\n", scope)
	if err != nil || len(value.AsList(value.AsMap(v)["x"])) != 2 || s.used < 2 {
		t.Errorf("a small loop: %v, cost %d", err, s.used)
	}
	// The list given, then the context cancelled: the loop, of no
	// expression, stops at its next element.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := cel.Function("stop", cel.Overload("stop_dyn", []*cel.Type{cel.DynType}, cel.DynType,
		cel.UnaryBinding(func(v ref.Val) ref.Val { cancel(); return v })))
	_, _, err = evalIn(NewSession(ctx, 0, stop), "x:\n  - $for: i in stop(lists.range(1000))\n    $each: 1\n", scope)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled in a loop: got %v, want context.Canceled", err)
	}
}
