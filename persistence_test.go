package main

import (
	"strings"
	"testing"
)

func TestNormalReturnTestMainSavesAfterDefers(t *testing.T) {
	const src = `package fixture
func Before(yes bool) {
	if yes { return } // before
}
func During(yes bool) {
	if yes { return } // during
}
func After(yes bool) {
	if yes { return } // after
}
func Deferred(yes bool) {
	if yes { return } // deferred
}
`
	f := newFixture(t, map[string]string{
		"go.mod":   fixtureMod,
		"value.go": src,
		"value_test.go": `package fixture
import "testing"
func TestMain(m *testing.M) {
	Before(true)
	defer Deferred(true)
	m.Run()
	After(true)
}
func TestValue(t *testing.T) { During(true) }
`,
	}, ".")
	points := f.success(t)
	for _, marker := range []string{"// before", "// during", "// after", "// deferred"} {
		wantBranch(t, points, f.path("value.go"), src, marker, "if", "false", true)
		wantStatement(t, points, f.path("value.go"), src, marker, "return", false)
	}
}

func TestPackageWithoutTestsPersistsInitializers(t *testing.T) {
	const src = `package fixture
var Value = initialize(true)
func initialize(yes bool) int {
	if yes { return 1 }
	return 0
}
`
	f := newFixture(t, map[string]string{
		"go.mod":   fixtureMod,
		"value.go": src,
	}, ".")
	// No original _test.go exists: the generated TestMain must make go test
	// execute the package's global initializer and then save its counters.
	points := f.success(t)
	wantBranch(t, points, f.path("value.go"), src, "if yes", "if", "false", true)
	wantStatement(t, points, f.path("value.go"), src, "if yes", "return 1", false)
}

func TestProductionMainPackage(t *testing.T) {
	const src = `package main
func main() { Value(true) }
func Value(yes bool) int {
	if yes { return 1 }
	return 0
}
`
	f := newFixture(t, map[string]string{
		"go.mod":  fixtureMod,
		"main.go": src,
		"main_test.go": `package main
import "testing"
func TestMainFunction(t *testing.T) { main() }
`,
	}, ".")
	points := f.success(t)
	wantBranch(t, points, f.path("main.go"), src, "if yes", "if", "false", true)
	wantStatement(t, points, f.path("main.go"), src, "if yes", "return 1", false)
	for _, p := range points {
		if p.Kind == "statement" && p.Range.Start.Line == sourceLine(t, src, "func main()") {
			t.Errorf("covered production main reported as uncovered: %+v", p)
		}
	}
}

func TestSelectedInternalDependencyAggregatesTestBinaries(t *testing.T) {
	const src = `package secret
func Value(n int) int {
	if n < 0 { return -1 }
	if n == 0 { return 0 }
	return 1
}
`
	f := newFixture(t, map[string]string{
		"go.mod": fixtureMod,
		"root.go": `package fixture
func Value() int { return 42 }
`,
		"root_test.go": `package fixture
import "testing"
func TestRoot(t *testing.T) { if Value() != 42 { t.Fatal("root changed") } }
`,
		"nested/internal/secret/value.go": src,
		"nested/negative/value_test.go": `package negative_test
import (
	"testing"
	"example.com/fixture/nested/internal/secret"
)
func TestNegative(t *testing.T) {
	if secret.Value(-1) != -1 { t.Fatal("negative changed") }
}
`,
		"nested/nonnegative/value_test.go": `package nonnegative_test
import (
	"os"
	"testing"
	"example.com/fixture/nested/internal/secret"
)
func TestMain(m *testing.M) { os.Exit(m.Run()) }
func TestNonnegative(t *testing.T) {
	if secret.Value(0) != 0 || secret.Value(1) != 1 { t.Fatal("nonnegative changed") }
}
`,
	}, ".")
	// The two test-only packages execute disjoint dependency paths. Neither
	// the root package nor a registry under the module root may import this
	// nested internal package directly.
	if points := f.success(t, "./..."); len(points) != 0 {
		t.Errorf("independent binaries did not collectively cover the selected internal dependency: %+v", points)
	}
}

