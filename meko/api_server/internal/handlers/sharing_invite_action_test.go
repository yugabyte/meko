package handlers_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/handlers"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// The invite belongs to bob; mallory is the one holding the credential.
const (
	inviteID    = "d290f1ee-6c54-4b01-90e6-d701748f0851"
	datapackID  = "7b1f1c9e-2d3a-4f55-9c88-0a1b2c3d4e5f"
	bobUserID   = "b2c3d4e5-f6a7-8901-bcde-f12345678901"
	malloryID   = "c3d4e5f6-a7b8-9012-cdef-123456789012"
	bobEmail    = "bob@example.com"
	ownerUserID = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
)

// inviteActionEnv wires a real handler over a real service and repo, with
// only the database faked. The service holds a concrete *SharingRepo, so
// sqlmock is the seam -- and it also lets each test assert on the SQL that
// does (or does not) run.
type inviteActionEnv struct {
	handler *handlers.SharingHandler
	mock    sqlmock.Sqlmock
	closeFn func()
}

func newInviteActionEnv(t *testing.T) *inviteActionEnv {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	repo := repository.NewSharingRepo(db)
	svc := service.NewSharingService(repo, nil)
	return &inviteActionEnv{
		handler: handlers.NewSharingHandler(svc),
		mock:    mock,
		closeFn: func() { _ = db.Close() },
	}
}

// rawBody hand-builds the request body. InviteActionRequest carries
// `json:"-"` on RecipientUserID, so marshaling the struct would drop the
// very field these tests need to put on the wire.
func rawBody(invite, recipient, action string) string {
	return fmt.Sprintf(
		`{"invite_id":%q,"recipient_user_id":%q,"user_action":%q}`,
		invite, recipient, action,
	)
}

// expectPendingInvite makes the invite lookup return a pending invite
// addressed to bob.
func (e *inviteActionEnv) expectPendingInvite() {
	now := time.Now()
	recipient := bobUserID
	e.mock.ExpectQuery("SELECT invite_id, datapack_id, owner_user_id").
		WithArgs(inviteID).
		WillReturnRows(sqlmock.NewRows([]string{
			"invite_id", "datapack_id", "owner_user_id", "recipient_email",
			"recipient_user_id", "grant_role", "status", "created_at", "updated_at",
		}).AddRow(
			inviteID, datapackID, ownerUserID, bobEmail,
			&recipient, string(models.GrantViewer), "pending", now, now,
		))
}

// post runs InviteAction as callerUserID, with bodyRecipient as the
// (untrusted) recipient_user_id in the request body.
func (e *inviteActionEnv) post(
	t *testing.T, callerUserID, bodyRecipient, action string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(
		http.MethodPost, "/api/v1/datapacks/invite-action",
		strings.NewReader(rawBody(inviteID, bodyRecipient, action)),
	)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.Set(auth.ContextKeyAccount, &models.Account{ID: callerUserID, Email: bobEmail})
	if err := e.handler.InviteAction(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	return rec
}

// TestInviteAction_RejectsCallerWhoIsNotRecipient is the security case.
// GET /datapacks/:id/outgoing-shares hands every owner and maintainer both
// the invite_id and the recipient's user_id, so a body-supplied recipient
// is enough to act on someone else's invite. The recipient must come from
// the authenticated account instead.
func TestInviteAction_RejectsCallerWhoIsNotRecipient(t *testing.T) {
	env := newInviteActionEnv(t)
	defer env.closeFn()

	env.expectPendingInvite()
	// The recipient-email fallback confirms bob, who is still not mallory.
	env.mock.ExpectQuery("SELECT user_id FROM meko_system.users").
		WithArgs(bobEmail).
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow(bobUserID))

	// mallory holds the credential but passes bob's id, as outgoing-shares
	// reported it. No UPDATE is queued, so the mock fails the test if the
	// invite is accepted anyway.
	rec := env.post(t, malloryID, bobUserID, "accept")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := env.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected SQL: %v", err)
	}
}

// TestInviteAction_IgnoresRecipientUserIDInBody is the other half: the body
// field is not consulted at all, so a caller acting on their own invite
// succeeds no matter what it claims.
func TestInviteAction_IgnoresRecipientUserIDInBody(t *testing.T) {
	env := newInviteActionEnv(t)
	defer env.closeFn()

	env.expectPendingInvite()
	env.mock.ExpectExec("UPDATE meko_system.datapack_invites").
		WithArgs(inviteID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	rec := env.post(t, bobUserID, malloryID, "decline")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := env.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected SQL: %v", err)
	}
}

// TestInviteAction_RequiresAccount guards the 401 branch: without an
// account in context there is no recipient to authorize against, and the
// body must not be trusted to supply one.
func TestInviteAction_RequiresAccount(t *testing.T) {
	env := newInviteActionEnv(t)
	defer env.closeFn()

	req := httptest.NewRequest(
		http.MethodPost, "/api/v1/datapacks/invite-action",
		strings.NewReader(rawBody(inviteID, bobUserID, "accept")),
	)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	if err := env.handler.InviteAction(echo.New().NewContext(req, rec)); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := env.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expected no SQL: %v", err)
	}
}
