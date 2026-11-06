package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jba/cover/internal/instrument"
)

func TestReport(t *testing.T) {
	dir := t.TempDir()
	counterDir := filepath.Join(dir, "counts")
	if err := os.Mkdir(counterDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(counterDir, "123.json"), []byte("[0,7,0,0]"), 0600); err != nil {
		t.Fatal(err)
	}
	point := func(file string, line, counter int) instrument.Point {
		return instrument.Point{
			File:    filepath.Join(dir, file),
			Kind:    "statement",
			Counter: counter,
			Range: instrument.Range{
				Start: instrument.Position{Line: line, Column: 1},
				End:   instrument.Position{Line: line, Column: 2},
			},
		}
	}
	points := []instrument.Point{
		point("b.go", 3, 0),
		point("a.go", 1, 1), // Covered: omitted from the report.
		point("b.go", 1, 2),
		point("a.go", 2, 3),
	}
	packages := []packageCoverage{{countDir: counterDir, count: 4, points: points}}
	var buf bytes.Buffer
	if err := encodeReport(packages, &buf); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%s 2:1 2:2 uncovered\n%s 1:1 1:2 uncovered\n%s 3:1 3:2 uncovered\n",
		points[3].File, points[2].File, points[0].File)
	if got := buf.String(); got != want {
		t.Fatalf("report = %q, want %q", got, want)
	}
	outPath := filepath.Join(dir, "output.txt")
	if err := writeReport(packages, outPath, io.Discard); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(outPath); err != nil || string(contents) != want {
		t.Fatalf("output file: contents %q, err %v", contents, err)
	}
	buf.Reset()
	if err := writeReport(packages, "-", &buf); err != nil || buf.String() != want {
		t.Fatalf("stdout destination: output %q, err %v", buf.String(), err)
	}
	for _, counter := range []int{-1, 4} {
		bad := slices.Clone(points)
		bad[0].Counter = counter
		packages[0].points = bad
		if err := encodeReport(packages, io.Discard); err == nil {
			t.Errorf("invalid counter %d accepted", counter)
		}
	}
	packages[0].points = points
	packages[0].count++
	if err := encodeReport(packages, io.Discard); err == nil {
		t.Error("invalid counter snapshot size accepted")
	}
}

func TestReportOutcomes(t *testing.T) {
	for _, tc := range []struct {
		kind, construct, outcome, word string
	}{
		{"statement", "", "", "uncovered"},
		{"branch", "if", "false", "skipped"},
		{"branch", "for", "skipped", "skipped"},
		{"branch", "switch", "no-match", "skipped"},
		{"branch", "type-switch", "no-match", "skipped"},
		{"branch", "if", "true", ""},
		{"branch", "for", "entered", ""},
		{"branch", "switch", "case", ""},
		{"branch", "switch", "default", ""},
		{"branch", "type-switch", "case", ""},
		{"branch", "type-switch", "default", ""},
		{"branch", "select", "case", ""},
		{"branch", "select", "default", ""},
	} {
		t.Run(tc.kind+"/"+tc.construct+"/"+tc.outcome, func(t *testing.T) {
			for _, covered := range []bool{false, true} {
				t.Run(fmt.Sprintf("covered=%t", covered), func(t *testing.T) {
					dir := t.TempDir()
					count := "0"
					if covered {
						count = "1"
					}
					if err := os.WriteFile(filepath.Join(dir, "123.json"), []byte("["+count+"]"), 0600); err != nil {
						t.Fatal(err)
					}
					// Filenames are preserved verbatim, even with internal spaces.
					file := filepath.Join(dir, "directory with spaces", "file  with spaces.go")
					p := instrument.Point{
						File: file, Kind: tc.kind, Construct: tc.construct, Outcome: tc.outcome,
						Range: instrument.Range{Start: instrument.Position{Line: 12, Column: 7},
							End: instrument.Position{Line: 14, Column: 19}},
					}
					var buf bytes.Buffer
					err := encodeReport([]packageCoverage{{countDir: dir, count: 1, points: []instrument.Point{p}}}, &buf)
					if err != nil {
						t.Fatal(err)
					}
					want := ""
					if !covered && tc.word != "" {
						want = file + " 12:7 14:19 " + tc.word + "\n"
					}
					if buf.String() != want {
						t.Fatalf("report = %q, want %q", buf.String(), want)
					}
				})
			}
		})
	}
}

