package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/labstack/echo/v4"

	"meko/api_server/internal/service"
)

func expectDesiredRev(mock sqlmock.Sqlmock, rev int64) {
	mock.ExpectQuery(`SELECT rev FROM meko_system\.environment_desired_revs`).
		WithArgs(envUUID).WillReturnRows(sqlmock.NewRows([]string{"rev"}).AddRow(rev))
}

func expectDesiredRows(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`FROM meko_system\.environment_desired_resources`).
		WithArgs(envUUID).WillReturnRows(adminOnlyDesiredRows())
}

// fastLongPoll shortens the hold so a test that waits it out stays quick.
func fastLongPoll(t *testing.T, every, maxWait time.Duration) {
	t.Helper()
	oldEvery, oldMax := service.DesiredPollEvery, service.DesiredMaxWait
	service.DesiredPollEvery, service.DesiredMaxWait = every, maxWait
	t.Cleanup(func() { service.DesiredPollEvery, service.DesiredMaxWait = oldEvery, oldMax })
}

func changeToken(t *testing.T, raw []byte) string {
	t.Helper()
	data := extensionsBody(t, raw)
	if string(data["long_poll"]) != "true" {
		t.Errorf("long_poll = %s, want true", data["long_poll"])
	}
	return string(data["change_token"])
}

// A stale since is answered at once, with no hold and no second token read.
func TestAgentDesiredState_StaleSinceAnswersAtOnce(t *testing.T) {
	fastLongPoll(t, time.Hour, time.Hour)
	h, mock, done := newScopeHandler(t)
	defer done()
	expectResolveAmpAgent(mock)
	expectDesiredRev(mock, 8)
	expectDesiredRows(mock)

	rec := agentRequest(t, h.AgentDesiredState, http.MethodGet,
		"/api/v1/agent/desired-state?since=7&wait=25s", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := changeToken(t, rec.Body.Bytes()); got != "8" {
		t.Errorf("change_token = %s, want 8", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A current since is held until the token moves, and the answer carries the new one.
func TestAgentDesiredState_HoldsUntilTheTokenMoves(t *testing.T) {
	fastLongPoll(t, time.Millisecond, time.Hour)
	h, mock, done := newScopeHandler(t)
	defer done()
	expectResolveAmpAgent(mock)
	expectDesiredRev(mock, 7)
	expectDesiredRev(mock, 7)
	expectDesiredRev(mock, 9)
	expectDesiredRows(mock)

	rec := agentRequest(t, h.AgentDesiredState, http.MethodGet,
		"/api/v1/agent/desired-state?since=7&wait=25s", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := changeToken(t, rec.Body.Bytes()); got != "9" {
		t.Errorf("change_token = %s, want 9", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// When the wait runs out with nothing changed, the agent still gets the current state:
// that answer is its periodic pass.
func TestAgentDesiredState_AnswersWhenTheWaitRunsOut(t *testing.T) {
	fastLongPoll(t, time.Hour, 20*time.Millisecond) // the cap, not the asked-for 25s, ends it
	h, mock, done := newScopeHandler(t)
	defer done()
	expectResolveAmpAgent(mock)
	expectDesiredRev(mock, 7)
	expectDesiredRows(mock)

	start := time.Now()
	rec := agentRequest(t, h.AgentDesiredState, http.MethodGet,
		"/api/v1/agent/desired-state?since=7&wait=25s", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if took := time.Since(start); took < 20*time.Millisecond || took > 5*time.Second {
		t.Errorf("held %s, want about the 20ms cap", took)
	}
	if got := changeToken(t, rec.Body.Bytes()); got != "7" {
		t.Errorf("change_token = %s, want 7", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// An agent that hangs up ends the hold, and nothing more is read for it.
func TestAgentDesiredState_HangUpEndsTheHold(t *testing.T) {
	fastLongPoll(t, time.Hour, time.Hour)
	h, mock, done := newScopeHandler(t)
	defer done()
	expectResolveAmpAgent(mock)
	expectDesiredRev(mock, 7)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/desired-state?since=7&wait=25s", nil).
		WithContext(ctx)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.Set("meko_api_key_id", keyA)
	if err := h.AgentDesiredState(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("wrote %q to an agent that hung up", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// Anything short of a valid since AND wait is today's immediate answer, never an
// error: an agent must not be refused desired state over a query string.
func TestAgentDesiredState_MalformedParamsAnswerAtOnce(t *testing.T) {
	fastLongPoll(t, time.Hour, time.Hour)
	for _, q := range []string{"", "?since=7", "?wait=25s", "?since=x&wait=25s", "?since=7&wait=soon",
		"?since=7&wait=-1s", "?since=7&wait=0s", "?since=7&wait=0"} {
		t.Run(q, func(t *testing.T) {
			h, mock, done := newScopeHandler(t)
			defer done()
			expectResolveAmpAgent(mock)
			expectDesiredRev(mock, 7)
			expectDesiredRows(mock)

			rec := agentRequest(t, h.AgentDesiredState, http.MethodGet, "/api/v1/agent/desired-state"+q, "", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `"change_token":7`) {
				t.Errorf("body %s lacks change_token 7", rec.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Error(err)
			}
		})
	}
}

// wait may be a bare number of seconds, as the OpenAPI spec first described it; it is
// held like a duration rather than dropped to an immediate answer.
func TestAgentDesiredState_IntegerWaitIsSeconds(t *testing.T) {
	fastLongPoll(t, time.Hour, 20*time.Millisecond)
	h, mock, done := newScopeHandler(t)
	defer done()
	expectResolveAmpAgent(mock)
	expectDesiredRev(mock, 7)
	expectDesiredRows(mock)

	start := time.Now()
	rec := agentRequest(t, h.AgentDesiredState, http.MethodGet, "/api/v1/agent/desired-state?since=7&wait=30", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if took := time.Since(start); took < 20*time.Millisecond {
		t.Errorf("answered after %s: wait=30 was not held", took)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
