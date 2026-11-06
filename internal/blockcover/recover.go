package blockcover

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
)

// A span identifies bytes inserted by cover in its output, not original bytes.
type span struct {
	start, end int
	text       string
}

func recoverEdits(src, output []byte, input, prefix string, first int) ([]Edit, []Block, error) {
	header := []byte("//line " + input + ":1:1\n")
	if !bytes.HasPrefix(output, header) {
		return nil, nil, fmt.Errorf("missing expected //line header")
	}
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, input, output, parser.ParseComments|parser.AllErrors)
	if err != nil {
		return nil, nil, fmt.Errorf("parse rewritten source: %w", err)
	}
	if len(f.Decls) == 0 {
		return nil, nil, fmt.Errorf("missing counter declaration")
	}
	decl, ok := f.Decls[len(f.Decls)-1].(*ast.GenDecl)
	if !ok {
		return nil, nil, fmt.Errorf("last declaration is not the counter variable")
	}
	blocks, err := readMetadata(decl, prefix+"Blocks", first)
	if err != nil {
		return nil, nil, err
	}
	// Never use adjusted positions here: the output contains both the injected
	// header and any original //line directives.
	offset := func(p token.Pos) int { return fs.PositionFor(p, false).Offset }
	start, end := len(header), offset(decl.Pos())-1
	if end < start || output[end] != '\n' {
		return nil, nil, fmt.Errorf("counter declaration lacks its added newline")
	}
	if !bytes.Equal(output[offset(decl.End()):], []byte{'\n'}) {
		return nil, nil, fmt.Errorf("unexpected bytes after counter declaration")
	}
	seen := make([]bool, len(blocks))
	var spans []span
	ast.Inspect(f, func(n ast.Node) bool {
		if n == decl || err != nil {
			return false
		}
		switch n := n.(type) {
		case *ast.IncDecStmt:
			index, ok := counterIndex(n.X, prefix+"Blocks")
			if !ok {
				break
			}
			if n.Tok != token.INC || index < 0 || index >= len(blocks) || seen[index] {
				err = fmt.Errorf("invalid or duplicate counter probe at output byte %d", offset(n.Pos()))
				return false
			}
			a, b := offset(n.Pos()), offset(n.End())
			if b >= len(output) || output[b] != ';' {
				err = fmt.Errorf("counter probe at output byte %d has no explicit semicolon", a)
				return false
			}
			seen[index] = true
			spans = append(spans, span{a, b + 1, prefix + "Hit(" + strconv.Itoa(blocks[index].Counter) + ");"})
		case *ast.IfStmt:
			if n.Else == nil {
				break
			}
			outer, ok := n.Else.(*ast.BlockStmt)
			if !ok {
				err = fmt.Errorf("else at output byte %d has no synthetic outer block", offset(n.Pos()))
				return false
			}
			a, b := offset(outer.Lbrace), offset(outer.Rbrace)
			if output[a] != '{' || output[b] != '}' {
				err = fmt.Errorf("invalid synthetic else braces at output byte %d", a)
				return false
			}
			spans = append(spans, span{a, a + 1, "{"}, span{b, b + 1, "}"})
		}
		return true
	})
	if err != nil {
		return nil, nil, err
	}
	for i, found := range seen {
		if !found {
			return nil, nil, fmt.Errorf("counter %d has metadata but no probe", i)
		}
	}
	edits, err := mapSpans(src, output, start, end, spans)
	if err != nil {
		return nil, nil, err
	}
	return edits, blocks, nil
}

func counterIndex(expr ast.Expr, name string) (int, bool) {
	index, ok := expr.(*ast.IndexExpr)
	if !ok {
		return 0, false
	}
	sel, ok := index.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Count" {
		return 0, false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Name != name {
		return 0, false
	}
	value, err := integer(index.Index)
	if err != nil || value > uint64(int(^uint(0)>>1)) {
		// Recognize malformed references too, so they cannot quietly survive.
		return -1, true
	}
	return int(value), true
}

// Recover the original offset only by exact matching of untouched chunks.
// Unknown mutations, missing wrappers, and overlapping spans are hard errors.
func mapSpans(src, output []byte, start, end int, spans []span) ([]Edit, error) {
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var edits []Edit
	original, rewritten := 0, start
	for _, s := range spans {
		if s.start < rewritten || s.end <= s.start || s.end > end {
			return nil, fmt.Errorf("overlapping or out-of-body insertion span [%d,%d)", s.start, s.end)
		}
		chunk := output[rewritten:s.start]
		if len(chunk) > len(src)-original || !bytes.Equal(chunk, src[original:original+len(chunk)]) {
			return nil, fmt.Errorf("unrecognized source change near original byte %d (output byte %d)", original, rewritten)
		}
		original += len(chunk)
		edits = append(edits, Edit{original, s.text})
		rewritten = s.end
	}
	if !bytes.Equal(output[rewritten:end], src[original:]) {
		return nil, fmt.Errorf("unrecognized trailing source change near original byte %d (output byte %d)", original, rewritten)
	}
	return edits, nil
}
