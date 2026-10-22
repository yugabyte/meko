package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The lines the CLI prints end without a period, as Go's error strings do: a period after a
// version, path or command reads as part of it ("Installing meko 0.20261022.1.").
func TestOutputLinesEndWithoutPeriod(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "fmt" {
				return true
			}
			var arg ast.Expr
			switch sel.Sel.Name {
			case "Fprintf":
				if len(call.Args) > 1 {
					arg = call.Args[1]
				}
			case "Sprintf":
				arg = call.Args[0]
			case "Fprintln", "Fprint":
				arg = call.Args[len(call.Args)-1]
			}
			if s, ok := lastLiteral(arg); ok && strings.HasSuffix(strings.TrimRight(s, "\n"), ".") {
				t.Errorf("%s: %q ends its line with a period", fset.Position(call.Pos()), s)
			}
			return true
		})
	}
}

// lastLiteral is the string literal that ends e, through a + concatenation.
func lastLiteral(e ast.Expr) (string, bool) {
	for {
		b, ok := e.(*ast.BinaryExpr)
		if !ok || b.Op != token.ADD {
			break
		}
		e = b.Y
	}
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}
