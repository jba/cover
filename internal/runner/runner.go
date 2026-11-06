// Package runner implements the cover command.
package runner

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jba/cover/internal/instrument"
)

type options struct {
	output  string
	run     string
	count   int
	timeout string
	tags    string
	race    bool
	raceSet bool
	verbose bool
}

// Run instruments the requested packages, runs their tests, and writes lines of
// "FILE START END WORD" to stdout (or the file named by -o). All go command output
// is sent to stderr. With -v, each record is followed by full source lines.
// Coverage is reported even if the tests fail.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var opts options
	flags := flag.NewFlagSet("cover", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.output, "o", "", "write coverage report to file (default stdout)")
	flags.StringVar(&opts.run, "run", "", "run only tests matching the regular expression")
	flags.IntVar(&opts.count, "count", 1, "run each test this many times (disables the test cache)")
	flags.StringVar(&opts.timeout, "timeout", "", "test timeout, as a Go duration (default go test timeout)")
	flags.StringVar(&opts.tags, "tags", "", "comma-separated build tags")
	flags.BoolVar(&opts.race, "race", false, "enable the Go race detector")
	flags.BoolVar(&opts.verbose, "v", false, "display full source lines below each report entry")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: cover [flags] [packages]")
		flags.PrintDefaults()
		fmt.Fprintln(stderr, `
Output: FILE START END WORD
  FILE is the original absolute source path.
  START and END are 1-based line:byte-column positions; END is exclusive.
  WORD is "uncovered" for a statement that never executed, or "skipped" for
  an unobserved if-false, zero-iteration loop, or switch no-match outcome.
  With -v, full source lines from START through END (inclusive) follow each
  record, ignoring columns and indented by four spaces.`)
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if opts.count < 0 {
		return errors.New("-count must not be negative")
	}
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "race" {
			opts.raceSet = true
		}
	})
	patterns := flags.Args()
	if len(patterns) == 0 {
		patterns = []string{"."}
	}
	pkgs, err := listPackages(ctx, patterns, opts, stderr)
	if err != nil {
		return err
	}
	var selected []listedPackage
	seen := make(map[string]bool)
	for _, pkg := range pkgs {
		if seen[pkg.Dir] {
			continue
		}
		seen[pkg.Dir] = true
		if pkg.Standard || pkg.Goroot || isVendor(pkg.Dir) {
			fmt.Fprintf(stderr, "cover: skipping standard or vendor package %s\n", pkg.ImportPath)
			continue
		}
		if pkg.ImportPath == "command-line-arguments" {
			return errors.New("cover: provide package patterns, not individual Go files")
		}
		selected = append(selected, pkg)
	}
	if len(selected) == 0 {
		return errors.New("cover: no instrumentable non-test Go files in requested packages")
	}

	// Keep all generated files outside the source tree: in particular, this
	// preserves relative module replacements, workspace membership, and cgo's
	// view of the original package directory.
	tempDir, err := os.MkdirTemp("", "cover-")
	if err != nil {
		return fmt.Errorf("create coverage directory: %w", err)
	}
	defer os.RemoveAll(tempDir)

	overlay := struct {
		Replace map[string]string
	}{Replace: make(map[string]string)}
	registryImport, err := prepareRegistry(selected, tempDir, overlay.Replace)
	if err != nil {
		return err
	}
	var covered []packageCoverage
	var testPackages []string
	for number, pkg := range selected {
		// A test-only package may exercise production code in another selected
		// package. It also needs a TestMain overlay to save that dependency.
		testPackages = append(testPackages, pkg.ImportPath)
		if len(pkg.GoFiles)+len(pkg.CgoFiles) == 0 {
			fmt.Fprintf(stderr, "cover: not instrumenting %s: no non-test Go files (tests will still run)\n", pkg.ImportPath)
			prefix, err := packagePrefix(pkg)
			if err != nil {
				return err
			}
			if err := prepareTestMain(pkg, prefix, registryImport, tempDir, number, overlay.Replace); err != nil {
				return err
			}
			continue
		}
		coverage, err := preparePackage(pkg, tempDir, registryImport, number, overlay.Replace)
		if err != nil {
			return err
		}
		covered = append(covered, coverage)
	}
	if len(covered) == 0 {
		return errors.New("cover: no instrumentable non-test Go files in requested packages")
	}
	overlayJSON, err := json.Marshal(overlay)
	if err != nil {
		return fmt.Errorf("encode overlay: %w", err)
	}
	overlayPath := filepath.Join(tempDir, "overlay.json")
	if err := os.WriteFile(overlayPath, overlayJSON, 0600); err != nil {
		return fmt.Errorf("write overlay: %w", err)
	}

	// The compiler understands a new directory supplied entirely by an
	// overlay, but vet tries to chdir to it on disk. Do not materialize the
	// registry in the user's module just to satisfy vet.
	testArgs := []string{"test", "-vet=off", "-overlay=" + overlayPath, fmt.Sprintf("-count=%d", opts.count)}
	if opts.run != "" {
		testArgs = append(testArgs, "-run="+opts.run)
	}
	if opts.timeout != "" {
		testArgs = append(testArgs, "-timeout="+opts.timeout)
	}
	if opts.tags != "" {
		testArgs = append(testArgs, "-tags="+opts.tags)
	}
	if opts.raceSet {
		testArgs = append(testArgs, fmt.Sprintf("-race=%t", opts.race))
	}
	testArgs = append(testArgs, testPackages...)
	cmd := exec.CommandContext(ctx, "go", testArgs...)
	cmd.Stdout = stderr
	cmd.Stderr = stderr
	testErr := cmd.Run()
	if testErr != nil {
		testErr = fmt.Errorf("go test: %w", testErr)
	}

	// Do not return on a test failure before reading snapshots from binaries
	// that completed TestMain. Abnormal exits may leave no snapshot.
	reportErr := writeReport(covered, opts.output, stdout, opts.verbose)
	return errors.Join(testErr, reportErr)
}

