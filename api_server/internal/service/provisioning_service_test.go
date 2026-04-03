package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"meko/api_server/internal/k8s"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// mockDatapackCreator records calls and returns configured responses.
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

// mockCRDCreator records calls and returns configured error.
type mockCRDCreator struct {
	err error

	mu         sync.Mutex
	called     bool
	lastParams k8s.MekoUserAccountParams
}

func (m *mockCRDCreator) CreateMekoUserAccount(_ context.Context, p k8s.MekoUserAccountParams) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.called = true
	m.lastParams = p
	return m.err
}

// mockTierConfigStore returns a fixed TierConfig or error.
type mockTierConfigStore struct {
	cfg *repository.TierConfig
	err error

	mu     sync.Mutex
	called bool
}

func (m *mockTierConfigStore) GetByTier(_ context.Context, _ string) (*repository.TierConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.called = true
	return m.cfg, m.err
}

type mockAccountLookup struct {
	acct *models.Account
	err  error

	mu     sync.Mutex
	called bool
}

func (m *mockAccountLookup) GetByID(_ context.Context, _ string) (*models.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.called = true
	return m.acct, m.err
}

type mockLangfuseProvisioner struct {
	enabled bool
	err     error

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
	return "http://localhost:3000"
}

func testTierStore() *mockTierConfigStore {
	return &mockTierConfigStore{
		cfg: &repository.TierConfig{
			Tier:          "free",
			MCPImage:      "registry/mcp:latest",
			RAGImage:    "registry/rag:0.1.0",
			MCPReplicas: 1,
			RAGReplicas:   1,
		},
	}
}

func testConfig() ProvisioningConfig {
	return ProvisioningConfig{
		CRDNamespace: "meko-tenants",
	}
}

func TestProvisionNewAccount_HappyPath(t *testing.T) {
	dp := &mockDatapackCreator{
		resp: &CreateResponse{
			DatapackID:  "dp-1",
			AgentID:     "agent-1",
			Name:        "meko_default_datapack",
			MCPUserName: "mcp_acc1",
			MCPServer: MCPServerInfo{
				URL:    "https://acc1.mcp.mekodev.com/mcp",
				APIKey: "mcp_secret",
			},
			DBAccess: DBAccessInfo{
				UserName: "db_meko_default_datapack",
				Password: "secret",
				ConnStr:  "postgresql://db_meko_default_datapack:secret@yb-host:5433/acc1_meko_default_datapack",
			},
		},
	}
	crd := &mockCRDCreator{}
	ts := testTierStore()
	acct := &mockAccountLookup{acct: &models.Account{
		ID:        "acc1",
		Email:     "acc1@example.com",
		FirstName: "Acc",
		LastName:  "One",
	}}
	lf := &mockLangfuseProvisioner{enabled: true}
	svc := NewProvisioningService(dp, crd, ts, acct, lf, testConfig())

	svc.ProvisionNewAccount(context.Background(), "acc1")

	if !dp.called {
		t.Fatal("expected DatapackCreator.Create to be called")
	}
	if dp.lastReq.Name != "meko_default_datapack" {
		t.Errorf("datapack name = %q, want meko_default_datapack", dp.lastReq.Name)
	}
	if dp.lastReq.AccountID != "acc1" {
		t.Errorf("account_id = %q, want acc1", dp.lastReq.AccountID)
	}

	if !ts.called {
		t.Fatal("expected TierConfigStore.GetByTier to be called")
	}

	if !crd.called {
		t.Fatal("expected CRDCreator.CreateMekoUserAccount to be called")
	}
	if !lf.called {
		t.Fatal("expected Langfuse provisioning to be called")
	}
	if lf.lastDatapackID != "dp-1" {
		t.Errorf("langfuse datapack id = %q, want dp-1", lf.lastDatapackID)
	}
	if lf.lastDatapackName != "meko_default_datapack" {
		t.Errorf("langfuse datapack name = %q, want meko_default_datapack", lf.lastDatapackName)
	}
	p := crd.lastParams
	if p.AccountID != "acc1" {
		t.Errorf("CRD accountID = %q, want acc1", p.AccountID)
	}
	if p.Namespace != "meko-tenants" {
		t.Errorf("CRD namespace = %q, want meko-tenants", p.Namespace)
	}
	if p.AccountTier != "free" {
		t.Errorf("CRD accountTier = %q, want free", p.AccountTier)
	}
	if p.YugabyteDBHost != "yb-host" {
		t.Errorf("CRD host = %q, want yb-host", p.YugabyteDBHost)
	}
	if p.YugabyteDBPort != 5433 {
		t.Errorf("CRD port = %d, want 5433", p.YugabyteDBPort)
	}
	if p.YugabyteDBUser != "mcp_acc1" {
		t.Errorf("CRD user = %q, want mcp_acc1", p.YugabyteDBUser)
	}
	if p.YugabyteDBPass != "mcp_secret" {
		t.Errorf("CRD password = %q, want mcp_secret", p.YugabyteDBPass)
	}
	if p.YugabyteDBName != "acc1_meko_default_datapack" {
		t.Errorf("CRD dbName = %q, want acc1_meko_default_datapack", p.YugabyteDBName)
	}
	if p.MCPImage != "registry/mcp:latest" {
		t.Errorf("CRD mcpImage = %q, want registry/mcp:latest", p.MCPImage)
	}
	if p.MCPReplicas != 1 {
		t.Errorf("CRD mcpReplicas = %d, want 1", p.MCPReplicas)
	}
	if p.RAGReplicas != 1 {
		t.Errorf("CRD ragReplicas = %d, want 1", p.RAGReplicas)
	}
	if p.LangfuseSecretKey != "sk" {
		t.Errorf("CRD langfuseSecretKey = %q, want sk", p.LangfuseSecretKey)
	}
	if p.LangfusePublicKey != "pk" {
		t.Errorf("CRD langfusePublicKey = %q, want pk", p.LangfusePublicKey)
	}
	if p.LangfuseBaseURL != "http://localhost:3000" {
		t.Errorf("CRD langfuseBaseURL = %q, want http://localhost:3000", p.LangfuseBaseURL)
	}
}

