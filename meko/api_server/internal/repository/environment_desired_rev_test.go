package repository

import (
	"database/sql"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/models"
)

func expectBump(mock sqlmock.Sqlmock) {
	mock.ExpectExec(`INSERT INTO meko_system\.environment_desired_revs`).
		WithArgs(envUUID).WillReturnResult(sqlmock.NewResult(0, 1))
}

// recordBumps returns the environments bumpDesiredRev is called for. It also fails the
// test if a bump comes before every expectation registered so far has been met --
// register the write's statements and its commit, not the bump's own statement, and
// a bump issued before the commit is caught here.
func recordBumps(t *testing.T, mock sqlmock.Sqlmock) *[]string {
	t.Helper()
	var bumped []string
	desiredRevBumped = func(env string) {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("bumped %s before the write finished: %v", env, err)
		}
		bumped = append(bumped, env)
	}
	t.Cleanup(func() { desiredRevBumped = nil })
	return &bumped
}

func wantBumps(t *testing.T, got *[]string, want ...string) {
	t.Helper()
	if strings.Join(*got, ",") != strings.Join(want, ",") {
		t.Errorf("bumped %v, want %v", *got, want)
	}
}

func newRevRepo(t *testing.T) (*EnvironmentRepo, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewEnvironmentRepo(db), mock
}

const upsertRE = `INSERT INTO meko_system\.environment_desired_resources`

// A changed upsert moves the token; an unchanged one writes nothing, reads its
// generation back, and leaves the token alone, so it wakes no agent.
func TestUpsertDesired_BumpsOnlyOnAChange(t *testing.T) {
	spec := []byte(`{"image":"x"}`)
	t.Run("changed", func(t *testing.T) {
		r, mock := newRevRepo(t)
		mock.ExpectQuery(upsertRE).WillReturnRows(sqlmock.NewRows([]string{"generation"}).AddRow(int64(3)))
		bumped := recordBumps(t, mock)
		d, err := r.UpsertDesired(envUUID, models.DesiredResource{Kind: "extension", Name: "x", Spec: spec})
		if err != nil || d.Generation != 3 {
			t.Fatalf("UpsertDesired = %+v, %v; want generation 3", d, err)
		}
		wantBumps(t, bumped, envUUID)
	})
	t.Run("unchanged", func(t *testing.T) {
		r, mock := newRevRepo(t)
		mock.ExpectQuery(upsertRE).WillReturnError(sql.ErrNoRows)
		mock.ExpectQuery(`SELECT generation FROM meko_system\.environment_desired_resources`).
			WithArgs(envUUID, "extension", "x").WillReturnRows(sqlmock.NewRows([]string{"generation"}).AddRow(int64(2)))
		bumped := recordBumps(t, mock)
		d, err := r.UpsertDesired(envUUID, models.DesiredResource{Kind: "extension", Name: "x", Spec: spec})
		if err != nil || d.Generation != 2 {
			t.Fatalf("UpsertDesired = %+v, %v; want the stored generation 2", d, err)
		}
		wantBumps(t, bumped)
	})
}

