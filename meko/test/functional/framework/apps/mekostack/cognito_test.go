package mekostack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/cognitomock"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	execpkg "github.com/yugabyte/meko/meko/test/functional/framework/exec"
	"github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
)

const quayManifestWithAliases = quayManifest + `
---
kind: Deployment
metadata: {name: meko-api-server}
spec:
  template:
    spec:
      hostAliases:
        - {ip: 10.0.0.2, hostnames: [cognito-idp.us-east-1.amazonaws.com]}
      containers:
        - name: meko-api-server
`

func TestValidateAcceptsOnlyKnownAuthModes(t *testing.T) {
	for mode, ok := range map[AuthMode]bool{"": true, AuthNone: true, AuthCognitoMock: true, "dex": false} {
		cfg := testConfig(t)
		cfg.Auth = mode
		if err := cfg.Validate(); (err == nil) != ok {
			t.Errorf("auth %q: Validate = %v, want ok=%v", mode, err, ok)
		}
	}
}

func TestCognitoOverlayConfiguresTheApiServerWithoutLosingItsOtherValues(t *testing.T) {
	cfg := testConfig(t)
	dp, _ := NewDataPlaneSecrets(nil)
	base, err := cfg.Values(dp, MinIO{AccessKey: "ak", SecretKey: "sk", APIEndpoint: "http://minio:9000", WorkerEndpoint: "http://10.0.0.1:9000"})
	if err != nil {
		t.Fatal(err)
	}
	spec := cfg.CognitoMock.WithDefaults()
	got := overlay(base, apiServerCognito(spec, cognitoMockHost{ClusterIP: "10.1.2.3"}, true))

	api := got["apiServer"].(map[string]any)
	if api["replicas"] != cfg.APIReplicas || api["staticArgs"].(map[string]any)["s3AuthMode"] != "minio" {
		t.Errorf("the overlay dropped what the base values set: %v", api)
	}
	if api["cognito"].(map[string]any)["enabled"] != true {
		t.Error("cognito is not enabled")
	}
	conf := api["config"].(map[string]any)
	for k, want := range map[string]string{
		"COGNITO_REGION": "us-east-1", "COGNITO_POOL_ID": cognitomock.DefaultPoolID,
		"COGNITO_CLIENT_ID": cognitomock.DefaultClientID, "COGNITO_DOMAIN": "", "S3_BUCKET": "mekoqa",
	} {
		if conf[k] != want {
			t.Errorf("config %s = %v, want %q", k, conf[k], want)
		}
	}
	b, _ := yaml.Marshal(api)
	for _, want := range []string{"10.1.2.3", "cognito-idp.us-east-1.amazonaws.com", "SSL_CERT_FILE", "/etc/cognito-mock/ca.crt", "secretName: cognito-mock"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("api-server values lack %q:\n%s", want, b)
		}
	}
	if _, has := base["apiServer"].(map[string]any)["cognito"]; has {
		t.Error("the overlay changed the base values")
	}
	bare := apiServerCognito(spec, cognitoMockHost{ClusterIP: "10.1.2.3"}, false)["apiServer"].(map[string]any)
	if _, has := bare["hostAliases"]; has {
		t.Error("pod additions were set for a chart that has no value for them")
	}
}

func TestRenderedSupportSeesHostAliases(t *testing.T) {
	cs, err := RenderedSupport(quayManifestWithAliases)
	if err != nil || !cs.APIServerExtras {
		t.Fatalf("with host aliases: %+v %v", cs, err)
	}
	cs, err = RenderedSupport(quayManifest + "\n---\nkind: Deployment\nmetadata: {name: meko-api-server}\nspec: {template: {spec: {containers: []}}}\n")
	if err != nil || cs.APIServerExtras {
		t.Fatalf("without host aliases: %+v %v", cs, err)
	}
}

type pool struct {
	mu     sync.Mutex
	logins []string
	users  map[string]string
}

