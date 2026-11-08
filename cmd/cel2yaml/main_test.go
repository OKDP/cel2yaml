package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write: the files (name -> content) in a new directory, returned.
func write(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// cli: the command run in dir with args (the files of dir named as in the
// arguments, relative to it): its status, stdout and stderr.
func cli(t *testing.T, dir string, args ...string) (status int, stdout, stderr string) {
	t.Helper()
	t.Chdir(dir)
	var out, errOut bytes.Buffer
	status = run(args, &out, &errOut)
	return status, out.String(), errOut.String()
}

// TestRun: the document compiled on the context files, to stdout or to the
// file of -o, the keys sorted.
func TestRun(t *testing.T) {
	dir := write(t, map[string]string{
		"in.cel.yaml": "z: ${{ data.test + 1.0 }}\nname: ${{ data.name }}\nitems:\n  - $for: i in data.list\n    $each: ${{ i * 2.0 }}\n",
		"ctx.yaml":    "data: {test: 1, name: demo, list: [1, 2]}\n",
	})
	const want = "items:\n- 2\n- 4\nname: demo\nz: 2\n"
	status, stdout, stderr := cli(t, dir, "--context", "ctx.yaml", "in.cel.yaml")
	if status != 0 || stdout != want || stderr != "" {
		t.Errorf("stdout: status %d, got %q, stderr %q, want %q", status, stdout, stderr, want)
	}
	status, stdout, stderr = cli(t, dir, "-context=ctx.yaml", "-o", "out.yaml", "in.cel.yaml")
	got, err := os.ReadFile(filepath.Join(dir, "out.yaml"))
	if status != 0 || stdout != "" || stderr != "" || err != nil || string(got) != want {
		t.Errorf("-o: status %d, stdout %q, stderr %q, file %q (%v)", status, stdout, stderr, got, err)
	}
}

// TestContexts: the context files merged in the order given, as Helm merges
// its -f files: maps deeply, a list, a scalar and a null of a later file
// replacing the earlier; the top-level keys are the variables.
func TestContexts(t *testing.T) {
	dir := write(t, map[string]string{
		"in.cel.yaml": "db: ${{ db }}\nlist: ${{ list }}\nname: ${{ name }}\nnothing: ${{ nothing }}\nonly: ${{ only.b }}\n",
		"a.yaml":      "db: {host: a, port: 1, opts: {x: 1}}\nlist: [1, 2, 3]\nname: first\nnothing: {gone: true}\nonly: {a: 1, b: 2}\n",
		"b.yaml":      "db: {host: b, opts: {y: 2}}\nlist: [9]\nname: second\nnothing: ~\n",
		"empty.yaml":  "# nothing\n",
	})
	status, stdout, stderr := cli(t, dir, "--context", "a.yaml", "--context", "empty.yaml", "--context", "b.yaml", "in.cel.yaml")
	const want = `db:
  host: b
  opts:
    x: 1
    "y": 2
  port: 1
list:
- 9
name: second
nothing: null
only: 2
`
	if status != 0 || stdout != want || stderr != "" {
		t.Errorf("status %d, stderr %q, got\n%s\nwant\n%s", status, stderr, stdout, want)
	}
	// In the other order, the other file wins.
	_, stdout, _ = cli(t, dir, "--context", "b.yaml", "--context", "a.yaml", "in.cel.yaml")
	if !strings.Contains(stdout, "name: first\n") || !strings.Contains(stdout, "- 1\n- 2\n- 3\n") || !strings.Contains(stdout, "gone: true") {
		t.Errorf("a.yaml last: got\n%s", stdout)
	}
	// Without a context file, no variable.
	status, _, stderr = cli(t, dir, "in.cel.yaml")
	if status != 1 || !strings.Contains(stderr, "undeclared reference to 'db'") {
		t.Errorf("no context: status %d, stderr %q", status, stderr)
	}
}

// TestContextErrors: a context file that is not a YAML map, that does not
// read, or does not exist fails, naming the file; nothing is written.
func TestContextErrors(t *testing.T) {
	dir := write(t, map[string]string{
		"in.cel.yaml": "a: 1\n",
		"list.yaml":   "- a\n- b\n",
		"scalar.yaml": "just text\n",
		"bad.yaml":    "a: [\n",
		"alias.yaml":  "a: &x 1\nb: *x\n",
	})
	// The bad file after a good one: the error names it.
	for ctx, want := range map[string]string{
		"list.yaml":    "error: list.yaml: the context must be a YAML map\n",
		"scalar.yaml":  "error: scalar.yaml: the context must be a YAML map\n",
		"bad.yaml":     "error: bad.yaml: yaml: line 1: did not find expected node content\n",
		"alias.yaml":   "error: alias.yaml: line 2: aliases are not read\n",
		"missing.yaml": "error: open missing.yaml: no such file or directory\n",
	} {
		status, stdout, stderr := cli(t, dir, "--context", ctx, "-o", "out.yaml", "in.cel.yaml")
		if status != 1 || stdout != "" || stderr != want {
			t.Errorf("%s: status %d, stdout %q, stderr %q, want %q", ctx, status, stdout, stderr, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "out.yaml")); err == nil {
		t.Error("out.yaml written after an error")
	}
}

// TestErrors: a document that does not compile fails with its file name and
// the path of the error; status 1, nothing on stdout, no output file.
func TestErrors(t *testing.T) {
	dir := write(t, map[string]string{
		"bad.cel.yaml": "a:\n  env: [1, '${{ nope }}']\n",
		"ctx.yaml":     "x: 1\n",
	})
	status, stdout, stderr := cli(t, dir, "--context", "ctx.yaml", "-o", "out.yaml", "bad.cel.yaml")
	if want := "error: bad.cel.yaml: a: env[1]: ${{ nope }}: ERROR: <input>:1:2: undeclared reference to 'nope'"; status != 1 || stdout != "" || !strings.HasPrefix(stderr, want) {
		t.Errorf("status %d, stdout %q, stderr %q, want %q", status, stdout, stderr, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.yaml")); err == nil {
		t.Error("out.yaml written after an error")
	}
	status, _, stderr = cli(t, dir, "missing.cel.yaml")
	if status != 1 || stderr != "error: open missing.cel.yaml: no such file or directory\n" {
		t.Errorf("a missing input: status %d, stderr %q", status, stderr)
	}
}

// TestLeftOut: a document a false $if leaves out writes nothing: an empty
// output.
func TestLeftOut(t *testing.T) {
	dir := write(t, map[string]string{"in.cel.yaml": "$if: ${{ on }}\na: 1\n", "ctx.yaml": "on: false\n"})
	if status, stdout, stderr := cli(t, dir, "--context", "ctx.yaml", "in.cel.yaml"); status != 0 || stdout != "" || stderr != "" {
		t.Errorf("stdout: status %d, got %q, stderr %q", status, stdout, stderr)
	}
	if status, _, _ := cli(t, dir, "--context", "ctx.yaml", "-o", "out.yaml", "in.cel.yaml"); status != 0 {
		t.Errorf("-o: status %d", status)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "out.yaml")); err != nil || len(got) != 0 {
		t.Errorf("-o: got %q (%v), want an empty file", got, err)
	}
}

// TestUsage: the version, and the usage errors, status 2.
func TestUsage(t *testing.T) {
	dir := write(t, map[string]string{"in.cel.yaml": "a: 1\n"})
	if status, stdout, _ := cli(t, dir, "--version"); status != 0 || stdout != "dev\n" {
		t.Errorf("--version: status %d, got %q", status, stdout)
	}
	version = "1.2.3"
	defer func() { version = "" }()
	if _, stdout, _ := cli(t, dir, "--version"); stdout != "1.2.3\n" {
		t.Errorf("--version set at build time: got %q", stdout)
	}
	for name, args := range map[string][]string{
		"no input":      {},
		"two inputs":    {"in.cel.yaml", "in.cel.yaml"},
		"unknown flag":  {"--output", "x", "in.cel.yaml"},
		"no context":    {"--context"},
		"flag after in": {"in.cel.yaml", "-o", "out.yaml"},
	} {
		status, stdout, stderr := cli(t, dir, args...)
		if status != 2 || stdout != "" || !strings.Contains(stderr, "usage: cel2yaml [--context ctx.yaml]... [-o out.yaml] in.cel.yaml") {
			t.Errorf("%s: status %d, stdout %q, stderr %q", name, status, stdout, stderr)
		}
	}
}
