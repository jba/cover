# cover

`cover` runs Go tests and reports **uncovered statements and branch outcomes** as
JSONL.

Go's built-in coverage measures statements, not branch outcomes. For example:

```go
func abs(n int) int {
	if n < 0 {
		n = -n
	}
	return n
}
```

A test that calls only `abs(-1)` gives this function 100% built-in statement
coverage: the `if` body and the `return` both execute. But the condition is never
false, so the path that skips the body is untested. `cover` reports that missing
`false` outcome at `n < 0`.

Requires Go 1.26 or later and module mode. The runtime is pure Go; Linux is tested.

```sh
go install github.com/jba/cover@latest
cover ./... > uncovered.jsonl
cover -run TestParse -race -o uncovered.jsonl ./parser
```

From a checkout, use `go run . [flags] [packages]`. Package patterns default to
`.`. Flags must precede package patterns.

## Flags

```
-o file       JSONL destination (default stdout; '-' also means stdout)
-run regexp   Select tests using go test's -run
-count n      Test repetitions (default 1, bypassing cached test results)
-timeout d    Forward a test timeout, e.g. 30s
-tags tags    Build tags, applied to package discovery and testing
-race         Enable Go's race detector for discovery and testing
```

Existing `GOFLAGS` and Go environment settings remain in effect. The tool
instruments only the requested packages' non-test Go source, not dependencies
unless also selected. Standard-library and vendored packages are skipped.
Test-only packages without production source are not instrumented, but their
tests still run and can exercise the selected production packages.

## Output

Only uncovered points are emitted. Test output and diagnostics go to stderr;
stdout is exclusively JSONL unless `-o` names an output file.

```jsonl
{"file":"/repo/foo.go","kind":"statement","range":{"start":{"line":12,"column":5},"end":{"line":12,"column":11}}}
{"file":"/repo/foo.go","kind":"branch","construct":"if","outcome":"false","range":{"start":{"line":11,"column":8},"end":{"line":11,"column":13}}}
{"file":"/repo/foo.go","kind":"branch","construct":"for","outcome":"skipped","range":{"start":{"line":20,"column":5},"end":{"line":20,"column":31}}}
```

- `file` is the absolute original-source filename.
- Ranges are half-open: inclusive start, exclusive end. Lines and byte columns
  are 1-based. `//line` directives do not change these physical coordinates.
- Statement ranges identify original statements; control-statement ranges cover
  their headers, not their bodies. This is statement-oriented coverage, **not an
  exact reproduction of `go test -cover`'s basic-block regions or percentages**.
- A branch range identifies the decision/header, not an invented empty body.
  Both missing outcomes can therefore have the same range. Uncovered statement
  records inside an unentered branch are also emitted; this overlap is intended.
- Results are sorted by filename, source position, and point description.

An empty report means all instrumented obligations were observed, **provided
that the command succeeded**. Missing coverage does not make the command fail;
listing, instrumentation, build, and test errors do. Snapshots that were saved
are still reported after a test failure, timeout, or abnormal exit; such a report
is partial and must not be treated as a successful coverage run. A process that
does not reach its save path can lose all its coverage.

## Branch obligations

| Construct | Outcomes | Report range |
| --- | --- | --- |
| `if` | `true`, `false` | Complete condition |
| Conditional `for`, any `range` | `entered`, `skipped` | Loop header |
| Unconditional `for` | `entered` | Loop header |
| Expression/type switch | One `case` per clause, explicit `default`, or implicit `no-match` | Clause header; switch header for `no-match` |
| `select` | One `case` per communication clause, explicit `default` | Clause header |

For both ordinary and range loops, `construct` is `"for"`. `entered` means at
least one body execution during a particular encounter with the loop. `skipped`
means that encounter finished normally without executing the body. A false
condition **after** one or more iterations does not satisfy `skipped`.