func TestProvisionNewAccount_DatapackFails(t *testing.T) {
	dp := &mockDatapackCreator{
		err: errors.New("universe full"),
	}
	crd := &mockCRDCreator{}
	svc := NewProvisioningService(dp, crd, testTierStore(), nil, nil, testConfig())

	svc.ProvisionNewAccount(context.Background(), "acc2")

	if !dp.called {
		t.Fatal("expected DatapackCreator.Create to be called")
	}
	if crd.called {
		t.Fatal("CRD creator should NOT be called when datapack creation fails")
	}
}

func TestProvisionNewAccount_CRDFails(t *testing.T) {
	dp := &mockDatapackCreator{
		resp: &CreateResponse{
			DatapackID:  "dp-2",
			MCPUserName: "mcp_acc3",
			MCPServer:   MCPServerInfo{APIKey: "mcp_pass"},
			DBAccess: DBAccessInfo{
				UserName: "db_user",
				Password: "pass",
				ConnStr:  "postgresql://db_user:pass@yb-host:5433/acc3_meko_default_datapack",
			},
		},
	}
	crd := &mockCRDCreator{
		err: errors.New("k8s api unreachable"),
	}
	svc := NewProvisioningService(dp, crd, testTierStore(), nil, nil, testConfig())

	svc.ProvisionNewAccount(context.Background(), "acc3")

	if !dp.called {
		t.Fatal("expected DatapackCreator.Create to be called")
	}
	if !crd.called {
		t.Fatal("expected CRDCreator.CreateMekoUserAccount to be called")
	}
}

