# Implementation contract

Module github.com/jba/cover. CLI at the module root. No external dependencies.

Internal package `internal/instrument` exports:
- Position {Line int `json:"line"`; Column int `json:"column"`}
- Range {Start Position `json:"start"`; End Position `json:"end"`}
- Point {File string `json:"file"`; Kind string `json:"kind"`; Construct string `json:"construct,omitempty"`; Outcome string `json:"outcome,omitempty"`; Range Range `json:"range"`; Counter int `json:"-"`}
- Instrument(filename string, src []byte, prefix string, nextCounter *int) ([]byte, []Point, error)

prefix is a valid reserved-ish identifier prefix unique to this package, e.g. __cover_a1b2. Instrumentation references prefix+"Hit"(counterID) and prefix+"Bool"(trueID,falseID, bool-expression) (a generic ~bool-preserving helper). Counters increment (not set), atomic via shared mmap runtime supplied by CLI. IDs allocate by incrementing *nextCounter, zero-based.

CLI at internal/runner, exported Run(ctx context.Context, args []string, stdout, stderr io.Writer) error. Root main.go just invokes runner.Run. Flags -o filename (default stdout JSONL), -run, -count (default 1), -timeout, -tags, -race. Positional package patterns default '.'; list packages via go list -json and instrument their non-test GoFiles/CgoFiles only. Use original-source absolute filenames in JSONL, ranges line/byte-column 1-based exclusive end. Compile rewritten sources through go test -overlay with virtual generated runtime Go file added inside each original package directory. Never copy modules or modify source. Tests' output to stderr, stdout exclusively JSONL.

Runtime strategy: per-package count file created/truncated by CLI, generated package Go file mmaps it shared; counters atomic.AddUint64 at aligned addresses. Binary runs need no TestMain wrapping or explicit flushing; OS-backed shared mapping survives os.Exit. Platform support Unix (initial Linux/macOS); reject unsupported runner platforms. Runtime generated imports syscall, os, sync/atomic, unsafe with collision-free package aliases. A generated package variable initializer maps the file or panics loudly; dependency ordering initializes it before original global initializers that call instrumented functions. Bool returns its input after Hit.

Loops: local bool declared at function entry; first body entry sets bool and hits entered branch counter. After loop, if !bool hit skipped counter. This captures skipped only on normal zero-iteration completion and does not misclassify initial-expression panics. Infinite for has entered obligation only (skipped structurally impossible).

Goto support: generated loop and switch flags are hoisted to function entry, before all original labels and declarations. No new blocks surround these controls. Each original statement has an entry prelude (statement counter, plus a flag reset where needed) inserted before its labels. Resolve labels to their unwrapped statements within each function, excluding nested function literals. Insert the target's prelude immediately before each goto keyword, after any labels on that goto. Jumps therefore cannot bypass target coverage or resets; the original loop/switch labels stay attached, preserving labeled break/continue. Block and empty statements need no entry prelude. Reserve the declaration edit before statement edits to handle identical offsets in compact source. Declare/reset/set flags with 0!=0 / 0==0 so shadowed bool/true/false identifiers do not interfere.

Statement coverage: executable original statement regions (not generated code), count on entry. Control statements use header ranges rather than spanning bodies. Branch coverage: if true/false (condition range); for/range entered/skipped (header range). Switch/type-switch/select clauses are instrumented as entry branch points; switches without defaults also have a no-match point. Conditions a||b are not split in initial version.
