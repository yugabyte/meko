package handlers_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/handlers"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

const testSharedAgentSlug = "adlc-review-agent"
const testSharedARN = "arn:aws:bedrock-agentcore:us-east-1:123456789012:runtime/shared-agentk"

// echoWithCaller returns a minimal Echo instance whose auth context resolves
// to callerID -- same pattern deployment_invoke_handler_test.go uses.
func echoWithCaller(callerID string) *echo.Echo {
	e := echo.New()
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set(auth.ContextKeyAccount, &models.Account{ID: callerID})
			return next(c)
		}
	})
	return e
}

// newSharedTestDeploymentService builds a DeploymentService against a real
// (DB-backed) SharedDeploymentRepo -- unlike agentCoreAPI/agentCoreDataAPI,
// there's no fake-injection point for it, so GetShared/InvokeShared tests
// need a real DB, same as the owner-scoped DeploymentRepo tests do.
func newSharedTestDeploymentService(t *testing.T, sharedRepo *repository.SharedDeploymentRepo, dataClient *fakeInvokeClient) *service.DeploymentService {
	t.Helper()
	svc, err := service.NewDeploymentService(
		context.Background(), nil, sharedRepo, service.DeploymentConfig{
			DefaultRegion: "us-east-1", AgentImagesRepo: "repo",
			ManagedExecutionRoleARN: "arn:aws:iam::123456789012:role/x", MekoAWSAccountID: "123456789012",
		},
		service.WithAgentCoreClients(nil, dataClient),
	)
	if err != nil {
		t.Fatalf("NewDeploymentService: %v", err)
	}
	return svc
}

// TestDeployment_GetShared_AnyCallerReachesTheConfiguredDeployment proves the
// point of the route: any authenticated caller gets the farm entry for
// agent_slug back, regardless of who they are -- there's no ownership
// concept here at all, unlike the owner-scoped /deployments/:id route.
func TestDeployment_GetShared_AnyCallerReachesTheConfiguredDeployment(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	sharedRepo := repository.NewSharedDeploymentRepo(db)
	if err := sharedRepo.Set(context.Background(), testSharedAgentSlug, testSharedARN); err != nil {
		t.Fatalf("seed shared arn: %v", err)
	}

	svc := newSharedTestDeploymentService(t, sharedRepo, &fakeInvokeClient{})
	h := handlers.NewDeploymentHandler(svc)
	e := echoWithCaller("some-other-user")
	e.GET("/api/v1/deployments/shared/:agent_slug", h.GetShared)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/deployments/shared/"+testSharedAgentSlug, nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			AgentSlug       string `json:"agent_slug"`
			Status          string `json:"status"`
			AgentRuntimeARN string `json:"agent_runtime_arn"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Data.AgentSlug != testSharedAgentSlug {
		t.Errorf("got agent_slug %q, want %q", body.Data.AgentSlug, testSharedAgentSlug)
	}
	if body.Data.Status != models.DeploymentStatusActive {
		t.Errorf("got status %q, want %q", body.Data.Status, models.DeploymentStatusActive)
	}
	if body.Data.AgentRuntimeARN != testSharedARN {
		t.Errorf("got agent_runtime_arn %q, want %q", body.Data.AgentRuntimeARN, testSharedARN)
	}
}

// TestDeployment_GetShared_404sWhenNotConfigured -- an agent_slug with no
// farm entry yet gets a clean 404, not a confusing lookup error. Also
// covers the case an operator hasn't onboarded ANY shared deployment: an
// empty table behaves identically to a missing row.
func TestDeployment_GetShared_404sWhenNotConfigured(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	sharedRepo := repository.NewSharedDeploymentRepo(db)

	svc := newSharedTestDeploymentService(t, sharedRepo, &fakeInvokeClient{})
	h := handlers.NewDeploymentHandler(svc)
	e := echoWithCaller("some-user")
	e.GET("/api/v1/deployments/shared/:agent_slug", h.GetShared)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/deployments/shared/no-such-agent", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestDeployment_InvokeShared_StreamsSSEBody mirrors
// TestDeployment_Invoke_StreamsSSEBody but through the shared route, calling
// InvokeAgentRuntime directly against the farm entry's ARN with no
// deployments-table row involved at all.
func TestDeployment_InvokeShared_StreamsSSEBody(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	sharedRepo := repository.NewSharedDeploymentRepo(db)
	if err := sharedRepo.Set(context.Background(), testSharedAgentSlug, testSharedARN); err != nil {
		t.Fatalf("seed shared arn: %v", err)
	}

	wantBody := "data: {\"type\":\"text_delta\",\"text\":\"hello\"}\n\ndata: {\"type\":\"done\"}\n\n"
	svc := newSharedTestDeploymentService(t, sharedRepo, &fakeInvokeClient{response: wantBody})

	h := handlers.NewDeploymentHandler(svc)
	e := echoWithCaller("some-other-user")
	e.POST("/api/v1/deployments/shared/:agent_slug/invoke", h.InvokeShared)

	srv := httptest.NewServer(e)
	defer srv.Close()

	// No meko_pat: the shared instance authenticates with its own
	// credential, not one forwarded from the caller.
	resp, err := http.Post(srv.URL+"/api/v1/deployments/shared/"+testSharedAgentSlug+"/invoke", "application/json",
		strings.NewReader(`{"datapack_id":"dp-1"}`))
	if err != nil {
		t.Fatalf("POST invoke: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	scanner := bufio.NewScanner(resp.Body)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	got := strings.Join(lines, "\n")
	for _, want := range []string{`"text":"hello"`, `"type":"done"`} {
		if !strings.Contains(got, want) {
			t.Errorf("streamed body missing %q; got: %s", want, got)
		}
	}
}
