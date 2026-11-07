# cover

`cover` runs Go tests and reports **uncovered statements and branch outcomes** as
line-oriented text.

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
false, so the path that skips the body is untested. `cover` reports `skipped` at
`n < 0` to identify that missing outcome.

Requires Go 1.26 or later and module mode. The runtime is pure Go; Linux is tested.

```sh
go install github.com/jba/cover@latest
cover ./... > uncovered.txt
cover -run TestParse -race -o uncovered.txt ./parser
cover -v ./parser
```

From a checkout, use `go run . [flags] [packages]`. Package patterns default to
`.`. Flags must precede package patterns.

## Flags

```
-o file       Report destination (default stdout; '-' also means stdout)
-run regexp   Select tests using go test's -run
-count n      Test repetitions (default 1, bypassing cached test results)
-timeout d    Forward a test timeout, e.g. 30s
-tags tags    Build tags, applied to package discovery and testing
-race         Enable Go's race detector for discovery and testing
-v            Display full source lines below each report entry
```

Existing `GOFLAGS` and Go environment settings remain in effect. The tool
instruments only the requested packages' non-test Go source, not dependencies
unless also selected. Standard-library and vendored packages are skipped.
Test-only packages without production source are not instrumented, but their
tests still run and can exercise the selected production packages.

## Output

Only uncovered points are emitted. Test output and diagnostics go to stderr;
stdout contains the report unless `-o` names an output file. Each report entry is:

```text
FILE START END WORD
```

For example:

```text
/repo/foo.go 11:8 11:13 skipped
/repo/foo.go 12:5 12:11 uncovered
/repo/foo.go 20:5 20:31 skipped
```

- `FILE` is the absolute original-source filename, without quoting. It may contain
  spaces; parse the start, end, and word from the right. Filenames containing
  newlines cannot be represented and cause an error.
- `START` and `END` are `line:column` positions.
- Ranges are half-open: inclusive start, exclusive end. Lines and byte columns
  are 1-based. `//line` directives do not change these physical coordinates.
- `uncovered` identifies a statement that never executed. Control-statement ranges
  cover their headers, not their bodies. This is statement-oriented coverage,
  **not an exact reproduction of `go test -cover`'s basic-block regions or percentages**.
- `skipped` identifies a decision whose skipping outcome was **never observed**:
  an `if` condition was never false, a loop never finished with zero iterations,
  or a switch without a default never finished without matching a case. Its
  range identifies the condition or header, not an invented empty body.
- Uncovered body statements are reported independently; overlapping ranges are
  intentional. Results are sorted by filename, source position, and word.

With `-v`, each entry is immediately followed by the original source lines from
its start line through its end line, inclusive, each prefixed with four spaces:

```text
/repo/foo.go 11:8 11:13 skipped
        if n < 0 {
```

Columns are ignored when displaying source: complete lines are shown, including
the end line even when its column is 1. Original indentation and blank lines are
preserved; CRLF line endings are displayed as LF. Overlapping entries repeat
their source lines. `-o` includes these excerpts too. This flag does not enable
`go test -v`; omit it (or use `-v=false`) for the record-only format.

An empty report means there are no gaps under these reporting rules, **provided
that the command succeeded**. It does not establish every possible branch
outcome: empty bodies, for example, have no statements to report. Missing
coverage does not make the command fail; listing, instrumentation, build, and
test errors do. Snapshots that were saved
are still reported after a test failure, timeout, or abnormal exit; such a report
is partial and must not be treated as a successful coverage run. A process that
does not reach its save path can lose all its coverage.

## Skipped outcomes

| Construct | Outcome that must be observed | Report range |
| --- | --- | --- |
| `if` | Condition evaluates to false | Complete condition |
| Conditional `for`, any `range` | Loop finishes with zero body executions | Loop header |
| Expression/type switch without a default | Switch finishes without matching a case | Switch header |

There are no separate records for true conditions, loop entry, switch case or
default entry, or select clause entry. Statement coverage reports unexecuted
code in those bodies instead. An untaken empty true branch is not reported.

For both ordinary and range loops, skipping means a particular encounter with
the loop finished normally without executing the body. A false condition
**after** one or more iterations does not satisfy `skipped`.

Each loop gets a local Boolean declared at function entry. First body entry sets
it; normal completion records `skipped` only if it is still false. This handles
`goto`, `break`, `continue`, early `return`, empty bodies, and
labeled loops.
Panicking during initialization, condition evaluation, or range evaluation does
not falsely record a skipped loop. A loop never reached can have both `uncovered`
and `skipped` records at its header. An unconditional `for` has no `skipped`
obligation.

State resets and statement-entry counters normally precede the statement's
labels. A `goto` to such a label replays that entry bookkeeping immediately
before jumping. Labels remain attached to their original statements, preserving
labeled `break` and `continue`. No new blocks surround loops or switches, and
hoisted declarations cannot be skipped by a jump. Labels and jumps are resolved
separately within each function, including function literals.

Statement coverage inside switch clauses measures **entry**, including entry by
`fallthrough`, not whether every expression in a multi-expression case matched.
Selects do not invent an implicit default branch. Whole-condition branch coverage
preserves short-circuit evaluation, but does **not** establish separate coverage
of `a` and `b` in `a || b`; condition/MC/DC coverage is not implemented.

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
- There is no feasibility analysis: logically impossible false outcomes,
  zero-iteration cases, and switch no-match outcomes still appear as obligations,
  except where structurally excluded (an unconditional `for` or an explicit default).

## Development

All command code and tests live in one root-level `package main`, grouped by
function in separate files.

```sh
go test ./...
go test -race ./...
go vet ./...
```

Tests include source positions and syntax/semantic preservation, real temporary
modules, sibling relative replacements, internal imports, embedded/testdata
assets, external-package tests, existing `TestMain`, atomic concurrency, subprocess
exits, goto target counter updates, and loop control-flow edge cases.