func newPoolServers(t *testing.T) (api, ctl *httptest.Server, p *pool) {
	t.Helper()
	p = &pool{users: map[string]string{}}
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/health":
			fmt.Fprint(w, `{"data":{"status":"ok","database":"ok","system_database":"ok","data_database":"ok"}}`)
		case "/api/v1/auth/login":
			var in struct{ Email, Password string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			p.mu.Lock()
			p.logins = append(p.logins, in.Email)
			p.mu.Unlock()
			fmt.Fprintf(w, `{"data":{"access_token":"tok-for-%s"}}`, in.Email)
		case "/api/v1/datapacks":
			if r.Header.Get("Authorization") == "" {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"error":{"code":"UNAUTHORIZED","message":"missing"}}`)
				return
			}
			fmt.Fprint(w, `{"data":[]}`)
		}
	}))
	ctl = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in cognitomock.UserSpec
		_ = json.NewDecoder(r.Body).Decode(&in)
		p.mu.Lock()
		p.users[in.Email] = in.Password
		p.mu.Unlock()
		fmt.Fprintf(w, `{"sub":"sub-%s","email":%q,"status":"CONFIRMED"}`, in.Email, in.Email)
	}))
	t.Cleanup(api.Close)
	t.Cleanup(ctl.Close)
	return api, ctl, p
}

func cognitoCluster(manifest string) *cluster {
	base := deployCluster(manifest)
	return &cluster{handle: func(line string, c call) (execpkg.Result, bool) {
		if strings.Contains(line, "get service cognito-mock") {
			return out("10.96.7.7")
		}
		if strings.Contains(line, "SELECT count(*)") {
			return out("99\n")
		}
		if strings.Contains(line, "RETURNING 1") {
			return out(strings.Repeat("1\n", strings.Count(line, "@mekoqa.test")) + "UPDATE\n")
		}
		return base.handle(line, c)
	}}
}

func cognitoApp(t *testing.T, cl *cluster) (*App, *pool, *[]cognitomock.Spec) {
	t.Helper()
	api, ctl, p := newPoolServers(t)
	cfg := testConfig(t)
	cfg.Auth = AuthCognitoMock
	a := New(cfg).WithRunner(cl.run).WithLogf(nil).WithPollInterval(time.Millisecond)
	a.deployMinIO = minioFake
	a.deployLLM = llmFake
	var specs []cognitomock.Spec
	a.deployCognito = func(_ context.Context, kube *k8s.Kubectl, spec cognitomock.Spec, _ cognitomock.Material) (cognitomock.Deployed, error) {
		specs = append(specs, spec)
		return cognitomock.Deployed{ClusterIP: "10.96.7.7"}, nil
	}
	a.dial = func(context.Context, string) error { return nil }
	a.newAPI = func(string) *mekoapi.Client { return mekoapi.New(api.URL) }
	a.ctl = cognitomock.NewClient(ctl.URL)
	return a, p, &specs
}

func TestDeployWithTheCognitoMock(t *testing.T) {
	cl := cognitoCluster(quayManifestWithAliases)
	a, p, specs := cognitoApp(t, cl)
	if err := a.Deploy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*specs) != 1 || (*specs)[0].ClientSecret == "" || (*specs)[0].Region != "us-east-1" {
		t.Fatalf("the mock was deployed with %+v", *specs)
	}
	secret := (*specs)[0].ClientSecret
	if a.AuthMode() != AuthCognitoMock {
		t.Errorf("AuthMode = %s", a.AuthMode())
	}

	lines := cl.lines()
	secretApply := cl.first("apply -f -")
	var applied []string
	for _, c := range cl.calls {
		if strings.Contains(c.stdin, "name: meko-api-server-cognito") {
			applied = append(applied, c.stdin)
		}
	}
	if len(applied) != 1 || !strings.Contains(applied[0], "cognito-client-secret: "+fmt.Sprintf("%q", secret)) {
		t.Errorf("the client secret Secret: %v", applied)
	}
	_ = secretApply
	if cl.first("get service minio") < 0 || cl.first(" upgrade --install ") < cl.first("get service minio") {
		t.Errorf("order wrong:\n%s", strings.Join(lines, "\n"))
	}
	for _, l := range lines {
		if strings.Contains(l, secret) {
			t.Errorf("the client secret reached a command line: %s", l)
		}
	}
	for _, c := range cl.calls {
		if strings.Contains(c.line(), "patch deployment meko-api-server") && strings.Contains(c.line(), "hostAliases") {
			t.Errorf("a chart that renders the host aliases was patched: %s", c.line())
		}
	}

	var values string
	for _, c := range cl.calls {
		if strings.Contains(c.line(), " upgrade --install ") && !strings.Contains(c.line(), "langfuse-first") {
			b, err := readFile(c.argv[len(c.argv)-3])
			if err != nil {
				t.Fatal(err)
			}
			values = string(b)
		}
	}
	for _, want := range []string{"COGNITO_POOL_ID", "hostAliases", "SSL_CERT_FILE", "10.96.7.7"} {
		if !strings.Contains(values, want) {
			t.Errorf("generated values lack %s:\n%s", want, values)
		}
	}
	if strings.Contains(values, secret) {
		t.Error("the client secret is in the values file")
	}

	if got := p.logins; len(got) == 0 || got[0] != defaultAccountEmail {
		t.Errorf("logins = %v, want the default account first", got)
	}
	if got := a.API().BaseURL(); got == "" {
		t.Error("no API client")
	}
	if cl.count("UPDATE meko_system.users SET user_type = 'enterprise' WHERE user_email IN ('default@mekoqa.test')") != 1 {
		t.Errorf("the default account was not lifted:\n%s", strings.Join(lines, "\n"))
	}
	if cl.count("cognito_sub = 'dev-local'") != 0 {
		t.Error("the dev account was lifted on a stack that has none")
	}
}

func TestDeployWithTheCognitoMockPatchesAChartWithoutHostAliases(t *testing.T) {
	cl := cognitoCluster(quayManifest)
	a, _, _ := cognitoApp(t, cl)
	if err := a.Deploy(context.Background()); err != nil {
		t.Fatal(err)
	}
	var patch string
	for _, c := range cl.calls {
		if strings.Contains(c.line(), "patch deployment meko-api-server") && strings.Contains(c.line(), "hostAliases") {
			patch = c.line()
		}
	}
	for _, want := range []string{"10.96.7.7", "cognito-idp.us-east-1.amazonaws.com", "SSL_CERT_FILE", "cognito-mock-ca"} {
		if !strings.Contains(patch, want) {
			t.Errorf("the patch lacks %q: %q", want, patch)
		}
	}
	if cl.count("rollout status deployment/meko-api-server") < 2 {
		t.Error("the api-server rollout after the patch was not awaited")
	}
}

func TestNewAccountsCreatesLogsInAndLiftsEachOnce(t *testing.T) {
	cl := cognitoCluster(quayManifestWithAliases)
	a, p, _ := cognitoApp(t, cl)
	a.auth = AuthCognitoMock
	accts, err := a.NewAccounts(context.Background(), "qa-x", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(accts) != 3 || accts[1].Email != "qa-x-2@mekoqa.test" || accts[1].Token != "tok-for-qa-x-2@mekoqa.test" || accts[1].Sub != "sub-qa-x-2@mekoqa.test" {
		t.Fatalf("accounts %+v", accts)
	}
	if len(p.logins) != 3 || p.users["qa-x-3@mekoqa.test"] == "" {
		t.Errorf("logins %v users %v", p.logins, p.users)
	}
	if cl.count("UPDATE meko_system.users SET user_type = 'enterprise' WHERE user_email IN ('qa-x-1@mekoqa.test', 'qa-x-2@mekoqa.test', 'qa-x-3@mekoqa.test')") != 1 {
		t.Errorf("one lift for all three expected:\n%s", strings.Join(cl.lines(), "\n"))
	}
}

func TestNewAccountsRefusesAStackWithoutAPool(t *testing.T) {
	a := New(testConfig(t)).WithRunner(readyCluster().run).WithLogf(nil)
	if _, err := a.NewAccounts(context.Background(), "x", 1); err == nil || !strings.Contains(err.Error(), "cognito_mock") {
		t.Fatalf("err = %v, want it to name cognito_mock", err)
	}
	if _, err := a.CognitoControl(context.Background()); err == nil {
		t.Fatal("a control client for a stack with no mock")
	}
}

func TestAccountEmailsAreKeptOutOfSQLInjection(t *testing.T) {
	cl := cognitoCluster(quayManifestWithAliases)
	a, _, _ := cognitoApp(t, cl)
	a.auth = AuthCognitoMock
	if _, err := a.NewAccounts(context.Background(), "x'; DROP TABLE users; --", 1); err == nil {
		t.Fatal("an email with a quote was accepted")
	}
	if cl.count("DROP TABLE") != 0 {
		t.Error("it reached the cluster")
	}
}

func TestAttachFindsTheMockInTheNamespace(t *testing.T) {
	base := cognitoCluster(quayManifest)
	cl := &cluster{handle: func(line string, c call) (execpkg.Result, bool) {
		if strings.Contains(line, "get namespace mekoqa-st-abc -o jsonpath") {
			return out("mekoqa-st-abc")
		}
		return base.handle(line, c)
	}}
	a, _, _ := cognitoApp(t, cl)
	a.cfg.Auth = AuthNone
	if err := a.Attach(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.AuthMode() != AuthCognitoMock {
		t.Errorf("AuthMode = %s, want what the namespace runs", a.AuthMode())
	}
	if a.defaultToken() == "" {
		t.Error("no default account after attach")
	}
}

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

func TestNewAccountsWaitsForTheDefaultDatapacksToHaveProjects(t *testing.T) {
	base := cognitoCluster(quayManifestWithAliases)
	polls := 0
	cl := &cluster{handle: func(line string, c call) (execpkg.Result, bool) {
		if strings.Contains(line, "SELECT count(*)") {
			polls++
			return out(fmt.Sprintf("%d\n", polls))
		}
		return base.handle(line, c)
	}}
	a, _, _ := cognitoApp(t, cl)
	a.auth = AuthCognitoMock
	if _, err := a.NewAccounts(context.Background(), "w", 3); err != nil {
		t.Fatal(err)
	}
	if polls != 3 {
		t.Fatalf("polled %d times, want it to stop at the poll that saw all 3 accounts", polls)
	}
}

func TestMCPReplicasOverrideTheValuesFiles(t *testing.T) {
	dp, _ := NewDataPlaneSecrets(nil)
	m := MinIO{AccessKey: "ak", SecretKey: "sk", APIEndpoint: "http://minio:9000", WorkerEndpoint: "http://10.0.0.1:9000"}
	for replicas, want := range map[int]any{0: nil, 2: 2} {
		cfg := testConfig(t)
		cfg.MCPReplicas = replicas
		v, err := cfg.Values(dp, m)
		if err != nil {
			t.Fatal(err)
		}
		mcp, _ := v["mekoMcpServer"].(map[string]any)
		if got := mcp["replicas"]; got != want {
			t.Errorf("MCPReplicas %d: mekoMcpServer.replicas = %v, want %v", replicas, got, want)
		}
	}
}

func TestHashPartitionsReachTheMigrateValues(t *testing.T) {
	dp, _ := NewDataPlaneSecrets(nil)
	m := MinIO{AccessKey: "ak", SecretKey: "sk", APIEndpoint: "http://minio:9000", WorkerEndpoint: "http://10.0.0.1:9000"}
	for n, want := range map[int]any{0: nil, 8: "8"} {
		cfg := testConfig(t)
		cfg.HashPartitions = n
		v, err := cfg.Values(dp, m)
		if err != nil {
			t.Fatal(err)
		}
		migrate, _ := v["apiServer"].(map[string]any)["migrate"].(map[string]any)
		if got := migrate["hashPartitions"]; got != want {
			t.Errorf("HashPartitions %d: apiServer.migrate.hashPartitions = %v, want %v", n, got, want)
		}
	}
}

func TestDeployGuardsDeploymentAndStatefulSetPodsFromScaleDown(t *testing.T) {
	cl := cognitoCluster(quayManifestWithAliases)
	a, _, _ := cognitoApp(t, cl)
	if err := a.Deploy(context.Background()); err != nil {
		t.Fatal(err)
	}
	guarded := map[string]bool{}
	for _, c := range cl.calls {
		if strings.Contains(c.stdin, "kind: PodDisruptionBudget") && strings.Contains(c.stdin, "maxUnavailable: 0") {
			for _, key := range []string{"pod-template-hash", "controller-revision-hash"} {
				if strings.Contains(c.stdin, "key: "+key) {
					guarded[key] = true
				}
			}
		}
	}
	if !guarded["pod-template-hash"] || !guarded["controller-revision-hash"] {
		t.Errorf("zero budgets applied for %v, want Deployment (pod-template-hash) and StatefulSet (controller-revision-hash) pods", guarded)
	}
}
