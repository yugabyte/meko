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
// version, path or command reads as part of it ("Meko is running 0.20261022.1."). This checks
// the strings fmt prints or formats and the strings functions return (stopServer's messages are
// printed by their callers): every line inside them, and the text they end with.
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
		check := func(e ast.Expr) {
			for _, l := range outputLines(e) {
				if strings.HasSuffix(l.text, ".") {
					t.Errorf("%s: %q ends a line with a period", fset.Position(l.pos), l.text)
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.ReturnStmt:
				for _, r := range n.Results {
					check(r)
				}
			case *ast.CallExpr:
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "fmt" {
					return true
				}
				switch sel.Sel.Name {
				case "Fprintf", "Fprintln", "Fprint":
					if len(n.Args) > 1 {
						for _, a := range n.Args[1:] {
							check(a)
						}
					}
				case "Sprintf":
					check(n.Args[0])
				}
			}
			return true
		})
	}
}

type outputLine struct {
	pos  token.Pos
	text string
}

// outputLines are the lines in the string literals that make up e (through + concatenation):
// each one a "\n" ends, and the text after the last literal's last "\n", which the caller's
// newline or the next output ends. Text from other expressions (%s, variables) isn't known.
func outputLines(e ast.Expr) []outputLine {
	var lits []*ast.BasicLit
	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		switch e := e.(type) {
		case *ast.BinaryExpr:
			if e.Op == token.ADD {
				walk(e.X)
				walk(e.Y)
			}
		case *ast.ParenExpr:
			walk(e.X)
		case *ast.BasicLit:
			if e.Kind == token.STRING {
				lits = append(lits, e)
			}
		}
	}
	walk(e)
	var lines []outputLine
	for i, lit := range lits {
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			continue
		}
		parts := strings.Split(s, "\n")
		last := len(parts) - 1
		if i != len(lits)-1 || !endsWith(e, lit) {
			last-- // the text after this literal's last newline goes on in what follows it
		}
		for _, p := range parts[:last+1] {
			lines = append(lines, outputLine{lit.Pos(), p})
		}
	}
	return lines
}

// endsWith says whether lit is the last thing in e, so nothing in e follows its text.
func endsWith(e ast.Expr, lit *ast.BasicLit) bool {
	for {
		switch x := e.(type) {
		case *ast.BinaryExpr:
			e = x.Y
		case *ast.ParenExpr:
			e = x.X
		default:
			return e == lit
		}
	}
}
