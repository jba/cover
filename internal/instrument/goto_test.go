package instrument

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGotoCounterUpdates(t *testing.T) {
	// Assertions below are on branch counts, not just uncovered points: a goto
	// resets its target state, and each encounter records exactly one outcome.
	// Native statement-probe placement is tested against Go separately.
	src := []byte(`package sample
func Loop() {
 n := 1
 goto loop
loop:
 for i := 0; i < n; i++ { n = 0 }
 if n == 0 { n = -1; goto loop }
}
func Range() {
 n := 1
 goto loop
loop:
 for range n { n = 0 }
 if n == 0 { n = -1; goto loop }
}
func Switch() {
 n := 1
 goto choose
choose:
 switch n { case 1: n = 0 }
 if n == 0 { n = -1; goto choose }
}
func TypeSwitch() {
 var x any = 1
 goto choose
choose:
 switch x.(type) { case int: x = nil }
 if x == nil { x = "done"; goto choose }
}
func LabeledGoto() {
 n := 0
 goto jump
jump:
 goto target
target:
 n++
 if n < 2 {goto jump}
}
func Nested() {
 n := 1
loop:
 for range n { n = 0 }
 if n == 0 { n = -1; goto loop }
 f := func() {
  n := 1
  goto loop
 loop:
  for range n { n = 0 }
  if n == 0 { n = -1; goto loop }
 }
 f()
}
func Shadow(bool, true, false int) {
 for range 1 {}
 switch bool {case 1:}
 _ = true + false
}
func Terminating() int {
 goto spin
spin:
 for {return 7}
}
`)
	type want struct {
		fn, kind, construct, outcome, text string
		count                              int
	}
	wants := []want{
		{"Loop", "branch", "for", "entered", "for i := 0; i < n; i++", 1},
		{"Loop", "branch", "for", "skipped", "for i := 0; i < n; i++", 1},
		{"Range", "branch", "for", "entered", "for range n", 1},
		{"Range", "branch", "for", "skipped", "for range n", 1},
		{"Switch", "branch", "switch", "case", "case 1:", 1},
		{"Switch", "branch", "switch", "no-match", "switch n", 1},
		{"TypeSwitch", "branch", "type-switch", "case", "case int:", 1},
		{"TypeSwitch", "branch", "type-switch", "no-match", "switch x.(type)", 1},
		{"Nested", "branch", "for", "entered", "for range n", 1},
		{"Nested", "branch", "for", "skipped", "for range n", 1},
	}
	next := 0
	out, ps, err := Instrument("sample_test.go", src, "__gotoCover", &next)
	if err != nil {
		t.Fatal(err)
	}
	var harness bytes.Buffer
	fmt.Fprintf(&harness, `package sample
import (
 "sync/atomic"
 "testing"
)
var counts [%d]atomic.Uint64
func __gotoCoverHit(i int){counts[i].Add(1)}
func __gotoCoverBool[T ~bool](a,b int,v T)T{if v {__gotoCoverHit(a)}else{__gotoCoverHit(b)};return v}
func TestCounts(t *testing.T){
`, next)
	lines := strings.Split(string(src), "\n")
	for _, fn := range []string{"Loop", "Range", "Switch", "TypeSwitch", "LabeledGoto", "Nested"} {
		fmt.Fprintf(&harness, "t.Run(%q,func(t *testing.T){for i:=range counts{counts[i].Store(0)};%s();\n", fn, fn)
		start := strings.Index(string(src), "func "+fn+"(")
		end := len(src)
		if j := strings.Index(string(src[start+1:]), "\nfunc "); j >= 0 {
			end = start + 1 + j
		}
		for _, w := range wants {
			if w.fn != fn {
				continue
			}
			matched := 0
			for _, p := range ps {
				if p.Kind != w.kind || p.Construct != w.construct || p.Outcome != w.outcome {
					continue
				}
				offset := 0
				for _, line := range lines[:p.Range.Start.Line-1] {
					offset += len(line) + 1
				}
				offset += p.Range.Start.Column - 1
				if offset < start || offset >= end || !strings.HasPrefix(string(src[offset:]), w.text) {
					continue
				}
				matched++
				fmt.Fprintf(&harness, "if got:=counts[%d].Load();got!=%d{t.Errorf(%q,got)}\n", p.Counter, w.count, fmt.Sprintf("%s %s %s %q at line %d: got %%d, want %d", w.kind, w.construct, w.outcome, w.text, p.Range.Start.Line, w.count))
			}
			if matched == 0 {
				t.Fatalf("no point for %+v", w)
			}
		}
		harness.WriteString("})\n")
	}
	harness.WriteString("Shadow(1,2,3);if Terminating()!=7{t.Fatal(\"termination changed\")}\n}\n")
	dir := t.TempDir()
	for name, data := range map[string][]byte{"go.mod": []byte("module example.com/goto-counts\n\ngo 1.24.0\n"), "sample_test.go": out, "runtime_test.go": harness.Bytes()} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "test", "-count=1", ".")
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s\nInstrumented:\n%s\nHarness:\n%s", err, output, out, harness.Bytes())
	}
}
