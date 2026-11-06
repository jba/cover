package blockcover

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const testPrefix = "_blockcoverTest"

func runCover(t *testing.T, src []byte) (input string, output []byte) {
	t.Helper()
	dir := t.TempDir()
	input, target := filepath.Join(dir, "input.go"), filepath.Join(dir, "native.go")
	if err := os.WriteFile(input, src, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "tool", "cover", "-mode=count", "-var="+testPrefix+"Blocks", "-o", target, input)
	if diagnostics, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go tool cover: %v\n%s", err, diagnostics)
	}
	output, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	return input, output
}

func applyEdits(t *testing.T, src []byte, edits []Edit) []byte {
	t.Helper()
	var out bytes.Buffer
	last := 0
	for _, e := range edits {
		if e.Offset < last || e.Offset > len(src) {
			t.Fatalf("invalid or unsorted insertion %+v after offset %d", e, last)
		}
		out.Write(src[last:e.Offset])
		out.WriteString(e.Text)
		last = e.Offset
	}
	out.Write(src[last:])
	return out.Bytes()
}

// This reader deliberately does not use the production metadata parser.
func nativeNumbers(t *testing.T, output []byte, key string) []int {
	t.Helper()
	pattern := `(?s)\b` + key + `: \[[^\]]+\]uint\d+\{(.*?)\n\t\},`
	m := regexp.MustCompile(pattern).FindSubmatch(output)
	if m == nil {
		t.Fatalf("missing native %s initializer", key)
	}
	text := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(m[1]), "")
	var result []int
	for _, part := range strings.Split(text, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.ParseUint(part, 0, 32)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, int(n))
	}
	return result
}

func checkNativeMetadata(t *testing.T, output []byte, blocks []Block, first int) {
	t.Helper()
	pos, stmts := nativeNumbers(t, output, "Pos"), nativeNumbers(t, output, "NumStmt")
	if len(pos) != 3*len(blocks) || len(stmts) != len(blocks) {
		t.Fatalf("metadata lengths: Pos=%d, NumStmt=%d, blocks=%d", len(pos), len(stmts), len(blocks))
	}
	for i, b := range blocks {
		want := Block{
			StartLine: pos[3*i], EndLine: pos[3*i+1],
			StartColumn: pos[3*i+2] & 65535, EndColumn: pos[3*i+2] >> 16,
			Statements: stmts[i], Counter: first + i,
		}
		if b != want {
			t.Errorf("block %d = %+v, native %+v", i, b, want)
		}
	}
}

func checkNativeBody(t *testing.T, src, native, got []byte, first int) {
	t.Helper()
	headerEnd := bytes.IndexByte(native, '\n') + 1
	declStart := bytes.LastIndex(native, []byte("\nvar "+testPrefix+"Blocks = struct {"))
	if headerEnd < 1 || declStart < headerEnd {
		t.Fatal("unexpected native output envelope")
	}
	probe := regexp.MustCompile(regexp.QuoteMeta(testPrefix) + `Blocks\.Count\[(\d+)\]\+\+;`)
	want := probe.ReplaceAllFunc(native[headerEnd:declStart], func(p []byte) []byte {
		index, err := strconv.Atoi(string(probe.FindSubmatch(p)[1]))
		if err != nil {
			t.Fatal(err)
		}
		return []byte(fmt.Sprintf("%sHit(%d);", testPrefix, first+index))
	})
	if !bytes.Equal(got, want) {
		t.Fatalf("inserted source differs from native probes/wrappers\noriginal:\n%s\ngot:\n%s\nwant:\n%s", src, got, want)
	}
}

func checkCompiles(t *testing.T, output []byte) {
	t.Helper()
	src := append(bytes.Clone(output), []byte("\nfunc "+testPrefix+"Hit(int) {}\n")...)
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, "instrumented.go", src, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse instrumented source: %v\n%s", err, src)
	}
	if _, err := new(types.Config).Check("p", fs, []*ast.File{f}, nil); err != nil {
		t.Fatalf("type-check instrumented source: %v\n%s", err, src)
	}
}

