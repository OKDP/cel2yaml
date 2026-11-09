package cel2yaml

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/cost"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/overloads"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
	"cel.dev/cel-go/ext"
	"cel.dev/cel-go/interpreter"
	"google.golang.org/protobuf/types/known/structpb"
)

// Session: what the evaluations of one compile share, those of its
// documents and of its expressions: a total CEL cost, when limited; the caller's host functions; the size of the
// values the document being evaluated has built (Document); and the CEL
// environments, one per set of variable names, each built once with those
// functions, which do not change during the session. Not safe for
// concurrent use: one per compile.
type Session struct {
	limit uint64 // 0: no limit
	used  uint64 // counted under a limit only
	built uint64 // the size of the values the document being evaluated built (grow), at most maxDocSize
	loops int    // how many $for bodies are running: what they build is counted in the cost (grow)
	funcs []cel.EnvOption
	envs  map[string]*cel.Env // by variable names, sorted (compile)
	sizes map[ref.Val]uint64  // the size of the lists and the maps the running evaluation has seen, by value (size); nil between two
}

// NewSession: the session of a compile: its expressions' total CEL cost at
// most limit (0: no limit), its documents calling the host functions funcs
// besides the language's own (functions.go). The context of each
// evaluation is the one its caller gives (Template.Eval, Render). Its
// programs, each built per expression (compile), check that context every
// interruptEvery iterations; under a limit, each evaluation may cost what
// the session has left, its cost counted (exec, program).
func NewSession(limit uint64, funcs ...cel.EnvOption) *Session {
	return &Session{limit: limit, funcs: funcs, envs: map[string]*cel.Env{}}
}

// Document: f, the evaluations of one document (Template.Eval), whose
// values are counted together against the size a document may build (32
// MiB: the values its directives build, those its expressions return, the
// copies of each element of a $for included), counted from 0; the size of a
// document evaluated by an expression of another (a template a host
// function evaluates, in a call of f) is counted in the other's too. The
// session is a document of its own until the first call: evaluations
// outside any Document add up.
func (s *Session) Document(f func() error) error {
	outer := s.built
	s.built = 0
	defer func() { s.built = addSat(outer, s.built) }()
	return f()
}

// Used: the CEL cost of the evaluations so far, those of the directives of
// a $for included; 0 without a limit, which counts none.
func (s *Session) Used() uint64 { return s.used }

// left: the cost the session may still use, under a limit.
func (s *Session) left() uint64 {
	if s.used >= s.limit {
		return 0
	}
	return s.limit - s.used
}

// options: how a program runs: the context checked every interruptEvery
// iterations; every value it builds sized as it is built, past maxDocSize
// the evaluation fails (track): a list of the same list twice, doubled forty
// times, is refused as it is built, not by what later walks it (Unary, ==,
// flatten, format, distinct); under a limit, an evaluation may cost the
// rest of the session's, no more, an equality costing the size of its
// sides (equalCost). The cost is tracked in any case: the overloads tracked
// here see the arguments of the calls that build values, which a decorator
// cannot.
func (s *Session) options() []cel.ProgramOption {
	opts := []cel.ProgramOption{
		cel.InterruptCheckFrequency(interruptEvery),
		cel.EvalOptions(cel.OptTrackCost),
		cel.CustomDecoratorV2(s.decorate),
		cel.CostTrackerOptions(
			cost.OverloadTracker(overloads.Equals, s.equalCost),
			cost.OverloadTracker(overloads.NotEquals, s.equalCost),
			cost.OverloadTracker(overloads.AddList, s.trackBuilt),
			cost.OverloadTracker(mapInsertKV, s.trackBuilt),
			cost.OverloadTracker(mapInsertMap, s.trackBuilt),
		),
	}
	if s.limit > 0 {
		opts = append(opts, cel.CostLimit(s.left()))
	}
	return opts
}

// The overloads a two-variable comprehension builds a map with, by their
// CEL ids (ext.TwoVarComprehensions): its accumulator, an entry added
// without allocating the map again.
const (
	mapInsertKV  = "@mapInsert_map_key_value"
	mapInsertMap = "@mapInsert_map_map"
)

// decorate: the node i, its result sized as built (track) when i builds a
// list or a map literal, or a concatenation whose overload is only known at
// run time (the overload of the accumulator of a comprehension is, tracked
// through its arguments: trackBuilt); everything else as it is.
func (s *Session) decorate(i interpreter.InterpretableV2) (interpreter.InterpretableV2, error) {
	switch n := i.(type) {
	case interpreter.InterpretableConstructor:
		return &sizedNode{i, s}, nil
	case interpreter.InterpretableCall:
		if n.Function() == operators.Add && n.OverloadID() != overloads.AddList {
			return &sizedNode{i, s}, nil
		}
	}
	return i, nil
}

