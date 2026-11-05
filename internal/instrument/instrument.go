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
		case *ast.BranchStmt:
			if n.Tok == token.GOTO {
				unsupported = fmt.Errorf("%s: goto is not yet supported", fs.Position(n.Pos()))
			}
		case *ast.Ident:
			if n.Name == prefix+"Hit" || n.Name == prefix+"Bool" {
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
				in.block(n.Body)
			}
		case *ast.FuncLit:
			in.block(n.Body)
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
	return fmt.Sprintf("%sLocal%d", in.prefix, in.serial)
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
	in.insert(anchor, in.hit(id))
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
		in.loop(n.Pos(), n.End(), anchor, n.Body, n.Cond != nil)
	case *ast.RangeStmt:
		in.loop(n.Pos(), n.End(), anchor, n.Body, true)
	case *ast.SwitchStmt:
		in.clauses("switch", n.Pos(), n.End(), anchor, n.Body)
	case *ast.TypeSwitchStmt:
		in.clauses("type-switch", n.Pos(), n.End(), anchor, n.Body)
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

func (in *instrumenter) loop(start, end, anchor token.Pos, body *ast.BlockStmt, canSkip bool) {
	flag := in.local()
	in.insert(anchor, "{var "+flag+" bool;")
	entry := in.point("branch", "for", "entered", start, body.Lbrace)
	in.insert(body.Lbrace+1, "if !"+flag+" {"+flag+"=true;"+in.hit(entry)+"};")
	in.block(body)
	if canSkip {
		skipped := in.point("branch", "for", "skipped", start, body.Lbrace)
		in.insert(end, ";if !"+flag+" {"+in.hit(skipped)+"};}")
	} else {
		in.insert(end, "}")
	}
}

func (in *instrumenter) clauses(construct string, start, end, anchor token.Pos, body *ast.BlockStmt) {
	hasDefault := false
	for _, s := range body.List {
		if s.(*ast.CaseClause).List == nil {
			hasDefault = true
		}
	}
	flag := ""
	if !hasDefault {
		flag = in.local()
		in.insert(anchor, "{var "+flag+" bool;")
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
			text = flag + "=true;" + text
		}
		in.insert(c.Colon+1, text)
		for _, s := range c.Body {
			in.stmt(s, s.Pos())
		}
	}
	if flag != "" {
		id := in.point("branch", construct, "no-match", start, body.Lbrace)
		in.insert(end, ";if !"+flag+" {"+in.hit(id)+"};}")
	}
}