func TestReportPositionOrder(t *testing.T) {
	file := filepath.Join(t.TempDir(), "example.go")
	point := func(startColumn, endLine, endColumn int, branch bool) instrument.Point {
		p := instrument.Point{
			File: file, Kind: "statement",
			Range: instrument.Range{
				Start: instrument.Position{Line: 2, Column: startColumn},
				End:   instrument.Position{Line: endLine, Column: endColumn},
			},
		}
		if branch {
			p.Kind, p.Construct, p.Outcome = "branch", "for", "skipped"
		}
		return p
	}
	var buf bytes.Buffer
	err := encodeReport([]packageCoverage{{
		countDir: t.TempDir(), count: 1,
		points: []instrument.Point{
			point(10, 2, 12, false),
			point(2, 3, 2, false),
			point(2, 2, 20, false),
			point(2, 2, 10, false),
			point(2, 2, 10, true),
		},
	}}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	want := file + " 2:2 2:10 skipped\n" +
		file + " 2:2 2:10 uncovered\n" +
		file + " 2:2 2:20 uncovered\n" +
		file + " 2:2 3:2 uncovered\n" +
		file + " 2:10 2:12 uncovered\n"
	if buf.String() != want {
		t.Fatalf("position/word order:\ngot  %q\nwant %q", buf.String(), want)
	}
}

func TestReportRejectsNewlineFilename(t *testing.T) {
	for _, newline := range []string{"\n", "\r"} {
		p := instrument.Point{File: "valid.go", Kind: "statement"}
		bad := p
		bad.File = "invalid" + newline + ".go"
		var buf bytes.Buffer
		err := encodeReport([]packageCoverage{{
			countDir: t.TempDir(), count: 1, points: []instrument.Point{p, bad},
		}}, &buf)
		if err == nil || !strings.Contains(err.Error(), "filename contains a newline") {
			t.Fatalf("newline filename error = %v", err)
		}
		if buf.Len() != 0 {
			t.Fatalf("partial output for unrepresentable filename: %q", buf.String())
		}
	}
}

func TestReportWriteError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "counts")
	want := errors.New("writer failed")
	err := encodeReport([]packageCoverage{{
		countDir: path,
		count:    1,
		points:   []instrument.Point{{Kind: "statement", Counter: 0}},
	}}, failingWriter{want})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func TestReadCounters(t *testing.T) {
	dir := t.TempDir()
	for name, contents := range map[string]string{
		"123.json":     "[1,2,0]",
		"456.json":     "[3,0,7]",
		".snapshot-42": "unfinished snapshot",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := readCounters(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	if want := []uint64{4, 2, 7}; !slices.Equal(got, want) {
		t.Fatalf("merged counts = %v, want %v", got, want)
	}
	for _, path := range []string{t.TempDir(), filepath.Join(dir, "absent")} {
		got, err := readCounters(path, 3)
		if err != nil || !slices.Equal(got, []uint64{0, 0, 0}) {
			t.Errorf("missing snapshots: got %v, %v", got, err)
		}
	}
}

func TestReadCountersInvalid(t *testing.T) {
	for _, contents := range []string{"not json", "[0]", "[0,0,0]", "null", "[null,0]", "{}", "[-1,0]", "[0.5,0]", "[18446744073709551616,0]", "[0,0] trailing"} {
		t.Run(contents, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "123.json"), []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := readCounters(dir, 2); err == nil {
				t.Error("invalid snapshot accepted")
			}
		})
	}
}

func TestPrepareRegistryRequiresModule(t *testing.T) {
	pkg := listedPackage{Dir: t.TempDir(), ImportPath: "example.com/gopath"}
	if _, err := prepareRegistry([]listedPackage{pkg}, t.TempDir(), map[string]string{}); err == nil || !strings.Contains(err.Error(), "module mode") {
		t.Fatalf("GOPATH registry error = %v", err)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestRunFlags(t *testing.T) {
	// These paths should not need to invoke the Go tool at all.
	t.Setenv("PATH", "")
	var out, diagnostic bytes.Buffer
	if err := Run(context.Background(), []string{"-help"}, &out, &diagnostic); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || !strings.Contains(diagnostic.String(), "Usage: cover") {
		t.Fatalf("help: stdout=%q stderr=%q", &out, &diagnostic)
	}
	for _, args := range [][]string{{"-count=-1"}, {"-nonexistent-flag"}} {
		if err := Run(context.Background(), args, &out, &diagnostic); err == nil {
			t.Errorf("Run(%q) succeeded", args)
		}
	}
}
