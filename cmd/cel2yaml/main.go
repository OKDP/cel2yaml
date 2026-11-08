// cel2yaml [--context ctx.yaml]... [-o out.yaml] in.cel.yaml
//
// compiles a YAML document with CEL expressions to a YAML document: the
// top-level keys of the context files are the variables of its expressions,
// the file is the output, written to out.yaml, or to stdout.

package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/okdp/cel2yaml"
	"github.com/okdp/cel2yaml/internal/value"
)

// version: set at build time (-ldflags "-X main.version=..."): make build,
// the release binaries; --version prints dev without it.
var version string

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run: the command with its arguments args, its output on stdout and its
// messages on stderr; the exit status: 0, 1 for an error of the files or of
// the compile, 2 for a usage error.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cel2yaml", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "usage: cel2yaml [--context ctx.yaml]... [-o out.yaml] in.cel.yaml")
		fs.PrintDefaults()
	}
	var contexts []string
	var output string
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Func("context", "a YAML map, repeatable: its top-level keys are the variables of the expressions; the files are merged in the order given, maps deeply, a list, a scalar or a null of a later file replacing the earlier", func(s string) error { contexts = append(contexts, s); return nil })
	fs.StringVar(&output, "o", "", "the file to write (default stdout)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	switch {
	case *showVersion:
		_, _ = fmt.Fprintln(stdout, cmp.Or(version, "dev"))
		return 0
	case fs.NArg() != 1:
		_, _ = fmt.Fprintf(stderr, "expected one input file, got %d\n", fs.NArg())
		fs.Usage()
		return 2
	}
	out, err := compile(fs.Arg(0), contexts)
	if err == nil && output != "" {
		err = os.WriteFile(output, out, 0o644)
	} else if err == nil {
		_, err = stdout.Write(out)
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

// compile: the document in the file in compiled on the context files, its
// errors starting with the name of its file; nil when it is left out.
func compile(in string, contexts []string) ([]byte, error) {
	scope, err := readContext(contexts)
	if err != nil {
		return nil, err
	}
	src, err := os.ReadFile(in)
	if err != nil {
		return nil, err
	}
	out, err := cel2yaml.Render(context.Background(), src, scope)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", in, err)
	}
	return out, nil
}

// readContext: the context files merged in order (value.Merge: maps merge,
// lists, scalars and nulls replace), each a YAML map or empty, read as a
// document is, its numbers float64; an error names the file.
func readContext(files []string) (cel2yaml.Scope, error) {
	scope := cel2yaml.Scope{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		n, err := cel2yaml.Parse(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		v, err := cel2yaml.Data(n)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		m, ok := v.(map[string]any)
		if v != nil && !ok {
			return nil, fmt.Errorf("%s: the context must be a YAML map", f)
		}
		scope = value.Merge(scope, m)
	}
	return scope, nil
}
