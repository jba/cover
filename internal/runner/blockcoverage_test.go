package runner_test

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A statement point represents one native Go coverage block, not necessarily
// one source statement. Its weight can be zero (for example, an empty body).
// Counts are deliberately absent: this tool reports uncovered blocks, and only
// the distinction between zero and nonzero matters.
type builtinBlock struct {
	file       string
	range_     sourceRange
	statements int
}

// wantBuiltinBlocks compares the ENTIRE uncovered block set, including weights
// and zero-statement blocks. Comparing just statement lines or just a subset
// would miss native probe placement differences around labels, declarations,
// empty bodies, and calls that panic. Both runs use the installed Go toolchain,
// so this test also follows the minimum supported toolchain's coverage layout.
func wantBuiltinBlocks(t *testing.T, f fixture, points []point, args ...string) {
	t.Helper()
	wantBuiltinBlocksWithFiles(t, f, points, nil, args...)
}

func wantBuiltinBlocksWithFiles(t *testing.T, f fixture, points []point, aliases map[string]string, args ...string) {
	t.Helper()
	profile := filepath.Join(t.TempDir(), "builtin.cover")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	goArgs := append([]string{"test", "-count=1", "-covermode=atomic", "-coverprofile=" + profile}, args...)
	cmd := exec.CommandContext(ctx, "go", goArgs...)
	cmd.Dir = f.root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("builtin go %s: %v\n%s", strings.Join(goArgs, " "), err, output)
	}
	data, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	want := parseBuiltinBlocks(t, f, string(data), aliases)
	got := make(map[builtinBlock]bool)
	for _, p := range points {
		if p.Kind != "statement" {
			continue
		}
		key := builtinBlock{p.File, p.Range, p.Statements}
		if got[key] {
			t.Errorf("duplicate native block: %+v", p)
		}
		got[key] = true
	}
	var missing, extra []string
	for block := range want {
		if !got[block] {
			missing = append(missing, formatBuiltinBlock(block))
		}
	}
	for block := range got {
		if !want[block] {
			extra = append(extra, formatBuiltinBlock(block))
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing)+len(extra) != 0 {
		t.Errorf("uncovered blocks differ from builtin go test coverage:\nmissing tool blocks (-):\n%s\nextra tool blocks (+):\n%s\nbuiltin profile:\n%s",
			strings.Join(missing, "\n"), strings.Join(extra, "\n"), data)
	}
}

func formatBuiltinBlock(b builtinBlock) string {
	return fmt.Sprintf("%s:%d.%d,%d.%d statements=%d", b.file,
		b.range_.Start.Line, b.range_.Start.Column, b.range_.End.Line, b.range_.End.Column, b.statements)
}

func parseBuiltinBlocks(t *testing.T, f fixture, profile string, aliases map[string]string) map[builtinBlock]bool {
	t.Helper()
	uncovered := make(map[builtinBlock]bool)
	seen := make(map[builtinBlock]bool)
	scanner := bufio.NewScanner(strings.NewReader(profile))
	if !scanner.Scan() || scanner.Text() != "mode: atomic" {
		t.Fatalf("unexpected builtin profile header: %q", profile)
	}
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) != 3 {
			t.Fatalf("invalid builtin profile record: %q", line)
		}
		colon := strings.LastIndexByte(fields[0], ':')
		if colon < 0 {
			t.Fatalf("builtin record has no filename: %q", line)
		}
		name, coordinates := fields[0][:colon], fields[0][colon+1:]
		// go test writes import-path-prefixed names, not filesystem paths.
		// Map them back to the original physical fixture, including subpackages.
		const prefix = "example.com/fixture/"
		if !strings.HasPrefix(name, prefix) {
			t.Fatalf("builtin profile names unexpected package: %q", line)
		}
		file := aliases[name]
		if file == "" {
			file = f.path(strings.TrimPrefix(name, prefix))
		}
		if _, err := os.Stat(file); err != nil {
			t.Fatalf("builtin profile does not name physical fixture source: %q: %v", file, err)
		}
		var r sourceRange
		if n, err := fmt.Sscanf(coordinates, "%d.%d,%d.%d", &r.Start.Line, &r.Start.Column, &r.End.Line, &r.End.Column); n != 4 || err != nil {
			t.Fatalf("invalid builtin block coordinates: %q: %v", line, err)
		}
		statements, err := strconv.Atoi(fields[1])
		if err != nil || statements < 0 {
			t.Fatalf("invalid builtin statement weight: %q", line)
		}
		count, err := strconv.ParseUint(fields[2], 10, 64)
		if err != nil {
			t.Fatalf("invalid builtin block count: %q: %v", line, err)
		}
		block := builtinBlock{file, r, statements}
		if seen[block] {
			t.Fatalf("duplicate builtin profile block: %q", line)
		}
		seen[block] = true
		if count == 0 {
			uncovered[block] = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(seen) == 0 {
		t.Fatal("builtin profile contains no blocks; differential comparison would be vacuous")
	}
	return uncovered
}