type listedPackage struct {
	Dir          string
	ImportPath   string
	Name         string
	GoFiles      []string
	CgoFiles     []string
	TestGoFiles  []string
	XTestGoFiles []string
	Standard     bool
	Goroot       bool
	Module       *listedModule
}

type listedModule struct {
	Path string
	Dir  string
	Main bool
}

func listPackages(ctx context.Context, patterns []string, opts options, stderr io.Writer) ([]listedPackage, error) {
	args := []string{"list", "-json"}
	if opts.tags != "" {
		args = append(args, "-tags="+opts.tags)
	}
	if opts.raceSet {
		args = append(args, fmt.Sprintf("-race=%t", opts.race))
	}
	args = append(args, "--")
	args = append(args, patterns...)
	cmd := exec.CommandContext(ctx, "go", args...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	var pkgs []listedPackage
	dec := json.NewDecoder(&output)
	for {
		var pkg listedPackage
		if err := dec.Decode(&pkg); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decode go list output: %w", err)
		}
		if pkg.Dir == "" || pkg.ImportPath == "" || pkg.Name == "" {
			return nil, errors.New("go list returned an incomplete package")
		}
		pkgs = append(pkgs, pkg)
	}
	return pkgs, nil
}

func isVendor(dir string) bool {
	for _, component := range strings.Split(filepath.Clean(dir), string(filepath.Separator)) {
		if component == "vendor" {
			return true
		}
	}
	return false
}

type packageCoverage struct {
	countDir string
	count    int
	points   []instrument.Point
}

