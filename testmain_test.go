package main

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
		{name: "testing alias", src: `package p; import test "testing"; func TestMain(m *test.M){m.Run()}`, want: []string{`defer __reg.Flush();`, `m.Run()`}},
		{name: "testing dot import", src: `package p; import . "testing"; func TestMain(m *M){m.Run()}`, want: []string{`defer __reg.Flush();`, `m.Run()`}},
		{name: "testing type alias", src: `package p; import "testing"; type M = testing.M; func TestMain(m *M){m.Run()}`, want: []string{`defer __reg.Flush();`, `m.Run()`}},
		{name: "unnamed parameter", src: `package p; import "testing"; func TestMain(*testing.M){}`, want: []string{`defer __reg.Flush();`}},
		{name: "syntactic selector", src: `package p; func TestMain(m *anything.M){}`, want: []string{`defer __reg.Flush();`}},
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
	for _, src := range []string{
		`package p;func f(){}`,
		`package p;type S struct{};func(S)TestMain(){}`,
		`package p;import "testing";func TestMain(t *testing.T){t.Fatal("ordinary test")}`,
		`package p;import test "testing";func TestMain(t *test.T){t.Fatal("ordinary test")}`,
		`package p;import . "testing";func TestMain(t *T){t.Fatal("ordinary test")}`,
		`package p;import "testing";type T = testing.T;func TestMain(t *T){t.Fatal("ordinary test")}`,
		`package p;func TestMain(*anything.T){}`,
	} {
		out, found, err := rewriteTestMain("p_test.go", []byte(src), "__reg", "example.com/r")
		if err != nil || found || string(out) != src {
			t.Fatalf("found=%v, err=%v, out=%s", found, err, out)
		}
	}
	for _, src := range []string{
		`package p;func TestMain(`,
		`package p;func TestMain(m int)`,
		`package p;func TestMain(){}`,
		`package p;func TestMain(m *M)`,
		`package p;func TestMain(m *M) int {return 0}`,
		`package p;func TestMain(m, n *M){}`,
		`package p;func TestMain(m *M, n int){}`,
		`package p;func TestMain(m ...*M){}`,
		`package p;func TestMain(m *(M)){}`,
		`package p;type Alias = testing.M;func TestMain(m *Alias){}`,
		`package p;type Alias = testing.T;func TestMain(t *Alias){}`,
		`package p;func TestMain[T any](m *M){}`,
		`package p;func TestMain(m *M){};func TestMain(m *M){}`,
	} {
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
	for name, src := range map[string]string{
		"a_test.go": `package p;func TestMain(m *M){}`,
		"b_test.go": `package p_test;func TestMain(m *M){}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0600); err != nil {
			t.Fatal(err)
		}
	}
	pkg.TestGoFiles = []string{"a_test.go"}
	pkg.XTestGoFiles = []string{"b_test.go"}
	if err := prepareTestMain(pkg, "__cover", "example.com/r", backing, 1, map[string]string{}); err == nil {
		t.Fatal("duplicate TestMain accepted")
	}
}

func TestPrepareTestMainFreeNamespace(t *testing.T) {
	for _, tc := range []struct {
		name, production, internal, external, wantPackage string
		cgo                                               bool
	}{
		{name: "internal ordinary", internal: `func TestMain(t *T){}`, wantPackage: "p_test"},
		{name: "external ordinary", external: `func TestMain(t *T){}`, wantPackage: "p"},
		{name: "production function", production: `func TestMain(){}`, wantPackage: "p_test"},
		{name: "production variable", production: `var other, TestMain int`, wantPackage: "p_test"},
		{name: "production type", production: `type TestMain struct{}`, wantPackage: "p_test"},
		{name: "production constant", production: `const TestMain = 1`, wantPackage: "p_test"},
		{name: "cgo declaration", production: `var TestMain int`, cgo: true, wantPackage: "p_test"},
		{name: "internal test variable", internal: `var TestMain int`, wantPackage: "p_test"},
		{name: "internal test type", internal: `type TestMain int`, wantPackage: "p_test"},
		{name: "method and local", production: `type S struct{};func(S)TestMain(){};func f(){TestMain:=0;_ = TestMain}`, wantPackage: "p"},
		{name: "internal hook external ordinary", internal: `func TestMain(m *M){}`, external: `func TestMain(t *T){}`, wantPackage: "p"},
		{name: "external hook internal ordinary", internal: `func TestMain(t *T){}`, external: `func TestMain(m *M){}`, wantPackage: "p_test"},
		{name: "both ordinary", internal: `func TestMain(t *T){}`, external: `func TestMain(t *T){}`},
		{name: "production and external ordinary", production: `var TestMain int`, external: `func TestMain(t *T){}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pkg := listedPackage{Dir: dir, Name: "p", ImportPath: "example.com/p"}
			sources := map[string]string{}
			for _, file := range []struct {
				name, packageName, source string
				list                      *[]string
			}{
				{"p.go", "p", tc.production, &pkg.GoFiles},
				{"p_test.go", "p", tc.internal, &pkg.TestGoFiles},
				{"x_test.go", "p_test", tc.external, &pkg.XTestGoFiles},
			} {
				if file.source == "" {
					continue
				}
				src := "package " + file.packageName + ";" + file.source
				path := filepath.Join(dir, file.name)
				if err := os.WriteFile(path, []byte(src), 0600); err != nil {
					t.Fatal(err)
				}
				sources[path] = src
				*file.list = append(*file.list, file.name)
			}
			if tc.cgo {
				pkg.CgoFiles, pkg.GoFiles = pkg.GoFiles, nil
			}
			// An inactive file must not reserve a namespace or count as a hook.
			if err := os.WriteFile(filepath.Join(dir, "inactive.go"), []byte(`package p;var TestMain int`), 0600); err != nil {
				t.Fatal(err)
			}
			overlay := map[string]string{}
			err := prepareTestMain(pkg, "__cover", "example.com/r", t.TempDir(), 0, overlay)
			if tc.wantPackage == "" {
				if err == nil || !strings.Contains(err.Error(), "unsupported TestMain") ||
					!strings.Contains(err.Error(), "both p and p_test") {
					t.Fatalf("want clear occupied namespace error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(overlay) != 1 {
				t.Fatalf("want one harness overlay, got %v", overlay)
			}
			for path, backing := range overlay {
				data, err := os.ReadFile(backing)
				if err != nil {
					t.Fatal(err)
				}
				f, err := parser.ParseFile(token.NewFileSet(), path, data, parser.AllErrors)
				if err != nil {
					t.Fatal(err)
				}
				if f.Name.Name != tc.wantPackage {
					t.Errorf("harness package = %s, want %s", f.Name.Name, tc.wantPackage)
				}
				if _, existing := sources[path]; existing && !strings.Contains(string(data), ".Flush()") {
					t.Errorf("existing hook was not rewritten: %s", data)
				}
				if original, existing := sources[path]; existing && strings.Contains(original, "t *T") {
					t.Errorf("ordinary TestMain was rewritten: %s", data)
				}
				if _, existing := sources[path]; !existing && strings.Contains(string(data), pkg.ImportPath) {
					t.Errorf("generated hook imports tested package instead of only registry: %s", data)
				}
			}
			for path, original := range sources {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != original {
					t.Errorf("original changed: %s, %v", data, err)
				}
			}
		})
	}
}
