package runner_test

import (
	"strings"
	"testing"
)

// Goto tests must not run in parallel: the fixture helpers change the working
// directory. success checks the text report and snapshots every original fixture file.

func wantGotoStatement(t *testing.T, points []point, file, src, marker string, want bool) {
	t.Helper()
	line := sourceLine(t, src, marker)
	var found []point
	for _, p := range points {
		if p.File == file && p.Kind == "statement" && p.Range.Start.Line == line {
			found = append(found, p)
		}
	}
	if (len(found) != 0) != want || len(found) > 1 {
		t.Errorf("%s:%d statement: found %d uncovered points, want present=%v\npoints: %+v",
			file, line, len(found), want, points)
	}
}

func TestGotoLoopReentry(t *testing.T) {
	const srcTemplate = `package fixture

func Forward() int {
	total, n := 0, 2
	goto Loop
	total += 100 // skipped-prelude
Loop:
	LOOP { // forward-loop
		total++
	}
	return total
}
func ZeroOnly() int {
	total, n := 0, 0
	goto Loop
Loop:
	LOOP { // zero-loop
		total++ // zero-body
	}
	return total
}
func EnteredThenZero() int {
	total, n, passes := 0, 2, 0
Loop:
	LOOP { // entered-then-zero
		total++
	}
	passes++
	if passes == 1 {
		n = 0
		goto Loop
	}
	return total*10 + passes
}
func ZeroThenEntered() int {
	total, n, passes := 0, 0, 0
Loop:
	LOOP { // zero-then-entered
		total++
	}
	passes++
	if passes == 1 {
		n = 2
		goto Loop
	}
	return total*10 + passes
}
func Infinite() int {
	goto Loop
Loop:
	for { // infinite-target
		return 7
	}
}
`
	const tests = `package fixture
import "testing"
func TestLoops(t *testing.T) {
	if got := Forward(); got != 2 { t.Fatalf("Forward() = %d, want 2", got) }
	if got := ZeroOnly(); got != 0 { t.Fatalf("ZeroOnly() = %d, want 0", got) }
	if got := EnteredThenZero(); got != 22 { t.Fatalf("EnteredThenZero() = %d, want 22", got) }
	if got := ZeroThenEntered(); got != 22 { t.Fatalf("ZeroThenEntered() = %d, want 22", got) }
	if got := Infinite(); got != 7 { t.Fatalf("Infinite() = %d, want 7", got) }
}
`
	for _, tc := range []struct{ name, loop string }{
		{"for", "for i := 0; i < n; i++"},
		{"range", "for range n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.ReplaceAll(srcTemplate, "LOOP", tc.loop)
			f := newFixture(t, map[string]string{
				"go.mod": fixtureMod, "goto.go": src, "goto_test.go": tests,
			}, ".")
			points := f.success(t, "-timeout", "15s")
			file := f.path("goto.go")
			for _, check := range []struct {
				marker  string
				skipped bool
			}{
				{"// forward-loop", true},
				{"// zero-loop", false},
				{"// entered-then-zero", false},
				{"// zero-then-entered", false},
			} {
				wantBranch(t, points, file, src, check.marker, "for", "skipped", check.skipped)
				wantGotoStatement(t, points, file, src, check.marker, false)
			}
			// Resetting on reentry matters especially when an earlier entry set
			// the flag true and the subsequent entry has no body iterations.
			wantBranch(t, points, file, src, "// infinite-target", "for", "skipped", false)
			wantGotoStatement(t, points, file, src, "// infinite-target", false)
			wantGotoStatement(t, points, file, src, "// skipped-prelude", true)
			wantGotoStatement(t, points, file, src, "// zero-body", true)
		})
	}
}