func preparePackage(pkg listedPackage, tempDir, registryImport string, number int, overlay map[string]string) (packageCoverage, error) {
	var result packageCoverage
	pkgDir, err := filepath.Abs(pkg.Dir)
	if err != nil {
		return result, fmt.Errorf("package directory %s: %w", pkg.Dir, err)
	}
	files := append(slices.Clone(pkg.GoFiles), pkg.CgoFiles...)
	slices.Sort(files)
	originals := make(map[string][]byte)
	var sourceForPrefix []byte
	// Test identifiers must not collide with generated runtime declarations or
	// with imports and locals added when preparing TestMain.
	allFiles := append(slices.Clone(files), pkg.TestGoFiles...)
	allFiles = append(allFiles, pkg.XTestGoFiles...)
	for _, name := range allFiles {
		path := filepath.Join(pkgDir, name)
		src, err := os.ReadFile(path)
		if err != nil {
			return result, fmt.Errorf("read %s: %w", path, err)
		}
		originals[name] = src
		sourceForPrefix = append(sourceForPrefix, src...)
		sourceForPrefix = append(sourceForPrefix, '\n')
	}
	prefix, err := uniquePrefix(sourceForPrefix)
	if err != nil {
		return result, err
	}
	// A virtual file must have a basename that Go's package loader accepts.
	virtualPath := filepath.Join(pkgDir, "zz"+prefix+"_runtime.go")
	for {
		if _, err := os.Lstat(virtualPath); errors.Is(err, os.ErrNotExist) {
			break
		} else if err != nil {
			return result, fmt.Errorf("check runtime filename: %w", err)
		}
		virtualPath = strings.TrimSuffix(virtualPath, ".go") + "_x.go"
	}
	var nextCounter int
	for i, name := range files {
		path := filepath.Join(pkgDir, name)
		rewritten, points, err := instrument.Instrument(path, originals[name], prefix, &nextCounter)
		if err != nil {
			return result, fmt.Errorf("instrument %s: %w", path, err)
		}
		for i := range points {
			points[i].File = path
		}
		result.points = append(result.points, points...)
		backing := filepath.Join(tempDir, fmt.Sprintf("pkg%d_source%d.go", number, i))
		if err := os.WriteFile(backing, rewritten, 0600); err != nil {
			return result, fmt.Errorf("write instrumented %s: %w", path, err)
		}
		overlay[path] = backing
	}
	if nextCounter < 0 {
		return result, errors.New("cover: too many counters")
	}
	result.count = nextCounter
	result.countDir = filepath.Join(tempDir, fmt.Sprintf("pkg%d_counts", number))
	if err := os.Mkdir(result.countDir, 0700); err != nil {
		return result, fmt.Errorf("create counter directory: %w", err)
	}
	runtimeSrc, err := runtimeSource(pkg.Name, prefix, result.countDir, registryImport, result.count)
	if err != nil {
		return result, err
	}
	runtimePath := filepath.Join(tempDir, fmt.Sprintf("pkg%d_runtime.go", number))
	if err := os.WriteFile(runtimePath, runtimeSrc, 0600); err != nil {
		return result, fmt.Errorf("write coverage runtime: %w", err)
	}
	overlay[virtualPath] = runtimePath
	if err := prepareTestMain(pkg, prefix, registryImport, tempDir, number, overlay); err != nil {
		return result, err
	}
	return result, nil
}

func packagePrefix(pkg listedPackage) (string, error) {
	files := append(slices.Clone(pkg.GoFiles), pkg.CgoFiles...)
	files = append(files, pkg.TestGoFiles...)
	files = append(files, pkg.XTestGoFiles...)
	var source []byte
	for _, name := range files {
		path := filepath.Join(pkg.Dir, name)
		src, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", path, err)
		}
		source = append(source, src...)
		source = append(source, '\n')
	}
	return uniquePrefix(source)
}

func uniquePrefix(source []byte) (string, error) {
	for {
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", fmt.Errorf("generate coverage identifier: %w", err)
		}
		prefix := "__cover_" + hex.EncodeToString(random[:])
		if !bytes.Contains(source, []byte(prefix)) {
			return prefix, nil
		}
	}
}

func writeReport(packages []packageCoverage, outputPath string, stdout io.Writer, verbose bool) error {
	w := stdout
	if outputPath != "" && outputPath != "-" {
		f, err := os.Create(outputPath)
		if err != nil {
			return fmt.Errorf("create coverage output: %w", err)
		}
		reportErr := encodeReport(packages, f, verbose)
		return errors.Join(reportErr, f.Close())
	}
	return encodeReport(packages, w, verbose)
}