func TestSelectedReplacedModuleSharesRegistry(t *testing.T) {
	f := newFixture(t, map[string]string{
		"main/go.mod": "module example.com/fixture\n\ngo 1.24.0\n\nrequire example.com/sibling v0.0.0\n\nreplace example.com/sibling => ../sibling\n",
		"main/value.go": `package fixture
import "example.com/sibling"
func Value() int { return sibling.Value(true) }
`,
		"main/value_test.go": `package fixture
import "testing"
func TestValue(t *testing.T) { if Value() != 1 { t.Fatal("main changed") } }
`,
		"sibling/go.mod": "module example.com/sibling\n\ngo 1.24.0\n",
		"sibling/value.go": `package sibling
func Value(yes bool) int {
	if yes { return 1 }
	return 0
}
`,
		"sibling/value_test.go": `package sibling
import "testing"
func TestValue(t *testing.T) { if Value(false) != 0 { t.Fatal("sibling changed") } }
`,
	}, "main")
	// The selected dependency's source lives outside the main module, and its
	// two outcomes are exercised by different modules' test binaries.
	if points := f.success(t, ".", "example.com/sibling"); len(points) != 0 {
		t.Errorf("selected replacement module did not share snapshots: %+v", points)
	}
}

func TestSubprocessTestMainSnapshotsAggregate(t *testing.T) {
	const src = `package fixture
func Value(n int) int {
	if n < 0 { return -1 }
	if n == 0 { return 0 }
	return 1
}
`
	f := newFixture(t, map[string]string{
		"go.mod":   fixtureMod,
		"value.go": src,
		"value_test.go": `package fixture
import (
	"os"
	"os/exec"
	"testing"
)
func TestMain(m *testing.M) { os.Exit(m.Run()) }
func TestSubprocess(t *testing.T) {
	switch os.Getenv("COVER_FIXTURE_CHILD") {
	case "zero":
		if Value(0) != 0 { t.Fatal("zero changed") }
		return
	case "positive":
		if Value(1) != 1 { t.Fatal("positive changed") }
		return
	}
	if Value(-1) != -1 { t.Fatal("negative changed") }
	for _, mode := range []string{"zero", "positive", "positive"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSubprocess$")
		cmd.Env = append(os.Environ(), "COVER_FIXTURE_CHILD="+mode)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child %s: %v\n%s", mode, err, output)
		}
	}
}
`,
	}, ".")
	t.Setenv("COVER_FIXTURE_CHILD", "")
	if points := f.success(t); len(points) != 0 {
		t.Errorf("parent and repeated subprocess snapshots did not aggregate: %+v", points)
	}
}

func TestAbnormalTestExitReturnsErrorAndPartialReport(t *testing.T) {
	for _, tc := range []struct {
		name, body, diagnostic string
	}{
		{"os.Exit", `fmt.Println("ABNORMAL EXIT MARKER"); os.Exit(3)`, "ABNORMAL EXIT MARKER"},
		{"panic", `panic("ABNORMAL PANIC MARKER")`, "ABNORMAL PANIC MARKER"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, map[string]string{
				"go.mod":   fixtureMod,
				"value.go": "package fixture\nfunc Value() int { return 1 }\n",
				"value_test.go": `package fixture
import (
	"fmt"
	"os"
	"testing"
)
var _ = fmt.Println
var _ = os.Exit
func TestAbnormal(t *testing.T) { Value(); ` + tc.body + ` }
`,
			}, ".")
			out, diagnostic, err := f.run(t)
			if err == nil {
				t.Fatalf("Run succeeded for abnormal %s\nstderr:\n%s", tc.name, diagnostic)
			}
			if !strings.Contains(diagnostic, tc.diagnostic) || !strings.Contains(diagnostic, "FAIL") {
				t.Errorf("lost abnormal test diagnostics:\n%s", diagnostic)
			}
			// Neither an exit in a test function nor an unrecovered test
			// panic is required to execute TestMain's final snapshot.
			decodePoints(t, out)
		})
	}
}
