package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/models"
)

const (
	provTestDatapackID = "11111111-1111-1111-1111-111111111111"
	provTestAccountID  = "acct-1"
)

var provTestAccount = &models.Account{ID: provTestAccountID, Email: "owner@example.com"}

// fakeLangfuse serves the endpoints the provisioning chain calls for an
// account whose org mapping already exists. createProject decides how
// POST /api/public/projects answers; a 2xx answer records project p1, which
// GET /api/public/organizations/projects then lists (tagged with
// provTestDatapackID, beside a project of another datapack).
type fakeLangfuse struct {
	createProject func(w http.ResponseWriter, r *http.Request, call int32)
	// deleteProject answers DELETE /api/public/projects/{id}; nil means 200.
	deleteProject func(w http.ResponseWriter, r *http.Request)
	projects      []string
	mu            sync.Mutex
	// listStatus, when set, is the status GET organizations/projects fails with.
	listStatus int
	// apiKeyFailures is how many project API key mints fail before one succeeds.
	apiKeyFailures     atomic.Int32
	createProjectCalls atomic.Int32
	deleteProjectCalls atomic.Int32
}

func (f *fakeLangfuse) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Drain the body so the server watches the connection and cancels
	// r.Context() when the client gives up; the hang handlers rely on it.
	_, _ = io.Copy(io.Discard, r.Body)
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/public/scim/Users":
		// The filter is `userName eq "<login email>"`; echo that user back.
		email := strings.TrimSuffix(strings.TrimPrefix(r.URL.Query().Get("filter"), `userName eq "`), `"`)
		_, _ = w.Write([]byte(`{"Resources":[{"id":"u1","userName":"` + email + `"}]}`))
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/public/scim/Users/"):
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && r.URL.Path == "/api/public/organizations/projects":
		if f.listStatus != 0 {
			http.Error(w, "nope", f.listStatus)
			return
		}
		f.mu.Lock()
		entries := []string{`{"id":"other","name":"x","metadata":{"datapack_id":"someone-else"}}`}
		for _, id := range f.projects {
			entries = append(entries, `{"id":"`+id+`","name":"dp","metadata":{"datapack_id":"`+provTestDatapackID+`"}}`)
		}
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"projects":[` + strings.Join(entries, ",") + `]}`))
	case r.Method == http.MethodPost && r.URL.Path == "/api/public/projects":
		rec := httptest.NewRecorder()
		f.createProject(rec, r, f.createProjectCalls.Add(1))
		if rec.Code < 300 {
			f.mu.Lock()
			f.projects = append(f.projects, "p1")
			f.mu.Unlock()
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	case r.Method == http.MethodPost && r.URL.Path == "/api/public/projects/p1/apiKeys":
		if f.apiKeyFailures.Add(-1) >= 0 {
			http.Error(w, "key mint failed", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"publicKey":"pk-p1","secretKey":"sk-p1"}`))
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/public/projects/"):
		f.deleteProjectCalls.Add(1)
		if f.deleteProject != nil {
			f.deleteProject(w, r)
		}
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusTeapot)
	}
}

func projectCreated(w http.ResponseWriter, _ *http.Request, _ int32) {
	_, _ = w.Write([]byte(`{"id":"p1","name":"dp"}`))
}

func newProvisionTestService(t *testing.T, fake *fakeLangfuse) (*LangfuseService, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	svc := NewLangfuseService(LangfuseConfig{
		SystemDB:    db,
		DataDB:      db,
		Host:        srv.URL,
		AdminAPIKey: "admin-key",
	})
	svc.provisionBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	return svc, mock
}

// expectAttemptLookups queues the two reads every provisioning attempt
// starts with: no project mapping yet, and an existing org mapping.
func expectAttemptLookups(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`FROM meko_system.langfuse_project_mapping`).
		WithArgs(provTestDatapackID).
		WillReturnRows(sqlmock.NewRows(
			[]string{"langfuse_project_id", "langfuse_project_name", "langfuse_public_key", "langfuse_secret_key"}))
	mock.ExpectQuery(`FROM meko_system.langfuse_org_mapping`).
		WithArgs(provTestAccountID).
		WillReturnRows(sqlmock.NewRows(
			[]string{"langfuse_org_id", "langfuse_org_name", "langfuse_org_public_key", "langfuse_org_secret_key"}).
			AddRow("o1", "meko-org", "pk-org", "sk-org"))
}

