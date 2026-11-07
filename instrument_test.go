package main

import (
	"bytes"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPositionsAndOriginalBytes(t *testing.T) {
	src := []byte("//go:build linux\n\npackage sample\n// untouched\nfunc f(a, b bool) {\n\tif a ||\n\t\tb { println(\"yes\") }\n}\n")
	next := 0
	out, points, err := Instrument("sample.go", src, "__testCover", &next)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out, []byte("//go:build linux\n\npackage sample\n// untouched\n")) {
		t.Fatalf("comments changed: %s", out)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "instrumented.go", out, parser.AllErrors); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var branches []Point
	for i, p := range points {
		if p.Counter != i {
			t.Fatalf("counter %d = %d", i, p.Counter)
		}
		if p.Kind == "branch" {
			branches = append(branches, p)
		}
	}
	if len(branches) != 2 {
		t.Fatalf("branches: %+v", branches)
	}
	want := Range{Position{6, 5}, Position{7, 4}}
	for _, p := range branches {
		if p.Range != want {
			t.Errorf("range = %+v; want %+v", p.Range, want)
		}
	}
	if next != len(points) {
		t.Fatalf("next = %d, points=%d", next, len(points))
	}
}

func TestLineDirectiveUsesPhysicalPositions(t *testing.T) {
	src := []byte("package p\n//line imaginary.go:500\nfunc f(x bool) { if x {} }\n")
	n := 0
	_, ps, err := Instrument("physical.go", src, "__c", &n)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.File != "physical.go" || p.Range.Start.Line != 3 {
			t.Errorf("point: %+v", p)
		}
	}
}

func TestRejectedInput(t *testing.T) {
	for _, tc := range []struct{ name, src, prefix, contains string }{
		{"invalid", "package p; func f(", "__c", ""},
		{"prefix", "package p", "for", "prefix"},
		{"collision", "package p; var __cHit int", "__c", "collision"},
		{"local collision", "package p; func f(){ var __cLocal1 int; _ = __cLocal1; for {break} }", "__c", "collision"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := 0
			_, _, err := Instrument("x.go", []byte(tc.src), tc.prefix, &n)
			if err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestInstrumentedSemantics(t *testing.T) {
	// Compile as well as parse: termination rules, label targets, and variable
	// scopes are all semantic constraints the parser alone cannot check.
	src := []byte(`package sample
import "testing"
type Flag bool
func namedBool(x Flag) int {if x {return 1};return 0}
var initialized = choose(true, false)
func choose(a,b bool) int { if x:=2; a { return x } else if b {return 3} else {return 4} }
func switches(x int) int {
 switch x {case 0: return 0; case 1: fallthrough; default: return 2}
}
func missingDefault(x int) {switch x {case 1: println("one")}}
func typeSwitch(x any) int {switch x.(type) {case int:return 1;default:return 2}}
func typeNoDefault(x any) {switch x.(type) {case int:}}
func forever() int {for {return 9}}
func loops() int {
 sum:=0
outer:
 for i:=0;i<3;i++ {
  for j:=0;j<3;j++ {if j==1 {continue outer};sum++}
 }
stop:
 for {sum++;break stop}
 for i:=range 0 {sum+=i}
 for range 2 {sum++}
 var i int8
 for i=range 2 {sum+=int(i)}
 for range []int{1,2} {sum++}
 for range "é" {sum++}
 for range func(yield func(int)bool){yield(1);yield(2)} {sum++}
 for ;false; {} // empty body
 return sum
}
func selectValue() int {ch:=make(chan int,1);ch<-3;select {case x:=<-ch:return x;default:return 9}}
func selectDefault() {var ch chan int;select {case <-ch:default:}}
func neverReturning() int {select{}}
func conditionPanic() {defer func(){recover()}();for panicBool() {}}
func rangePanic() {defer func(){recover()}();for range panicSlice() {}}
func panicBool()bool {panic("condition")}
func panicSlice()[]int {panic("range")}
func literals()int {f:=func(x bool)int {if x{return 2};return 3};return f(true)}
func TestSemantics(t *testing.T) {
 if namedBool(Flag(true))!=1 || namedBool(Flag(false))!=0 {t.Fatal("named bool")}
 if initialized!=2 || choose(false,true)!=3 || choose(false,false)!=4 {t.Fatal("if")}
 if switches(0)!=0||switches(1)!=2||switches(9)!=2||typeSwitch(0)!=1||typeSwitch("")!=2 {t.Fatal("switch")}
 missingDefault(0);typeNoDefault("")
 if loops()!=12 {t.Fatal("loops",loops())}
 if forever()!=9||selectValue()!=3||literals()!=2 {t.Fatal("return")}
 selectDefault();conditionPanic();rangePanic()
}
`)
	n := 0
	out, points, err := Instrument("sample_test.go", src, "__coverTest", &n)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", []byte("module example.com/instrument-test\n\ngo 1.23\n"))
	write("sample_test.go", out)
	write("runtime_test.go", []byte(fmt.Sprintf(`package sample
import "sync/atomic"
var counts [%d]atomic.Uint64
func __coverTestHit(i int) {counts[i].Add(1)}
func __coverTestBool[T ~bool](t,f int,b T)T {if b {__coverTestHit(t)}else{__coverTestHit(f)};return b}
`, n)))
	cmd := exec.Command("go", "test", "-count=1", ".")
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s\nInstrumented:\n%s", err, output, out)
	}
	if len(points) == 0 {
		t.Fatal("no points")
	}
}
