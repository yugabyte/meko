package handlers_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/handlers"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// fakeInvokeClient is a minimal agentCoreDataAPI-shaped fake for exercising
// the HTTP streaming path end to end.
type fakeInvokeClient struct {
	response string
}

func (f *fakeInvokeClient) InvokeAgentRuntime(
	ctx context.Context, in *bedrockagentcore.InvokeAgentRuntimeInput, _ ...func(*bedrockagentcore.Options),
) (*bedrockagentcore.InvokeAgentRuntimeOutput, error) {
	return &bedrockagentcore.InvokeAgentRuntimeOutput{Response: io.NopCloser(strings.NewReader(f.response))}, nil
}

// TestDeployment_Invoke_StreamsSSEBody seeds a real active deployment row,
// wires a minimal Echo instance around DeploymentHandler (built from a
// fake-backed DeploymentService, not the full api.NewRouter wiring -- that
// constructs DeploymentService from real ambient AWS config with no
// injection point, see deployment_service.go's WithAgentCoreClients doc
// comment), and asserts the client reads back the exact SSE body a real
// listener (httptest.NewServer, not httptest.NewRecorder -- recorders
// don't meaningfully exercise chunked transfer/flushing) delivered.
func TestDeployment_Invoke_StreamsSSEBody(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := repository.NewDeploymentRepo(db)
	wantBody := "data: {\"type\":\"text_delta\",\"text\":\"hello\"}\n\ndata: {\"type\":\"done\"}\n\n"
	svc, err := service.NewDeploymentService(
		context.Background(), repo, service.DeploymentConfig{
			DefaultRegion: "us-east-1", AgentImagesRepo: "repo",
			ManagedExecutionRoleARN: "arn:aws:iam::123456789012:role/x", MekoAWSAccountID: "123456789012",
		},
		service.WithAgentCoreClients(nil, &fakeInvokeClient{response: wantBody}),
	)
	if err != nil {
		t.Fatalf("NewDeploymentService: %v", err)
	}

	dep, err := repo.Create(context.Background(), repository.CreateDeploymentParams{
		UserID: "user-1", AgentSlug: "adlc-review-agent",
		Runtime: models.DeploymentRuntimeAgentCore, AccountMode: models.DeploymentAccountModeManaged,
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatalf("seed deployment: %v", err)
	}
	defer repo.DeleteByID(context.Background(), "user-1", dep.ID)
	if err := repo.UpdateStatus(context.Background(), dep.ID, models.DeploymentStatusActive, "",
		"arn:aws:bedrock-agentcore:us-east-1:123456789012:runtime/test", "runtime-id-1", "repo/adlc-review-agent:latest"); err != nil {
		t.Fatalf("seed active status: %v", err)
	}

	// Minimal Echo instance carrying just enough auth context for
	// auth.GetAccount(c) to resolve -- real cookie/JWT auth is exercised by
	// internal/auth's own tests, not re-tested here.
	e := echo.New()
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set(auth.ContextKeyAccount, &models.Account{ID: "user-1"})
			return next(c)
		}
	})
	h := handlers.NewDeploymentHandler(svc)
	e.POST("/api/v1/deployments/:id/invoke", h.Invoke)

	srv := httptest.NewServer(e)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/v1/deployments/"+dep.ID+"/invoke", "application/json", strings.NewReader(`{"datapack_id":"dp-1"}`))
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
