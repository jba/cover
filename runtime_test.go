package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Exercise the generated runtime independently of the instrumenter, including
// the go command's support for a previously nonexistent overlay directory.
func TestRuntimeVirtualFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, contents string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write("go.mod", "module example.com/runtimefixture\n\ngo 1.24.0\n")
	const prefix = "__cover_runtime_test"
	// Ordinary aliases must not clash with generated imports. In particular,
	// initialization before the generated init must not have its hits reset.
	write("a.go", `package fixture
var os, json, atomic, filepath, strconv = 1, 2, 3, 4, 5
var initialized = initialValue()
func initialValue() int { __cover_runtime_testHit(0); return 42 }
`)
	overlay := map[string]string{}
	registryImport, err := prepareRegistry([]listedPackage{{
		Dir: dir, Module: &listedModule{Path: "example.com/runtimefixture", Dir: dir, Main: true},
	}}, t.TempDir(), overlay)
	if err != nil {
		t.Fatal(err)
	}
	write("a_test.go", fmt.Sprintf(`package fixture
import (
	childOS "os"
	"os/exec"
	"sync"
	"testing"
	registry %q
)
func TestMain(m *testing.M) {
	defer registry.Flush()
	code := m.Run()
	// The deferred save must overwrite, not add a second copy of these hits.
	if got := registry.Finish(code); got != code { childOS.Exit(got) }
}
func TestRuntime(t *testing.T) {
	if initialized != 42 { t.Fatal(initialized) }
	if childOS.Getenv("COVER_RUNTIME_CHILD") == "1" {
		__cover_runtime_testHit(1)
		childOS.Exit(registry.Finish(0))
	}
	type flag bool
	var yes flag = true
	var no flag
	if !__cover_runtime_testBool(2, 3, yes) { t.Fatal("true") }
	if __cover_runtime_testBool(2, 3, no) { t.Fatal("false") }
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 { __cover_runtime_testHit(4) }
		}()
	}
	wg.Wait()
	for range 3 {
		cmd := exec.Command(childOS.Args[0], "-test.run=^TestRuntime$")
		cmd.Env = append(childOS.Environ(), "COVER_RUNTIME_CHILD=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child: %%v\n%%s", err, output)
		}
	}
}
`, registryImport))
	countDir := filepath.Join(dir, "counts")
	if err := os.Mkdir(countDir, 0700); err != nil {
		t.Fatal(err)
	}
	src, err := runtimeSource("fixture", prefix, countDir, registryImport, 5)
	if err != nil {
		t.Fatal(err)
	}
	backing := write("runtime.backing", string(src))
	virtual := filepath.Join(dir, "zz_cover_runtime.go")
	overlay[virtual] = backing
	overlayJSON, err := json.Marshal(map[string]any{"Replace": overlay})
	if err != nil {
		t.Fatal(err)
	}
	overlayPath := write("overlay.json", string(overlayJSON))
	cmd := exec.Command("go", "test", "-vet=off", "-overlay="+overlayPath, "-count=1", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go test with virtual runtime and registry: %v\n%s", err, output)
	}
	for virtual := range overlay {
		if _, err := os.Stat(virtual); !os.IsNotExist(err) {
			t.Fatalf("virtual file exists on disk: %s: %v", virtual, err)
		}
	}
	counts, err := readCounters(countDir, 5)
	if err != nil {
		t.Fatal(err)
	}
	// Global initialization runs in the parent and all three children.
	want := []uint64{4, 3, 1, 1, 8000}
	if !slices.Equal(counts, want) {
		t.Errorf("counters = %v, want %v", counts, want)
	}
	files, err := os.ReadDir(countDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 {
		t.Errorf("got %d process snapshots, want 4", len(files))
	}
	// A save failure must fail the binary, rather than silently reporting
	// uncovered points as though the tests had simply never hit them.
	src, err = runtimeSource("fixture", prefix, filepath.Join(dir, "absent"), registryImport, 5)
	if err != nil {
		t.Fatal(err)
	}
	write("runtime.backing", string(src))
	cmd = exec.Command("go", "test", "-vet=off", "-overlay="+overlayPath, "-count=1", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	if output, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(output), "cover: save counters:") {
		t.Fatalf("save failure: err %v\n%s", err, output)
	}
}

func TestRegistry(t *testing.T) {
	dir := t.TempDir()
	src, err := registrySource()
	if err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		"go.mod":      "module example.com/registryfixture\n\ngo 1.24.0\n",
		"registry.go": string(src),
		"registry_test.go": `package coverruntime
import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)
func TestFinish(t *testing.T) {
	if os.Getenv("COVER_REGISTRY_CHILD") == "1" {
		Register(func() error { return errors.New("fixture save failed") })
		Flush()
		os.Exit(99)
	}
	if got := Finish(7); got != 7 { t.Fatalf("Finish = %d", got) }
	var active, calls atomic.Int32
	var once sync.Once
	Register(func() error {
		if active.Add(1) != 1 { t.Error("concurrent saves") }
		defer active.Add(-1)
		calls.Add(1)
		// Registering during a callback must not deadlock. The new exporter
		// belongs to the next save, not the current callback snapshot.
		once.Do(func() { Register(func() error { calls.Add(1); return nil }) })
		return nil
	})
	if got := Finish(3); got != 3 { t.Fatal(got) }
	if got := calls.Load(); got != 1 { t.Fatalf("first save calls = %d", got) }
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); Finish(0) }()
	}
	wg.Wait()
	if got := calls.Load(); got != 17 { t.Fatalf("save calls = %d", got) }
	var exitCode int
	Exit(func(code int) { exitCode = code }, 19)
	if exitCode != 19 { t.Fatalf("Exit code = %d", exitCode) }
	Register(func() error { return errors.New("save one failed") })
	Register(func() error { calls.Add(1); return nil })
	before := calls.Load()
	if got := Finish(7); got != 2 { t.Fatalf("failed Finish = %d", got) }
	if got := calls.Load(); got != before+3 { t.Fatalf("not all callbacks ran: %d", got) }
	cmd := exec.Command(os.Args[0], "-test.run=^TestFinish$")
	cmd.Env = append(os.Environ(), "COVER_REGISTRY_CHILD=1")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 { t.Fatalf("Flush exit: %v, %s", err, out) }
	if !strings.Contains(string(out), "cover: save counters: fixture save failed") { t.Fatalf("diagnostic: %s", out) }
}
`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "test", "-count=1", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go test registry: %v\n%s", err, output)
	}
}
