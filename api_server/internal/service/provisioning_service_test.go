package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"meko/api_server/internal/models"
)

// mockDatapackCreator records calls to Create and returns configured
// responses.
type mockDatapackCreator struct {
	resp *CreateResponse
	err  error

	mu      sync.Mutex
	called  bool
	lastReq CreateRequest
}

func (m *mockDatapackCreator) Create(req CreateRequest) (*CreateResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.called = true
	m.lastReq = req
	return m.resp, m.err
}

// mockAccountLookup returns a fixed account or error.
type mockAccountLookup struct {
	acct *models.Account
	err  error

	mu     sync.Mutex
	called bool
	lastID string
}

func (m *mockAccountLookup) GetByID(_ context.Context, id string) (*models.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.called = true
	m.lastID = id
	return m.acct, m.err
}

// mockLangfuseProvisioner records calls and returns configured keys/errors.
type mockLangfuseProvisioner struct {
	enabled bool
	err     error
	host    string

	mu               sync.Mutex
	called           bool
	lastAccountID    string
	lastDatapackID   string
	lastDatapackName string
}

func (m *mockLangfuseProvisioner) Enabled() bool {
	return m.enabled
}

func (m *mockLangfuseProvisioner) GenerateProjectKeysForDatapack(
	accountID string,
	_ string,
	_ string,
	_ string,
	datapackID string,
	datapackName string,
) (*ProjectKeys, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.called = true
	m.lastAccountID = accountID
	m.lastDatapackID = datapackID
	m.lastDatapackName = datapackName
	if m.err != nil {
		return nil, m.err
	}
	return &ProjectKeys{
		ProjectID:   "proj-1",
		ProjectName: "proj",
		PublicKey:   "pk",
		SecretKey:   "sk",
	}, nil
}

func (m *mockLangfuseProvisioner) GetOrgKeysForAccount(_ string) (string, string, error) {
	return "org-pk", "org-sk", nil
}

func (m *mockLangfuseProvisioner) Host() string {
	if m.host != "" {
		return m.host
	}
	return "http://localhost:3000"
}

func TestProvisionNewAccount_HappyPath(t *testing.T) {
	dp := &mockDatapackCreator{
		resp: &CreateResponse{
			DatapackID:  "dp-1",
			AgentID:     "meko_agent",
			Name:        DefaultDatapackName,
			MCPUserName: "mcp_user_1",
			MCPServer: MCPServerInfo{
				URL:    "https://user_1.mcp.mekodev.com/mcp",
				APIKey: "mcp_secret",
			},
			DBAccess: DBAccessInfo{
				UserName: "mcp_user_1",
				Password: "secret",
				ConnStr:  "postgresql://mcp_user_1:secret@yb-host:5433/yugabyte",
			},
		},
	}
	acct := &mockAccountLookup{acct: &models.Account{
		ID:        "user_1",
		Email:     "user1@example.com",
		FirstName: "User",
		LastName:  "One",
	}}
	lf := &mockLangfuseProvisioner{enabled: true}

	svc := NewProvisioningService(dp, acct, lf)
	svc.ProvisionNewAccount(context.Background(), "user_1")

	if !dp.called {
		t.Fatal("expected DatapackCreator.Create to be called")
	}
	if dp.lastReq.Name != DefaultDatapackName {
		t.Errorf("datapack name = %q, want %q",
			dp.lastReq.Name, DefaultDatapackName)
	}
	if dp.lastReq.UserID != "user_1" {
		t.Errorf("user_id = %q, want user_1", dp.lastReq.UserID)
	}
	if dp.lastReq.AgentID != "" {
		t.Errorf("agent_id = %q, want \"\" (defaults to repository.DefaultAgentID)",
			dp.lastReq.AgentID)
	}

	if !acct.called {
		t.Fatal("expected AccountLookup.GetByID to be called for langfuse")
	}
	if acct.lastID != "user_1" {
		t.Errorf("account lookup id = %q, want user_1", acct.lastID)
	}

	if !lf.called {
		t.Fatal("expected Langfuse provisioning to be called")
	}
	if lf.lastDatapackID != "dp-1" {
		t.Errorf("langfuse datapack id = %q, want dp-1", lf.lastDatapackID)
	}
	if lf.lastDatapackName != DefaultDatapackName {
		t.Errorf("langfuse datapack name = %q, want %q",
			lf.lastDatapackName, DefaultDatapackName)
	}
	if lf.lastAccountID != "user_1" {
		t.Errorf("langfuse account id = %q, want user_1", lf.lastAccountID)
	}
}

func TestProvisionNewAccount_DatapackFails(t *testing.T) {
	dp := &mockDatapackCreator{err: errors.New("universe full")}
	acct := &mockAccountLookup{}
	lf := &mockLangfuseProvisioner{enabled: true}

	svc := NewProvisioningService(dp, acct, lf)
	svc.ProvisionNewAccount(context.Background(), "user_2")

	if !dp.called {
		t.Fatal("expected DatapackCreator.Create to be called")
	}
	if lf.called {
		t.Fatal("Langfuse should NOT be called when datapack creation fails")
	}
	if acct.called {
		t.Fatal("AccountLookup should NOT be called when datapack creation fails")
	}
}

