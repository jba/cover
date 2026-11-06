package runner_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jba/cover/internal/runner"
)

// These tests deliberately use real, independent modules. In particular, testing
// this repository itself would not catch a runner that loses a module's identity,
// relative replacements, working directory, or non-Go files.
//
// Do not add t.Parallel: Run uses the process working directory.
const fixtureMod = "module example.com/fixture\n\ngo 1.24.0\n"

type position struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

type sourceRange struct {
	Start position `json:"start"`
	End   position `json:"end"`
}

type point struct {
	File      string      `json:"file"`
	Kind      string      `json:"kind"`
	Construct string      `json:"construct,omitempty"`
	Outcome   string      `json:"outcome,omitempty"`
	Range     sourceRange `json:"range"`
}

type fixture struct {
	root string
}

func newFixture(t *testing.T, files map[string]string, workdir string) fixture {
	t.Helper()
	root := t.TempDir()
	for name, contents := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(filepath.Join(root, filepath.FromSlash(workdir)))
	return fixture{root: root}
}

func (f fixture) path(name string) string {
	return filepath.Join(f.root, filepath.FromSlash(name))
}

type fileState struct {
	Mode    fs.FileMode
	ModTime time.Time
	Data    string
}

func snapshot(t *testing.T, root string) map[string]fileState {
	t.Helper()
	result := map[string]fileState{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		result[name] = fileState{Mode: info.Mode(), ModTime: info.ModTime(), Data: string(data)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// run also checks that the overlay did not leave generated files or edits behind.
// A separate timeout bounds mistakes in the runner itself, not just go test.
func (f fixture) run(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	before := snapshot(t, f.root)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	var out, diagnostic bytes.Buffer
	err = runner.Run(ctx, args, &out, &diagnostic)
	after := snapshot(t, f.root)
	if !reflect.DeepEqual(before, after) {
		var changed []string
		for name, old := range before {
			if now, ok := after[name]; !ok || now != old {
				changed = append(changed, name)
			}
		}
		for name := range after {
			if _, ok := before[name]; !ok {
				changed = append(changed, name+" (added)")
			}
		}
		sort.Strings(changed)
		t.Errorf("Run modified the original fixture: %v", changed)
	}
	return out.String(), diagnostic.String(), err
}

func (f fixture) success(t *testing.T, args ...string) []point {
	t.Helper()
	out, diagnostic, err := f.run(t, args...)
	if err != nil {
		t.Fatalf("Run(%q): %v\nstderr:\n%s\nstdout:\n%s", args, err, diagnostic, out)
	}
	return decodePoints(t, out)
}

// Decoding one object per line, with unknown fields disallowed, also proves that
// go test's diagnostics, summaries, and user output did not leak onto stdout.
func decodePoints(t *testing.T, output string) []point {
	t.Helper()
	var points []point
	seen := map[point]bool{}
	scanner := bufio.NewScanner(strings.NewReader(output))
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		if strings.TrimSpace(text) == "" {
			t.Fatalf("blank stdout line %d, expected JSONL: %q", line, output)
		}
		dec := json.NewDecoder(strings.NewReader(text))
		dec.DisallowUnknownFields()
		var p point
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("stdout line %d is not a coverage point: %v\n%s", line, err, text)
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			t.Fatalf("stdout line %d has trailing content: %q", line, text)
		}
		if p.Kind != "statement" && p.Kind != "branch" {
			t.Fatalf("unexpected point kind %q: %+v", p.Kind, p)
		}
		if p.Kind == "branch" && (p.Construct == "" || p.Outcome == "") {
			t.Fatalf("branch lacks construct/outcome: %+v", p)
		}
		if !filepath.IsAbs(p.File) {
			t.Fatalf("coverage filename is not an original absolute path: %+v", p)
		}
		src, err := os.ReadFile(p.File)
		if err != nil {
			t.Fatalf("coverage filename is not an existing source: %+v: %v", p, err)
		}
		lines := bytes.Split(src, []byte("\n"))
		for _, pos := range []position{p.Range.Start, p.Range.End} {
			if pos.Line < 1 || pos.Line > len(lines) ||
				pos.Column < 1 || pos.Column > len(lines[pos.Line-1])+1 {
				t.Fatalf("invalid byte position %+v in %+v", pos, p)
			}
		}
		start, end := p.Range.Start, p.Range.End
		if end.Line < start.Line || (end.Line == start.Line && end.Column <= start.Column) {
			t.Fatalf("range is not nonempty and exclusive-ended: %+v", p)
		}
		if seen[p] {
			t.Fatalf("duplicate uncovered point: %+v", p)
		}
		seen[p] = true
		points = append(points, p)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return points
}

func sourceLine(t *testing.T, src, uniqueText string) int {
	t.Helper()
	if strings.Count(src, uniqueText) != 1 {
		t.Fatalf("source marker %q is not unique", uniqueText)
	}
	return 1 + strings.Count(src[:strings.Index(src, uniqueText)], "\n")
}

// want reports whether this obligation should be in the *uncovered* output.
// Never assert exact statement totals: those may evolve independently of the
// observable branch contract.
func wantBranch(t *testing.T, points []point, file, src, marker, construct, outcome string, want bool) {
	t.Helper()
	line := sourceLine(t, src, marker)
	var found []point
	for _, p := range points {
		if p.File == file && p.Kind == "branch" && p.Construct == construct &&
			p.Outcome == outcome && p.Range.Start.Line == line {
			found = append(found, p)
		}
	}
	if (len(found) != 0) != want || len(found) > 1 {
		t.Errorf("%s:%d %s/%s: found %d uncovered points, want present=%v\npoints: %+v",
			file, line, construct, outcome, len(found), want, points)
	}
}

func TestModuleIdentityAndOriginalFiles(t *testing.T) {
	const src = `package identity

import (
	_ "embed"
	"example.com/identity/internal/secret"
	"example.com/sibling"
)

//go:embed assets/message.txt
var Message string

type Token struct { Value int }

func Value(token Token) int {
	if token.Value < 0 {
		return -1
	}
	return token.Value + secret.Value() + sibling.Value()
}
`
	f := newFixture(t, map[string]string{
		"main/go.mod":      "module example.com/identity\n\ngo 1.24.0\n\nrequire example.com/sibling v0.0.0\n\nreplace example.com/sibling => ../sibling\n",
		"main/identity.go": src,
		"main/internal/secret/secret.go": `package secret
func Value() int { return 20 }
`,
		"main/assets/message.txt": "embedded message\n",
		"main/testdata/input.txt": "testdata message\n",
		"main/identity_test.go": `package identity_test

import (
	"fmt"
	"os"
	"testing"
	"example.com/identity"
)

func TestOriginalIdentity(t *testing.T) {
	fmt.Println("user output must not be JSONL")
	if got := identity.Value(identity.Token{Value: 1}); got != 42 {
		t.Fatalf("Value = %d", got)
	}
	if identity.Message != "embedded message\n" { t.Fatal("lost embedded asset") }
	data, err := os.ReadFile("testdata/input.txt")
	if err != nil || string(data) != "testdata message\n" {
		t.Fatalf("lost package working directory/testdata: %q, %v", data, err)
	}
}
`,
		"sibling/go.mod": "module example.com/sibling\n\ngo 1.24.0\n",
		"sibling/sibling.go": `package sibling
func Value() int { return 21 }
`,
	}, "main")
	points := f.success(t)
	wantBranch(t, points, f.path("main/identity.go"), src, "if token.Value < 0", "if", "true", true)
	wantBranch(t, points, f.path("main/identity.go"), src, "if token.Value < 0", "if", "false", false)
	for _, p := range points {
		if p.File != f.path("main/identity.go") {
			t.Errorf("default pattern instrumented a dependency or test: %+v", p)
		}
	}
}

func TestExistingTestMain(t *testing.T) {
	for _, tc := range []struct {
		name, imports, body string
	}{
		{"return", "", "m.Run()"},
		{"os.Exit", `import "os"`, "os.Exit(m.Run())"},
		{"deferred os.Exit", `import "os"`, "defer os.Exit(0); m.Run()"},
		{"aliased Exit", `import process "os"`, "process.Exit(m.Run())"},
		{"dot Exit", `import . "os"`, "Exit(m.Run())"},
		{
			"shadowed os", `import "os"; var _ = os.Args`,
			`os := struct{ Exit func(string) }{func(s string) { if s != "shadow" { panic(s) } }}; os.Exit("shadow"); m.Run()`,
		},
		{
			"shadowed alias", `import process "os"; var _ = process.Args`,
			`process := struct{ Exit func(string) }{func(s string) { if s != "shadow" { panic(s) } }}; process.Exit("shadow"); m.Run()`,
		},
		{
			"shadowed dot Exit", `import . "os"; var _ = Args`,
			`Exit := func(s string) { if s != "shadow" { panic(s) } }; Exit("shadow"); m.Run()`,
		},
	} {
		for _, external := range []bool{false, true} {
			name := tc.name
			if external {
				name += "/external"
			}
			t.Run(name, func(t *testing.T) {
				const src = `package fixture
func Value(yes bool) int {
	if yes {
		return 1
	}
	return 0
}
`
				packageName, importFixture, value := "fixture", "", "Value"
				if external {
					packageName = "fixture_test"
					importFixture = `import "example.com/fixture"`
					value = "fixture.Value"
				}
				f := newFixture(t, map[string]string{
					"go.mod":   fixtureMod,
					"value.go": src,
					"value_test.go": fmt.Sprintf(`package %s
import "testing"
%s
%s
func TestMain(m *testing.M) { %s }
func TestValue(t *testing.T) {
	if %s(true) != 1 { t.Fatal("wrong value") }
}
`, packageName, importFixture, tc.imports, tc.body, value),
				}, ".")
				points := f.success(t)
				wantBranch(t, points, f.path("value.go"), src, "if yes", "if", "true", false)
				wantBranch(t, points, f.path("value.go"), src, "if yes", "if", "false", true)
			})
		}
	}
}

func TestForLoopOutcomesAndControlFlow(t *testing.T) {
	const src = `package fixture

func EnteredOnly(n int) int {
	total := 0
	for i := 0; i < n; i++ { // entered
		total++
	}
	return total
}
func SkippedOnly(n int) int {
	total := 0
	for i := 0; i < n; i++ { // skipped
		total++
	}
	return total
}
func Both(n int) {
	for i := 0; i < n; i++ { // both
	}
}
func NeverCalled(n int) {
	for i := 0; i < n; i++ { // never
	}
}
func EmptyBody(n int) {
	for i := 0; i < n; i++ {} // empty
}
func ReturnFromLoop(n int) int {
	for i := 0; i < n; i++ { // return
		return i + 1
	}
	return 0
}
func BreakLoop(n int) {
	for i := 0; i < n; i++ { // break
		break
	}
}
func ContinueLoop(n int) {
	for i := 0; i < n; i++ { // continue
		continue
	}
}
func Labeled(n int) int {
	total := 0
Outer:
	for i := 0; i < n; i++ { // outer
		for j := 0; j < 3; j++ { // inner
			if j == 0 { continue }
			if i == 0 { continue Outer }
			total++
			break Outer
		}
	}
	return total
}
func Infinite() int {
	for { // infinite
		return 1
	}
}
func InfiniteNeverCalled() {
	for {} // infinite-never
}
func panicValue() int { panic("initial expression") }
func PanicInit() {
	for i := panicValue(); i < 1; i++ { // panic-init
	}
}
func panicCondition() bool { panic("initial condition") }
func PanicCondition() {
	for panicCondition() { // panic-condition
	}
}
`
	f := newFixture(t, map[string]string{
		"go.mod":   fixtureMod,
		"loops.go": src,
		"loops_test.go": `package fixture
import "testing"
func TestLoops(t *testing.T) {
	if EnteredOnly(2) != 2 || SkippedOnly(0) != 0 { t.Fatal("counted loop changed") }
	Both(0)
	Both(2)
	EmptyBody(2)
	if ReturnFromLoop(2) != 1 { t.Fatal("return changed") }
	BreakLoop(2)
	ContinueLoop(2)
	if Labeled(2) != 1 { t.Fatal("labeled control flow changed") }
	if Infinite() != 1 { t.Fatal("infinite return changed") }
	for _, fn := range []func(){PanicInit, PanicCondition} {
		func() {
			defer func() {
				if recover() == nil { t.Error("initial panic was lost") }
			}()
			fn()
		}()
	}
}
`,
	}, ".")
	points := f.success(t)
	for _, tc := range []struct {
		marker           string
		entered, skipped bool
	}{
		{"// entered", false, true},
		{"// skipped", true, false},
		{"// both", false, false},
		{"// never", true, true},
		{"// empty", false, true},
		{"// return", false, true},
		{"// break", false, true},
		{"// continue", false, true},
		{"// outer", false, true},
		{"// inner", false, true},
		{"// panic-init", true, true},
		{"// panic-condition", true, true},
	} {
		wantBranch(t, points, f.path("loops.go"), src, tc.marker, "for", "entered", tc.entered)
		wantBranch(t, points, f.path("loops.go"), src, tc.marker, "for", "skipped", tc.skipped)
	}
	// An infinite loop has no structurally possible skipped obligation.
	wantBranch(t, points, f.path("loops.go"), src, "for { // infinite\n", "for", "entered", false)
	wantBranch(t, points, f.path("loops.go"), src, "for { // infinite\n", "for", "skipped", false)
	wantBranch(t, points, f.path("loops.go"), src, "// infinite-never", "for", "entered", true)
	wantBranch(t, points, f.path("loops.go"), src, "// infinite-never", "for", "skipped", false)
}

func TestRangeLoops(t *testing.T) {
	const src = `package fixture

func Slice(xs []int) int {
	total := 0
	for _, x := range xs { // slice
		total += x
	}
	return total
}
func Map(xs map[string]int) int {
	total := 0
	for _, x := range xs { // map
		total += x
	}
	return total
}
func String(s string) int {
	total := 0
	for range s { // string
		total++
	}
	return total
}
func Integer(n int) int {
	total := 0
	for range n { // integer
		total++
	}
	return total
}
func Channel(ch <-chan int) int {
	total := 0
	for x := range ch { // channel
		total += x
	}
	return total
}
func Iterator() int {
	total := 0
	seq := func(yield func(int) bool) { yield(7) }
	for x := range seq { // iterator
		total += x
	}
	return total
}
func EmptyRange(xs []int) {
	for range xs {} // empty-range
}
func RangeBoth(xs []int) {
	for range xs {} // range-both
}
func RangeReturn(xs []int) int {
	for _, x := range xs { // range-return
		return x
	}
	return 0
}
func RangeLabeled(xs []int) int {
	total := 0
Outer:
	for _, x := range xs { // range-outer
		for j := range 3 { // range-inner
			if j == 0 { continue }
			if x == 0 { continue Outer }
			total++
			break Outer
		}
	}
	return total
}
func panicSlice() []int { panic("range expression") }
func PanicRange() {
	for range panicSlice() { // range-panic
	}
}
func NilArrayPointer() int {
	var xs *[2]int
	total := 0
	for range xs { // range-nil-array
		total++
	}
	return total
}
`
	f := newFixture(t, map[string]string{
		"go.mod":   fixtureMod,
		"range.go": src,
		"range_test.go": `package fixture
import "testing"
func TestRanges(t *testing.T) {
	if Slice([]int{2, 3}) != 5 { t.Fatal("slice changed") }
	if Map(nil) != 0 { t.Fatal("map changed") }
	if String("éx") != 2 { t.Fatal("string changed") }
	if Integer(0) != 0 { t.Fatal("integer changed") }
	ch := make(chan int, 1)
	ch <- 4
	close(ch)
	if Channel(ch) != 4 { t.Fatal("channel changed") }
	if Iterator() != 7 { t.Fatal("iterator changed") }
	EmptyRange([]int{1})
	RangeBoth(nil)
	RangeBoth([]int{1})
	if RangeReturn([]int{9}) != 9 { t.Fatal("range return changed") }
	if RangeLabeled([]int{0, 1}) != 1 { t.Fatal("range labels changed") }
	if NilArrayPointer() != 2 { t.Fatal("nil array pointer range changed") }
	func() {
		defer func() {
			if recover() == nil { t.Error("range expression panic was lost") }
		}()
		PanicRange()
	}()
}
`,
	}, ".")
	points := f.success(t)
	for _, tc := range []struct {
		marker           string
		entered, skipped bool
	}{
		{"// slice", false, true},
		{"// map", true, false},
		{"// string", false, true},
		{"// integer", true, false},
		{"// channel", false, true},
		{"// iterator", false, true},
		{"// empty-range", false, true},
		{"// range-both", false, false},
		{"// range-return", false, true},
		{"// range-outer", false, true},
		{"// range-inner", false, true},
		{"// range-panic", true, true},
		{"// range-nil-array", false, true},
	} {
		wantBranch(t, points, f.path("range.go"), src, tc.marker, "for", "entered", tc.entered)
		wantBranch(t, points, f.path("range.go"), src, tc.marker, "for", "skipped", tc.skipped)
	}
}

func TestJSONSourcePositions(t *testing.T) {
	// The non-ASCII identifier makes byte columns distinguishable from rune
	// columns; the tab must count as one byte, not as an editor's tab width.
	const src = `package fixture
func Positive(é int) bool {
	if é > 0 {
		return true
	}
	return false
}
`
	f := newFixture(t, map[string]string{
		"go.mod":      fixtureMod,
		"position.go": src,
		"position_test.go": `package fixture
import "testing"
func TestPosition(t *testing.T) {
	if !Positive(1) { t.Fatal("changed result") }
}
`,
	}, ".")
	points := f.success(t)
	wantBranch(t, points, f.path("position.go"), src, "if é > 0", "if", "false", true)
	wantBranch(t, points, f.path("position.go"), src, "if é > 0", "if", "true", false)
	line := sourceLine(t, src, "if é > 0")
	coveredReturn := sourceLine(t, src, "return true")
	uncoveredReturn := sourceLine(t, src, "return false")
	foundUncoveredReturn := false
	for _, p := range points {
		if p.Kind == "branch" && p.Construct == "if" && p.Range.Start.Line == line {
			want := sourceRange{Start: position{Line: line, Column: 5}, End: position{Line: line, Column: 11}}
			if p.Range != want {
				t.Errorf("condition range = %+v, want byte-column range %+v", p.Range, want)
			}
		}
		if p.File != f.path("position.go") {
			t.Errorf("point names generated or test source: %+v", p)
		}
		if p.Kind == "statement" {
			if p.Range.Start.Line == coveredReturn {
				t.Errorf("covered return reported as uncovered: %+v", p)
			}
			if p.Range.Start.Line == uncoveredReturn {
				foundUncoveredReturn = true
			}
		}
	}
	if !foundUncoveredReturn {
		t.Error("uncovered return statement missing from report")
	}
}

func TestFullyCoveredOutputAndDefaultCount(t *testing.T) {
	f := newFixture(t, map[string]string{
		"go.mod":   fixtureMod,
		"value.go": "package fixture\nfunc Value() int { return 42 }\n",
		"value_test.go": `package fixture
import (
	"os"
	"testing"
)
func TestValue(t *testing.T) {
	if Value() != 42 { t.Fatal("wrong result") }
	f, err := os.OpenFile(os.Getenv("COVER_FIXTURE_COUNT_FILE"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil { t.Fatal(err) }
	if _, err := f.WriteString("run\n"); err != nil { t.Fatal(err) }
	if err := f.Close(); err != nil { t.Fatal(err) }
}
`,
	}, ".")
	countFile := filepath.Join(t.TempDir(), "runs")
	t.Setenv("COVER_FIXTURE_COUNT_FILE", countFile)
	for i := 0; i < 2; i++ {
		if points := f.success(t); len(points) != 0 {
			t.Errorf("fully covered program produced uncovered points: %+v", points)
		}
	}
	data, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "run\nrun\n" {
		t.Errorf("default -count must run tests once per invocation, without cached results: %q", data)
	}
}

func TestFlagsOutputAndRecursivePattern(t *testing.T) {
	const rootSource = `package fixture
func Root(yes bool) int {
	if yes { return 1 }
	return 0
}
`
	const childSource = `package child
func Child(yes bool) int {
	if yes { return 1 }
	return 0
}
`
	const tests = `package %s
import (
	"os"
	"path/filepath"
	"testing"
)
func TestSelected(t *testing.T) {
	if !SelectedTag { t.Fatal("build tag not applied") }
	if %s(%s) != %s { t.Fatal("wrong result") }
	f, err := os.OpenFile(filepath.Join(os.Getenv("COVER_FIXTURE_COUNTS"), "%s"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil { t.Fatal(err) }
	if _, err := f.WriteString("run\n"); err != nil { t.Fatal(err) }
	if err := f.Close(); err != nil { t.Fatal(err) }
}
func TestNotSelected(t *testing.T) { t.Fatal("-run was not passed through") }
`
	f := newFixture(t, map[string]string{
		"go.mod":              fixtureMod,
		"root.go":             rootSource,
		"tag.go":              "//go:build coverfixture\n\npackage fixture\nconst SelectedTag = true\n",
		"root_test.go":        fmt.Sprintf(tests, "fixture", "Root", "true", "1", "root"),
		"child/child.go":      childSource,
		"child/tag.go":        "//go:build coverfixture\n\npackage child\nconst SelectedTag = true\n",
		"child/child_test.go": fmt.Sprintf(tests, "child", "Child", "false", "0", "child"),
	}, ".")
	countDir := t.TempDir()
	t.Setenv("COVER_FIXTURE_COUNTS", countDir)
	outputFile := filepath.Join(t.TempDir(), "uncovered.jsonl")
	out, diagnostic, err := f.run(t, "-run", "^TestSelected$", "-count", "2",
		"-timeout", "30s", "-tags", "coverfixture", "-o", outputFile, "./...")
	if err != nil {
		t.Fatalf("Run with flags: %v\n%s\n%s", err, diagnostic, out)
	}
	if out != "" {
		t.Errorf("-o must leave stdout empty, got %q", out)
	}
	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatal(err)
	}
	points := decodePoints(t, string(data))
	wantBranch(t, points, f.path("root.go"), rootSource, "if yes", "if", "false", true)
	wantBranch(t, points, f.path("root.go"), rootSource, "if yes", "if", "true", false)
	wantBranch(t, points, f.path("child/child.go"), childSource, "if yes", "if", "true", true)
	wantBranch(t, points, f.path("child/child.go"), childSource, "if yes", "if", "false", false)
	for _, pkg := range []string{"root", "child"} {
		data, err := os.ReadFile(filepath.Join(countDir, pkg))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "run\nrun\n" {
			t.Errorf("-count=2 for %s: got %q", pkg, data)
		}
	}
}

func TestMultiplePackagePatterns(t *testing.T) {
	const src = `package %s
func Value(yes bool) int {
	if yes { return 1 }
	return 0
}
`
	files := map[string]string{
		"go.mod":               fixtureMod,
		"excluded/excluded.go": "package excluded\n",
		"excluded/excluded_test.go": `package excluded
import "testing"
func TestMustNotRun(t *testing.T) { t.Fatal("unselected package ran") }
`,
	}
	for _, name := range []string{"alpha", "beta"} {
		files[name+"/value.go"] = fmt.Sprintf(src, name)
		files[name+"/value_test.go"] = fmt.Sprintf(`package %s
import "testing"
func TestValue(t *testing.T) { if Value(true) != 1 { t.Fatal("wrong result") } }
`, name)
	}
	f := newFixture(t, files, ".")
	points := f.success(t, "./alpha", "./beta")
	for _, name := range []string{"alpha", "beta"} {
		source := files[name+"/value.go"]
		wantBranch(t, points, f.path(name+"/value.go"), source, "if yes", "if", "false", true)
	}
	for _, p := range points {
		if p.File != f.path("alpha/value.go") && p.File != f.path("beta/value.go") {
			t.Errorf("unselected package in output: %+v", p)
		}
	}
}

func TestTestOnlyPackageExercisesSelectedLibrary(t *testing.T) {
	// The library has no tests of its own. Dropping the test-only package from
	// go test's targets would silently leave every library point uncovered.
	f := newFixture(t, map[string]string{
		"go.mod": fixtureMod,
		"lib/value.go": `package lib
func Value(yes bool) int {
	if yes {
		return 1
	}
	return 0
}
`,
		"integration/value_test.go": `package integration_test
import (
	"os"
	"testing"
	"example.com/fixture/lib"
)
func TestLibrary(t *testing.T) {
	if lib.Value(true) != 1 || lib.Value(false) != 0 {
		t.Fatal("library results changed")
	}
	if err := os.WriteFile(os.Getenv("COVER_FIXTURE_TEST_ONLY_MARKER"), []byte("ran\n"), 0600); err != nil {
		t.Fatal(err)
	}
}
`,
	}, ".")
	marker := filepath.Join(t.TempDir(), "test-only-package-ran")
	t.Setenv("COVER_FIXTURE_TEST_ONLY_MARKER", marker)
	if points := f.success(t, "./..."); len(points) != 0 {
		t.Errorf("test-only package should cover all library points, got %+v", points)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("test-only package did not run: %v", err)
	}
	if string(data) != "ran\n" {
		t.Errorf("test-only package marker = %q, want ran", data)
	}
}

func TestFailedTestsAndTimeoutReturnErrors(t *testing.T) {
	const src = `package fixture
func Value(yes bool) int {
	if yes { return 1 }
	return 0
}
`
	for _, tc := range []struct {
		name, body string
		args       []string
	}{
		{"failure", `fmt.Println("FAILURE OUTPUT MARKER"); t.Fatal("intentional fixture failure")`, nil},
		{"timeout", `time.Sleep(5 * time.Second)`, []string{"-timeout", "100ms"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, map[string]string{
				"go.mod":   fixtureMod,
				"value.go": src,
				"value_test.go": fmt.Sprintf(`package fixture
import (
	"fmt"
	"testing"
	"time"
)
var _ = fmt.Println
var _ = time.Second
func TestFailure(t *testing.T) { Value(true); %s }
`, tc.body),
			}, ".")
			out, diagnostic, err := f.run(t, tc.args...)
			if err == nil {
				t.Fatalf("Run succeeded for %s\nstderr: %s", tc.name, diagnostic)
			}
			points := decodePoints(t, out)
			if tc.name == "failure" {
				// t.Fatal still lets m.Run and TestMain complete normally.
				wantBranch(t, points, f.path("value.go"), src, "if yes", "if", "true", false)
				wantBranch(t, points, f.path("value.go"), src, "if yes", "if", "false", true)
			}
			// A timeout may bypass TestMain's save. Only require a valid
			// partial report, not recovery of the interrupted test's hits.
			if !strings.Contains(diagnostic, "FAIL") {
				t.Errorf("missing go test failure diagnostics on stderr:\n%s", diagnostic)
			}
			if tc.name == "failure" && !strings.Contains(diagnostic, "FAILURE OUTPUT MARKER") {
				t.Errorf("test stdout was not routed to stderr:\n%s", diagnostic)
			}
			if tc.name == "timeout" && !strings.Contains(diagnostic, "timed out") {
				t.Errorf("-timeout was not forwarded:\n%s", diagnostic)
			}
		})
	}
}