// sizedNode: the node whose result, a value it just built, is sized.
type sizedNode struct {
	interpreter.InterpretableV2
	s *Session
}

// Exec: the node's value, tracked.
func (n *sizedNode) Exec(frame *interpreter.ExecutionFrame) ref.Val {
	v := n.InterpretableV2.Exec(frame)
	n.s.track(v)
	return v
}

// Eval: Exec of the activation.
func (n *sizedNode) Eval(ctx interpreter.Activation) ref.Val {
	return interpreter.EvalActivation(ctx, n.Exec)
}

// track: v, a value an evaluation just built, sized: past maxDocSize, the
// evaluation fails at once (CEL's way to end one, a panic it recovers, as
// the context's, exec turns it to sizeError). The size of a list or a map
// is kept per value for what holds it next: walking a shared tree, whose
// parts a list holds many times, stops where it was sized.
func (s *Session) track(v ref.Val) {
	if s.size(v) > maxDocSize {
		panic(interpreter.EvalCancelledError{Cause: interpreter.MemoryLimitExceeded, Message: "value too large"})
	}
}

// trackBuilt: the size of the result of a concatenation or of an insertion
// of an entry from the sizes of its arguments, the result being a new value
// or the accumulator updated in place (the same value, its elements added
// since it was sized); no walk over elements a comprehension added before.
// The cost of the call stays CEL's.
func (s *Session) trackBuilt(args []ref.Val, result ref.Val) *uint64 {
	var n uint64
	for _, a := range args {
		n = addSat(n, s.size(a))
	}
	s.keep(result, n)
	s.track(result)
	return nil
}

// size: the size of the CEL value v, as data (memSize's count, a value
// held many times counted as many times), that of a list or a map once
// per evaluation (sizes): every shared part walked once, whatever its depth.
func (s *Session) size(v ref.Val) uint64 {
	switch t := v.(type) {
	case *types.Optional:
		if t.HasValue() {
			return s.size(t.GetValue())
		}
		return scalarMem
	case traits.Mapper:
		if n, ok := s.memo(v); ok {
			return n
		}
		n := uint64(containerMem)
		for it := t.Iterator(); it.HasNext() == types.True; {
			k := it.Next()
			n = addSat(n, slotMem+s.size(k))
			if e, ok := t.Find(k); ok {
				n = addSat(n, s.size(e))
			}
		}
		return s.keep(v, n)
	case traits.Lister:
		if n, ok := s.memo(v); ok {
			return n
		}
		n := uint64(containerMem)
		for it := t.Iterator(); it.HasNext() == types.True; {
			n = addSat(n, slotMem+s.size(it.Next()))
		}
		return s.keep(v, n)
	}
	n, _ := memSize(v, maxDocSize)
	return n
}

// memo: the size kept for v, a list or a map; a value that is not a
// pointer has no identity to keep one by.
func (s *Session) memo(v ref.Val) (uint64, bool) {
	if reflect.ValueOf(v).Kind() != reflect.Pointer {
		return 0, false
	}
	n, ok := s.sizes[v]
	return n, ok
}

// keep: n, the size of v, kept (memo) and returned.
func (s *Session) keep(v ref.Val, n uint64) uint64 {
	if reflect.ValueOf(v).Kind() == reflect.Pointer {
		s.sizes[v] = n
	}
	return n
}

// equalUnitBytes: how many bytes of the sides of an equality count as one
// unit of its CEL cost, about what such a comparison walks in a unit of the
// limit, a microsecond, and more than CEL counts for a list of scalars (0.1
// a unit per element of the top level of a list).
const equalUnitBytes = 256

// equalCost: the CEL cost of l == r or l != r of two lists or two maps: the
// smaller side's size, at one unit per equalUnitBytes, at least 1. CEL
// counts the elements of one level only; a comparison walks every element
// of both sides, which a comprehension repeats, each comparison at most
// maxDocSize once walked (track), the context and the limit bounding how
// many. Other values keep CEL's cost.
func (s *Session) equalCost(args []ref.Val, _ ref.Val) *uint64 {
	if len(args) != 2 || !container(args[0]) || !container(args[1]) {
		return nil
	}
	c := 1 + min(s.size(args[0]), s.size(args[1]))/equalUnitBytes
	return &c
}

// container: v is a list or a map.
func container(v ref.Val) bool {
	switch v.(type) {
	case traits.Lister, traits.Mapper:
		return true
	}
	return false
}

