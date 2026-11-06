package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
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
	if err := encodeReport(packages, &buf, false); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%s 2:1 2:2 uncovered\n%s 1:1 1:2 uncovered\n%s 3:1 3:2 uncovered\n",
		points[3].File, points[2].File, points[0].File)
	if got := buf.String(); got != want {
		t.Fatalf("report = %q, want %q", got, want)
	}
	outPath := filepath.Join(dir, "output.txt")
	if err := writeReport(packages, outPath, io.Discard, false); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(outPath); err != nil || string(contents) != want {
		t.Fatalf("output file: contents %q, err %v", contents, err)
	}
	buf.Reset()
	if err := writeReport(packages, "-", &buf, false); err != nil || buf.String() != want {
		t.Fatalf("stdout destination: output %q, err %v", buf.String(), err)
	}
	for _, counter := range []int{-1, 4} {
		bad := slices.Clone(points)
		bad[0].Counter = counter
		packages[0].points = bad
		if err := encodeReport(packages, io.Discard, false); err == nil {
			t.Errorf("invalid counter %d accepted", counter)
		}
	}
	packages[0].points = points
	packages[0].count++
	if err := encodeReport(packages, io.Discard, false); err == nil {
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
					err := encodeReport([]packageCoverage{{countDir: dir, count: 1, points: []instrument.Point{p}}}, &buf, false)
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
	}}, &buf, false)
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
		}}, &buf, false)
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
	}}, failingWriter{want}, false)
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func TestVerboseReportSourceLines(t *testing.T) {
	longLine := "\t" + strings.Repeat("界", 40_000) + " // longer than a Scanner token"
	for _, tc := range []struct {
		name, source, excerpt string
		start, end            instrument.Position
	}{
		{
			name:    "single line ignores columns",
			source:  "\t  before := \"café 雪\"; after() // keep trailing spaces  \n",
			start:   instrument.Position{Line: 1, Column: 5},
			end:     instrument.Position{Line: 1, Column: 12},
			excerpt: "    \t  before := \"café 雪\"; after() // keep trailing spaces  \n",
		},
		{
			name:    "same line ignores reversed out-of-bounds columns",
			source:  "whole original line\n",
			start:   instrument.Position{Line: 1, Column: 999},
			end:     instrument.Position{Line: 1, Column: 1},
			excerpt: "    whole original line\n",
		},
		{
			name:    "multiline includes column-one end and blank lines",
			source:  "not displayed\n\tfirst()\n\n \tlast()  \nnot displayed either\n",
			start:   instrument.Position{Line: 2, Column: 999},
			end:     instrument.Position{Line: 4, Column: 1},
			excerpt: "    \tfirst()\n    \n     \tlast()  \n",
		},
		{
			name:    "CRLF and unterminated final line",
			source:  "skip\r\n\tfirst()\r\n\r\n  最後()",
			start:   instrument.Position{Line: 2, Column: 2},
			end:     instrument.Position{Line: 4, Column: 1},
			excerpt: "    \tfirst()\n    \n      最後()\n",
		},
		{
			name:    "empty physical line",
			source:  "\n",
			start:   instrument.Position{Line: 1, Column: 1},
			end:     instrument.Position{Line: 1, Column: 999},
			excerpt: "    \n",
		},
		{
			name:    "only trailing CR is stripped",
			source:  "\tfirst\rmiddle\r\n",
			start:   instrument.Position{Line: 1, Column: 1},
			end:     instrument.Position{Line: 1, Column: 2},
			excerpt: "    \tfirst\rmiddle\n",
		},
		{
			name:    "very long line without final newline",
			source:  longLine,
			start:   instrument.Position{Line: 1, Column: 2},
			end:     instrument.Position{Line: 1, Column: 3},
			excerpt: "    " + longLine + "\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "original source.go")
			if err := os.WriteFile(file, []byte(tc.source), 0600); err != nil {
				t.Fatal(err)
			}
			p := instrument.Point{
				File: file, Kind: "statement",
				Range: instrument.Range{Start: tc.start, End: tc.end},
			}
			packages := []packageCoverage{{countDir: dir, count: 1, points: []instrument.Point{p}}}
			header := fmt.Sprintf("%s %d:%d %d:%d uncovered\n",
				file, tc.start.Line, tc.start.Column, tc.end.Line, tc.end.Column)
			for _, verbose := range []bool{false, true} {
				var buf bytes.Buffer
				if err := encodeReport(packages, &buf, verbose); err != nil {
					t.Fatalf("verbose=%t: %v", verbose, err)
				}
				want := header
				if verbose {
					want += tc.excerpt
				}
				if got := buf.String(); got != want {
					t.Errorf("verbose=%t: report = %q, want %q", verbose, got, want)
				}
			}
		})
	}
}