func encodeReport(packages []packageCoverage, w io.Writer, verbose bool) error {
	var uncovered []instrument.Point
	for _, pkg := range packages {
		counts, err := readCounters(pkg.countDir, pkg.count)
		if err != nil {
			return err
		}
		for _, point := range pkg.points {
			if point.Counter < 0 || point.Counter >= len(counts) {
				return fmt.Errorf("invalid counter %d for %s", point.Counter, point.File)
			}
			if counts[point.Counter] == 0 && reportWord(point) != "" {
				if strings.ContainsAny(point.File, "\r\n") {
					return fmt.Errorf("coverage filename contains a newline: %q", point.File)
				}
				uncovered = append(uncovered, point)
			}
		}
	}
	// Instrumentation may discover nested function literals in a separate walk;
	// present all points in original source order rather than allocation order.
	slices.SortFunc(uncovered, func(a, b instrument.Point) int {
		return cmp.Or(
			strings.Compare(a.File, b.File),
			cmp.Compare(a.Range.Start.Line, b.Range.Start.Line),
			cmp.Compare(a.Range.Start.Column, b.Range.Start.Column),
			cmp.Compare(a.Range.End.Line, b.Range.End.Line),
			cmp.Compare(a.Range.End.Column, b.Range.End.Column),
			strings.Compare(reportWord(a), reportWord(b)),
		)
	})
	var sources map[string][][]byte
	if verbose {
		sources = make(map[string][][]byte)
	}
	for _, point := range uncovered {
		var lines [][]byte
		if verbose {
			var err error
			lines, err = sourceRangeLines(point, sources)
			if err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "%s %d:%d %d:%d %s\n",
			point.File, point.Range.Start.Line, point.Range.Start.Column,
			point.Range.End.Line, point.Range.End.Column, reportWord(point)); err != nil {
			return fmt.Errorf("write coverage output: %w", err)
		}
		for _, line := range lines {
			if _, err := fmt.Fprintf(w, "    %s\n", bytes.TrimSuffix(line, []byte("\r"))); err != nil {
				return fmt.Errorf("write coverage source: %w", err)
			}
		}
	}
	return nil
}

// sourceRangeLines ignores columns, including the exclusive end column: the
// complete start and end lines are both displayed. Cache each original file so
// overlapping ranges do not repeatedly read it.
func sourceRangeLines(point instrument.Point, sources map[string][][]byte) ([][]byte, error) {
	lines, ok := sources[point.File]
	if !ok {
		data, err := os.ReadFile(point.File)
		if err != nil {
			return nil, fmt.Errorf("read coverage source %s: %w", point.File, err)
		}
		lines = bytes.Split(data, []byte("\n"))
		// A final newline terminates the last physical line; it does not
		// create another. An empty file likewise has no source lines.
		if len(lines[len(lines)-1]) == 0 {
			lines = lines[:len(lines)-1]
		}
		sources[point.File] = lines
	}
	start, end := point.Range.Start.Line, point.Range.End.Line
	if start < 1 || end < start || end > len(lines) {
		return nil, fmt.Errorf("%s: invalid source line range %d-%d (file has %d lines)",
			point.File, start, end, len(lines))
	}
	return lines[start-1 : end], nil
}

// Other branch outcomes are represented by statement coverage of their bodies,
// not by separate records. Effectively empty bodies have no statement records.
func reportWord(point instrument.Point) string {
	if point.Kind == "statement" {
		return "uncovered"
	}
	if point.Kind == "branch" {
		switch {
		case point.Construct == "if" && point.Outcome == "false",
			point.Construct == "for" && point.Outcome == "skipped",
			(point.Construct == "switch" || point.Construct == "type-switch") && point.Outcome == "no-match":
			return "skipped"
		}
	}
	return ""
}

// readCounters merges complete per-process snapshots. An absent directory (or
// an empty one) means the package never completed a save, so its counters are
// all zero. Temporary files left by an interrupted save are not snapshots.
func readCounters(dir string, count int) ([]uint64, error) {
	if count < 0 {
		return nil, fmt.Errorf("invalid counter count %d", count)
	}
	counts := make([]uint64, count)
	files, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return counts, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read counter directory %s: %w", dir, err)
	}
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, file.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read counter snapshot %s: %w", path, err)
		}
		var snapshot []uint64
		if err := json.Unmarshal(data, &snapshot); err != nil {
			return nil, fmt.Errorf("decode counter snapshot %s: %w", path, err)
		}
		// encoding/json accepts null as a uint64 and silently produces zero.
		// A snapshot contains only numbers, so null anywhere is corrupt data.
		if bytes.Contains(data, []byte("null")) {
			return nil, fmt.Errorf("counter snapshot %s contains null counters", path)
		}
		if len(snapshot) != count {
			return nil, fmt.Errorf("counter snapshot %s has %d counters, want %d", path, len(snapshot), count)
		}
		for i, value := range snapshot {
			counts[i] += value
		}
	}
	return counts, nil
}