func TestProvisionNewAccount_TierConfigFails(t *testing.T) {
	dp := &mockDatapackCreator{
		resp: &CreateResponse{
			DatapackID:  "dp-tc",
			MCPUserName: "mcp_acc_tc",
			MCPServer:   MCPServerInfo{APIKey: "mcp_pass"},
			DBAccess: DBAccessInfo{
				UserName: "db_user",
				Password: "pass",
				ConnStr:  "postgresql://db_user:pass@yb-host:5433/acc_tc_meko_default_datapack",
			},
		},
	}
	crd := &mockCRDCreator{}
	ts := &mockTierConfigStore{err: errors.New("tier not found")}
	svc := NewProvisioningService(dp, crd, ts, nil, nil, testConfig())

	svc.ProvisionNewAccount(context.Background(), "acc_tc")

	if !dp.called {
		t.Fatal("expected DatapackCreator.Create to be called")
	}
	if crd.called {
		t.Fatal("CRD creator should NOT be called when tier config lookup fails")
	}
}

func TestParseConnStr_Valid(t *testing.T) {
	host, port, dbName, err := parseConnStr("postgresql://user:pass@myhost:5433/mydb")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "myhost" {
		t.Errorf("host = %q, want myhost", host)
	}
	if port != 5433 {
		t.Errorf("port = %d, want 5433", port)
	}
	if dbName != "mydb" {
		t.Errorf("dbName = %q, want mydb", dbName)
	}
}

func TestParseConnStr_DefaultPort(t *testing.T) {
	host, port, dbName, err := parseConnStr("postgresql://user:pass@myhost/mydb")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "myhost" {
		t.Errorf("host = %q, want myhost", host)
	}
	if port != 5433 {
		t.Errorf("port = %d, want 5433 (default)", port)
	}
	if dbName != "mydb" {
		t.Errorf("dbName = %q, want mydb", dbName)
	}
}

func TestParseConnStr_MissingHost(t *testing.T) {
	_, _, _, err := parseConnStr("postgresql:///mydb")
	if err == nil {
		t.Fatal("expected error for missing host")
	}
}

func TestParseConnStr_MissingDB(t *testing.T) {
	_, _, _, err := parseConnStr("postgresql://user:pass@host:5433/")
	if err == nil {
		t.Fatal("expected error for missing database name")
	}
}

func TestParseConnStr_InvalidURI(t *testing.T) {
	_, _, _, err := parseConnStr("://broken")
	if err == nil {
		t.Fatal("expected error for invalid URI")
	}
}

func TestProvisionNewAccount_AccountIDTooLong(t *testing.T) {
	// 63 - 1 ("_") - 21 ("meko_default_datapack") = 41 chars max for accountID.
	// Use a 42-char ID to trigger the guard.
	longID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dp := &mockDatapackCreator{}
	crd := &mockCRDCreator{}
	svc := NewProvisioningService(dp, crd, testTierStore(), nil, nil, testConfig())

	svc.ProvisionNewAccount(context.Background(), longID)

	if dp.called {
		t.Fatal("DatapackCreator.Create should NOT be called when account ID is too long")
	}
	if crd.called {
		t.Fatal("CRDCreator should NOT be called when account ID is too long")
	}
}

func TestProvisionNewAccount_AccountIDMaxLength(t *testing.T) {
	// Exactly 41 chars -> DB ident = 41 + 1 + 21 = 63, should succeed.
	maxID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dp := &mockDatapackCreator{
		resp: &CreateResponse{
			DatapackID:  "dp-max",
			MCPUserName: "mcp_" + maxID,
			MCPServer:   MCPServerInfo{APIKey: "mcp_pass"},
			DBAccess: DBAccessInfo{
				UserName: "db_user",
				Password: "pass",
				ConnStr:  "postgresql://db_user:pass@yb-host:5433/" + maxID + "_meko_default_datapack",
			},
		},
	}
	crd := &mockCRDCreator{}
	svc := NewProvisioningService(dp, crd, testTierStore(), nil, nil, testConfig())

	svc.ProvisionNewAccount(context.Background(), maxID)

	if !dp.called {
		t.Fatal("DatapackCreator.Create should be called for max-length account ID")
	}
	if !crd.called {
		t.Fatal("CRDCreator should be called for max-length account ID")
	}
}
