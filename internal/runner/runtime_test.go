package runner

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// Exercise the generated runtime independently of the instrumenter, including
// the go command's support for adding a previously nonexistent overlay file.
func TestRuntimeVirtualFile(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("mmap runtime is supported on Linux and macOS")
	}
	dir := t.TempDir()
	write := func(name, contents string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write("go.mod", "module example.com/runtimefixture\n\ngo 1.23\n")
	const prefix = "__cover_runtime_test"
	// These ordinary aliases would clash with unaliased runtime imports.
	write("a.go", `package fixture
var os, syscall, atomic, unsafe = 1, 2, 3, 4
var initialized = initialValue()
func initialValue() int { __cover_runtime_testHit(0); return 42 }
`)
	write("a_test.go", `package fixture
import (
	childOS "os"
	"os/exec"
	"sync"
	"testing"
)
func TestRuntime(t *testing.T) {
	if initialized != 42 { t.Fatal(initialized) }
	if childOS.Getenv("COVER_RUNTIME_CHILD") == "1" {
		__cover_runtime_testHit(1)
		childOS.Exit(0)
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
			t.Fatalf("child: %v\n%s", err, output)
		}
	}
}
`)
	countPath := write("counts", string(make([]byte, 5*8)))
	src, err := runtimeSource("fixture", prefix, countPath, 5*8)
	if err != nil {
		t.Fatal(err)
	}
	backing := write("runtime.backing", string(src))
	virtual := filepath.Join(dir, "zz_cover_runtime.go")
	overlay, err := json.Marshal(map[string]any{"Replace": map[string]string{virtual: backing}})
	if err != nil {
		t.Fatal(err)
	}
	overlayPath := write("overlay.json", string(overlay))
	cmd := exec.Command("go", "test", "-overlay="+overlayPath, "-count=1", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go test with virtual runtime: %v\n%s", err, output)
	}
	if _, err := os.Stat(virtual); !os.IsNotExist(err) {
		t.Fatalf("virtual runtime exists on disk: %v", err)
	}
	counts, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatal(err)
	}
	// Package global initialization runs in the parent and all three children.
	for i, want := range []uint64{4, 3, 1, 1, 8000} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			if got := binary.NativeEndian.Uint64(counts[i*8:]); got != want {
				t.Errorf("counter %d = %d, want %d", i, got, want)
			}
		})
	}
}
