package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The lifecycle states are written out in three places and cannot be one constant:
// ycp and meko are separate Go modules that must not cross-import, and the spec is
// not Go at all. This test READS the other two as data (reading a file is not
// importing a module) and fails the moment one disagrees.
//
// The server tolerates a state it does not know, so drift no longer strands a fleet
// -- but it does publish a contract the server does not implement, and it makes the
// "state this control plane does not know" warning fire for a perfectly standard
// state. The meko_api_server filter in .github/workflows/meko-pr-build.yml lists
// both files below so that a PR editing either one still runs this test; without
// that it would only catch drift arriving alongside an api_server change, which is
// not the shape drift arrives in.
const (
	// Relative to this package's directory, which is where `go test` runs.
	ycpTypesPath   = "../../../../ycp/internal/api/types.go"
	ycpOpenAPIPath = "../../../design/ycp-openapi.yaml"
)

func TestEnvStateListsAgreeAcrossTheRepo(t *testing.T) {
	want := sortedStates(envStates)

	for _, src := range []struct {
		what  string
		where string
		got   []string
	}{
		{"the agent's wire enum", ycpTypesPath, statesFromGoConsts(t, ycpTypesPath)},
		{"the published contract", ycpOpenAPIPath, statesFromOpenAPI(t, ycpOpenAPIPath)},
	} {
		if !equalStates(src.got, want) {
			t.Errorf("%s (%s) disagrees with envStates in environment_service.go\n  it has: %v\n  we have: %v\n"+
				"add the state to all three: this map, %s and %s",
				src.what, src.where, src.got, want, ycpTypesPath, ycpOpenAPIPath)
		}
	}
}

func sortedStates(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func equalStates(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// statesFromGoConsts reads the values of the `EnvState` const block. Parsed rather
// than grepped so that reformatting the block, or renaming a constant, does not
// look like a state change.
func statesFromGoConsts(t *testing.T, path string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "EnvState" {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s in %s: %v", lit.Value, path, err)
				}
				out = append(out, s)
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("found no EnvState constants in %s -- has the type been renamed?", path)
	}
	sort.Strings(out)
	return out
}

// specEnvStateEnum matches the EnvState schema's inline enum. Scanned rather than
// unmarshalled to keep this test -- and so the api_server module -- free of a YAML
// dependency: a go.mod edit here wakes the vuln-scan job, whose path filter is
// go.mod/go.sum alone, and this test is not worth that coupling.
var specEnvStateEnum = regexp.MustCompile(`(?s)\bEnvState:.*?\benum:\s*\[([^\]]*)\]`)

// statesFromOpenAPI reads the enum off components.schemas.EnvState.
func statesFromOpenAPI(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := specEnvStateEnum.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("found no inline EnvState enum in %s -- has the schema moved, "+
			"or been rewritten as a block list?", path)
	}
	var out []string
	for _, s := range strings.Split(string(m[1]), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
