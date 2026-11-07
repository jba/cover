package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestOrdinaryTestMainCompatibility(t *testing.T) {
	// Every case runs with the same exact selection as native go test. The
	// ordinary test checks its name too: renaming it is not a compatible fix.
	const ordinary = `func TestMain(t *testing.T) {
	if t.Name() != "TestMain" { t.Fatalf("test renamed to %q", t.Name()) }
	Ordinary(true)
}
func TestNotSelected(t *testing.T) { t.Fatal("test selection changed") }
`
	const externalOrdinary = `func TestMain(t *testing.T) {
	if t.Name() != "TestMain" { t.Fatalf("test renamed to %q", t.Name()) }
	fixture.Ordinary(true)
}
func TestNotSelected(t *testing.T) { t.Fatal("test selection changed") }
`
	for _, tc := range []struct {
		name, packageName, internal, external string
		hook                                  bool
	}{
		{
			name: "internal ordinary",
			internal: `import "testing"
` + ordinary,
		},
		{
			name: "external ordinary",
			external: `import ("testing"; "example.com/fixture")
` + externalOrdinary,
		},
		{
			name: "internal ordinary dot import external hook",
			internal: `import . "testing"
` + strings.ReplaceAll(ordinary, "*testing.T", "*T"),
			external: `import ("os"; test "testing"; "example.com/fixture")
func TestMain(m *test.M) { fixture.Hook(true); os.Exit(m.Run()) }
`,
			hook: true,
		},
		{
			name: "external ordinary type alias internal hook",
			internal: `import . "testing"
func TestMain(m *M) { Hook(true); m.Run() }
`,
			external: `import (test "testing"; "example.com/fixture")
type T = test.T
` + strings.ReplaceAll(externalOrdinary, "*testing.T", "*T"),
			hook: true,
		},
		{
			name: "internal ordinary import alias",
			internal: `import test "testing"
` + strings.ReplaceAll(ordinary, "*testing.T", "*test.T"),
		},
		{
			name:        "main package internal ordinary external generated hook",
			packageName: "main",
			internal: `import "testing"
` + ordinary,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := tc.packageName
			if name == "" {
				name = "fixture"
			}
			src := "package " + name + `
func Ordinary(yes bool) {
	if yes { return } // ordinary
}
func Hook(yes bool) {
	if yes { return } // hook
}
`
			if name == "main" {
				src += "func main() { Ordinary(true) }\n"
			}
			files := map[string]string{"go.mod": fixtureMod, "value.go": src}
			if tc.internal != "" {
				files["value_test.go"] = "package " + name + "\n" + tc.internal
			}
			if tc.external != "" {
				files["external_test.go"] = "package " + name + "_test\n" + tc.external
			}
			f := newFixture(t, files, ".")
			nativeTestMainSelection(t)
			points := f.success(t, "-run", "^TestMain$")
			wantBranch(t, points, f.path("value.go"), src, "// ordinary", "if", "false", true)
			wantStatement(t, points, f.path("value.go"), src, "// ordinary", "return", false)
			wantBranch(t, points, f.path("value.go"), src, "// hook", "if", "false", true)
			wantStatement(t, points, f.path("value.go"), src, "// hook", "return", !tc.hook)
		})
	}
}

func TestProductionTestMainUsesExternalGeneratedHook(t *testing.T) {
	for _, declaration := range []string{
		"func TestMain() { Value(false) }",
		"var TestMain int",
		"type TestMain struct{}",
	} {
		t.Run(declaration, func(t *testing.T) {
			src := `package main
func main() { Value(false) }
func Value(yes bool) {
	if yes { return }
}
` + declaration + "\n"
			f := newFixture(t, map[string]string{
				"go.mod":   fixtureMod,
				"value.go": src,
				"value_test.go": `package main
import "testing"
func TestValue(t *testing.T) { Value(true) }
`,
			}, ".")
			points := f.success(t)
			wantBranch(t, points, f.path("value.go"), src, "if yes", "if", "false", true)
			wantStatement(t, points, f.path("value.go"), src, "if yes", "return", false)
		})
	}
}

func TestOrdinaryTestMainBothNamespacesUnsupported(t *testing.T) {
	f := newFixture(t, map[string]string{
		"go.mod":   fixtureMod,
		"value.go": "package fixture\nfunc Value() int { return 1 }\n",
		"value_test.go": `package fixture
import "testing"
func TestMain(t *testing.T) {
	if t.Name() != "TestMain" { t.Fatal(t.Name()) }
}
`,
		"external_test.go": `package fixture_test
import "testing"
func TestMain(t *testing.T) {
	if t.Name() != "TestMain" { t.Fatal(t.Name()) }
}
`,
	}, ".")
	// Go permits both ordinary TestMain tests, but adding a coverage hook
	// cannot preserve both names when neither package namespace is free.
	nativeTestMainSelection(t)
	out, diagnostic, err := f.run(t, "-run", "^TestMain$")
	if err == nil || !strings.Contains(err.Error(), "unsupported TestMain") ||
		!strings.Contains(err.Error(), "both fixture and fixture_test") {
		t.Fatalf("want clear occupied namespace error, got %v\nstderr:\n%s\nstdout:\n%s", err, diagnostic, out)
	}
}

func nativeTestMainSelection(t *testing.T) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "go", "test", "-count=1", "-run=^TestMain$", ".")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native go test: %v\n%s", err, output)
	}
}
