// Package instrument adds coverage bookkeeping without relocating source files.
package instrument

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
)

// Position is a 1-based source line and byte column.
type Position struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

// Range is a half-open range in the original source.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Point identifies a statement or a control-flow outcome.
type Point struct {
	File      string `json:"file"`
	Kind      string `json:"kind"`
	Construct string `json:"construct,omitempty"`
	Outcome   string `json:"outcome,omitempty"`
	Range     Range  `json:"range"`
	Counter   int    `json:"-"`
}

type insertion struct {
	offset int
	text   string
}
type instrumenter struct {
	filename string
	src      []byte
	fset     *token.FileSet
	file     *token.File
	prefix   string
	next     *int
	points   []Point
	edits    []insertion
	serial   int
	fn       *function
}

// Labels have function scope. Bookkeeping for a labelled statement normally
// precedes its labels, so a goto must replay that bookkeeping before jumping.
// Keep this separate from statement traversal to handle forward references.
type function struct {
	labels   map[string]ast.Stmt
	gotos    []*ast.BranchStmt
	preludes map[ast.Stmt]string
	locals   []string
}

// Instrument inserts references to prefix+Hit(int) and prefix+Bool(int,int,bool).
// It preserves all original bytes, including build constraints and comments.
// Counter IDs are allocated starting at *nextCounter.
func Instrument(filename string, src []byte, prefix string, nextCounter *int) ([]byte, []Point, error) {
	if !token.IsIdentifier(prefix) || token.Lookup(prefix).IsKeyword() {
		return nil, nil, fmt.Errorf("invalid instrumentation prefix %q", prefix)
	}
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, filename, src, parser.ParseComments|parser.AllErrors)
	if err != nil {
		return nil, nil, err
	}
	var unsupported error
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			if strings.HasPrefix(n.Name, prefix) {
				unsupported = fmt.Errorf("%s: instrumentation identifier collision", fs.Position(n.Pos()))
			}
		}
		return true
	})
	if unsupported != nil {
		return nil, nil, unsupported
	}
	in := &instrumenter{filename: filename, src: src, fset: fs, file: fs.File(f.Pos()), prefix: prefix, next: nextCounter}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			if n.Body != nil {
				in.function(n.Body)
			}
		case *ast.FuncLit:
			in.function(n.Body)
		}
		return true
	})
	sort.SliceStable(in.edits, func(i, j int) bool { return in.edits[i].offset < in.edits[j].offset })
	var out bytes.Buffer
	pos := 0
	for _, e := range in.edits {
		out.Write(src[pos:e.offset])
		out.WriteString(e.text)
		pos = e.offset
	}
	out.Write(src[pos:])
	return out.Bytes(), in.points, nil
}

func (in *instrumenter) insert(p token.Pos, s string) {
	in.edits = append(in.edits, insertion{in.file.Offset(p), s})
}
func (in *instrumenter) position(p token.Pos) Position {
	// Ignore //line directives: ranges always address the physical original file.
	q := in.fset.PositionFor(p, false)
	return Position{q.Line, q.Column}
}
func (in *instrumenter) point(kind, construct, outcome string, start, end token.Pos) int {
	id := *in.next
	*in.next++
	in.points = append(in.points, Point{File: in.filename, Kind: kind, Construct: construct, Outcome: outcome, Range: Range{in.position(start), in.position(end)}, Counter: id})
	return id
}
func (in *instrumenter) hit(id int) string { return in.prefix + "Hit(" + strconv.Itoa(id) + ");" }
func (in *instrumenter) local() string {
	in.serial++
	name := fmt.Sprintf("%sLocal%d", in.prefix, in.serial)
	in.fn.locals = append(in.fn.locals, name)
	return name
}

func (in *instrumenter) function(body *ast.BlockStmt) {
	fn := &function{
		labels:   make(map[string]ast.Stmt),
		preludes: make(map[ast.Stmt]string),
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			// The outer AST walk instruments this function separately.
			return false
		case *ast.LabeledStmt:
			target := n.Stmt
			for {
				label, ok := target.(*ast.LabeledStmt)
				if !ok {
					break
				}
				target = label.Stmt
			}
			fn.labels[n.Label.Name] = target
		case *ast.BranchStmt:
			if n.Tok == token.GOTO {
				fn.gotos = append(fn.gotos, n)
			}
		}
		return true
	})
	old := in.fn
	in.fn = fn
	// Reserve the insertion before walking the body: declarations must precede
	// other edits at the same offset (e.g. func f(){for ...}).
	declarations := len(in.edits)
	in.insert(body.Lbrace+1, "")
	in.block(body)
	for _, name := range fn.locals {
		// Predeclared bool/true/false may be shadowed by original declarations.
		in.edits[declarations].text += "var " + name + "=0!=0;"
	}
	for _, g := range fn.gotos {
		// Insert at the goto keyword, not before its labels: a goto can itself
		// be a jump target and must still replay its outgoing target's prelude.
		in.insert(g.Pos(), fn.preludes[fn.labels[g.Label.Name]])
	}
	in.fn = old
}