func TestBuiltinBlockCoverage(t *testing.T) {
	// Every route gets a fresh real module. The None routes expose every
	// uncalled body, including zero-weight bodies; the other routes check that
	// covered blocks disappear, rather than only checking uncovered metadata.
	for _, tc := range []struct {
		name, src, tests string
	}{
		{"linear-declarations-empty-panic", builtinLinearSource, builtinLinearTests},
		{"if-switch-type-switch-select", builtinDecisionsSource, builtinDecisionsTests},
		{"loops-labels-goto", builtinLoopsSource, builtinLoopsTests},
		{"closures-initializers-rangefunc", builtinClosuresSource, builtinClosuresTests},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, route := range []string{"Left", "Right", "None"} {
				t.Run(route, func(t *testing.T) {
					f := newFixture(t, map[string]string{
						"go.mod": fixtureMod, "blocks.go": tc.src, "blocks_test.go": tc.tests,
					}, ".")
					args := []string{"-run", "^Test" + route + "$", "-count=1", "-timeout", "15s"}
					points := f.success(t, args...)
					wantBuiltinBlocks(t, f, points, args...)
				})
			}
		})
	}
}

func TestBuiltinBlockCoverageLineDirectives(t *testing.T) {
	const src = `package fixture

//line virtual.go:100
func Adjusted(n int) int {
	if n > 0 {
		return n
	}
	return 0
}

//line virtualcolumns.go:300:7
func Columns(n int) int {
	if n == 0 { return 1 }
	return n
}
`
	const tests = `package fixture
import "testing"
func TestLeft(t *testing.T) {
	if Adjusted(1) != 1 || Columns(0) != 1 { t.Fatal("changed result") }
}
func TestRight(t *testing.T) {
	if Adjusted(0) != 0 || Columns(2) != 2 { t.Fatal("changed result") }
}
func TestNone(t *testing.T) {}
`
	for _, route := range []string{"Left", "Right", "None"} {
		t.Run(route, func(t *testing.T) {
			f := newFixture(t, map[string]string{
				"go.mod": fixtureMod, "blocks.go": src, "blocks_test.go": tests,
			}, ".")
			args := []string{"-run", "^Test" + route + "$", "-count=1", "-timeout", "15s"}
			out, diagnostic, err := f.run(t, args...)
			if err != nil {
				t.Fatalf("Run: %v\n%s\n%s", err, diagnostic, out)
			}
			points := decodePointsWithLineDirectives(t, out, true)
			// The builtin profile uses the directive's filename. The tool's
			// file identity remains the actual original file on disk, while
			// its statement coordinates follow the installed toolchain.
			wantBuiltinBlocksWithFiles(t, f, points, map[string]string{
				"example.com/fixture/virtual.go":        f.path("blocks.go"),
				"example.com/fixture/virtualcolumns.go": f.path("blocks.go"),
			}, args...)
			wantBranch(t, points, f.path("blocks.go"), src, "if n > 0", "if", "true", route != "Left")
			wantBranch(t, points, f.path("blocks.go"), src, "if n == 0", "if", "true", route != "Left")
		})
	}
}

