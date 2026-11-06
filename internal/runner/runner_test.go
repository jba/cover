package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	if strings.Contains(strings.ToLower(buf.String()), "counter") {
		t.Fatalf("counter leaked into JSON: %s", &buf)
	}
	var got []instrument.Point
	dec := json.NewDecoder(&buf)
	for {
		var p instrument.Point
		err := dec.Decode(&p)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, p)
	}
	want := []instrument.Point{points[3], points[2], points[0]}
	for i := range want {
		want[i].Counter = 0 // Counter is intentionally not serialized.
	}
	if !slices.Equal(got, want) {
		t.Fatalf("report = %+v, want %+v", got, want)
	}
	outPath := filepath.Join(dir, "output.jsonl")
	if err := writeReport(packages, outPath, io.Discard); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(outPath); err != nil || len(contents) == 0 {
		t.Fatalf("output file: contents %q, err %v", contents, err)
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

func TestReportWriteError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "counts")
	want := errors.New("writer failed")
	err := encodeReport([]packageCoverage{{
		countDir: path,
		count:    1,
		points:   []instrument.Point{{Counter: 0}},
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
