# cel2yaml

cel2yaml compiles one YAML document with [CEL](https://cel.dev)
expressions to one YAML document. The document is shaped like the
document it computes, `${{ <CEL> }}` computes a value, and a few
`$` directives (`$if`, `$for`, `$merge`...) give the maps and the lists
their form. The whole file is the output: no key is reserved, none is
read for another purpose than to be written.

```yaml
# service.cel.yaml
name: ${{ app.name }}-${{ env }}
replicas: "${{ env == 'prod' ? 3 : 1 }}"
ports:
  - $for: p in app.ports
    $each: {containerPort: "${{ p }}"}
debug: {$if: "${{ env != 'prod' }}", level: trace}
```

```yaml
# context.yaml
app: {name: web, ports: [80, 443]}
env: dev
```

```console
$ cel2yaml --context context.yaml service.cel.yaml
debug:
  level: trace
name: web-dev
ports:
- containerPort: 80
- containerPort: 443
replicas: 1
```

The output is deterministic: the keys of every map are sorted, and the
same document on the same context gives the same bytes. The evaluation
is hermetic (no clock, no randomness, no file, no network) and bounded
(a [cost limit](#cost-and-size-limits) and a cap on the size it builds),
so that a document from a source you have not reviewed can be compiled.

## Install

Each GitHub release attaches `cel2yaml` for Linux, macOS and Windows,
amd64 and arm64, as archives with their SHA-256 sums
(`cel2yaml_<version>_<os>_<arch>.tar.gz`, `.zip` for Windows), built by
goreleaser, an SBOM each. The checksums are signed keyless with cosign
by the release workflow and each archive's build provenance is attested:

```console
$ cosign verify-blob --bundle cel2yaml_<version>_checksums.txt.sigstore.json \
    --certificate-identity "https://github.com/OKDP/cel2yaml/.github/workflows/release-please.yml@refs/heads/main" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    cel2yaml_<version>_checksums.txt
$ sha256sum --ignore-missing -c cel2yaml_<version>_checksums.txt
$ gh attestation verify <archive> --repo OKDP/cel2yaml
```

`go install` installs a release by its module path:

```console
$ go install github.com/okdp/cel2yaml/cmd/cel2yaml@vX.Y.Z
```

From a checkout, `make build` writes `bin/cel2yaml`. As a library:
`go get github.com/okdp/cel2yaml`.

## Command

```text
cel2yaml [--context ctx.yaml]... [-o out.yaml] in.cel.yaml
cel2yaml --version
```

| Option | Meaning |
|---|---|
| `--context <file>` | a YAML map, repeatable: its top-level keys are the variables of the expressions. Without one, there is no variable |
| `-o <file>` | the file to write; the default is the standard output |
| `--version` | print the version and exit |

- **Context files.** Each file is a YAML map, or empty; any other
  document is an error naming the file. A file holding
  `data: {test: 1}` makes `${{ data.test }}` resolve. The files are
  merged in the order given, as Helm merges its `-f` files: maps merge
  deeply, a list, a scalar or a null of a later file replaces the
  earlier value. A context is read as the document is, so its numbers
  are float64 ([numbers](#numbers)).
- **Output.** The compiled document, written as YAML, the keys of each
  map sorted. A document a false `$if` leaves out writes nothing: an
  empty output (an empty file with `-o`). Nothing is written to `-o`
  when the compile fails.
- **Errors.** On the standard error, `error: <input>: <path>: <message>`,
  and status 1; a usage error has status 2. The cost limit is
  1000000, the [default of the library](#cost-and-size-limits).

## Language

A document is YAML 1.2, as yaml.v3 reads it. A string holding
`${{ ... }}` is an expression; a map holding a key that starts with `$`
is a directive.

### Expressions

| Scalar | Value |
|---|---|
| `${{ expr }}`, the whole scalar | the expression's value, with its type: number, boolean, string, list, map or null |
| text and expressions (`https://${{ host }}`) | a string: the text, and each expression's value as text (`str`) |
| `$${{` | a literal `${{` |
| no `${{` | the scalar as YAML reads it |

The whole scalar means nothing around the expression, not even a space
inside the quotes, or the final newline of a `|` or `>` block scalar
(`>-` and `|-` keep a lone expression). The `}}` closing an expression is
found skipping quoted strings (`'...'`, `"..."`, a `\` escaping the next
character) and nested braces: `${{ {"k": "}}"}.k }}` is one expression.
Nothing else of CEL is read: a `}}` in a `//` comment closes the
expression, and a raw or triple-quoted string is read as quoted strings.
A scalar without the closing `}}` fails with
`unterminated ${{ in "<scalar>"`.

Map keys are never evaluated: `${{ x }}: 1` is the key `${{ x }}`.
Computed keys come from `$for` (`$key`) or `$merge`. A key starting with
`$` is a directive; `$$` writes one: `$$schema` is the key `$schema`.

Every expression is compiled once, when the document is loaded, before
any is evaluated, in every branch: a syntax error, an undeclared name or
an unknown function fails the compile whether the document takes the
branch or not. Variables are dynamic: a type error (`no such overload`)
or a missing key (`no such key`) is an evaluation error, found only when
the expression is evaluated. The same goes for the directives: a
misspelled or misplaced one fails in every branch.

The value of an expression is converted to data, as cel-go converts it to
JSON (`google.protobuf.Value`): maps with string keys, lists, numbers,
strings, booleans and null, at any depth.

| CEL value | Data |
|---|---|
| int, uint, double | a double, written without decimals when integral; an integer beyond 2^53 is its digits, as text (`"9007199254740993"`) |
| a NaN or an infinite double (`0.0/0.0`) | the text `"NaN"`, `"Infinity"`, `"-Infinity"` |
| duration, timestamp | its text, as `string()` writes it: `3600s`, RFC 3339 |
| bytes | their base64 (`b'abc'` is `"YWJj"`) |
| optional | its value: `{'o': m.?k}` is `{o: v}` |

What has no data form fails the expression: a map key that is not a
string (`[{1: 2}]`), an optional without a value (`x.?nope` read
as a value), a type (`type(x)`).

### Numbers

Every number of the context and of the document's YAML (`1`, `0x1F`) is
a CEL `double`; a CEL literal `1` is an `int`. Comparisons mix them
(`n > 1`, `n == 1`); arithmetic does not: `n + 1` fails with
`no such overload`. Write `n + 1.0` or `int(n) + 1`. As text
(interpolation, `str`), a number prints as Go prints a float64:
`8080`, `2.5`, `1e+06` (from one million on). `string(int(x))` prints
every digit.

### YAML reading

| Plain scalar | Value |
|---|---|
| `true`, `false`, `True`, `FALSE` | the boolean |
| `y`, `yes`, `on`, `n`, `no`, `off` (any case) | the string |
| `0777`, `0o17`, `0x1F`, `0b101`, `1_000` | `511`, `15`, `31`, `5`, `1000` |
| `2001-12-14` | the string: timestamps are not decoded |
| `~`, `null`, empty | null |
| `.nan`, `.inf`, `-.inf` | refused: `line <n>: json: cannot marshal from Go float64: unsupported value: NaN` |

A tagged scalar is decoded as its tag says (`!!str 1` is the string
`"1"`) and fails when it cannot be (`!!bool yes`). Text that is not UTF-8
(a `!!binary`) is refused. Keys are taken as written: `on:` is the key
`on`, and of a key written twice, the later value wins. A second YAML
document, after `---`, is ignored. A `|` or `>` block scalar that ends
the input without a final line break keeps its newline, as in a Helm
values file. A merge key `<<` is a plain key.

Aliases are refused: `line <n>: aliases are not read`, `<n>` the line of
the alias. A value used twice is a `$vars` binding, read by name.

### Quoting

YAML ends or splits a plain scalar on characters CEL uses. Quote the
scalar, or write a long expression as a `>-` block scalar.

| Pitfall | Write |
|---|---|
| `: ` in a plain scalar (a ternary, a map literal) | `v: "${{ on ? 'a' : 'b' }}"` |
| ` #` starts a comment | `v: "${{ 'a #b' }}"` |
| `,` `[` `]` `{` `}` inside a flow collection | `{name: "${{ n }}"}` |
| a final newline (`\|`, `>`) makes text | `v: >-` |
| quotes of the scalar's own kind | `"${{ x == 'a' }}"` |

### Directives

| Directive | In | Value | Result |
|---|---|---|---|
| `$if` | a map | a condition | the map is left out when the condition is not true |
| `$value` | a map | any node | the map is this value |
| `$else` | a map with `$if` and `$value` | any node | the map is this value when the condition is not true |
| `$for` with `$key`, `$value` | a map | `<name> in <expression>` | a map, one key per element |
| `$for` with `$each` | a list item | `<name> in <expression>` | items per element |
| `$vars` | next to `$for` | a map of bindings | bindings per element, in order |
| `$merge` | a map | an expression returning a map | merged over the map's keys |
| `$spread` | a list item | an expression returning a list | its items, spliced in |

A directive is allowed where its form says; any other key beside it is
an error naming the keys allowed (`$iff: not allowed here (allowed: $if,
$merge)`). A directive is not a variable: the document's own keys never
become variables.

**`$if`** tests a condition as Helm does: false, null, `0`, `""`, an
empty map and an empty list are false, anything else is true. On a plain
map, the map is left out when the condition is false (the key of a map,
the item of a list, the whole document at its root). With `$value`, the
node is the value of the map; with `$value` and `$else`, `$else` when the
condition is false (`$else` is not read without `$if`, and not
evaluated when the condition is true). `$else` nests: an else if.

```yaml
tls: {$if: "${{ tls.enabled }}", secretName: ${{ tls.secret }}}
mode: {$if: "${{ prod }}", $value: strict, $else: lenient}
```

**`$for`** builds a map or a list from a list, `<name> in <expression>`;
a null list has no element, anything but a list fails. In a map, `$key`
and `$value` are evaluated per element (`$if` may guard the map); in a
list item, `$each` is, a list spliced, a value left out by `$if` none.
`$vars` binds names per element, in order, each seeing the previous.

```yaml
labels: {$for: l in labels, $key: "${{ l.name }}", $value: "${{ l.value }}"}
args:
  - $for: a in args
    $vars: {up: "${{ a.upperAscii() }}"}
    $each: ["--${{ up }}", "${{ a }}"]
```

CEL iterates a map in no fixed order; iterate over the sorted keys
(`m.map(k, k).sort()`) for a list built from one.

**`$merge`** merges the map an expression returns over the keys of the
enclosing map, which it overrides, maps deeply, lists and scalars
replaced. **`$spread`** splices the items of a list into the enclosing
list; a value that is not a list gives none.

```yaml
config: {replicas: 1, $merge: "${{ overrides }}"}
args: [--serve, {$spread: "${{ extraArgs }}"}]
```

## Functions

Every expression may call CEL's standard library, the CEL libraries
below, each at a pinned version, and the host functions. Nothing else is
callable: no file, no clock, no randomness, no environment.

| Library | Version | Functions |
|---|---|---|
| strings (`ext.Strings`) | 5 | `charAt`, `indexOf`, `lastIndexOf`, `lowerAscii`, `upperAscii`, `replace`, `split`, `substring`, `trim`, `join`, `format` (a precision of 100 at most), `reverse`, `strings.quote` |
| lists (`ext.Lists`) | 3 | `lists.range`, `slice`, `flatten`, `distinct`, `reverse`, `sort`, `sortBy` |
| bindings (`ext.Bindings`) | 1 | `cel.bind(name, value, expr)` |
| two-variable comprehensions (`ext.TwoVarComprehensions`) | 0 | `all(k, v, ...)`, `exists(k, v, ...)`, `existsOne`, `transformList`, `transformMap`, `transformMapEntry` |
| encoders (`ext.Encoders`) | 0 | `base64.encode`, `base64.decode` |
| optional types (`cel.OptionalTypes`) | 2 | `x.?field`, `m[?key]`, `l[?index]`, `orValue`, `hasValue`, `value`, `or`, `optional.of`, `optional.none`, `optional.ofNonZeroValue`, `optFlatMap`, `l.first()` and `l.last()` (optionals: not the host function `first(list)`), optional entries `{?'k': opt}` |

A cel-go upgrade does not change what a document may call until a
version is raised here. The math, sets and regex extensions are not
enabled.

The host functions reproduce the Helm and sprig functions of the same
names, each deterministic and hermetic, and take one argument of any
type, converted to data:

| Function | Returns | Result |
|---|---|---|
| `str(x)` | string | `x` as Go templates print it: null is `""`, a number as a float64, a list `[a b]`, a map `map[k:v]`; interpolation converts with it |
| `first(list)` | any | the first element that is not empty (null, `false`, `0`, `""`, `[]`, `{}`), else `""`; a value that is not a list gives `""`. `first([x, d])` is Helm's `x \| default d` |
| `sha256(x)` | string | the hex SHA-256 of `str(x)`; null is hashed as `""` |
| `toJson(x)`, `json.encode(x)` | string | Go's `encoding/json`: compact, keys sorted, `<`, `>`, `&` escaped |
| `toPrettyJson(x)` | string | `toJson`'s, indented by two spaces |
| `toYaml(x)` | string | sigs.k8s.io/yaml: keys sorted, YAML 1.1 quoting, no final newline |
| `quote(x)` | string | Go's `%q` of `str(x)`; null gives `""` |

An encoding that fails fails the expression, where Helm would print
`""`. A value is written in the output as data: `config: ${{ settings }}`
is a map; the encoders are for a value that must be text.

A library adds its own functions ([library](#library)).

## Cost and size limits

Every expression the directives evaluate counts in the compile's CEL
cost, an expression in a `$for` once per element, and a host function
what it reads and builds: a call costs 1, plus a unit a byte of its
argument, about its serialisation, and the bytes of the string it
returns (`sha256`, `toJson`, `toYaml`, `quote` of a large value). Once
the total passes the limit (checked after each expression), the compile
fails with `the expressions of the compile exceed its CEL cost limit
(<n>)`. A `$for` counts too, as it repeats its body without an
evaluation of CEL's own: each element costs 1, and what its body builds
costs its size. The limit of `cel2yaml` and of `Render` is
`DefaultCostLimit`, 1000000; `NewSession` takes another, 0 for none.

The context of the session is checked before each expression, at each
element of a `$for` and every 100 iterations of a comprehension: a
cancelled context stops the compile. The language has no recursion and no
include, and every loop runs over a list or a map it is given: the work is
bounded by the document and the size of the data it iterates.

Whatever the limit, a document builds 32 MiB of values at most, about the
bytes they take in memory: what its directives build, what its
expressions return, a value counted each time a list holds it again, as
each is a copy. Past it, the compile fails with
`the values the document builds exceed 32 MiB`.

## Errors

An error names the path of the expression in the document, a list position
following its key without a separator:

```text
app: env[2]: value: ${{ values.nope }}: no such key: nope
```

`${{ <expression> }}: <message>` is the expression and CEL's message
(a compile error gives its line and column in the expression). A
mistake in the language is reported at the path of the directive:
`l[0]: $for: in a list item, needs $each`. The command prefixes the
name of the input file.

## Library

```go
import "github.com/okdp/cel2yaml"

scope := cel2yaml.Scope{"data": map[string]any{"test": 1}}
out, err := cel2yaml.Render(ctx, []byte(`a: ${{ data.test }}`), scope)
// out: "a: 1\n"
```

`Render` compiles a document on a scope, a map of named values (the
variables of the expressions; YAML and JSON values, `map[string]any`,
`[]any`, numbers, strings, booleans, nil), and writes it as YAML, the
keys sorted. It returns nil for a document a false `$if` leaves out, or
an empty one. `Data` reads a YAML node as such a value, as the command
reads a context file.

A `Session` carries what the compiles of a caller share: its context, a
total CEL cost, the host functions of the caller and the CEL
environments, built once:

```go
s := cel2yaml.NewSession(ctx, cel2yaml.DefaultCostLimit, hostFuncs...)
out, err := s.Render(src, scope)
fmt.Println(s.Used()) // the CEL cost so far
```

A host function is a `cel.EnvOption`; `Func` declares one of one
argument of any type, `Unary` binds one, `Costed` counts the calls of a
function in the session's cost as the language's own:

```go
upper := cel2yaml.Func("shout", cel.StringType, func(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("not a string: %v", v)
	}
	return strings.ToUpper(s) + "!", nil
})
s := cel2yaml.NewSession(ctx, cel2yaml.DefaultCostLimit, upper)
```

For a caller that compiles the parts of a document separately, the
steps of `Render`: `Parse` reads a YAML document as a `*yaml.Node`
(refusing aliases and scalars without a data form, with their line);
`Session.Template` compiles a node against the sorted names of the
variables it is evaluated on, checking every directive and compiling
every expression in every branch; `Template.Eval` evaluates it on a scope
and returns its data, and whether it is kept (`false` when a `$if`
leaves it out). Compile everything that is evaluated together before
evaluating any of it. `Session.Document` groups evaluations as one
document for the size cap: counted from 0, then added to the document
that holds it, for one a host function evaluates inside an expression
of another.

| Function | Use |
|---|---|
| `Render(ctx, src, scope)` | a document to YAML, with `DefaultCostLimit` |
| `NewSession(ctx, limit, funcs...)` | a session: context, total CEL cost, host functions |
| `(*Session).Render(src, scope)` | the same in a session |
| `Parse(src)`, `Data(node)` | a YAML document as a node, a node as data |
| `(*Session).Template(node, names)` | a node compiled, in every branch |
| `(*Template).Eval(scope)` | its data, and whether it is kept |
| `(*Session).Document(f)` | evaluations counted as one document |
| `(*Session).Used()` | the CEL cost so far |
| `Func`, `Unary`, `Costed` | host functions |

A session is not safe for concurrent use: one per compile.

## Development

```console
$ make check    # gofmt, vet, golangci-lint, go mod tidy, build, race tests
$ make fuzz     # the fuzz tests, 30s each (FUZZTIME=60s)
```

`make help` lists the targets. Commits follow
[Conventional Commits](https://www.conventionalcommits.org): release-please
reads them to open the release pull request, and goreleaser attaches the
binaries to its release.

## License

Apache 2.0, see [LICENSE](LICENSE).