func expectMappingWritten(mock sqlmock.Sqlmock) {
	mock.ExpectExec(`INSERT INTO meko_system.langfuse_project_mapping`).
		WithArgs(provTestDatapackID, "p1", "dp", "pk-p1", "sk-p1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`FROM meko_system.langfuse_project_mapping`).
		WithArgs(provTestDatapackID).
		WillReturnRows(sqlmock.NewRows(
			[]string{"langfuse_project_id", "langfuse_project_name", "langfuse_public_key", "langfuse_secret_key"}).
			AddRow("p1", "dp", "pk-p1", "sk-p1"))
}

func TestEnsureProjectForDatapack_Succeeds(t *testing.T) {
	fake := &fakeLangfuse{createProject: projectCreated}
	svc, mock := newProvisionTestService(t, fake)
	expectAttemptLookups(mock)
	expectMappingWritten(mock)

	keys, err := svc.EnsureProjectForDatapack(context.Background(), provTestAccount, provTestDatapackID, "dp")
	if err != nil {
		t.Fatalf("EnsureProjectForDatapack: %v", err)
	}
	if keys == nil || keys.ProjectID != "p1" {
		t.Fatalf("keys = %+v, want project p1", keys)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureProjectForDatapack_RetriesThenSucceeds(t *testing.T) {
	fake := &fakeLangfuse{createProject: func(w http.ResponseWriter, r *http.Request, call int32) {
		if call < 3 {
			http.Error(w, "upstream sad", http.StatusBadGateway)
			return
		}
		projectCreated(w, r, call)
	}}
	svc, mock := newProvisionTestService(t, fake)
	for range 3 {
		expectAttemptLookups(mock)
	}
	expectMappingWritten(mock)

	if _, err := svc.EnsureProjectForDatapack(
		context.Background(), provTestAccount, provTestDatapackID, "dp",
	); err != nil {
		t.Fatalf("EnsureProjectForDatapack: %v", err)
	}
	if got := fake.createProjectCalls.Load(); got != 3 {
		t.Fatalf("createProject calls = %d, want 3", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureProjectForDatapack_GivesUp(t *testing.T) {
	fake := &fakeLangfuse{createProject: func(w http.ResponseWriter, _ *http.Request, _ int32) {
		http.Error(w, "upstream sad", http.StatusInternalServerError)
	}}
	svc, mock := newProvisionTestService(t, fake)
	for range 3 {
		expectAttemptLookups(mock)
	}

	_, err := svc.EnsureProjectForDatapack(context.Background(), provTestAccount, provTestDatapackID, "dp")
	if !errors.Is(err, ErrLangfuseUnavailable) {
		t.Fatalf("err = %v, want ErrLangfuseUnavailable", err)
	}
	if got := fake.createProjectCalls.Load(); got != 3 {
		t.Fatalf("createProject calls = %d, want 3 (1 + len(backoff))", got)
	}
	// No mapping row is written for a failed provision.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureProjectForDatapack_StopsAtDeadline(t *testing.T) {
	fake := &fakeLangfuse{createProject: func(_ http.ResponseWriter, r *http.Request, _ int32) {
		<-r.Context().Done() // Langfuse hangs until the client gives up.
	}}
	svc, mock := newProvisionTestService(t, fake)
	svc.provisionBackoff = []time.Duration{time.Hour}
	mock.MatchExpectationsInOrder(false)
	expectAttemptLookups(mock)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := svc.EnsureProjectForDatapack(ctx, provTestAccount, provTestDatapackID, "dp")
	if !errors.Is(err, ErrLangfuseUnavailable) {
		t.Fatalf("err = %v, want ErrLangfuseUnavailable", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("returned after %s; the deadline should have cut the hung request and the backoff", elapsed)
	}
}

func TestEnsureProjectForDatapack_ReusesProjectFromFailedAttempt(t *testing.T) {
	// Attempt 1 creates p1, then minting its key fails. Attempt 2 must find
	// p1 through its datapack_id tag instead of creating a suffixed duplicate.
	fake := &fakeLangfuse{createProject: projectCreated}
	fake.apiKeyFailures.Store(1)
	svc, mock := newProvisionTestService(t, fake)
	expectAttemptLookups(mock)
	expectAttemptLookups(mock)
	expectMappingWritten(mock)

	keys, err := svc.EnsureProjectForDatapack(context.Background(), provTestAccount, provTestDatapackID, "dp")
	if err != nil {
		t.Fatalf("EnsureProjectForDatapack: %v", err)
	}
	if keys.ProjectID != "p1" {
		t.Fatalf("project = %q, want the reused p1", keys.ProjectID)
	}
	if got := fake.createProjectCalls.Load(); got != 1 {
		t.Fatalf("createProject calls = %d, want 1", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureProjectForDatapack_ProjectListFailureStillCreates(t *testing.T) {
	// The reuse lookup is best-effort: an older Langfuse without GET
	// organizations/projects (404), or a failing one (500), still provisions
	// by creating the project.
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			fake := &fakeLangfuse{createProject: projectCreated, listStatus: status}
			svc, mock := newProvisionTestService(t, fake)
			expectAttemptLookups(mock)
			expectMappingWritten(mock)

			if _, err := svc.EnsureProjectForDatapack(
				context.Background(), provTestAccount, provTestDatapackID, "dp",
			); err != nil {
				t.Fatalf("EnsureProjectForDatapack: %v", err)
			}
			if got := fake.createProjectCalls.Load(); got != 1 {
				t.Fatalf("createProject calls = %d, want 1", got)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDiscardProjectsForDatapack_DeletesEveryTaggedProject(t *testing.T) {
	fake := &fakeLangfuse{projects: []string{"p1", "p2"}}
	svc, mock := newProvisionTestService(t, fake)
	mock.ExpectQuery(`FROM meko_system.langfuse_org_mapping`).
		WithArgs(provTestAccountID).
		WillReturnRows(sqlmock.NewRows(
			[]string{"langfuse_org_id", "langfuse_org_name", "langfuse_org_public_key", "langfuse_org_secret_key"}).
			AddRow("o1", "meko-org", "pk-org", "sk-org"))
	mock.ExpectExec(`DELETE FROM meko_system.langfuse_project_mapping`).
		WithArgs(provTestDatapackID).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := svc.DiscardProjectsForDatapack(context.Background(), provTestAccountID, provTestDatapackID); err != nil {
		t.Fatalf("DiscardProjectsForDatapack: %v", err)
	}
	// p1 and p2, not the other datapack's project.
	if got := fake.deleteProjectCalls.Load(); got != 2 {
		t.Fatalf("delete project calls = %d, want 2", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func expectCleanupLookups(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`FROM meko_system.langfuse_project_mapping`).
		WithArgs(provTestDatapackID).
		WillReturnRows(sqlmock.NewRows(
			[]string{"langfuse_project_id", "langfuse_project_name", "langfuse_public_key", "langfuse_secret_key"}).
			AddRow("p1", "dp", "pk-p1", "sk-p1"))
	mock.ExpectQuery(`FROM meko_system.langfuse_org_mapping`).
		WithArgs(provTestAccountID).
		WillReturnRows(sqlmock.NewRows(
			[]string{"langfuse_org_id", "langfuse_org_name", "langfuse_org_public_key", "langfuse_org_secret_key"}).
			AddRow("o1", "meko-org", "pk-org", "sk-org"))
	mock.ExpectExec(`DELETE FROM meko_system.langfuse_project_mapping`).
		WithArgs(provTestDatapackID).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestCleanupForDeletedDatapack_LangfuseFailureDoesNotBlock(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter, r *http.Request){
		"500":       func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) },
		"404":       func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "gone", http.StatusNotFound) },
		"hang":      func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() },
		"succeeded": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) },
	}
	orig := langfuseProjectDeleteTimeout
	langfuseProjectDeleteTimeout = 200 * time.Millisecond
	t.Cleanup(func() { langfuseProjectDeleteTimeout = orig })

	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &fakeLangfuse{deleteProject: handler}
			svc, mock := newProvisionTestService(t, fake)
			expectCleanupLookups(mock)

			if err := svc.CleanupForDeletedDatapack(provTestAccountID, provTestDatapackID); err != nil {
				t.Fatalf("CleanupForDeletedDatapack: %v", err)
			}
			if got := fake.deleteProjectCalls.Load(); got != 1 {
				t.Fatalf("delete project calls = %d, want 1", got)
			}
			// The local mapping row is removed either way.
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCleanupForDeletedDatapack_NoMapping(t *testing.T) {
	fake := &fakeLangfuse{}
	svc, mock := newProvisionTestService(t, fake)
	mock.ExpectQuery(`FROM meko_system.langfuse_project_mapping`).
		WithArgs(provTestDatapackID).
		WillReturnRows(sqlmock.NewRows(
			[]string{"langfuse_project_id", "langfuse_project_name", "langfuse_public_key", "langfuse_secret_key"}))

	if err := svc.CleanupForDeletedDatapack(provTestAccountID, provTestDatapackID); err != nil {
		t.Fatalf("CleanupForDeletedDatapack: %v", err)
	}
	if got := fake.deleteProjectCalls.Load(); got != 0 {
		t.Fatalf("delete project calls = %d, want 0", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