func TestVerboseReportRepeatedRangesAndSourceCache(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "source.go")
	if err := os.WriteFile(file, []byte("original first\n\toriginal second\n"), 0600); err != nil {
		t.Fatal(err)
	}
	otherFile := filepath.Join(dir, "z-other.go")
	if err := os.WriteFile(otherFile, []byte("different original source"), 0600); err != nil {
		t.Fatal(err)
	}
	multiline := instrument.Point{
		File: file, Kind: "statement",
		Range: instrument.Range{
			Start: instrument.Position{Line: 1, Column: 2},
			End:   instrument.Position{Line: 2, Column: 1},
		},
	}
	overlap := multiline
	overlap.Kind, overlap.Construct, overlap.Outcome = "branch", "if", "false"
	overlap.Range.Start = instrument.Position{Line: 2, Column: 3}
	overlap.Range.End = instrument.Position{Line: 2, Column: 8}
	other := instrument.Point{File: otherFile, Kind: "statement", Range: instrument.Range{
		Start: instrument.Position{Line: 1, Column: 1},
		End:   instrument.Position{Line: 1, Column: 2},
	}}
	packages := []packageCoverage{
		{countDir: dir, count: 1, points: []instrument.Point{overlap, multiline}},
		{countDir: dir, count: 1, points: []instrument.Point{other, multiline}},
	}
	var buf bytes.Buffer
	removed := false
	w := reportWriterFunc(func(p []byte) (int, error) {
		n, err := buf.Write(p)
		if !removed && strings.Contains(buf.String(), "    original first\n") {
			// Reporting must keep using its cached original source, even for
			// another package's point with exactly the same filename.
			if err := os.Remove(file); err != nil {
				return n, err
			}
			removed = true
		}
		return n, err
	})
	if err := encodeReport(packages, w, true); err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("source excerpt was not written")
	}
	block := file + " 1:2 2:1 uncovered\n    original first\n    \toriginal second\n"
	want := block + block + file + " 2:3 2:8 skipped\n    \toriginal second\n" +
		otherFile + " 1:1 1:2 uncovered\n    different original source\n"
	if got := buf.String(); got != want {
		t.Fatalf("repeated/overlapping snippets:\ngot  %q\nwant %q", got, want)
	}
}

func TestVerboseReportSourceErrors(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		start, end   int
	}{
		{"zero start", "one\ntwo\n", 0, 1},
		{"negative start", "one\ntwo\n", -1, 1},
		{"zero end", "one\ntwo\n", 1, 0},
		{"negative end", "one\ntwo\n", 1, -1},
		{"reversed range", "one\ntwo\n", 2, 1},
		{"start beyond EOF", "one\ntwo\n", 3, 3},
		{"end beyond EOF", "one\ntwo\n", 1, 3},
		{"empty source", "", 1, 1},
		{"no phantom line after trailing LF", "one\n", 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "source.go")
			if err := os.WriteFile(file, []byte(tc.source), 0600); err != nil {
				t.Fatal(err)
			}
			p := instrument.Point{File: file, Kind: "statement", Range: instrument.Range{
				Start: instrument.Position{Line: tc.start, Column: 1},
				End:   instrument.Position{Line: tc.end, Column: 2},
			}}
			err := encodeReport([]packageCoverage{{countDir: dir, count: 1, points: []instrument.Point{p}}}, io.Discard, true)
			if err == nil || !strings.Contains(err.Error(), file) {
				t.Errorf("invalid source range error = %v, want error naming %q", err, file)
			}
		})
	}
	for _, directory := range []bool{false, true} {
		t.Run(fmt.Sprintf("read failure/directory=%t", directory), func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "missing.go")
			if directory {
				file = dir
			}
			p := instrument.Point{File: file, Kind: "statement", Range: instrument.Range{
				Start: instrument.Position{Line: 1, Column: 1},
				End:   instrument.Position{Line: 1, Column: 2},
			}}
			packages := []packageCoverage{{countDir: dir, count: 1, points: []instrument.Point{p}}}
			// Default reporting never needs to open a referenced source file.
			if err := encodeReport(packages, io.Discard, false); err != nil {
				t.Fatalf("nonverbose reporting tried to read source: %v", err)
			}
			err := encodeReport(packages, io.Discard, true)
			if err == nil || !strings.Contains(err.Error(), file) {
				t.Fatalf("source-read error = %v, want error naming %q", err, file)
			}
			if !directory && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("source-read error lost os.ErrNotExist: %v", err)
			}
		})
	}
}

func TestVerboseReportOmittedPointsDoNotReadSource(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "123.json"), []byte("[1,0]"), 0600); err != nil {
		t.Fatal(err)
	}
	// All paths are deliberately absent, and all line bounds invalid. Neither
	// source reads nor range validation are needed for records not displayed.
	points := []instrument.Point{
		{Kind: "statement", Counter: 0},
		{Kind: "branch", Construct: "if", Outcome: "false", Counter: 0},
		{Kind: "branch", Construct: "for", Outcome: "skipped", Counter: 0},
		{Kind: "branch", Construct: "switch", Outcome: "no-match", Counter: 0},
		{Kind: "branch", Construct: "type-switch", Outcome: "no-match", Counter: 0},
	}
	for _, outcome := range []struct{ construct, outcome string }{
		{"if", "true"}, {"for", "entered"},
		{"switch", "case"}, {"switch", "default"},
		{"type-switch", "case"}, {"type-switch", "default"},
		{"select", "case"}, {"select", "default"},
	} {
		points = append(points, instrument.Point{
			Kind: "branch", Construct: outcome.construct, Outcome: outcome.outcome, Counter: 1,
		})
	}
	for i := range points {
		points[i].File = filepath.Join(dir, fmt.Sprintf("missing-%d.go", i))
	}
	var buf bytes.Buffer
	if err := encodeReport([]packageCoverage{{countDir: dir, count: 2, points: points}}, &buf, true); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("covered/ignored points produced snippets or headers: %q", &buf)
	}
}

