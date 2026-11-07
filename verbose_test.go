package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests use the independent-module fixture helpers, including their
// snapshots of original files. Do not run in parallel: the helpers change cwd.
func TestVerboseReportCLI(t *testing.T) {
	const original = `package fixture
//line generated.go:900
func Value(é bool) string {
	if é { // original decision, not rewritten instrumentation
		return "雪" // covered source must not be displayed
	}
	return ( // original multiline return
		"never " + // preserve this comment and indentation
			"called")
}`
	// Physical positions must refer to the original file, not //line's filename
	// or line numbers. CRLF must disappear from displayed excerpts only.
	source := strings.ReplaceAll(original, "\n", "\r\n")
	f := newFixture(t, map[string]string{
		"go.mod":         fixtureMod,
		"source file.go": source,
		"source_test.go": `package fixture
import (
	"fmt"
	"testing"
)
func TestValue(t *testing.T) {
	fmt.Println("PASSING USER OUTPUT MUST STAY HIDDEN")
	t.Log("PASSING TEST LOG MUST STAY HIDDEN")
	if Value(true) != "雪" { t.Fatal("wrong result") }
}
func TestFailure(t *testing.T) {
	Value(true)
	fmt.Println("FAILING USER OUTPUT BELONGS ON STDERR")
	t.Fatal("intentional failure")
}
`,
	}, ".")
	run := func(t *testing.T, args ...string) (string, string) {
		t.Helper()
		args = append(args, "-run=^TestValue$")
		out, diagnostic, err := f.run(t, args...)
		if err != nil {
			t.Fatalf("Run(%q): %v\nstderr:\n%s\nstdout:\n%s", args, err, diagnostic, out)
		}
		// -v describes cover's source display, not go test's verbosity. Test
		// names, successful test output and logs must remain suppressed.
		for _, unwanted := range []string{
			"=== RUN", "--- PASS", "PASSING USER OUTPUT", "PASSING TEST LOG",
		} {
			if strings.Contains(diagnostic, unwanted) {
				t.Errorf("-v enabled go test verbosity (%q):\n%s", unwanted, diagnostic)
			}
		}
		if !strings.Contains(diagnostic, "ok") || !strings.Contains(diagnostic, "example.com/fixture") {
			t.Errorf("go test package result missing from stderr:\n%s", diagnostic)
		}
		return out, diagnostic
	}
	plain, _ := run(t)
	points := decodePoints(t, plain)
	file := f.path("source file.go")
	if len(points) != 2 {
		t.Fatalf("report = %+v, want one skipped decision and one uncovered return", points)
	}
	wantBranch(t, points, file, source, "if é", "if", "false", true)
	wantGotoStatement(t, points, file, source, "return (", true)
	wantGotoStatement(t, points, file, source, `return "雪"`, false)
	var wantVerbose strings.Builder
	lines := strings.Split(original, "\n")
	for i, p := range points {
		// Keep the original report's header bytes, order, byte columns, and
		// exclusive endpoint, then append whole inclusive physical lines.
		header := strings.Split(strings.TrimSuffix(plain, "\n"), "\n")[i]
		fmt.Fprintln(&wantVerbose, header)
		if p.File != file {
			t.Fatalf("report named non-original source: %+v", p)
		}
		for line := p.Range.Start.Line; line <= p.Range.End.Line; line++ {
			fmt.Fprintf(&wantVerbose, "    %s\n", lines[line-1])
		}
	}
	if !strings.Contains(wantVerbose.String(), "skipped\n    \tif é") ||
		!strings.Contains(wantVerbose.String(), "uncovered\n    \treturn (") ||
		!strings.Contains(wantVerbose.String(), "    \t\t\t\"called\")\n") {
		t.Fatalf("fixture did not exercise both report words and multiline excerpts:\n%s", &wantVerbose)
	}
	for _, tc := range []struct {
		name    string
		args    []string
		verbose bool
		file    bool
	}{
		{name: "explicit false", args: []string{"-v=false"}},
		{name: "verbose", args: []string{"-v"}, verbose: true},
		{name: "explicit true", args: []string{"-v=true"}, verbose: true},
		{name: "verbose stdout destination", args: []string{"-v", "-o", "-"}, verbose: true},
		{name: "verbose file destination", args: []string{"-v"}, verbose: true, file: true},
		{name: "false file destination", args: []string{"-v=false"}, file: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string(nil), tc.args...)
			var outputFile string
			if tc.file {
				// Outside the fixture, so the fixture's original-file snapshot
				// still checks every original file without excluding report paths.
				outputFile = filepath.Join(t.TempDir(), "report.txt")
				args = append(args, "-o", outputFile)
			}
			got, _ := run(t, args...)
			if tc.file {
				if got != "" {
					t.Errorf("-o leaked report to stdout: %q", got)
				}
				data, err := os.ReadFile(outputFile)
				if err != nil {
					t.Fatal(err)
				}
				got = string(data)
			}
			want := plain
			if tc.verbose {
				want = wantVerbose.String()
			}
			if got != want {
				t.Errorf("report:\ngot  %q\nwant %q", got, want)
			}
		})
	}

	t.Run("failed go test output stays on stderr", func(t *testing.T) {
		out, diagnostic, err := f.run(t, "-v", "-run=^TestFailure$")
		if err == nil {
			t.Fatal("Run succeeded despite failing fixture test")
		}
		if out != wantVerbose.String() {
			t.Errorf("failed-test report:\ngot  %q\nwant %q", out, wantVerbose.String())
		}
		if !strings.Contains(diagnostic, "FAILING USER OUTPUT BELONGS ON STDERR") ||
			!strings.Contains(diagnostic, "intentional failure") ||
			!strings.Contains(diagnostic, "FAIL") {
			t.Errorf("go test failure output missing from stderr:\n%s", diagnostic)
		}
		if strings.Contains(diagnostic, "=== RUN") {
			t.Errorf("cover -v enabled go test verbosity:\n%s", diagnostic)
		}
	})
}