const builtinLinearSource = `package fixture

var Sink int

func Straight(n int) int {
	value := n
	value++

	// This comment and blank line belong to the same native block.
	value *= 2
	return value
}
func abort() { panic("callee panic") }
func ImplicitPanic() {
	Sink++
	abort()
	Sink += 100 // Native coverage marks this covered despite the callee panic.
}
func ExplicitPanic() {
	Sink++
	panic("explicit panic")
	Sink += 200 // An explicit panic terminates the preceding native block.
}
func Early(n int) int {
	if n < 0 {
		return -1
	}
	value := n + 1
	return value
}
func ReturnBeforeDead() int {
	return 3
	Sink++
	return 4
}
func Empty() {}
func EmptyMultiline() {

	// No statements, but still a coverage block.
}
func EmptyBranches(yes bool) {
	if yes {} else {}
	{}
}
func Declarations() int {
	type (
		Count int

		Other int
	)

	const (
		first = 1

		second = 2
	)

	var (
		left Count = first

		right Other = second
	)
	// Declarations aggregate in the containing native block.
	return int(left) + int(right)
}
func Unicode(é int) int {
	// Columns are bytes, including tabs and the multibyte identifier.
	if é > 0 { return é }
	return 0
}
`

const builtinLinearTests = `package fixture
import "testing"
func caught(t *testing.T, f func()) {
	t.Helper()
	defer func() { if recover() == nil { t.Error("expected panic") } }()
	f()
}
func TestLeft(t *testing.T) {
	if Straight(2) != 6 || Early(-1) != -1 || Unicode(1) != 1 { t.Fatal("changed result") }
	caught(t, ImplicitPanic)
	caught(t, ExplicitPanic)
	EmptyBranches(true)
}
func TestRight(t *testing.T) {
	if Straight(3) != 8 || Early(2) != 3 || Declarations() != 3 || ReturnBeforeDead() != 3 || Unicode(0) != 0 { t.Fatal("changed result") }
	Empty()
	EmptyMultiline()
	EmptyBranches(false)
}
func TestNone(t *testing.T) {}
`

const builtinDecisionsSource = `package fixture

var Sink int
func Decide(n int) int {
	if n < 0 {
		return -1
	} else if n == 0 {
		return 0
	} else {
		Sink++
	}
	if n > 10 { Sink += 10 }
	return n
}
func Switch(n int) int {
	switch n {
	case 0:
	case 1:
		Sink++
		fallthrough
	case 2, 3:
		return 2
	default:
		return 9
	}
	return 0
}
func SwitchNoDefault(n int) {
	switch n {
	case 1:
		Sink++
	case 2:
	}
	Sink++
}
func EmptySwitch() { switch 0 {} }
func Types(v any) int {
	switch x := v.(type) {
	case nil:
	case int:
		return x
	case string, []byte:
		return 2
	default:
		return 9
	}
	return 0
}
func TypesNoDefault(v any) {
	switch v.(type) {
	case int:
		Sink++
	case string:
	}
}
func EmptyTypes(v any) { switch v.(type) {} }
func Select(ch chan int) int {
	select {
	case n := <-ch:
		return n
	default:
		return -1
	}
}
func SelectEmptyCase(ch chan int) {
	select {
	case <-ch:
	default:
	}
}
func Send(ch chan int) {
	select {
	case ch <- 3:
		Sink++
	default:
	}
}
func EmptySelect() { select {} }
`

const builtinDecisionsTests = `package fixture
import "testing"
func TestLeft(t *testing.T) {
	if Decide(-1) != -1 || Decide(0) != 0 || Switch(1) != 2 || Switch(0) != 0 || Types(7) != 7 { t.Fatal("changed result") }
	SwitchNoDefault(1)
	TypesNoDefault(1)
	ch := make(chan int, 1)
	Send(ch)
	if Select(ch) != 3 { t.Fatal("receive changed") }
	SelectEmptyCase(ch)
	EmptySwitch()
	EmptyTypes(1)
}
func TestRight(t *testing.T) {
	if Decide(11) != 11 || Switch(9) != 9 || Types(nil) != 0 || Types("x") != 2 || Types(false) != 9 { t.Fatal("changed result") }
	SwitchNoDefault(9)
	TypesNoDefault(false)
	ch := make(chan int, 1)
	if Select(ch) != -1 { t.Fatal("default changed") }
	ch <- 4
	Send(ch)
	SelectEmptyCase(ch)
}
func TestNone(t *testing.T) {}
`