func TestVerboseReportSourceWriteErrors(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "source.go")
	if err := os.WriteFile(file, []byte("first\nsecond\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p := instrument.Point{File: file, Kind: "statement", Range: instrument.Range{
		Start: instrument.Position{Line: 1, Column: 1},
		End:   instrument.Position{Line: 2, Column: 1},
	}}
	header := file + " 1:1 2:1 uncovered\n"
	wantErr := errors.New("source writer failed")
	// Fail at the prefix, within a prefix, within source content, at its LF,
	// and on the following line, after the header has succeeded in full.
	for _, extra := range []int{0, 2, 4, 6, len("    first"), len("    first\n")} {
		t.Run(fmt.Sprintf("after %d source bytes", extra), func(t *testing.T) {
			remaining := len(header) + extra
			w := reportWriterFunc(func(p []byte) (int, error) {
				if len(p) > remaining {
					n := remaining
					remaining = 0
					return n, wantErr
				}
				remaining -= len(p)
				return len(p), nil
			})
			err := encodeReport([]packageCoverage{{countDir: dir, count: 1, points: []instrument.Point{p}}}, w, true)
			if !errors.Is(err, wantErr) {
				t.Fatalf("source writing error = %v, want %v", err, wantErr)
			}
		})
	}
}

func TestVerboseReportOutputDestinations(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "source.go")
	if err := os.WriteFile(file, []byte("\treturn 42"), 0600); err != nil {
		t.Fatal(err)
	}
	p := instrument.Point{File: file, Kind: "statement", Range: instrument.Range{
		Start: instrument.Position{Line: 1, Column: 2},
		End:   instrument.Position{Line: 1, Column: 11},
	}}
	packages := []packageCoverage{{countDir: dir, count: 1, points: []instrument.Point{p}}}
	want := file + " 1:2 1:11 uncovered\n    \treturn 42\n"
	for _, outputPath := range []string{"", "-", filepath.Join(dir, "report.txt")} {
		t.Run(fmt.Sprintf("output=%q", outputPath), func(t *testing.T) {
			var buf bytes.Buffer
			if err := writeReport(packages, outputPath, &buf, true); err != nil {
				t.Fatal(err)
			}
			got := buf.String()
			if outputPath != "" && outputPath != "-" {
				if got != "" {
					t.Errorf("file destination leaked report to stdout: %q", got)
				}
				data, err := os.ReadFile(outputPath)
				if err != nil {
					t.Fatal(err)
				}
				got = string(data)
			}
			if got != want {
				t.Errorf("report = %q, want %q", got, want)
			}
		})
	}
}

type reportWriterFunc func([]byte) (int, error)

func (f reportWriterFunc) Write(p []byte) (int, error) { return f(p) }

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
	for _, help := range []string{"-h", "-help"} {
		out.Reset()
		diagnostic.Reset()
		if err := Run(context.Background(), []string{help}, &out, &diagnostic); err != nil {
			t.Fatal(err)
		}
		if out.Len() != 0 {
			t.Fatalf("%s wrote help to stdout: %q", help, &out)
		}
		for _, text := range []string{
			"Usage: cover", "Output: FILE START END WORD",
			"original absolute source path", "1-based line:byte-column", "END is exclusive",
			`"uncovered"`, `"skipped"`, "if-false", "zero-iteration loop", "switch no-match",
		} {
			if !strings.Contains(diagnostic.String(), text) {
				t.Errorf("%s output lacks %q:\n%s", help, text, &diagnostic)
			}
		}
		if !regexp.MustCompile(`(?m)^  -v(?:\s|$)`).MatchString(diagnostic.String()) {
			t.Errorf("%s output lacks the -v flag:\n%s", help, &diagnostic)
		}
		helpText := strings.ToLower(diagnostic.String())
		for _, text := range []string{"full source lines", "inclusive", "four spaces", "ignoring columns"} {
			if !strings.Contains(helpText, text) {
				t.Errorf("%s output does not describe verbose source display (%q):\n%s", help, text, &diagnostic)
			}
		}
	}
	for _, args := range [][]string{{"-count=-1"}, {"-nonexistent-flag"}} {
		if err := Run(context.Background(), args, &out, &diagnostic); err == nil {
			t.Errorf("Run(%q) succeeded", args)
		}
	}
}
