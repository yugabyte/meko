package handlers_test

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/handlers"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

const viewerScopeDatapackID = "2c6a3e0e-6d4b-4f5e-9a39-0f6b3c1f8d21"

// newObserveHandlerWithMocks wires an ObserveHandler over sqlmock-backed
// system and Langfuse databases. The system DB answers the project mapping.
func newObserveHandlerWithMocks(t *testing.T) (*handlers.ObserveHandler, sqlmock.Sqlmock, sqlmock.Sqlmock) {
	t.Helper()
	sysDB, sysMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	lfDB, lfMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sysDB.Close(); lfDB.Close() })
	svc := service.NewLangfuseService(service.LangfuseConfig{SystemDB: sysDB, LangfuseDB: lfDB})
	return handlers.NewObserveHandler(svc, false), sysMock, lfMock
}

func expectProjectMapping(m sqlmock.Sqlmock) {
	m.ExpectQuery(regexp.QuoteMeta("FROM meko_system.langfuse_project_mapping")).
		WithArgs(viewerScopeDatapackID).
		WillReturnRows(sqlmock.NewRows([]string{
			"langfuse_project_id", "langfuse_project_name", "langfuse_public_key", "langfuse_secret_key",
		}).AddRow("proj-1", "p", "pk", "sk"))
}

func newDatapackContext(path, accountID, grant string) (echo.Context, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.Set(auth.ContextKeyAccount, &models.Account{ID: accountID})
	c.Set(auth.ContextKeyDatapack, &models.Datapack{ID: viewerScopeDatapackID, Grant: grant})
	return c, rec
}

// ownerID is a UUID because the owner-email lookup skips anything else.
const ownerID = "44444444-4444-4444-4444-444444444444"

var conversationRow = []string{
	"conversation_id", "agent_id", "user_id", "started_at", "title",
	"first_message", "messages", "last_activity_at", "llm_calls",
}

// Viewers see every member's conversations, but not who started them; other
// roles still get the owner and their email.
func TestConversationList_HidesOwnersFromAViewer(t *testing.T) {
	for name, tc := range map[string]struct {
		grant     string
		wantOwner bool
	}{
		"viewer":      {grant: string(models.GrantViewer)},
		"contributor": {grant: string(models.GrantContributor), wantOwner: true},
	} {
		t.Run(name, func(t *testing.T) {
			h, sysMock, lfMock := newObserveHandlerWithMocks(t)
			sysMock.MatchExpectationsInOrder(false)
			lfMock.MatchExpectationsInOrder(false)
			expectProjectMapping(sysMock)
			now := time.Now()
			lfMock.ExpectQuery("LIMIT +\\$3 OFFSET \\$4").
				WillReturnRows(sqlmock.NewRows(conversationRow).
					AddRow("conv1", "agent", ownerID, now, "t", "hello", 2, now, 1))
			lfMock.ExpectQuery(".").WillReturnRows(sqlmock.NewRows([]string{"session_id", "n"}))
			if tc.wantOwner {
				sysMock.ExpectQuery("FROM meko_system.users").
					WillReturnRows(sqlmock.NewRows([]string{"user_id", "user_email"}).AddRow(ownerID, "o@x.com"))
			}

			c, rec := newDatapackContext("/", "acct-1", tc.grant)
			if err := h.ConversationList(c); err != nil {
				t.Fatal(err)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d (body: %s)", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, "conv1") {
				t.Fatalf("the conversation is missing: %s", body)
			}
			if got := strings.Contains(body, ownerID); got != tc.wantOwner {
				t.Errorf("owner in response = %v, want %v: %s", got, tc.wantOwner, body)
			}
			if tc.wantOwner && !strings.Contains(body, "o@x.com") {
				t.Errorf("a contributor lost the owner's email: %s", body)
			}
			if err := sysMock.ExpectationsWereMet(); err != nil {
				t.Error(err)
			}
		})
	}
}