// interruptEvery: how many iterations of a comprehension run between two
// checks of the context.
const interruptEvery = 100

// ToData: a CEL value as data, the values' model, as cel-go converts it to
// JSON (google.protobuf.Value): maps with string keys, []any lists, float64
// numbers, strings, booleans, null. An integer beyond 2^53 is its digits,
// a NaN or infinite number "NaN" or "[-]Infinity", bytes their base64, a
// timestamp or a duration its text (CEL's string()), an optional its value.
// A map key that is not a string, an optional without a value, a type fail.
func ToData(v ref.Val) (any, error) {
	pb, err := v.ConvertToNative(types.JSONValueType)
	if err != nil {
		return nil, err
	}
	return pb.(*structpb.Value).AsInterface(), nil
}

// The sizes memSize and dataSize count, about the bytes Go takes: a
// scalar, a string's or bytes' header besides its bytes (scalarMem); an
// entry of a map, besides its key's bytes, or an item of a list, besides
// its value (slotMem); a map or a list itself (containerMem).
const (
	scalarMem    = 16
	slotMem      = 16
	containerMem = 48
)

// memSize: the size of the CEL value v as data (ToData), walked until it
// passes left: its size and true, or false once past it. A value that
// holds another many times counts it each time, as its conversion copies
// it each time, and the walk stops at left.
func memSize(v ref.Val, left uint64) (uint64, bool) {
	n := uint64(scalarMem)
	switch t := v.(type) {
	case types.String:
		n += uint64(len(t))
	case types.Bytes:
		n += uint64(len(t)) * 4 / 3 // base64
	case *types.Optional:
		if t.HasValue() {
			return memSize(t.GetValue(), left)
		}
	case traits.Mapper:
		n = containerMem
		for it := t.Iterator(); it.HasNext() == types.True && n <= left; {
			k := it.Next()
			kn, _ := memSize(k, left-n)
			n = addSat(n, slotMem+kn)
			if e, ok := t.Find(k); ok && n <= left {
				en, _ := memSize(e, left-n)
				n = addSat(n, en)
			}
		}
	case traits.Lister:
		n = containerMem
		for it := t.Iterator(); it.HasNext() == types.True && n <= left; {
			en, _ := memSize(it.Next(), left-n)
			n = addSat(n, slotMem+en)
		}
	}
	return n, n <= left
}

// dataSize: the size of v, data, as memSize counts it, walked until it
// passes left.
func dataSize(v any, left uint64) (uint64, bool) {
	n := uint64(scalarMem)
	switch t := v.(type) {
	case string:
		n += uint64(len(t))
	case M:
		n = containerMem
		for k, e := range t {
			if n > left {
				break
			}
			en, _ := dataSize(e, left-n)
			n = addSat(n, slotMem+uint64(len(k))+en)
		}
	case []any:
		n = containerMem
		for _, e := range t {
			if n > left {
				break
			}
			en, _ := dataSize(e, left-n)
			n = addSat(n, slotMem+en)
		}
	}
	return n, n <= left
}

// expr: an expression of a document, compiled once, in the environment of
// the names in scope at its place, to the program its evaluations run.
type expr struct {
	src string // as written between ${{ and }}, trimmed, for messages
	env *cel.Env
	ast *cel.Ast
	prg cel.Program
	cap uint64 // the cost its program may use, under a limit: what the session had left when it was built
}

// compile: the expression src, as written (CEL's errors give its line and
// column), compiled in the environment of the variable names, sorted: one
// per set of names, built once per session. An error names the expression,
// as an evaluation error does: ${{ <src> }}: <error>.
func (s *Session) compile(src string, names []string) (*expr, error) {
	key := strings.Join(names, ",")
	env, ok := s.envs[key]
	if !ok {
		// CEL extensions, each at a pinned version: a cel-go release adds
		// functions to a library under a new version, which would otherwise
		// reach every document with the upgrade; a version is raised on
		// purpose, with the README's CEL libraries table. Then the functions
		// documents may call: the language's, then the caller's. Every variable is dynamic:
		// values are plain YAML data.
		opts := append([]cel.EnvOption{ext.Strings(ext.StringsVersion(5)), ext.Lists(ext.ListsVersion(3)), ext.Bindings(ext.BindingsVersion(1)),
			ext.TwoVarComprehensions(ext.TwoVarComprehensionsVersion(0)), ext.Encoders(ext.EncodersVersion(0)), cel.OptionalTypes(cel.OptionalTypesVersion(2))},
			hostFunctions()...)
		opts = append(opts, s.funcs...)
		for _, n := range names {
			opts = append(opts, cel.Variable(n, cel.DynType))
		}
		var err error
		if env, err = cel.NewEnv(opts...); err != nil {
			return nil, err
		}
		s.envs[key] = env
	}
	e := &expr{src: strings.TrimSpace(src), env: env}
	ast, iss := env.Compile(src)
	err := iss.Err()
	if err == nil {
		e.ast, e.cap = ast, s.left()
		e.prg, err = env.Program(ast, s.options()...)
	}
	if err != nil {
		return nil, fmt.Errorf("${{ %s }}: %w", e.src, err)
	}
	return e, nil
}