func TestInstrumentNative(t *testing.T) {
	fixtures := map[string]string{
		"no_functions": "package p",
		"empty":        "package p\nfunc Empty() {}\nfunc Semicolon() {;}\n",
		"blank_comments": `//go:build !ignore

// Package p has blank lines and comments.
package p

func f(x int) int {
	// leading comment

	x++

	/* a multiline
	   block comment */
	x += 2
	// trailing comment

	return x
}
`,
		"else_if": `package p
func f(x int) int {
	if x == 0 { x++ } else /* else in a comment */ if x == 1 {
		if x > 0 { x-- } else { x++ }
	} else /* another comment */ { x += 2 }
	if x < 0 {} else {}
	return x
}
`,
		"labels": `package p
func f(x int) int {
	if x < 0 { goto Done }
Loop:
	for x > 0 {
		x--
		if x == 1 { continue Loop }
		if x == 2 { break Loop }
	}
	if x == 7 { goto Again }
Again:
	x++
Done:
	return x
}
func g() {
	goto Empty
Empty:
	;
}
`,
		"switch_select": `package p
func f(x any, ch chan int) int {
	n := 0
	switch n {
	case 0, 1: n++
	case 2:
	default: n--
	}
	switch v := x.(type) {
	case int: n += v
	case string: n += len(v)
	default:
	}
	select {
	case v := <-ch: n += v
	case ch <- n:
	default:
	}
	switch {}
	switch x.(type) {}
	return n
}
func forever() { select {} }
`,
		"func_literals": `package p
var a = func() int { return 1 }()
func f(n int) int {
	g := func(x int) int {
		if x > 0 { return x }
		return -x
	}
	if z := func() int { return n }(); z > 0 { n = g(z) }
	for i := 0; i < 2; i++ { n += func() int { return i }() }
	return g(n)
}
func other() (n int) {
	defer func() { n++ }()
	return
}`,
		"line_directives": `package p
//line other.go:100
func f(x int) int {
	if x > 0 { x++ } else if x == 0 { x-- } else { x = 7 }
	return x
}
//line :8:20
func g() { f(0) }
//line third.go:50:4
func h() {}
`,
		"crlf_unicode": "package p\r\n// αβγ\r\nfunc café(x int) int {\r\n\r\n\tx++\r\n\treturn x\r\n}",
		"consecutive_labels": `package p
func f(x int) int {
	if x < 0 { goto A }
	if x > 0 { goto B }
A:
B:
	x++
	return x
}
`,
	}
	for name, source := range fixtures {
		t.Run(name, func(t *testing.T) {
			src := []byte(source)
			edits, blocks, err := Instrument("original.go", src, testPrefix, 47)
			if err != nil {
				t.Fatal(err)
			}
			_, native := runCover(t, src)
			checkNativeMetadata(t, native, blocks, 47)
			got := applyEdits(t, src, edits)
			checkNativeBody(t, src, native, got, 47)
			checkCompiles(t, got)
			if name == "empty" {
				if len(blocks) == 0 {
					t.Fatal("empty functions lost their zero-statement blocks")
				}
				zero := false
				for _, block := range blocks {
					zero = zero || block.Statements == 0
				}
				if !zero {
					t.Fatal("empty function lost its zero-statement block")
				}
			}
		})
	}
}

