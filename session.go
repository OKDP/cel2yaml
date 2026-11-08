package cel2yaml

import (
	"context"
	"fmt"
	"strings"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
	"cel.dev/cel-go/ext"
	"google.golang.org/protobuf/types/known/structpb"
)

// Session: what the evaluations of one compile share, those of its
// documents and of its expressions: its context, which cancels them; a
// total CEL cost, when limited; the caller's host functions; the size of the
// values the document being evaluated has built (Document); and the CEL
// environments, one per set of variable names, each built once with those
// functions, which do not change during the session. Not safe for
// concurrent use: one per compile.
type Session struct {
	ctx   context.Context
	limit uint64 // 0: no limit
	used  uint64 // counted under a limit only
	built uint64 // the size of the values the document being evaluated built (grow), at most maxDocSize
	loops int    // how many $for bodies are running: what they build is counted in the cost (grow)
	funcs []cel.EnvOption
	envs  map[string]*cel.Env // by variable names, sorted (compile)
}

// NewSession: the session of a compile: cancelled with ctx, its
// expressions' total CEL cost at most limit (0: no limit), its documents
// calling the host functions funcs besides the language's own
// (functions.go). Its programs, each built per expression (compile), check
// the context every interruptEvery iterations; under a limit, each
// evaluation may cost what the session has left, its cost counted (exec,
// program).
func NewSession(ctx context.Context, limit uint64, funcs ...cel.EnvOption) *Session {
	return &Session{ctx: ctx, limit: limit, funcs: funcs, envs: map[string]*cel.Env{}}
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
// iterations; under a limit, an evaluation may cost the rest of the
// session's, no more.
func (s *Session) options() []cel.ProgramOption {
	opts := []cel.ProgramOption{cel.InterruptCheckFrequency(interruptEvery)}
	if s.limit > 0 {
		opts = append(opts, cel.CostLimit(s.left()))
	}
	return opts
}

// interruptEvery: how many iterations of a comprehension run between two
// checks of the context.
const interruptEvery = 100

// toData: a CEL value as data, the values' model, as cel-go converts it to
// JSON (google.protobuf.Value): maps with string keys, []any lists, float64
// numbers, strings, booleans, null. An integer beyond 2^53 is its digits,
// a NaN or infinite number "NaN" or "[-]Infinity", bytes their base64, a
// timestamp or a duration its text (CEL's string()), an optional its value.
// A map key that is not a string, an optional without a value, a type fail.
func toData(v ref.Val) (any, error) {
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

// memSize: the size of the CEL value v as data (toData), walked until it
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
func (s *Session) run(e *expr, scope M) (any, error) {
	v, err := s.exec(e, scope)
	if err != nil {
		return nil, fmt.Errorf("${{ %s }}: %w", e.src, err)
	}
	return v, nil
}

// exec: the expression e evaluated in scope, as data, within the session:
// its context checked before and during the evaluation; under a limit, the
// evaluation may cost what the session has left, and the session fails
// once the total cost of its evaluations passes the limit.
func (s *Session) exec(e *expr, scope M) (any, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	prg, err := s.program(e)
	if err != nil {
		return nil, err
	}
	out, details, err := prg.ContextEval(s.ctx, scope)
	if details != nil && details.ActualCost() != nil {
		s.used += *details.ActualCost()
	}
	switch {
	case s.limit > 0 && s.used > s.limit:
		return nil, s.costError()
	case err != nil && s.ctx.Err() != nil:
		return nil, s.ctx.Err() // interrupted: the context's error
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
	if err := s.grow(n); err != nil {
		return nil, err
	}
	return toData(out)
}

// program: the program of e, built again for what the session has left
// when that is less than it was built for, so that an evaluation costs
// that at most, not the limit: an expression late in a compile cannot use
// the whole limit again.
func (s *Session) program(e *expr) (cel.Program, error) {
	if s.limit == 0 || s.left() >= e.cap {
		return e.prg, nil
	}
	prg, err := e.env.Program(e.ast, s.options()...)
	if err != nil {
		return nil, fmt.Errorf("${{ %s }}: %w", e.src, err)
	}
	e.prg, e.cap = prg, s.left()
	return prg, nil
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
func (s *Session) grow(n uint64) error {
	if s.built = addSat(s.built, n); s.built > maxDocSize {
		return s.sizeError()
	}
	if s.loops > 0 {
		return s.charge(n)
	}
	return nil
}

// charge: n units of the work of the directives counted in the CEL cost,
// under a limit, the context checked: an element of a $for, what a $for
// builds (grow).
func (s *Session) charge(n uint64) error {
	if err := s.ctx.Err(); err != nil {
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