// The bump follows the commit -- sqlmock matches in order -- and a replace that
// changed no row commits without one.
func TestReplaceDesired_BumpsAfterTheCommitOnlyOnAChange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		written int64
		bump    bool
	}{{"changed", 1, true}, {"unchanged", 0, false}} {
		t.Run(tc.name, func(t *testing.T) {
			r, mock := newRevRepo(t)
			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT kind, name FROM meko_system\.environment_desired_resources`).
				WillReturnRows(sqlmock.NewRows([]string{"kind", "name"}).AddRow("agent", "a"))
			mock.ExpectExec(upsertRE).WillReturnResult(sqlmock.NewResult(0, tc.written))
			mock.ExpectCommit()
			bumped := recordBumps(t, mock)
			if err := r.ReplaceDesired(envUUID, []models.DesiredResource{{Kind: "agent", Name: "a"}}); err != nil {
				t.Fatalf("ReplaceDesired: %v", err)
			}
			if tc.bump {
				wantBumps(t, bumped, envUUID)
			} else {
				wantBumps(t, bumped)
			}
		})
	}
}

// A prune is a change even when every kept row is unchanged.
func TestReplaceDesired_APruneBumps(t *testing.T) {
	r, mock := newRevRepo(t)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT kind, name FROM meko_system\.environment_desired_resources`).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "name"}).AddRow("agent", "a").AddRow("agent", "b"))
	mock.ExpectExec(upsertRE).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM meko_system\.environment_desired_resources`).
		WithArgs(envUUID, "agent", "b").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	bumped := recordBumps(t, mock)
	if err := r.ReplaceDesired(envUUID, []models.DesiredResource{{Kind: "agent", Name: "a"}}); err != nil {
		t.Fatalf("ReplaceDesired: %v", err)
	}
	wantBumps(t, bumped, envUUID)
}

func TestDeleteDesired_BumpsOnlyWhenARowWent(t *testing.T) {
	for _, gone := range []int64{0, 1} {
		r, mock := newRevRepo(t)
		mock.ExpectExec(`DELETE FROM meko_system\.environment_desired_resources`).
			WillReturnResult(sqlmock.NewResult(0, gone))
		bumped := recordBumps(t, mock)
		if err := r.DeleteDesired(envUUID, "extension", "x"); err != nil {
			t.Fatalf("DeleteDesired: %v", err)
		}
		if gone > 0 {
			wantBumps(t, bumped, envUUID)
		} else {
			wantBumps(t, bumped)
		}
	}
}

// The write has committed by the time the bump runs, so a failed bump must not turn
// a stored change into an error the caller would retry.
func TestBumpFailureDoesNotFailTheWrite(t *testing.T) {
	r, mock := newRevRepo(t)
	mock.ExpectExec(`DELETE FROM meko_system\.environment_desired_resources`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO meko_system\.environment_desired_revs`).
		WillReturnError(errors.New("connection reset"))
	if err := r.DeleteDesired(envUUID, "extension", "x"); err != nil {
		t.Fatalf("DeleteDesired = %v, want nil: the delete committed", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// Every write to environment_desired_resources has to move the change token, or the
// agent sees it only on its next tick and nothing fails. This pins the writers and,
// for each, the function that commits it and must call bumpDesiredRev. A new writer
// fails here until it is added -- with its bump.
func TestEveryDesiredWriterBumpsTheChangeToken(t *testing.T) {
	committer := map[string]string{
		"ReplaceDesired":        "ReplaceDesired",
		"upsertDesired":         "upsertDesired",
		"DeleteDesired":         "DeleteDesired",
		"applyDesiredPatchesTx": "applyStoragePoolPatches", // runs on its caller's transaction
	}
	write := regexp.MustCompile(`(?is)(insert\s+into|delete\s+from|update)\s+meko_system\.environment_desired_resources\b`)

	fset := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	if err != nil || len(paths) == 0 {
		t.Fatalf("glob the package sources: %v (%d files)", err, len(paths))
	}
	funcs := map[string]*ast.FuncDecl{}
	writeConsts := map[string]bool{}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				funcs[d.Name.Name] = d
			case *ast.GenDecl:
				for _, sp := range d.Specs {
					if vs, ok := sp.(*ast.ValueSpec); ok && d.Tok == token.CONST {
						for i, n := range vs.Names {
							if i < len(vs.Values) && write.MatchString(literalText(vs.Values[i])) {
								writeConsts[n.Name] = true
							}
						}
					}
				}
			}
		}
	}
	writers := map[string]bool{}
	for name, fn := range funcs {
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BasicLit:
				if write.MatchString(n.Value) {
					writers[name] = true
				}
			case *ast.Ident:
				if writeConsts[n.Name] {
					writers[name] = true
				}
			}
			return true
		})
	}
	for name := range writers {
		c, ok := committer[name]
		if !ok {
			t.Errorf("%s writes environment_desired_resources but is not a known writer: "+
				"call bumpDesiredRev after its commit and add it to this test", name)
			continue
		}
		if !calls(funcs[c], "bumpDesiredRev") {
			t.Errorf("%s commits a desired-state write but never calls bumpDesiredRev", c)
		}
	}
	for name := range committer {
		if !writers[name] {
			t.Errorf("%s no longer writes desired state; drop it from this test", name)
		}
	}
}

func literalText(e ast.Expr) string {
	var b strings.Builder
	ast.Inspect(e, func(n ast.Node) bool {
		if l, ok := n.(*ast.BasicLit); ok {
			b.WriteString(l.Value)
		}
		return true
	})
	return b.String()
}

func calls(fn *ast.FuncDecl, name string) bool {
	found := false
	if fn == nil {
		return false
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == name {
				found = true
			}
		}
		return !found
	})
	return found
}