func TestNativeCounts(t *testing.T) {
	src := []byte(`package main
var calls = func() int { return 2 }()
func empty() {}
func f(x int) int {
	total := 0
	for i := 0; i < x; i++ {
		if i%2 == 0 { total++ } else if i%3 == 0 { total += 3 } else { total-- }
	}
	if x < 0 { goto Negative }
	switch x {
	case 0: total += 5
	case 2:
		total++
		fallthrough
	default: total += func() int { return x + calls }()
	}
	return total
Negative:
	total--
	return total
}
`)
	input, native := runCover(t, src)
	edits, blocks, err := Instrument("counts.go", src, testPrefix, 103)
	if err != nil {
		t.Fatal(err)
	}
	body := applyEdits(t, src, edits)
	driver := "\nfunc main(){empty(); for _, n := range []int{-2,0,1,2,3,7}{f(n)}; for _, n := range COUNTERS {println(n)}}\n"
	native = append(native, strings.ReplaceAll(driver, "COUNTERS", testPrefix+"Blocks.Count")...)
	body = append(body, fmt.Sprintf("\nvar counts [%d]uint32\nfunc %sHit(n int){ counts[n-103]++ }\n", len(blocks), testPrefix)...)
	body = append(body, strings.ReplaceAll(driver, "COUNTERS", "counts")...)
	run := func(name string, data []byte) []byte {
		t.Helper()
		target := filepath.Join(filepath.Dir(input), name)
		if err := os.WriteFile(target, data, 0600); err != nil {
			t.Fatal(err)
		}
		output, err := exec.Command("go", "run", target).CombinedOutput()
		if err != nil {
			t.Fatalf("go run %s: %v\n%s", name, err, output)
		}
		return output
	}
	if want, got := run("native_counts.go", native), run("our_counts.go", body); !bytes.Equal(got, want) {
		t.Fatalf("counter values differ\ngot:\n%s\nnative:\n%s", got, want)
	}
}

func TestInputErrors(t *testing.T) {
	tests := []struct {
		name, source, prefix, message string
		first                         int
	}{
		{"prefix", "package p", "not-valid", "invalid instrumentation prefix", 0},
		{"keyword", "package p", "for", "invalid instrumentation prefix", 0},
		{"negative", "package p", testPrefix, "negative first counter", -1},
		{"syntax", "package p\nfunc f( {", testPrefix, "error.go", 0},
		{"collision_blocks", "package p\nvar " + testPrefix + "Blocks int", testPrefix, "identifier collision", 0},
		{"collision_hit", "package p\nfunc " + testPrefix + "Hit(int) {}", testPrefix, "identifier collision", 0},
		{"long_line", "package p\n//" + strings.Repeat("x", 65533), testPrefix, "column limit", 0},
		{"directive_column", "package p\n//line huge.go:1:65536\nfunc f() {}", testPrefix, "metadata limits", 0},
		{"counter_overflow", "package p\nfunc f() {}\nfunc g() {}", testPrefix, "counter IDs overflow", int(^uint(0) >> 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			edits, blocks, err := Instrument("error.go", []byte(test.source), test.prefix, test.first)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("got error %v, want containing %q", err, test.message)
			}
			if edits != nil || blocks != nil {
				t.Fatalf("error returned partial instrumentation: %+v, %+v", edits, blocks)
			}
		})
	}
}

func TestCounterBoundary(t *testing.T) {
	max := int(^uint(0) >> 1)
	edits, blocks, err := Instrument("boundary.go", []byte("package p\nfunc f() {}"), testPrefix, max)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].Counter != max || !strings.Contains(edits[0].Text, strconv.Itoa(max)) {
		t.Fatalf("unexpected boundary counter: %+v, %+v", edits, blocks)
	}
}

func TestColumnBoundary(t *testing.T) {
	src := []byte("package p\nfunc f()" + strings.Repeat(" ", 65534-len("func f(){}")) + "{}")
	edits, blocks, err := Instrument("boundary.go", src, testPrefix, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].EndColumn < 65533 {
		t.Fatalf("unexpected boundary block: %+v", blocks)
	}
	_, native := runCover(t, src)
	checkNativeMetadata(t, native, blocks, 0)
	checkNativeBody(t, src, native, applyEdits(t, src, edits), 0)
}

func TestStatementLimit(t *testing.T) {
	src := []byte("package p\nfunc f() {\nx := 0\n" + strings.Repeat("x++\n", 65534) + "}\n")
	_, _, err := Instrument("statement_limit.go", src, testPrefix, 0)
	if err == nil || !strings.Contains(err.Error(), "statement count reaches the legacy 65535 limit") {
		t.Fatalf("missing statement truncation error: %v", err)
	}
}

func TestCoverFailure(t *testing.T) {
	t.Run("missing_tool", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		_, _, err := Instrument("error.go", []byte("package p"), testPrefix, 0)
		if err == nil || !strings.Contains(err.Error(), "go tool cover") {
			t.Fatalf("missing tool error: %v", err)
		}
	})
	t.Run("failed_tool", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "go"), []byte("#!/bin/sh\nprintf 'synthetic cover failure' >&2\nexit 1\n"), 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
		_, _, err := Instrument("error.go", []byte("package p"), testPrefix, 0)
		if err == nil || !strings.Contains(err.Error(), "synthetic cover failure") || !strings.Contains(err.Error(), "error.go") {
			t.Fatalf("missing cover diagnostic: %v", err)
		}
	})
}