func TestGotoLabelsAndLoopControl(t *testing.T) {
	const src = `package fixture

func AdjacentLabels() int {
	total, passes, n := 0, 0, 1
	goto First
First:
Second:
	for i := 0; i < n; i++ { // adjacent-loop
		total++
	}
	passes++
	if passes == 1 {
		n = 0
		goto Second
	}
	return total*10 + passes
}
func LabelledControl() int {
	total, passes := 0, 0
	goto Outer
Outer:
	for i := 0; i < 3; i++ { // control-outer
		for j := 0; j < 2; j++ { // control-inner
			if i == 0 {
				continue Outer
			}
			total++
			break Outer
		}
	}
	passes++
	if passes == 1 {
		goto Outer
	}
	return total*10 + passes
}
func RangeLabelledControl() int {
	total, passes := 0, 0
	goto Outer
Outer:
	for i := range 3 { // range-control-outer
		for range 2 { // range-control-inner
			if i == 0 {
				continue Outer
			}
			total++
			break Outer
		}
	}
	passes++
	if passes == 1 {
		goto Outer
	}
	return total*10 + passes
}
func LeaveLoop() int {
	goto Loop
Loop:
	for i := 0; i < 2; i++ { // leave-loop
		goto Done
	}
Done:
	return 7 // leave-loop-return
}
func JumpOverConstructs() int {
	total := 0
	goto After
	for i := 0; i < 1; i++ { // jumped-for
		total += 10
	}
	for range 1 { // jumped-range
		total += 100
	}
	switch 1 { // jumped-switch
	case 1: // jumped-case
		total += 1000 // jumped-case-body
	}
	switch any(1).(type) { // jumped-type-switch
	case int: // jumped-type-case
		total += 10000 // jumped-type-case-body
	}
After:
	return total // jumped-return
}
`
	const tests = `package fixture
import "testing"
func TestLabels(t *testing.T) {
	if got := AdjacentLabels(); got != 12 { t.Fatalf("AdjacentLabels() = %d, want 12", got) }
	if got := LabelledControl(); got != 22 { t.Fatalf("LabelledControl() = %d, want 22", got) }
	if got := RangeLabelledControl(); got != 22 { t.Fatalf("RangeLabelledControl() = %d, want 22", got) }
	if got := LeaveLoop(); got != 7 { t.Fatalf("LeaveLoop() = %d, want 7", got) }
	if got := JumpOverConstructs(); got != 0 { t.Fatalf("JumpOverConstructs() = %d, want 0", got) }
}
`
	f := newFixture(t, map[string]string{
		"go.mod": fixtureMod, "goto.go": src, "goto_test.go": tests,
	}, ".")
	points := f.success(t, "-timeout", "15s")
	file := f.path("goto.go")
	wantBranch(t, points, file, src, "// adjacent-loop", "for", "skipped", false)
	wantGotoStatement(t, points, file, src, "// adjacent-loop", false)
	// Wrapping a labelled loop in a block would make these break/continue
	// targets invalid. A goto leaving a loop must not manufacture a skip.
	for _, marker := range []string{
		"// control-outer", "// control-inner", "// range-control-outer",
		"// range-control-inner", "// leave-loop\n",
	} {
		wantBranch(t, points, file, src, marker, "for", "skipped", true)
		wantGotoStatement(t, points, file, src, marker, false)
	}
	for _, marker := range []string{"// jumped-for", "// jumped-range"} {
		wantBranch(t, points, file, src, marker, "for", "skipped", true)
		wantGotoStatement(t, points, file, src, marker, true)
	}
	wantBranch(t, points, file, src, "// jumped-switch", "switch", "no-match", true)
	wantBranch(t, points, file, src, "// jumped-type-switch", "type-switch", "no-match", true)
	wantGotoStatement(t, points, file, src, "// jumped-switch", true)
	wantGotoStatement(t, points, file, src, "// jumped-type-switch", true)
	wantGotoStatement(t, points, file, src, "// jumped-case-body", true)
	wantGotoStatement(t, points, file, src, "// jumped-type-case-body", true)
	wantGotoStatement(t, points, file, src, "// jumped-return", false)
	wantGotoStatement(t, points, file, src, "// leave-loop-return", false)
}

