package blockcover

import (
	"fmt"
	"go/ast"
	"go/token"
	"strconv"
)

func readMetadata(decl *ast.GenDecl, name string, first int) ([]Block, error) {
	fail := func(reason string) ([]Block, error) {
		return nil, fmt.Errorf("invalid %s counter metadata: %s", name, reason)
	}
	if decl.Tok != token.VAR || len(decl.Specs) != 1 {
		return fail("expected a single var declaration")
	}
	spec, ok := decl.Specs[0].(*ast.ValueSpec)
	if !ok || len(spec.Names) != 1 || spec.Names[0].Name != name || spec.Type != nil || len(spec.Values) != 1 {
		return fail("unexpected variable name or initializer")
	}
	lit, ok := spec.Values[0].(*ast.CompositeLit)
	if !ok {
		return fail("initializer is not a struct literal")
	}
	st, ok := lit.Type.(*ast.StructType)
	if !ok || st.Fields == nil || len(st.Fields.List) != 3 {
		return fail("expected Count, Pos, NumStmt struct fields")
	}
	names := []string{"Count", "Pos", "NumStmt"}
	types := []string{"uint32", "uint32", "uint16"}
	lengths := make([]uint64, 3)
	for i, field := range st.Fields.List {
		if len(field.Names) != 1 || field.Names[0].Name != names[i] || field.Tag != nil {
			return fail("unexpected struct field")
		}
		n, err := arrayLength(field.Type, types[i])
		if err != nil {
			return fail(err.Error())
		}
		lengths[i] = n
	}
	if lengths[0] > ^uint64(0)/3 || lengths[1] != 3*lengths[0] || lengths[2] != lengths[0] {
		return fail("inconsistent array sizes")
	}
	if len(lit.Elts) != 2 {
		return fail("expected keyed Pos and NumStmt initializers")
	}
	values := make(map[string][]uint64)
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			return fail("unkeyed initializer")
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || (key.Name != "Pos" && key.Name != "NumStmt") || values[key.Name] != nil {
			return fail("unexpected or repeated initializer key")
		}
		idx := 1
		if key.Name == "NumStmt" {
			idx = 2
		}
		arr, ok := kv.Value.(*ast.CompositeLit)
		if !ok {
			return fail("initializer is not an array literal")
		}
		n, err := arrayLength(arr.Type, types[idx])
		if err != nil || n != lengths[idx] || uint64(len(arr.Elts)) != n {
			return fail("initializer array size disagrees with declaration")
		}
		nums := make([]uint64, len(arr.Elts))
		for i, elt := range arr.Elts {
			nums[i], err = integer(elt)
			if err != nil || (idx == 1 && nums[i] > 1<<32-1) || (idx == 2 && nums[i] > 1<<16-1) {
				return fail("invalid or overflowing metadata integer")
			}
			if idx == 2 && nums[i] == 1<<16-1 {
				return fail("statement count reaches the legacy 65535 limit and may be truncated")
			}
		}
		values[key.Name] = nums
	}
	pos, nums := values["Pos"], values["NumStmt"]
	if pos == nil || nums == nil {
		return fail("missing Pos or NumStmt initializer")
	}
	maxInt := int(^uint(0) >> 1)
	if len(nums) > 0 && first > maxInt-(len(nums)-1) {
		return fail("counter IDs overflow int")
	}
	blocks := make([]Block, len(nums))
	for i, statements := range nums {
		if pos[3*i] == 0 || pos[3*i+1] == 0 || pos[3*i] > uint64(maxInt) || pos[3*i+1] > uint64(maxInt) {
			return fail("invalid or overflowing source line")
		}
		blocks[i] = Block{
			StartLine: int(pos[3*i]), EndLine: int(pos[3*i+1]),
			StartColumn: int(pos[3*i+2] & 0xffff), EndColumn: int(pos[3*i+2] >> 16),
			Statements: int(statements), Counter: first + i,
		}
	}
	return blocks, nil
}

func arrayLength(expr ast.Expr, element string) (uint64, error) {
	array, ok := expr.(*ast.ArrayType)
	if !ok {
		return 0, fmt.Errorf("expected array type")
	}
	id, ok := array.Elt.(*ast.Ident)
	if !ok || id.Name != element {
		return 0, fmt.Errorf("unexpected array element type")
	}
	return integer(array.Len)
}

// Cover prints integer literals and the array bound 3 * N. Deliberately avoid
// evaluating arbitrary Go expressions or accepting an unknown output format.
func integer(expr ast.Expr) (uint64, error) {
	if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.INT {
		return strconv.ParseUint(lit.Value, 0, 64)
	}
	if mul, ok := expr.(*ast.BinaryExpr); ok && mul.Op == token.MUL {
		a, err := integer(mul.X)
		if err != nil {
			return 0, err
		}
		b, err := integer(mul.Y)
		if err != nil {
			return 0, err
		}
		if b != 0 && a > ^uint64(0)/b {
			return 0, fmt.Errorf("overflowing integer multiplication")
		}
		return a * b, nil
	}
	return 0, fmt.Errorf("expected unsigned integer literal or product")
}