func TestUnsupportedGoTestFlags(t *testing.T) {
	f := newFixture(t, map[string]string{
		"go.mod":   fixtureMod,
		"value.go": "package fixture\nfunc Value() int { return 1 }\n",
	}, ".")
	for _, flag := range []string{"-v", "-bench", "-not-a-cover-flag"} {
		t.Run(flag, func(t *testing.T) {
			out, _, err := f.run(t, flag)
			if err == nil {
				t.Errorf("unsupported runner flag %s was accepted", flag)
			}
			decodePoints(t, out)
		})
	}
}

func TestConcurrentCounters(t *testing.T) {
	// Besides exercising shared atomics, calling both outcomes from many
	// goroutines catches counters accidentally reset by individual calls.
	const src = `package fixture
import "sync"
func Exercise() {
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ { // concurrent-loop
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ { // worker-loop
				if (i+j)%2 == 0 {
					continue
				}
			}
		}(i)
	}
	wg.Wait()
}
`
	f := newFixture(t, map[string]string{
		"go.mod":        fixtureMod,
		"concurrent.go": src,
		"concurrent_test.go": `package fixture
import "testing"
func TestConcurrent(t *testing.T) { Exercise() }
`,
	}, ".")
	for _, mode := range []string{"normal", "race"} {
		t.Run(mode, func(t *testing.T) {
			var args []string
			if mode == "race" {
				if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
					t.Skip("race integration test requires amd64 or arm64")
				}
				cgo, err := exec.CommandContext(t.Context(), "go", "env", "CGO_ENABLED").Output()
				if err != nil {
					t.Fatal(err)
				}
				if strings.TrimSpace(string(cgo)) != "1" {
					t.Skip("race integration test requires cgo")
				}
				args = []string{"-race"}
			}
			points := f.success(t, args...)
			for _, marker := range []string{"// concurrent-loop", "// worker-loop"} {
				wantBranch(t, points, f.path("concurrent.go"), src, marker, "for", "entered", false)
				wantBranch(t, points, f.path("concurrent.go"), src, marker, "for", "skipped", true)
			}
			for _, outcome := range []string{"true", "false"} {
				wantBranch(t, points, f.path("concurrent.go"), src, "if (i+j)%2 == 0", "if", outcome, false)
			}
		})
	}
}