// run: the value of the expression e in scope, as data, evaluated within
// the session (exec); an error names the expression.
func (s *Session) run(ctx context.Context, e *expr, scope M) (any, error) {
	v, err := s.exec(ctx, e, scope)
	if err != nil {
		return nil, fmt.Errorf("${{ %s }}: %w", e.src, err)
	}
	return v, nil
}

// exec: the expression e evaluated in scope, as data, within the session:
// its context checked before and during the evaluation; under a limit, the
// evaluation may cost what the session has left, and the session fails
// once the total cost of its evaluations passes the limit.
func (s *Session) exec(ctx context.Context, e *expr, scope M) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	prg, err := s.program(e)
	if err != nil {
		return nil, err
	}
	outer := s.sizes
	s.sizes = map[ref.Val]uint64{}
	defer func() { s.sizes = outer }()
	out, details, err := prg.ContextEval(ctx, scope)
	if s.limit > 0 && details != nil && details.ActualCost() != nil {
		s.used += *details.ActualCost()
	}
	switch {
	case s.limit > 0 && s.used > s.limit:
		return nil, s.costError()
	case err != nil && ctx.Err() != nil:
		return nil, ctx.Err() // interrupted: the context's error
	case isValueLimit(err):
		return nil, s.sizeError()
	case err != nil:
		return nil, err
	}
	// The value as data, its size counted first: a list of references to
	// one large value is small in CEL, and as large as its copies once
	// converted. What has no data form fails the expression.
	n, ok := memSize(out, maxDocSize-s.built)
	if !ok {
		return nil, s.sizeError()
	}
	if err := s.grow(ctx, n); err != nil {
		return nil, err
	}
	return ToData(out)
}

// program: the program of e, built again when the session has left less
// than half of what it was built for, so that an evaluation costs twice
// what is left at most, not the limit, an expression late in a compile
// cannot use the whole limit again, and a $for does not build a program per
// element (the session's total check still fails it past the limit, exec).
func (s *Session) program(e *expr) (cel.Program, error) {
	if s.limit == 0 || s.left() >= e.cap/2 {
		return e.prg, nil
	}
	prg, err := e.env.Program(e.ast, s.options()...)
	if err != nil {
		return nil, fmt.Errorf("${{ %s }}: %w", e.src, err)
	}
	e.prg, e.cap = prg, s.left()
	return prg, nil
}

// isValueLimit: err is CEL's refusal of a value past maxValueUnits.
func isValueLimit(err error) bool {
	var c interpreter.EvalCancelledError
	return errors.As(err, &c) && c.Cause == interpreter.MemoryLimitExceeded
}

// maxDocSize: the size of the values a document may build (memSize, about
// the bytes they take in memory), whatever the cost limit: those its
// directives build, those its expressions return, the copies of each
// element of a $for included. 32 MiB holds a large configuration many
// times over; a document past it amplifies its input.
const maxDocSize = 32 << 20

// grow: n, the size of values the document builds, counted: past
// maxDocSize, the document fails; inside a $for, n counts in the CEL cost
// too (charge), what a loop repeats being no CEL evaluation of its own.
func (s *Session) grow(ctx context.Context, n uint64) error {
	if s.built = addSat(s.built, n); s.built > maxDocSize {
		return s.sizeError()
	}
	if s.loops > 0 {
		return s.charge(ctx, n)
	}
	return nil
}

// charge: n units of the work of the directives counted in the CEL cost,
// under a limit, the context checked: an element of a $for, what a $for
// builds (grow).
func (s *Session) charge(ctx context.Context, n uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.limit > 0 {
		if s.used = addSat(s.used, n); s.used > s.limit {
			return s.costError()
		}
	}
	return nil
}

// costError: the error of a compile past its CEL cost limit.
func (s *Session) costError() error {
	return fmt.Errorf("the expressions of the compile exceed its CEL cost limit (%d)", s.limit)
}

// sizeError: the error of a document past maxDocSize.
func (s *Session) sizeError() error {
	return fmt.Errorf("the values the document builds exceed %d MiB", maxDocSize>>20)
}
