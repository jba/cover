// Package runner implements the cover command.
package runner

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
}

// Run instruments the requested packages, runs their tests, and writes one JSON
// object per uncovered point to stdout (or the file named by -o). All go command
// output is sent to stderr. Coverage is reported even if the tests fail.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var opts options
	flags := flag.NewFlagSet("cover", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.output, "o", "", "write uncovered points as JSONL to file (default stdout)")
	flags.StringVar(&opts.run, "run", "", "run only tests matching the regular expression")
	flags.IntVar(&opts.count, "count", 1, "run each test this many times (disables the test cache)")
	flags.StringVar(&opts.timeout, "timeout", "", "test timeout, as a Go duration (default go test timeout)")
	flags.StringVar(&opts.tags, "tags", "", "comma-separated build tags")
	flags.BoolVar(&opts.race, "race", false, "enable the Go race detector")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: cover [flags] [packages]")
		flags.PrintDefaults()
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
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return fmt.Errorf("cover: unsupported platform %s (supported: linux, darwin)", runtime.GOOS)
	}
	patterns := flags.Args()
	if len(patterns) == 0 {
		patterns = []string{"."}
	}
	pkgs, err := listPackages(ctx, patterns, opts, stderr)
	if err != nil {
		return err
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
	var covered []packageCoverage
	var testPackages []string
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
		// A test-only package may exercise production code in another selected
		// package. Keep it in the test invocation even though it needs no overlay.
		testPackages = append(testPackages, pkg.ImportPath)
		if len(pkg.GoFiles)+len(pkg.CgoFiles) == 0 {
			fmt.Fprintf(stderr, "cover: not instrumenting %s: no non-test Go files (tests will still run)\n", pkg.ImportPath)
			continue
		}
		coverage, err := preparePackage(pkg, tempDir, len(covered), overlay.Replace)
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

	testArgs := []string{"test", "-overlay=" + overlayPath, fmt.Sprintf("-count=%d", opts.count)}
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

	// Do not return on a test failure before reading the shared files. A failed
	// test (including one calling os.Exit) can still produce useful coverage.
	reportErr := writeReport(covered, opts.output, stdout)
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
	countFile string
	size      int
	points    []instrument.Point
}

func preparePackage(pkg listedPackage, tempDir string, number int, overlay map[string]string) (packageCoverage, error) {
	var result packageCoverage
	pkgDir, err := filepath.Abs(pkg.Dir)
	if err != nil {
		return result, fmt.Errorf("package directory %s: %w", pkg.Dir, err)
	}
	files := append(slices.Clone(pkg.GoFiles), pkg.CgoFiles...)
	slices.Sort(files)
	originals := make(map[string][]byte)
	var sourceForPrefix []byte
	// Test files aren't rewritten, but their identifiers must not collide with
	// declarations in the generated file either.
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
	if nextCounter < 0 || nextCounter > int(^uint(0)>>1)/8 {
		return result, errors.New("cover: too many counters")
	}
	result.size = max(nextCounter, 1) * 8
	result.countFile = filepath.Join(tempDir, fmt.Sprintf("pkg%d_counts", number))
	f, err := os.OpenFile(result.countFile, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return result, fmt.Errorf("create counter file: %w", err)
	}
	truncateErr := f.Truncate(int64(result.size))
	closeErr := f.Close()
	if err := errors.Join(truncateErr, closeErr); err != nil {
		return result, fmt.Errorf("initialize counter file: %w", err)
	}
	runtimeSrc, err := runtimeSource(pkg.Name, prefix, result.countFile, result.size)
	if err != nil {
		return result, err
	}
	runtimePath := filepath.Join(tempDir, fmt.Sprintf("pkg%d_runtime.go", number))
	if err := os.WriteFile(runtimePath, runtimeSrc, 0600); err != nil {
		return result, fmt.Errorf("write coverage runtime: %w", err)
	}
	overlay[virtualPath] = runtimePath
	return result, nil
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

func writeReport(packages []packageCoverage, outputPath string, stdout io.Writer) error {
	w := stdout
	if outputPath != "" && outputPath != "-" {
		f, err := os.Create(outputPath)
		if err != nil {
			return fmt.Errorf("create coverage output: %w", err)
		}
		reportErr := encodeReport(packages, f)
		return errors.Join(reportErr, f.Close())
	}
	return encodeReport(packages, w)
}

func encodeReport(packages []packageCoverage, w io.Writer) error {
	var uncovered []instrument.Point
	for _, pkg := range packages {
		counts, err := os.ReadFile(pkg.countFile)
		if err != nil {
			return fmt.Errorf("read counters: %w", err)
		}
		if len(counts) != pkg.size {
			return fmt.Errorf("counter file %s has size %d, want %d", pkg.countFile, len(counts), pkg.size)
		}
		for _, point := range pkg.points {
			if point.Counter < 0 || point.Counter >= len(counts)/8 {
				return fmt.Errorf("invalid counter %d for %s", point.Counter, point.File)
			}
			if binary.NativeEndian.Uint64(counts[point.Counter*8:]) == 0 {
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
			strings.Compare(a.Kind, b.Kind),
			strings.Compare(a.Construct, b.Construct),
			strings.Compare(a.Outcome, b.Outcome),
		)
	})
	enc := json.NewEncoder(w)
	for _, point := range uncovered {
		if err := enc.Encode(point); err != nil {
			return fmt.Errorf("write coverage output: %w", err)
		}
	}
	return nil
}