Each loop gets a local Boolean declared at function entry. First body entry sets
it and records `entered`; normal completion records `skipped` only if it is still
false. This handles `goto`, `break`, `continue`, early `return`, empty bodies, and
labeled loops.
Panicking during initialization, condition evaluation, or range evaluation does
not falsely record a skipped loop. A loop never reached has neither outcome
covered. An unconditional `for` has no `skipped` obligation.

State resets and statement-entry counters normally precede the statement's
labels. A `goto` to such a label replays that entry bookkeeping immediately
before jumping. Labels remain attached to their original statements, preserving
labeled `break` and `continue`. No new blocks surround loops or switches, and
hoisted declarations cannot be skipped by a jump. Labels and jumps are resolved
separately within each function, including function literals.

Switch clauses measure **entry**, including entry by `fallthrough`, not whether
every expression in a multi-expression case matched. Selects do not invent an
implicit default branch. Whole-condition branch coverage preserves short-circuit
evaluation, but does **not** establish separate coverage of `a` and `b` in
`a || b`; condition/MC/DC coverage is not implemented.

## Implementation and limitations

It instruments source through a Go build overlay: package import paths,
module/workspace context, relative `replace` directives, and original files stay
unchanged on disk. Counters use atomic increments; saved snapshots are merged
across test binaries.

The tool parses original ASTs and inserts bookkeeping without reformatting source.
It allocates IDs and physical source ranges before instrumentation. Rewritten
files, per-package runtimes, and a small shared registry package are supplied with
`go test -overlay`. Non-Go assets, embedded files, internal imports, and
external-package tests still see the original package directory and identity.
The registry is a virtual public subdirectory of a selected module; it has no
files on disk and adds no module requirements.

Each package keeps an ordinary in-memory array of `atomic.Uint64` counters. At
test completion, the registry saves each loaded instrumented package's counters
as JSON, using atomic loads to take the snapshot. Test-only packages also get a
save path, so their tests can contribute coverage to selected libraries.

If there is no `TestMain`, the tool generates one that runs `m.Run()`, saves, and
exits. An existing `TestMain` instead gets a deferred save for normal return, and
direct calls to the imported `os.Exit` inside its body are rewritten to save
before exiting. Import aliases and dot imports are supported; shadowed identifiers
are left alone. Deferred and goroutine exit calls retain their execution timing.
Existing test setup, teardown, return behavior, and exit codes are preserved
unless saving fails, in which case the command fails with a diagnostic.

Each package and process has its own snapshot file. Repeated saves by one process
replace its previous snapshot rather than double-counting it. The driver merges
snapshots from separate test binaries and subprocesses after `go test` exits.
Temporary files are removed afterward. No mmap, unsafe code, or runtime exit
hooks are used.

Current limitations:

- Panics, timeouts, and direct exits outside `TestMain` may prevent saving.
  Calls through exit-function aliases or helpers outside `TestMain` are not
  intercepted. This tool does not install Go's internal runtime exit hooks.
- Automatic vet is disabled for the instrumented test run: vet cannot use a
  virtual registry directory as its working directory. Run `go vet ./...`
  separately on the original source.
- GOPATH mode is not supported.
- Go's own overlay restrictions still apply, notably to some cgo scenarios and
  files under `GOMODCACHE`. cgo is not comprehensively tested.
- A test must not leave background processes running past test completion; their
  late counter updates cannot be reliably included.
- Statement instrumentation does not cover package-level initializer expressions
  directly (but does cover functions they call). Assembly is not instrumented.
- There is no feasibility analysis: logically impossible Boolean outcomes and
  zero-iteration cases still appear as obligations, except unconditional `for`.

## Development

```sh
go test ./...
go test -race ./...
go vet ./...
```

Tests include source positions and syntax/semantic preservation, real temporary
modules, sibling relative replacements, internal imports, embedded/testdata
assets, external-package tests, existing `TestMain`, atomic concurrency, subprocess
exits, goto target counter updates, and loop control-flow edge cases.
