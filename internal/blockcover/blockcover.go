// Package blockcover obtains basic-block instrumentation from the selected Go
// toolchain. It does not implement Go's basic-block discovery algorithm.
package blockcover

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Edit inserts Text at a byte Offset in the original source. Edits are returned
// in source order; preserve their order when several have the same Offset.
type Edit struct {
	Offset int
	Text   string
}

// Block describes a native coverage block and its globally allocated counter.
// Positions and statement counts are exactly those reported by go tool cover,
// including zero-statement blocks and the toolchain's treatment of //line.
type Block struct {
	StartLine, StartColumn, EndLine, EndColumn, Statements, Counter int
}

// Instrument runs go tool cover in count mode on the unmodified src, then
// replaces its counters with calls to prefix+"Hit". The caller supplies that
// function (normally using atomic increments). Counter IDs start at firstCounter.
//
// The returned edits contain only probes and native synthetic else braces, not
// the tool's line directive or counter declaration. They can therefore be
// combined with other insertions into the original source without changing
// native block discovery. filename is used for diagnostics, not as an input
// path; src need not exist on disk. Go's normal GOTOOLCHAIN selection applies.
//
// Legacy cover metadata packs columns into 16 bits. Rather than silently
// truncate positions, Instrument conservatively rejects physical lines longer
// than 65534 bytes and AST positions with overflowing //line-adjusted columns.
// It also rejects native statement counts of 65535, which may have been clamped
// by cover; a block must have fewer than 65535 statements.
//
// Cover runs per file. On toolchains that honor //line in metadata, duplicate
// virtual ranges aliased across multiple files can be deduplicated differently
// by package-mode coverage. Ordinary files with unique positions are unaffected.
func Instrument(filename string, src []byte, prefix string, firstCounter int) ([]Edit, []Block, error) {
	if !token.IsIdentifier(prefix) || token.Lookup(prefix).IsKeyword() {
		return nil, nil, fmt.Errorf("blockcover %s: invalid instrumentation prefix %q", filename, prefix)
	}
	if firstCounter < 0 {
		return nil, nil, fmt.Errorf("blockcover %s: negative first counter %d", filename, firstCounter)
	}
	if err := checkSource(filename, src, prefix); err != nil {
		return nil, nil, fmt.Errorf("blockcover %s: %w", filename, err)
	}
	dir, err := os.MkdirTemp("", "blockcover-")
	if err != nil {
		return nil, nil, fmt.Errorf("blockcover %s: create temporary directory: %w", filename, err)
	}
	defer os.RemoveAll(dir)
	input, output := filepath.Join(dir, "input.go"), filepath.Join(dir, "output.go")
	if err := os.WriteFile(input, src, 0600); err != nil {
		return nil, nil, fmt.Errorf("blockcover %s: write temporary input: %w", filename, err)
	}
	cmd := exec.Command("go", "tool", "cover", "-mode=count", "-var="+prefix+"Blocks", "-o", output, input)
	if diagnostics, err := cmd.CombinedOutput(); err != nil {
		return nil, nil, fmt.Errorf("blockcover %s: go tool cover: %w: %s", filename, err, strings.TrimSpace(string(diagnostics)))
	}
	rewritten, err := os.ReadFile(output)
	if err != nil {
		return nil, nil, fmt.Errorf("blockcover %s: read go tool cover output: %w", filename, err)
	}
	edits, blocks, err := recoverEdits(src, rewritten, input, prefix, firstCounter)
	if err != nil {
		return nil, nil, fmt.Errorf("blockcover %s: unsupported go tool cover output: %w", filename, err)
	}
	return edits, blocks, nil
}

func checkSource(filename string, src []byte, prefix string) error {
	for line, rest := 1, src; ; line++ {
		text, tail, more := bytes.Cut(rest, []byte{'\n'})
		if len(text) > 65534 {
			return fmt.Errorf("line %d exceeds the 16-bit coverage column limit", line)
		}
		if !more {
			break
		}
		rest = tail
	}
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, filename, src, parser.ParseComments|parser.AllErrors)
	if err != nil {
		return err
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil || err != nil {
			return false
		}
		if id, ok := n.(*ast.Ident); ok && (id.Name == prefix+"Blocks" || id.Name == prefix+"Hit") {
			err = fmt.Errorf("instrumentation identifier collision at %s", fs.PositionFor(id.Pos(), false))
			return false
		}
		for _, p := range []token.Pos{n.Pos(), n.End()} {
			pos := fs.PositionFor(p, true)
			if pos.Column > 65535 || uint64(pos.Line) > 1<<32-1 {
				err = fmt.Errorf("position %s exceeds the legacy coverage metadata limits", pos)
				return false
			}
		}
		return true
	})
	return err
}