func (in *instrumenter) prelude(s ast.Stmt, anchor token.Pos, text string) {
	in.fn.preludes[s] += text
	in.insert(anchor, text)
}
func (in *instrumenter) block(b *ast.BlockStmt) {
	for _, s := range b.List {
		in.stmt(s, s.Pos())
	}
}

func (in *instrumenter) stmt(s ast.Stmt, anchor token.Pos) {
	if l, ok := s.(*ast.LabeledStmt); ok {
		in.stmt(l.Stmt, anchor)
		return
	}
	switch s.(type) {
	case *ast.EmptyStmt:
		return
	case *ast.BlockStmt:
		in.block(s.(*ast.BlockStmt))
		return
	}
	end := s.End()
	switch n := s.(type) {
	case *ast.IfStmt:
		end = n.Body.Lbrace
	case *ast.ForStmt:
		end = n.Body.Lbrace
	case *ast.RangeStmt:
		end = n.Body.Lbrace
	case *ast.SwitchStmt:
		end = n.Body.Lbrace
	case *ast.TypeSwitchStmt:
		end = n.Body.Lbrace
	case *ast.SelectStmt:
		end = n.Body.Lbrace
	}
	id := in.point("statement", "", "", s.Pos(), end)
	in.prelude(s, anchor, in.hit(id))
	switch n := s.(type) {
	case *ast.IfStmt:
		t := in.point("branch", "if", "true", n.Cond.Pos(), n.Cond.End())
		f := in.point("branch", "if", "false", n.Cond.Pos(), n.Cond.End())
		in.insert(n.Cond.Pos(), fmt.Sprintf("%sBool(%d,%d,(", in.prefix, t, f))
		in.insert(n.Cond.End(), "))")
		in.block(n.Body)
		if n.Else != nil {
			if _, ok := n.Else.(*ast.IfStmt); ok {
				// An inserted statement cannot stand between else and if. Make its
				// implicit else block explicit, preserving the original lexical scope.
				in.insert(n.Else.Pos(), "{")
				in.stmt(n.Else, n.Else.Pos())
				in.insert(n.Else.End(), "}")
			} else {
				in.stmt(n.Else, n.Else.Pos())
			}
		}
	case *ast.ForStmt:
		in.loop(n, anchor, n.Body, n.Cond != nil)
	case *ast.RangeStmt:
		in.loop(n, anchor, n.Body, true)
	case *ast.SwitchStmt:
		in.clauses("switch", n, anchor, n.Body)
	case *ast.TypeSwitchStmt:
		in.clauses("type-switch", n, anchor, n.Body)
	case *ast.SelectStmt:
		for _, s := range n.Body.List {
			c := s.(*ast.CommClause)
			outcome := "case"
			if c.Comm == nil {
				outcome = "default"
			}
			id := in.point("branch", "select", outcome, c.Pos(), c.Colon+1)
			in.insert(c.Colon+1, in.hit(id))
			for _, s := range c.Body {
				in.stmt(s, s.Pos())
			}
		}
	}
}

func (in *instrumenter) loop(s ast.Stmt, anchor token.Pos, body *ast.BlockStmt, canSkip bool) {
	flag := in.local()
	in.prelude(s, anchor, flag+"=0!=0;")
	entry := in.point("branch", "for", "entered", s.Pos(), body.Lbrace)
	in.insert(body.Lbrace+1, "if !"+flag+" {"+flag+"=0==0;"+in.hit(entry)+"};")
	in.block(body)
	if canSkip {
		skipped := in.point("branch", "for", "skipped", s.Pos(), body.Lbrace)
		in.insert(s.End(), ";if !"+flag+" {"+in.hit(skipped)+"};")
	}
}

func (in *instrumenter) clauses(construct string, s ast.Stmt, anchor token.Pos, body *ast.BlockStmt) {
	hasDefault := false
	for _, s := range body.List {
		if s.(*ast.CaseClause).List == nil {
			hasDefault = true
		}
	}
	flag := ""
	if !hasDefault {
		flag = in.local()
		in.prelude(s, anchor, flag+"=0!=0;")
	}
	for _, s := range body.List {
		c := s.(*ast.CaseClause)
		outcome := "case"
		if c.List == nil {
			outcome = "default"
		}
		id := in.point("branch", construct, outcome, c.Pos(), c.Colon+1)
		text := in.hit(id)
		if flag != "" {
			text = flag + "=0==0;" + text
		}
		in.insert(c.Colon+1, text)
		for _, s := range c.Body {
			in.stmt(s, s.Pos())
		}
	}
	if flag != "" {
		id := in.point("branch", construct, "no-match", s.Pos(), body.Lbrace)
		in.insert(s.End(), ";if !"+flag+" {"+in.hit(id)+"};")
	}
}