func TestRejectChangedOutput(t *testing.T) {
	src := []byte("package p\nfunc f(x int) int {if x > 0 {x++} else {x--}; return x}\n")
	input, native := runCover(t, src)
	mutate := func(old, replacement string) []byte {
		t.Helper()
		if !bytes.Contains(native, []byte(old)) {
			t.Fatalf("mutation target %q not found", old)
		}
		return bytes.Replace(native, []byte(old), []byte(replacement), 1)
	}
	tests := map[string][]byte{
		"header":             mutate("//line ", "//bad "),
		"original_mutation":  mutate("x > 0", "x < 0"),
		"probe_index":        mutate("Blocks.Count[0]++", "Blocks.Count[99999]++"),
		"probe_missing":      mutate(testPrefix+"Blocks.Count[0]++;", ""),
		"probe_decrement":    mutate("Blocks.Count[0]++", "Blocks.Count[0]--"),
		"probe_semicolon":    mutate("Blocks.Count[0]++;", "Blocks.Count[0]++\n"),
		"probe_duplicate":    mutate("Blocks.Count[1]++", "Blocks.Count[0]++"),
		"metadata_sizes":     mutate("Pos       [3 * ", "Pos       [4 * "),
		"metadata_key":       mutate("NumStmt:", "Unknown:"),
		"metadata_type":      mutate("NumStmt   [", "Unknown   ["),
		"declaration_name":   mutate("var "+testPrefix+"Blocks =", "var wrong ="),
		"extra_declaration":  append(bytes.Clone(native), []byte("\nvar unrelated = 1\n")...),
		"trailing_comment":   append(bytes.Clone(native), []byte("// extra\n")...),
		"no_added_newline":   mutate("\n\nvar "+testPrefix+"Blocks", "\nvar "+testPrefix+"Blocks"),
		"extra_body_comment": mutate("package p", "package p /* unexpected */"),
	}
	// Removing just the synthetic braces leaves a valid original else block.
	wrapperStart := bytes.Index(native, []byte("else{"))
	if wrapperStart < 0 {
		t.Fatal("native synthetic else wrapper not found")
	}
	wrapperEnd := bytes.Index(native[wrapperStart:], []byte("}}"))
	if wrapperEnd < 0 {
		t.Fatal("native synthetic else wrapper not found")
	}
	a, b := wrapperStart+len("else"), wrapperStart+wrapperEnd+1
	noWrapper := append(bytes.Clone(native[:a]), native[a+1:b]...)
	noWrapper = append(noWrapper, native[b+1:]...)
	tests["missing_else_wrapper"] = noWrapper
	for name, output := range tests {
		t.Run(name, func(t *testing.T) {
			edits, blocks, err := recoverEdits(src, output, input, testPrefix, 0)
			if err == nil || edits != nil || blocks != nil {
				t.Fatalf("unrecognized change accepted: edits=%+v blocks=%+v error=%v", edits, blocks, err)
			}
		})
	}
}

func TestMapSpans(t *testing.T) {
	edits, err := mapSpans([]byte("ab"), []byte("aXYb"), 0, 4, []span{{2, 3, "second"}, {1, 2, "first"}})
	want := []Edit{{1, "first"}, {1, "second"}}
	if err != nil || !reflect.DeepEqual(edits, want) {
		t.Fatalf("same-offset insertion order: %+v, %v, want %+v", edits, err, want)
	}
	for _, spans := range [][]span{
		{{1, 3, ""}, {2, 3, ""}},
		{{0, 0, ""}},
		{{0, 5, ""}},
		{{-1, 1, ""}},
	} {
		if _, err := mapSpans([]byte("ab"), []byte("aXYb"), 0, 4, spans); err == nil {
			t.Errorf("accepted invalid spans %+v", spans)
		}
	}
}