func TestGotoSwitchReentry(t *testing.T) {
	const src = `package fixture

func SwitchReentry() int {
	value, total, passes := 1, 0, 0
	goto Dispatch
Dispatch:
	switch value { // switch-reentry
	case 1: // switch-matched
		total += 10 // switch-matched-body
		break Dispatch
	case 2: // switch-unmatched
		total += 100 // switch-unmatched-body
	}
	passes++
	if passes == 1 {
		value = 9
		goto Dispatch
	}
	return total + passes
}
func TypeSwitchReentry() int {
	var value any = 1
	total, passes := 0, 0
	goto Dispatch
Dispatch:
	switch v := value.(type) { // type-reentry
	case int: // type-matched
		total += v // type-matched-body
		break Dispatch
	case string: // type-unmatched
		total += len(v) // type-unmatched-body
	}
	passes++
	if passes == 1 {
		value = false
		goto Dispatch
	}
	return total*10 + passes
}
func NoMatchOnly() int {
	total := 0
	goto Dispatch
Dispatch:
	switch 9 { // switch-no-match
	case 1: // switch-never-matched
		total++ // switch-never-matched-body
	}
	return total
}
func TypeNoMatchOnly() int {
	total := 0
	goto Dispatch
Dispatch:
	switch any(false).(type) { // type-no-match
	case int: // type-never-matched
		total++ // type-never-matched-body
	}
	return total
}
func LeaveSwitch() int {
	goto Dispatch
Dispatch:
	switch 1 { // leave-switch
	case 1: // leave-switch-matched
		goto Done // leave-switch-matched-body
	case 2: // leave-switch-unmatched
		return -1 // leave-switch-unmatched-body
	}
Done:
	return 7
}
func LeaveTypeSwitch() int {
	goto Dispatch
Dispatch:
	switch any(1).(type) { // leave-type
	case int: // leave-type-matched
		goto Done // leave-type-matched-body
	case string: // leave-type-unmatched
		return -1 // leave-type-unmatched-body
	}
Done:
	return 8
}
func LabelledSwitchBreak() int {
	total, passes := 0, 0
	goto Dispatch
Dispatch:
	switch 1 { // break-switch
	case 1: // break-switch-matched
		total++ // break-switch-matched-body
		break Dispatch
	default: // break-switch-default
		total += 100 // break-switch-default-body
	}
	passes++
	if passes == 1 {
		goto Dispatch
	}
	return total*10 + passes
}
`
	const tests = `package fixture
import "testing"
func TestSwitches(t *testing.T) {
	if got := SwitchReentry(); got != 12 { t.Fatalf("SwitchReentry() = %d, want 12", got) }
	if got := TypeSwitchReentry(); got != 12 { t.Fatalf("TypeSwitchReentry() = %d, want 12", got) }
	if got := NoMatchOnly(); got != 0 { t.Fatalf("NoMatchOnly() = %d, want 0", got) }
	if got := TypeNoMatchOnly(); got != 0 { t.Fatalf("TypeNoMatchOnly() = %d, want 0", got) }
	if got := LeaveSwitch(); got != 7 { t.Fatalf("LeaveSwitch() = %d, want 7", got) }
	if got := LeaveTypeSwitch(); got != 8 { t.Fatalf("LeaveTypeSwitch() = %d, want 8", got) }
	if got := LabelledSwitchBreak(); got != 22 { t.Fatalf("LabelledSwitchBreak() = %d, want 22", got) }
}
`
	f := newFixture(t, map[string]string{
		"go.mod": fixtureMod, "goto.go": src, "goto_test.go": tests,
	}, ".")
	points := f.success(t, "-timeout", "15s")
	file := f.path("goto.go")
	for _, tc := range []struct {
		header, matchedBody, unmatchedBody, construct string
		noMatch                                       bool
	}{
		{"// switch-reentry", "// switch-matched-body", "// switch-unmatched-body", "switch", false},
		{"// type-reentry", "// type-matched-body", "// type-unmatched-body", "type-switch", false},
		{"// leave-switch\n", "// leave-switch-matched-body", "// leave-switch-unmatched-body", "switch", true},
		{"// leave-type\n", "// leave-type-matched-body", "// leave-type-unmatched-body", "type-switch", true},
	} {
		wantGotoStatement(t, points, file, src, tc.header, false)
		wantBranch(t, points, file, src, tc.header, tc.construct, "no-match", tc.noMatch)
		wantGotoStatement(t, points, file, src, tc.matchedBody, false)
		wantGotoStatement(t, points, file, src, tc.unmatchedBody, true)
	}
	for _, tc := range []struct{ header, unmatchedBody, construct string }{
		{"// switch-no-match", "// switch-never-matched-body", "switch"},
		{"// type-no-match", "// type-never-matched-body", "type-switch"},
	} {
		wantGotoStatement(t, points, file, src, tc.header, false)
		wantBranch(t, points, file, src, tc.header, tc.construct, "no-match", false)
		wantGotoStatement(t, points, file, src, tc.unmatchedBody, true)
	}
	wantGotoStatement(t, points, file, src, "// break-switch\n", false)
	wantBranch(t, points, file, src, "// break-switch\n", "switch", "no-match", false)
	wantGotoStatement(t, points, file, src, "// break-switch-matched-body", false)
	wantGotoStatement(t, points, file, src, "// break-switch-default-body", true)
}

