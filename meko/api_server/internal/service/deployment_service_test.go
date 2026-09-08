package service_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"strings"
	"testing"

	_ "github.com/lib/pq"

	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// testConnStr matches internal/handlers' own test-DB helper (same local
// YugabyteDB instance every other repo-backed test in this codebase uses).
const testConnStr = "postgresql://localhost:5433/yugabyte?user=yugabyte&password=yugabyte&sslmode=disable"

// openTestDB skips (not fails) when no local test database is reachable,
// same convention internal/handlers' own openTestDB uses.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", testConnStr)
	if err != nil {
		t.Skipf("test database unavailable: open: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("test database unavailable: ping: %v", err)
	}
	var exists bool
	if err := db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		                WHERE table_schema = 'meko_system' AND table_name = 'deployments')`,
	).Scan(&exists); err != nil || !exists {
		t.Skip("test database missing meko_system.deployments; run migrations first")
	}
	return db
}

// fakeAgentCoreData is a hand-rolled agentCoreDataAPI fake, following the
// same "no existing precedent for mocking an AWS SDK client" rationale
// agentCoreAPI's own doc comment gives -- this is the first test to
// actually exercise WithAgentCoreClients.
type fakeAgentCoreData struct {
	response string
	err      error
}

func (f *fakeAgentCoreData) InvokeAgentRuntime(
	ctx context.Context, in *bedrockagentcore.InvokeAgentRuntimeInput, _ ...func(*bedrockagentcore.Options),
) (*bedrockagentcore.InvokeAgentRuntimeOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &bedrockagentcore.InvokeAgentRuntimeOutput{Response: io.NopCloser(strings.NewReader(f.response))}, nil
}

func newTestDeploymentService(t *testing.T, cfg service.DeploymentConfig) *service.DeploymentService {
	t.Helper()
	// nil repo is safe for every case exercised here: validation (including
	// the cfg.Enabled() gate) runs before any repo call.
	svc, err := service.NewDeploymentService(context.Background(), nil, nil, cfg)
	if err != nil {
		t.Fatalf("NewDeploymentService: unexpected error: %v", err)
	}
	return svc
}

func enabledTestConfig() service.DeploymentConfig {
	return service.DeploymentConfig{
		DefaultRegion:           "us-east-1",
		AgentImagesRepo:         "123456789012.dkr.ecr.us-east-1.amazonaws.com/meko-labs-agents",
		ManagedExecutionRoleARN: "arn:aws:iam::123456789012:role/meko-agentcore-execution",
		MekoAWSAccountID:        "123456789012",
	}
}

func TestDeploymentConfig_Enabled(t *testing.T) {
	if (service.DeploymentConfig{}).Enabled() {
		t.Error("empty config should not be enabled")
	}
	if !enabledTestConfig().Enabled() {
		t.Error("fully-configured config should be enabled")
	}
}

// TestDeploymentConfig_ImageURI locks in the naming convention a new
// deployable lab relies on: no new config field or env var, just a new
// SupportedDeploymentAgentSlugs entry and an image published to this path.
func TestDeploymentConfig_ImageURI(t *testing.T) {
	cfg := service.DeploymentConfig{AgentImagesRepo: "123456789012.dkr.ecr.us-east-1.amazonaws.com/meko-labs-agents"}
	for _, slug := range []string{"adlc-review-agent", "some-future-lab-agent"} {
		want := cfg.AgentImagesRepo + "/" + slug + ":latest"
		if got := cfg.ImageURI(slug); got != want {
			t.Errorf("ImageURI(%q) = %q, want %q", slug, got, want)
		}
	}
}

func TestCreateManaged_RequiresUserID(t *testing.T) {
	svc := newTestDeploymentService(t, enabledTestConfig())
	_, err := svc.CreateManaged(context.Background(), "", "adlc-review-agent")
	if !errors.Is(err, service.ErrValidation) {
		t.Errorf("CreateManaged(user_id=\"\") err = %v, want ErrValidation", err)
	}
}

func TestCreateManaged_RejectsUnsupportedAgentSlug(t *testing.T) {
	svc := newTestDeploymentService(t, enabledTestConfig())
	for _, slug := range []string{"", "some-other-agent", "ADLC-REVIEW-AGENT"} {
		_, err := svc.CreateManaged(context.Background(), "user-1", slug)
		if !errors.Is(err, service.ErrValidation) {
			t.Errorf("CreateManaged(agent_slug=%q) err = %v, want ErrValidation", slug, err)
		}
	}
}

func TestCreateManaged_DisabledWhenNotConfigured(t *testing.T) {
	svc := newTestDeploymentService(t, service.DeploymentConfig{})
	_, err := svc.CreateManaged(context.Background(), "user-1", "adlc-review-agent")
	if err == nil {
		t.Fatal("expected error when service is not configured")
	}
	if errors.Is(err, service.ErrValidation) {
		t.Error("disabled-service error should not be classified as ErrValidation")
	}
}

func TestStartBYOC_RequiresUserID(t *testing.T) {
	svc := newTestDeploymentService(t, enabledTestConfig())
	_, err := svc.StartBYOC(context.Background(), "", "adlc-review-agent")
	if !errors.Is(err, service.ErrValidation) {
		t.Errorf("StartBYOC(user_id=\"\") err = %v, want ErrValidation", err)
	}
}

func TestStartBYOC_RejectsUnsupportedAgentSlug(t *testing.T) {
	svc := newTestDeploymentService(t, enabledTestConfig())
	_, err := svc.StartBYOC(context.Background(), "user-1", "not-adlc-review-agent")
	if !errors.Is(err, service.ErrValidation) {
		t.Errorf("StartBYOC(agent_slug=invalid) err = %v, want ErrValidation", err)
	}
}

func TestConfirmBYOC_RejectsMalformedRoleARN(t *testing.T) {
	svc := newTestDeploymentService(t, enabledTestConfig())
	for _, arn := range []string{
		"",
		"not-an-arn",
		"arn:aws:iam::123:role/too-short-account-id",
		"arn:aws:iam::123456789012:user/not-a-role",
	} {
		_, err := svc.ConfirmBYOC(context.Background(), "user-1", "dep-1", arn)
		if !errors.Is(err, service.ErrValidation) {
			t.Errorf("ConfirmBYOC(role_arn=%q) err = %v, want ErrValidation", arn, err)
		}
	}
}

func TestList_RequiresUserID(t *testing.T) {
	svc := newTestDeploymentService(t, enabledTestConfig())
	_, err := svc.List(context.Background(), "")
	if !errors.Is(err, service.ErrValidation) {
		t.Errorf("List(user_id=\"\") err = %v, want ErrValidation", err)
	}
}

func TestGet_RequiresID(t *testing.T) {
	svc := newTestDeploymentService(t, enabledTestConfig())
	_, err := svc.Get(context.Background(), "user-1", "")
	if !errors.Is(err, service.ErrValidation) {
		t.Errorf("Get(id=\"\") err = %v, want ErrValidation", err)
	}
}

// newRepoBackedDeploymentService builds a real (DB-backed) DeploymentService
// with a fake AgentCore data-plane client -- Invoke's not-found/non-active
// checks run a real repo.GetByID before any AWS call, so (unlike every test
// above) a nil repo would panic here.
func newRepoBackedDeploymentService(t *testing.T, db *sql.DB, data *fakeAgentCoreData) *service.DeploymentService {
	t.Helper()
	repo := repository.NewDeploymentRepo(db)
	svc, err := service.NewDeploymentService(
		context.Background(), repo, nil, enabledTestConfig(),
		service.WithAgentCoreClients(nil, data),
	)
	if err != nil {
		t.Fatalf("NewDeploymentService: unexpected error: %v", err)
	}
	return svc
}

func TestInvoke_RejectsMissingDeployment(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	svc := newRepoBackedDeploymentService(t, db, &fakeAgentCoreData{})

	_, err := svc.Invoke(context.Background(), "user-1", "does-not-exist", []byte(`{}`))
	if err == nil {
		t.Fatal("expected an error for a missing deployment")
	}
	if errors.Is(err, service.ErrValidation) {
		t.Error("not-found error should not be classified as ErrValidation")
	}
}

func TestInvoke_RejectsNonActiveDeployment(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	repo := repository.NewDeploymentRepo(db)
	svc := newRepoBackedDeploymentService(t, db, &fakeAgentCoreData{})

	dep, err := repo.Create(context.Background(), repository.CreateDeploymentParams{
		UserID: "user-1", AgentSlug: "adlc-review-agent",
		Runtime: models.DeploymentRuntimeAgentCore, AccountMode: models.DeploymentAccountModeManaged,
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatalf("seed deployment: %v", err)
	}
	defer repo.DeleteByID(context.Background(), "user-1", dep.ID)
	// Freshly created rows start "pending" (not "active") -- never provisioned.

	_, err = svc.Invoke(context.Background(), "user-1", dep.ID, []byte(`{}`))
	if !errors.Is(err, service.ErrValidation) {
		t.Errorf("Invoke on a non-active deployment: err = %v, want ErrValidation", err)
	}
}

func TestInvoke_StreamsResponseBodyThrough(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	repo := repository.NewDeploymentRepo(db)
	wantBody := "data: {\"type\":\"text_delta\",\"text\":\"hello\"}\n\ndata: {\"type\":\"done\"}\n\n"
	svc := newRepoBackedDeploymentService(t, db, &fakeAgentCoreData{response: wantBody})

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

	body, err := svc.Invoke(context.Background(), "user-1", dep.ID, []byte(`{"datapack_id":"dp-1"}`))
	if err != nil {
		t.Fatalf("Invoke: unexpected error: %v", err)
	}
	defer body.Close()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if string(got) != wantBody {
		t.Errorf("streamed body = %q, want %q", got, wantBody)
	}
}