const builtinLoopsSource = `package fixture

func Loops(n int, xs []int) int {
	total := 0
	for i := 0; i < n; i++ {
		if i == 1 { continue }
		total += i
		if i > 2 { break }
	}
	for _, x := range xs {
		total += x
	}
	for range n {}
	for i := 0; i < n; i++ {}
	return total
}
func Forever() int {
	for { return 7 }
}
func GotoLoop(n int) int {
	total, passes := 0, 0
	goto Loop
	total += 100
Loop:
	for i := 0; i < n; i++ { // Native header counter is skipped by goto.
		total++
	}
	passes++
	if passes == 1 {
		n = 0
		goto Loop
	}
	return total
}
func AdjacentLabels() int {
	total := 0
	goto First
First:
Second:
	for range 1 {
		total++
		break Second
	}
	return total
}
func GotoTargets() int {
	x := 0
	goto Assignment
	x = 100
Assignment:
	x++
	goto Conditional
Conditional:
	if x == 1 { x++ }
	goto Dispatch
Dispatch:
	switch x {
	case 2:
		x++
	default:
		x = 100
	}
	goto Typed
Typed:
	switch any(x).(type) {
	case int:
		x++
	}
	goto Selection
Selection:
	select {
	default:
		x++
	}
	goto Block
Block:
	{ x++ }
	goto Empty
Empty:
	;
	x++
	return x
}
func LabelledControl() int {
	total := 0
Outer:
	for i := range 3 {
		for j := range 2 {
			if i == 0 { continue Outer }
			total += j + 1
			break Outer
		}
	}
	return total
}
func TerminalLabel(p *int) {
	*p++
	goto Done
	*p += 100
Done:
}
func Backward() int {
	x := 0
Again:
	x++
	if x < 2 { goto Again }
	return x
}
`

const builtinLoopsTests = `package fixture
import "testing"
func TestLeft(t *testing.T) {
	if Loops(4, []int{2, 3}) != 10 || Forever() != 7 || GotoLoop(2) != 2 || AdjacentLabels() != 1 || GotoTargets() != 7 { t.Fatal("changed result") }
}
func TestRight(t *testing.T) {
	if Loops(0, nil) != 0 || GotoLoop(0) != 0 || LabelledControl() != 1 || Backward() != 2 { t.Fatal("changed result") }
	x := 0
	TerminalLabel(&x)
	if x != 1 { t.Fatal("terminal label changed") }
}
func TestNone(t *testing.T) {}
`

const builtinClosuresSource = `package fixture

var Global = func() int {
	value := 1
	if value < 0 { return -1 }
	return value + 1
}()
var Worker = func(yes bool) int {
	if yes { return Global }
	return 0
}
var EmptyWorker = func() {}
func Nested(yes bool) int {
	outer := func(n int) int {
		inner := func() int {
			if yes { return n + 1 }
			return 0
		}
		return inner()
	}
	unused := func() {}
	_ = unused
	return outer(2)
}
func Iterator(n int) int {
	seq := func(yield func(int) bool) {
		for i := range n {
			if !yield(i) { return }
		}
	}
	total := 0
	for value := range seq {
		total += value
		if value == 2 { break }
	}
	return total
}
func LabelNamespaces() int {
	x := 0
	worker := func() int {
		value := 0
		goto Target
	Target:
		value++
		return value
	}
	goto Target
Target:
	for range 1 { x += worker() }
	return x
}
`

const builtinClosuresTests = `package fixture
import "testing"
func TestLeft(t *testing.T) {
	if Global != 2 || Worker(true) != 2 || Nested(true) != 3 || Iterator(4) != 3 || LabelNamespaces() != 1 { t.Fatal("changed result") }
}
func TestRight(t *testing.T) {
	if Worker(false) != 0 || Nested(false) != 0 || Iterator(0) != 0 { t.Fatal("changed result") }
	EmptyWorker()
}
func TestNone(t *testing.T) {}
`