func TestGotoOrdinaryStatementTargets(t *testing.T) {
	const src = `package fixture

func AssignmentTarget() int {
	value := 0
	goto Target
	value = 100 // assignment-skipped
Target:
	value++ // assignment-target
	return value
}
func IfTarget() int {
	value := 0
	goto Target
Target:
	if value == 0 { // if-target
		value = 2
	}
	return value
}
func SelectTarget() int {
	ch := make(chan int, 1)
	ch <- 3
	goto Target
Target:
	select { // select-target
	case value := <-ch: // select-matched
		return value // select-matched-body
	default: // select-unmatched
		return -1 // select-unmatched-body
	}
}
func GotoTarget() int {
	total, passes := 0, 0
	goto First
First:
Second:
	goto Loop // goto-target
Loop:
	for range 1 { // goto-chain-loop
		total++
	}
	passes++
	if passes == 1 {
		goto Second
	}
	return total*10 + passes
}
func BlockTarget() int {
	value := 0
	goto Target
Target:
	{
		value++ // block-target-body
	}
	return value
}
func EmptyTarget() int {
	value := 0
	goto Target
Target:
	;
	value++ // after-empty-target
	return value
}
func TerminalLabel(value *int) {
	*value++
	goto Done
	*value += 100 // terminal-skipped
Done:
}
func UntakenGoto() int {
	if false {
		goto Target // untaken-goto
	}
	return 4
Target:
	return 99 // untaken-target
}
`
	const tests = `package fixture
import "testing"
func TestTargets(t *testing.T) {
	if got := AssignmentTarget(); got != 1 { t.Fatalf("AssignmentTarget() = %d, want 1", got) }
	if got := IfTarget(); got != 2 { t.Fatalf("IfTarget() = %d, want 2", got) }
	if got := SelectTarget(); got != 3 { t.Fatalf("SelectTarget() = %d, want 3", got) }
	if got := GotoTarget(); got != 22 { t.Fatalf("GotoTarget() = %d, want 22", got) }
	if got := BlockTarget(); got != 1 { t.Fatalf("BlockTarget() = %d, want 1", got) }
	if got := EmptyTarget(); got != 1 { t.Fatalf("EmptyTarget() = %d, want 1", got) }
	value := 0
	TerminalLabel(&value)
	if value != 1 { t.Fatalf("TerminalLabel() left %d, want 1", value) }
	if got := UntakenGoto(); got != 4 { t.Fatalf("UntakenGoto() = %d, want 4", got) }
}
`
	f := newFixture(t, map[string]string{
		"go.mod": fixtureMod, "goto.go": src, "goto_test.go": tests,
	}, ".")
	points := f.success(t, "-timeout", "15s")
	file := f.path("goto.go")
	// Conditions and select cases can execute even if the statement's own hit
	// was placed before its label. Assert both kinds of coverage separately.
	for _, marker := range []string{
		"// assignment-target", "// if-target", "// select-target",
		"// goto-target", "// goto-chain-loop", "// block-target-body", "// after-empty-target",
	} {
		wantGotoStatement(t, points, file, src, marker, false)
	}
	for _, marker := range []string{
		"// assignment-skipped", "// terminal-skipped", "// untaken-goto", "// untaken-target",
	} {
		wantGotoStatement(t, points, file, src, marker, true)
	}
	wantBranch(t, points, file, src, "// if-target", "if", "false", true)
	wantGotoStatement(t, points, file, src, "// select-matched-body", false)
	wantGotoStatement(t, points, file, src, "// select-unmatched-body", true)
	wantBranch(t, points, file, src, "// goto-chain-loop", "for", "skipped", true)
}

func TestGotoFunctionLiteralLabelNamespaces(t *testing.T) {
	const src = `package fixture

func Namespaces() int {
	total, passes := 0, 0
	worker := func() int {
		subtotal, repeats, n := 0, 0, 1
		nested := func() int {
			value := 0
			goto Target
		Target:
			value++ // nested-target
			return value
		}
		goto Target
	Target:
		for i := 0; i < n; i++ { // literal-loop
			subtotal += nested()
		}
		repeats++
		if repeats == 1 {
			n = 0
			goto Target
		}
		return subtotal
	}
	goto Target
Target:
	switch passes { // namespace-switch
	case 0: // namespace-matched
		total += worker() // namespace-matched-body
	case 2: // namespace-unmatched
		total += 100 // namespace-unmatched-body
	}
	passes++
	if passes == 1 {
		goto Target
	}
	return total*10 + passes
}
`
	const tests = `package fixture
import "testing"
func TestNamespaces(t *testing.T) {
	if got := Namespaces(); got != 12 { t.Fatalf("Namespaces() = %d, want 12", got) }
}
`
	f := newFixture(t, map[string]string{
		"go.mod": fixtureMod, "goto.go": src, "goto_test.go": tests,
	}, ".")
	points := f.success(t, "-timeout", "15s")
	file := f.path("goto.go")
	// All three functions use Target, but it names a different statement kind
	// in each. Replay must never refer to another function's local flag.
	wantGotoStatement(t, points, file, src, "// nested-target", false)
	wantGotoStatement(t, points, file, src, "// literal-loop", false)
	wantGotoStatement(t, points, file, src, "// namespace-switch", false)
	wantBranch(t, points, file, src, "// literal-loop", "for", "skipped", false)
	wantBranch(t, points, file, src, "// namespace-switch", "switch", "no-match", false)
	wantGotoStatement(t, points, file, src, "// namespace-matched-body", false)
	wantGotoStatement(t, points, file, src, "// namespace-unmatched-body", true)
}
