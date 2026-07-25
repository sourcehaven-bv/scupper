package scupper

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
)

// resolveFuncDirectives maps each ignore-func directive line to the whole line
// span of the function declaration it guards, returning those spans as ignored
// ranges.
//
// A directive is expected on the line(s) immediately above a func declaration
// (the go-test-coverage convention), e.g.:
//
//	//scupper:ignore-func startup wiring
//	func main() { ... }
//
// The guarded function is the FuncDecl whose own declaration line is the first
// one at or after the directive line, once intervening comment/blank lines are
// skipped. A directive with no such function below it is a hard error — the same
// "no silent swallow" rule the block directives follow.
func resolveFuncDirectives(path string, dirLines []int, d Directives) ([]Range, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("%s: ignore-%s needs to parse the file, but it failed: %w", path, "func", err)
	}

	// Collect every function declaration's line span, sorted by start line.
	type fn struct{ start, end int }
	var fns []fn
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		start := fset.Position(fd.Pos()).Line
		end := fset.Position(fd.End()).Line
		fns = append(fns, fn{start, end})
	}
	sort.Slice(fns, func(i, j int) bool { return fns[i].start < fns[j].start })

	out := make([]Range, 0, len(dirLines))
	for _, dl := range dirLines {
		// The guarded function is the first one whose declaration starts at or
		// after the directive line. Comments/blank lines between the directive
		// and `func` are fine — the AST start line is the `func` keyword's line.
		var chosen *fn
		for i := range fns {
			if fns[i].start >= dl {
				chosen = &fns[i]
				break
			}
		}
		if chosen == nil {
			return nil, fmt.Errorf("%s:%d: %q has no function declaration below it", path, dl, d.Func)
		}
		out = append(out, Range{chosen.start, chosen.end})
	}
	return out, nil
}
