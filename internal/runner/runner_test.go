package runner

import (
	"bytes"
	"context"
	"encoding/binary"
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
	counterPath := filepath.Join(dir, "counts")
	data := make([]byte, 4*8)
	binary.NativeEndian.PutUint64(data[8:], 7)
	if err := os.WriteFile(counterPath, data, 0600); err != nil {
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
	packages := []packageCoverage{{countFile: counterPath, size: len(data), points: points}}
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
	packages[0].size++
	if err := encodeReport(packages, io.Discard); err == nil {
		t.Error("invalid count file size accepted")
	}
}

func TestReportWriteError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "counts")
	if err := os.WriteFile(path, make([]byte, 8), 0600); err != nil {
		t.Fatal(err)
	}
	want := errors.New("writer failed")
	err := encodeReport([]packageCoverage{{
		countFile: path,
		size:      8,
		points:    []instrument.Point{{Counter: 0}},
	}}, failingWriter{want})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
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
