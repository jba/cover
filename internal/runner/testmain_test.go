package runner

import (
	"bytes"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRewriteTestMain(t *testing.T) {
	const registry = "example.com/fixture/coverruntime"
	for _, tc := range []struct {
		name, src     string
		want, notWant []string
	}{
		{name: "normal return", src: `package p; import "testing"; func TestMain(m *testing.M){m.Run()}`, want: []string{`defer __reg.Flush();`, `m.Run()`}},
		{name: "direct", src: `package p; import ("os";"testing"); func TestMain(m *testing.M){os.Exit(m.Run())}`, want: []string{`__reg.Exit(os.Exit,m.Run())`}},
		{name: "alias", src: `package p; import (proc "os";"testing"); func TestMain(m *testing.M){proc.Exit(m.Run())}`, want: []string{`__reg.Exit(proc.Exit,m.Run())`}},
		{name: "dot", src: `package p; import (. "os";"testing"); func TestMain(m *testing.M){Exit(m.Run())}`, want: []string{`__reg.Exit(Exit,m.Run())`}},
		{name: "deferred", src: `package p; import ("os";"testing"); func TestMain(m *testing.M){defer os.Exit(0);m.Run()}`, want: []string{`defer __reg.Exit(os.Exit,0);m.Run()`}, notWant: []string{`os.Exit(__reg.Finish`}},
		{name: "goroutine", src: `package p; import ("os";"testing"); func TestMain(m *testing.M){go os.Exit(m.Run())}`, want: []string{`go __reg.Exit(os.Exit,m.Run())`}},
		{name: "parentheses", src: `package p; import ("os";"testing"); func TestMain(m *testing.M){((os.Exit))(m.Run())}`, want: []string{`__reg.Exit(((os.Exit)),m.Run())`}},
		{name: "shadow alias", src: `package p; import ("os";"testing"); type fake struct{Exit func(int)}; func TestMain(m *testing.M){func(os fake){os.Exit(1)}(fake{});os.Exit(m.Run())}`, want: []string{`os.Exit(1)`, `__reg.Exit(os.Exit,m.Run())`}, notWant: []string{`__reg.Exit(os.Exit,1)`}},
		{name: "shadow dot", src: `package p; import (. "os";"testing"); func TestMain(m *testing.M){{Exit:=func(int){};Exit(1)};Exit(m.Run())}`, want: []string{`Exit(1)`, `__reg.Exit(Exit,m.Run())`}, notWant: []string{`__reg.Exit(Exit,1)`}},
		{name: "outside main", src: `package p; import ("os";"testing"); func TestMain(m *testing.M){m.Run()};func helper(){os.Exit(2)}`, want: []string{`func helper(){os.Exit(2)}`}, notWant: []string{`__reg.Exit(os.Exit,2)`}},
		{name: "comments", src: "//go:build example\n\npackage p // package comment\nimport \"testing\"\nfunc TestMain(m *testing.M){ /* body */ m.Run() }", want: []string{"//go:build example\n\npackage p", `// package comment`, `/* body */`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := []byte(tc.src)
			before := bytes.Clone(src)
			out, found, err := rewriteTestMain("main_test.go", src, "__reg", registry)
			if err != nil {
				t.Fatal(err)
			}
			if !found {
				t.Fatal("TestMain not found")
			}
			if !bytes.Equal(src, before) {
				t.Fatal("mutated original source")
			}
			if _, err := parser.ParseFile(token.NewFileSet(), "rewritten_test.go", out, parser.AllErrors); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			for _, s := range tc.want {
				if !strings.Contains(string(out), s) {
					t.Errorf("missing %q in %s", s, out)
				}
			}
			for _, s := range tc.notWant {
				if strings.Contains(string(out), s) {
					t.Errorf("unexpected %q in %s", s, out)
				}
			}
			if strings.Count(string(out), "defer __reg.Flush();") != 1 {
				t.Errorf("wrong number of Flush defers: %s", out)
			}
		})
	}
}

func TestRewriteTestMainAbsentAndErrors(t *testing.T) {
	for _, src := range []string{`package p;func f(){}`, `package p;type S struct{};func(S)TestMain(){}`} {
		out, found, err := rewriteTestMain("p_test.go", []byte(src), "__reg", "example.com/r")
		if err != nil || found || string(out) != src {
			t.Fatalf("found=%v, err=%v, out=%s", found, err, out)
		}
	}
	for _, src := range []string{`package p;func TestMain(`, `package p;func TestMain(m int)`, `package p;func TestMain(){};func TestMain(){}`} {
		if _, _, err := rewriteTestMain("p_test.go", []byte(src), "__reg", "example.com/r"); err == nil {
			t.Fatalf("accepted %s", src)
		}
	}
}

func TestPrepareTestMain(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(map[bool]string{false: "internal", true: "external"}[external], func(t *testing.T) {
			dir := t.TempDir()
			backing := t.TempDir()
			name := "p"
			if external {
				name = "p_test"
			}
			src := "package " + name + `;import "testing";func TestMain(m *testing.M){m.Run()}`
			filename := filepath.Join(dir, "p_test.go")
			if err := os.WriteFile(filename, []byte(src), 0600); err != nil {
				t.Fatal(err)
			}
			pkg := listedPackage{Dir: dir, ImportPath: "example.com/p", Name: "p"}
			if external {
				pkg.XTestGoFiles = []string{"p_test.go"}
			} else {
				pkg.TestGoFiles = []string{"p_test.go"}
			}
			overlay := map[string]string{}
			if err := prepareTestMain(pkg, "__cover", "example.com/r", backing, 0, overlay); err != nil {
				t.Fatal(err)
			}
			if len(overlay) != 1 || overlay[filename] == "" {
				t.Fatalf("overlay=%v", overlay)
			}
			data, err := os.ReadFile(overlay[filename])
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(data, []byte("defer __coverTestRuntime.Flush();")) {
				t.Fatalf("no flush: %s", data)
			}
			original, err := os.ReadFile(filename)
			if err != nil || string(original) != src {
				t.Fatalf("original changed: %s, %v", original, err)
			}
		})
	}
	dir := t.TempDir()
	backing := t.TempDir()
	// Existing names must not be overwritten, whether on disk or in the overlay.
	collision := filepath.Join(dir, "zz__cover_testmain_test.go")
	if err := os.WriteFile(collision, []byte("sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	overlay := map[string]string{filepath.Join(dir, "zz__cover_testmain_x_test.go"): "sentinel"}
	pkg := listedPackage{Dir: dir, Name: "p", ImportPath: "example.com/p"}
	if err := prepareTestMain(pkg, "__cover", "example.com/r", backing, 0, overlay); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(dir, "zz__cover_testmain_x_x_test.go")
	data, err := os.ReadFile(overlay[generated])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "generated.go", data, parser.AllErrors); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(".Finish(m.Run())")) {
		t.Fatalf("no generated finish: %s", data)
	}
	if _, err := os.Stat(generated); !os.IsNotExist(err) {
		t.Fatalf("virtual file exists: %v", err)
	}
	// Duplicate TestMain definitions across internal/external packages are invalid.
	for _, name := range []string{"a_test.go", "b_test.go"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`package p;func TestMain(){}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	pkg.TestGoFiles = []string{"a_test.go", "b_test.go"}
	if err := prepareTestMain(pkg, "__cover", "example.com/r", backing, 1, map[string]string{}); err == nil {
		t.Fatal("duplicate TestMain accepted")
	}
}