var memoryRow = []string{"id", "data", "agent_id", "conversation_id", "user_id", "created_at"}

// Viewers see every member's memories, but not who added them.
func TestMemoriesList_HidesOwnersFromAViewer(t *testing.T) {
	for name, tc := range map[string]struct {
		grant     string
		wantOwner bool
	}{
		"viewer":      {grant: string(models.GrantViewer)},
		"contributor": {grant: string(models.GrantContributor), wantOwner: true},
	} {
		t.Run(name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*)")).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
			mock.ExpectQuery(regexp.QuoteMeta("SELECT")).
				WillReturnRows(sqlmock.NewRows(memoryRow).
					AddRow("m1", "a fact", "agent", "conv1", ownerID, time.Now()))

			svc := service.NewMemoriesService(repository.NewMemoriesRepo(db, nil, "", "", "", true), true)
			lookup := &fakeDatapackLookup{shared: map[string]*models.Datapack{
				"acct-1:" + viewerScopeDatapackID: {ID: viewerScopeDatapackID, Grant: tc.grant},
			}}
			h := handlers.NewMemoriesHandler(svc, nil, lookup)

			req := httptest.NewRequest(http.MethodGet,
				"/api/v1/memories?datapack_id="+viewerScopeDatapackID, nil)
			rec := httptest.NewRecorder()
			c := echo.New().NewContext(req, rec)
			c.Set(auth.ContextKeyAccount, &models.Account{ID: "acct-1"})

			if err := h.ListGET(c); err != nil {
				t.Fatal(err)
			}
			body := rec.Body.String()
			if rec.Code != http.StatusOK || !strings.Contains(body, "a fact") {
				t.Fatalf("got %d: %s", rec.Code, body)
			}
			if got := strings.Contains(body, ownerID); got != tc.wantOwner {
				t.Errorf("owner in response = %v, want %v: %s", got, tc.wantOwner, body)
			}
		})
	}
}

// Viewers can't delete memories, not even ones they added before they were a
// viewer. The service is nil: reaching it would panic.
func TestMemoriesDelete_ViewerForbidden(t *testing.T) {
	lookup := &fakeDatapackLookup{shared: map[string]*models.Datapack{
		"viewer-1:" + viewerScopeDatapackID: {ID: viewerScopeDatapackID, Grant: string(models.GrantViewer)},
	}}
	h := handlers.NewMemoriesHandler(nil, nil, lookup)

	req := httptest.NewRequest(http.MethodDelete,
		"/api/v1/memories/33333333-3333-3333-3333-333333333333?datapack_id="+viewerScopeDatapackID, nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.SetParamNames("memory_id")
	c.SetParamValues("33333333-3333-3333-3333-333333333333")
	c.Set(auth.ContextKeyAccount, &models.Account{ID: "viewer-1"})

	if err := h.DeleteByID(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d (body: %s)", rec.Code, rec.Body.String())
	}
}

// Keep-as-memory writes to memory rows, which a viewer may not do. The service
// is nil: reaching it would panic, so a 403 proves the handler stopped first.
func TestKeepAsMemory_ViewerForbidden(t *testing.T) {
	lookup := &fakeDatapackLookup{shared: map[string]*models.Datapack{
		"viewer-1:" + viewerScopeDatapackID: {ID: viewerScopeDatapackID, Grant: string(models.GrantViewer)},
	}}
	h := handlers.NewPromoteMemoriesHandler(nil, lookup)

	body := `{"datapack_id":"` + viewerScopeDatapackID + `","memory_ids":["33333333-3333-3333-3333-333333333333"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/keep-as-memory", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.Set(auth.ContextKeyAccount, &models.Account{ID: "viewer-1"})

	if err := h.KeepAsMemory(c); err != nil {
		t.Fatal(err)
	}

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d (body: %s)", rec.Code, rec.Body.String())
	}
}