func TestProvisionNewAccount_LangfuseDisabled(t *testing.T) {
	dp := &mockDatapackCreator{
		resp: &CreateResponse{
			DatapackID: "dp-disabled",
			Name:       DefaultDatapackName,
		},
	}
	acct := &mockAccountLookup{}
	lf := &mockLangfuseProvisioner{enabled: false}

	svc := NewProvisioningService(dp, acct, lf)
	svc.ProvisionNewAccount(context.Background(), "user_3")

	if !dp.called {
		t.Fatal("expected DatapackCreator.Create to be called")
	}
	if lf.called {
		t.Fatal("Langfuse should NOT be called when disabled")
	}
	if acct.called {
		t.Fatal("AccountLookup should NOT be called when langfuse disabled")
	}
}

func TestProvisionNewAccount_LangfuseNil(t *testing.T) {
	dp := &mockDatapackCreator{
		resp: &CreateResponse{
			DatapackID: "dp-nil",
			Name:       DefaultDatapackName,
		},
	}

	svc := NewProvisioningService(dp, nil, nil)
	svc.ProvisionNewAccount(context.Background(), "user_4")

	if !dp.called {
		t.Fatal("expected DatapackCreator.Create to be called")
	}
}

func TestProvisionNewAccount_AccountLookupErrors(t *testing.T) {
	dp := &mockDatapackCreator{
		resp: &CreateResponse{
			DatapackID: "dp-acct-err",
			Name:       DefaultDatapackName,
		},
	}
	acct := &mockAccountLookup{err: errors.New("db connection lost")}
	lf := &mockLangfuseProvisioner{enabled: true}

	svc := NewProvisioningService(dp, acct, lf)
	svc.ProvisionNewAccount(context.Background(), "user_5")

	if !dp.called {
		t.Fatal("expected DatapackCreator.Create to be called")
	}
	if !acct.called {
		t.Fatal("expected AccountLookup.GetByID to be called")
	}
	if lf.called {
		t.Fatal("Langfuse should NOT be called when account lookup fails")
	}
}

func TestProvisionNewAccount_AccountNotFound(t *testing.T) {
	dp := &mockDatapackCreator{
		resp: &CreateResponse{
			DatapackID: "dp-acct-nil",
			Name:       DefaultDatapackName,
		},
	}
	acct := &mockAccountLookup{acct: nil}
	lf := &mockLangfuseProvisioner{enabled: true}

	svc := NewProvisioningService(dp, acct, lf)
	svc.ProvisionNewAccount(context.Background(), "user_6")

	if lf.called {
		t.Fatal("Langfuse should NOT be called when account is nil")
	}
}

func TestProvisionNewAccount_LangfuseEnabledButNoAccountStore(t *testing.T) {
	dp := &mockDatapackCreator{
		resp: &CreateResponse{
			DatapackID: "dp-no-store",
			Name:       DefaultDatapackName,
		},
	}
	lf := &mockLangfuseProvisioner{enabled: true}

	svc := NewProvisioningService(dp, nil, lf)
	svc.ProvisionNewAccount(context.Background(), "user_7")

	if !dp.called {
		t.Fatal("expected DatapackCreator.Create to be called")
	}
	if lf.called {
		t.Fatal("Langfuse should NOT be called without an account store")
	}
}

func TestProvisionNewAccount_LangfuseGenerateError(t *testing.T) {
	dp := &mockDatapackCreator{
		resp: &CreateResponse{
			DatapackID: "dp-lf-err",
			Name:       DefaultDatapackName,
		},
	}
	acct := &mockAccountLookup{acct: &models.Account{ID: "user_8"}}
	lf := &mockLangfuseProvisioner{
		enabled: true,
		err:     errors.New("langfuse api unreachable"),
	}

	svc := NewProvisioningService(dp, acct, lf)
	svc.ProvisionNewAccount(context.Background(), "user_8")

	if !lf.called {
		t.Fatal("expected Langfuse provisioning to be attempted")
	}
}

func TestProvisionNewAccount_FallsBackToDefaultName(t *testing.T) {
	dp := &mockDatapackCreator{
		resp: &CreateResponse{
			DatapackID: "dp-fallback",
			Name:       "",
		},
	}
	acct := &mockAccountLookup{acct: &models.Account{ID: "user_9"}}
	lf := &mockLangfuseProvisioner{enabled: true}

	svc := NewProvisioningService(dp, acct, lf)
	svc.ProvisionNewAccount(context.Background(), "user_9")

	if lf.lastDatapackName != DefaultDatapackName {
		t.Errorf("langfuse datapack name = %q, want %q (fallback)",
			lf.lastDatapackName, DefaultDatapackName)
	}
}
