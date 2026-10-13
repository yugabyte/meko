// Package mekostack is the meko stack App: it deploys the meko Helm chart into
// one namespace the way a BYOC customer installs it, waits until the stack
// answers real requests, and tears it down again.
//
// What is deployed is the chart's own: the cloud, env and mode presets, then one
// generated values file with the image tags, fresh secrets and the S3 settings.
// Around the helm release the App creates the pieces the chart cannot: the
// labelled namespace, the Quay pull secret and a MinIO for knowledge-base
// uploads. It reaches the api-server and mcp-server the way the amp stack reaches
// its controller: the cluster-internal address directly, a kubectl port-forward
// when that does not answer.
package mekostack

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/cognitomock"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/llmmock"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/minio"
	"github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
	"github.com/yugabyte/meko/meko/test/functional/framework/kubemeta"
)

const (
	pullSecretName = "quay-registry-creds"
	ragSecretName  = "rag-agent-secret"
	minioName      = "minio"

	apiDeployment      = "meko-api-server"
	apiService         = "meko-api-server-service"
	apiServicePort     = 80
	apiPodPort         = 25433
	mcpService         = "meko-mcp-server"
	mcpServicePort     = 80
	mcpPodPort         = 8000
	mcpPath            = "/mcp"
	langfuseDeployment = "langfuse-web-worker"

	tserverSelector  = "app=yb-tserver"
	tserverContainer = "yb-tserver"
	ysqlsh           = "/home/yugabyte/bin/ysqlsh"
)

var (
	ragWorkers = []string{"rag-agent-text", "rag-agent-conversations", "rag-agent-pdf"}

	// initContainers are the steps a stack passes through before its pods can
	// serve: the extensions, then the langfuse schema in two parts.
	langfuseInit = []initStep{
		{deployment: langfuseDeployment, selector: "app=" + langfuseDeployment, container: "metadata-schema-init"},
		{deployment: langfuseDeployment, selector: "app=" + langfuseDeployment, container: "tracing-schema-init"},
	}
	initContainers = append([]initStep{
		{deployment: apiDeployment, selector: "app=" + apiDeployment, container: "ensure-extensions", marker: "extensions ready"},
	}, langfuseInit...)
)

// initStep is an init container a stack must have finished. marker, when set, is
// a log line the container prints on success; the exit code is the authority.
type initStep struct {
	deployment, selector, container, marker string
}

type App struct {
	cfg     Config
	kube    *k8s.Kubectl
	cluster *k8s.Kubectl
	helmRun k8s.Runner
	logf    func(string, ...any)

	dial          func(ctx context.Context, addr string) error
	newAPI        func(url string) *mekoapi.Client
	deployMinIO   func(ctx context.Context, kube *k8s.Kubectl, spec minio.Spec) (endpoint string, err error)
	deployCognito func(ctx context.Context, kube *k8s.Kubectl, spec cognitomock.Spec, mat cognitomock.Material) (cognitomock.Deployed, error)
	deployLLM     func(ctx context.Context, kube *k8s.Kubectl, spec llmmock.Spec) (*llmmock.Server, error)
	rand          io.Reader
	now           func() time.Time
	pollEvery     time.Duration

	mu        sync.Mutex
	pfs       []*k8s.PortForward
	apiURL    string
	mcpURL    string
	llmURL    string
	secrets   []string
	values    map[string]any
	nsCreated bool
	support   ChartSupport
	backing   *Backing

	ysqlMu sync.Mutex
	ysqlUp bool

	dp      DataPlaneSecrets
	minio   *MinIO
	cognito cognitoMockHost
	// auth is what the stack runs, token the default account's access token and
	// ctl the Cognito mock's control client. The last two exist only with the mock.
	auth  AuthMode
	token string
	ctl   *cognitomock.Client
}

func New(cfg Config) *App {
	k := k8s.New(k8s.Config{Context: cfg.Context, Namespace: cfg.Namespace})
	return &App{
		cfg:           cfg,
		kube:          k,
		cluster:       k.InNamespace(""),
		helmRun:       k8s.OSRun,
		logf:          log.Printf,
		dial:          dialTCP,
		newAPI:        func(url string) *mekoapi.Client { return mekoapi.New(url) },
		deployMinIO:   deployMinIO,
		deployCognito: deployCognito,
		deployLLM:     deployLLMMock,
		now:           time.Now,
		pollEvery:     3 * time.Second,
	}
}

func (a *App) WithRunner(r k8s.Runner) *App {
	a.kube = a.kube.WithRunner(r)
	a.cluster = a.kube.InNamespace("")
	a.helmRun = r
	return a
}

func (a *App) WithStarter(s k8s.Starter) *App {
	a.kube = a.kube.WithStarter(s)
	a.cluster = a.kube.InNamespace("")
	return a
}

func (a *App) WithLogf(f func(string, ...any)) *App { a.logf = f; return a }

func (a *App) WithPollInterval(d time.Duration) *App { a.pollEvery = d; return a }

func (a *App) log(format string, args ...any) {
	if a.logf == nil {
		return
	}
	a.mu.Lock()
	lits := a.secrets
	a.mu.Unlock()
	a.logf("%s", RedactArgs(fmt.Sprintf(format, args...), lits))
}

func (a *App) Namespace() string { return a.cfg.Namespace }

func (a *App) Kube() *k8s.Kubectl { return a.kube }

func (a *App) APIURL() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.apiURL
}

func (a *App) MCPURL() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mcpURL
}

// Deploy stands the stack up. On failure after the namespace exists it records
// diagnostics and appends a summary to the error, and deletes nothing.
func (a *App) Deploy(ctx context.Context) error {
	err := a.deploy(ctx)
	if err != nil && a.nsCreated {
		err = fmt.Errorf("%w\n%s", err, a.failureReport(ctx))
	}
	return err
}

func (a *App) deploy(ctx context.Context) error {
	if a.cfg.Namespace == "" {
		return errors.New("mekostack: namespace is required")
	}
	if err := a.cfg.Validate(); err != nil {
		return fmt.Errorf("mekostack: %w", err)
	}
	dp, err := NewDataPlaneSecrets(a.rand)
	if err != nil {
		return err
	}
	if err := a.checkRender(ctx, dp); err != nil {
		return err
	}
	if a.cfg.Auth == AuthCognitoMock && !a.cognitoExtrasOK() {
		a.log("mekostack: the chart has no apiServer.hostAliases, the Cognito wiring is applied as a patch after the install")
	}
	if err := a.ensureNamespace(ctx); err != nil {
		return err
	}
	if err := a.ensurePullSecret(ctx); err != nil {
		return err
	}
	if err := a.ensureNoScaleDown(ctx); err != nil {
		return err
	}
	if err := a.setupBacking(ctx, dp); err != nil {
		return err
	}
	var cognito cognitoMockHost
	if a.cfg.Auth == AuthCognitoMock {
		if cognito, err = a.setupCognitoMock(ctx, dp.CognitoClientSecret); err != nil {
			return err
		}
	}
	m, err := a.setupMinIO(ctx)
	if err != nil {
		return err
	}
	if err := a.setupLLM(ctx); err != nil {
		return err
	}
	values, err := a.cfg.Values(dp, m)
	if err != nil {
		return err
	}
	if a.cfg.Auth == AuthCognitoMock {
		values = overlay(values, apiServerCognito(a.cognitoSpec(), cognito, a.cognitoExtrasOK()))
	}
	a.mu.Lock()
	a.dp, a.minio, a.cognito = dp, &m, cognito
	a.values = values
	a.secrets = append(SecretLiterals(values), dp.CognitoClientSecret)
	a.mu.Unlock()
	path, err := a.writeValues(values)
	if err != nil {
		return err
	}
	if err := a.repairRelease(ctx); err != nil {
		return fmt.Errorf("clear an earlier attempt's release: %w", err)
	}
	if err := a.installLangfuseFirst(ctx, values); err != nil {
		return err
	}
	if err := a.migrateWithOneReplica(ctx, values); err != nil {
		return err
	}
	a.log("mekostack: helm upgrade --install %s in %s (values %s)", a.cfg.Release, a.cfg.Namespace, path)
	if err := a.upgrade(ctx, path); err != nil {
		return err
	}
	if err := a.waitWorkloads(ctx); err != nil {
		return err
	}
	if err := a.afterInstall(ctx, m, cognito); err != nil {
		return err
	}
	return a.waitReady(ctx)
}

// ProbeAPI reads the datapack list through the API server's service proxy. That
// lands on whichever api-server pod the Service picks, as a client's request
// does, so unlike a port-forward it survives pods being replaced. The proxy
// cannot carry a bearer token, so a stack that authenticates is probed on
// /api/v1/health, which still reads both databases.
func (a *App) ProbeAPI(ctx context.Context) error {
	base := fmt.Sprintf("/api/v1/namespaces/%s/services/%s:%d/proxy/api/v1", a.cfg.Namespace, apiService, apiServicePort)
	if a.cognitoMock() {
		body, err := a.kube.GetRaw(ctx, base+"/health")
		if err != nil {
			return err
		}
		if !healthOK(body) {
			return fmt.Errorf("health answered %s", tail(body, 200))
		}
		return nil
	}
	body, err := a.kube.GetRaw(ctx, base+"/datapacks")
	if err != nil {
		return err
	}
	if !strings.Contains(body, `"data"`) {
		return fmt.Errorf("datapack list answered %s", tail(body, 200))
	}
	return nil
}

// healthOK reads /health inside its {"data": ...} envelope. A database field the
// build does not report is not a failure, so an older release probes the same way.
func healthOK(body string) bool {
	var env struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil || env.Data["status"] != "ok" {
		return false
	}
	for _, k := range []string{"database", "system_database", "data_database"} {
		if v, ok := env.Data[k]; ok && v != "ok" {
			return false
		}
	}
	return true
}

// Upgrade moves a stack Deploy stood up to another chart and image version with
// one helm upgrade, keeping its secrets and MinIO: an upgrade in place, the way
// a customer runs one, so none of the bring-up workarounds apply.
func (a *App) Upgrade(ctx context.Context, chartPath, imageVersion string) error {
	a.mu.Lock()
	dp, m := a.dp, a.minio
	a.mu.Unlock()
	if m == nil {
		return errors.New("mekostack: Upgrade needs a stack this App deployed")
	}
	a.cfg.ChartPath, a.cfg.ImageVersion = chartPath, imageVersion
	if err := a.cfg.Validate(); err != nil {
		return fmt.Errorf("mekostack: %w", err)
	}
	if err := a.checkRender(ctx, dp); err != nil {
		return err
	}
	values, err := a.cfg.Values(dp, *m)
	if err != nil {
		return err
	}
	a.mu.Lock()
	cognito := a.cognito
	a.mu.Unlock()
	if a.cfg.Auth == AuthCognitoMock {
		values = overlay(values, apiServerCognito(a.cognitoSpec(), cognito, a.cognitoExtrasOK()))
	}
	a.mu.Lock()
	a.values = values
	a.secrets = append(SecretLiterals(values), dp.CognitoClientSecret)
	a.mu.Unlock()
	path, err := a.writeValues(values)
	if err != nil {
		return err
	}
	a.log("mekostack: helm upgrade %s in %s to chart %s, images %s", a.cfg.Release, a.cfg.Namespace, chartPath, imageVersion)
	if err := a.upgrade(ctx, path); err != nil {
		return err
	}
	if err := a.waitWorkloads(ctx); err != nil {
		return err
	}
	if err := a.afterInstall(ctx, *m, cognito); err != nil {
		return err
	}
	return a.waitReady(ctx)
}

// Attach binds to a stack that already runs in the namespace and checks it
// answers. It creates and changes nothing.
func (a *App) Attach(ctx context.Context) error {
	if a.cfg.Namespace == "" {
		return errors.New("mekostack: namespace is required")
	}
	if _, err := a.cluster.Get(ctx, "namespace", a.cfg.Namespace, "{.metadata.name}"); err != nil {
		return fmt.Errorf("mekostack: namespace %s: %w", a.cfg.Namespace, err)
	}
	if _, err := a.loadBacking(ctx); err != nil {
		return err
	}
	a.detectAuth(ctx)
	if err := a.ensureNoScaleDown(ctx); err != nil {
		a.log("mekostack: %v", err)
	}
	return a.waitReady(ctx)
}

// checkRender renders the chart with this stack's values and refuses any meko
// image that is not on Quay. It runs before the cluster is touched, so a chart or
// preset that drifts to another registry fails here and not as a pull error.
func (a *App) checkRender(ctx context.Context, dp DataPlaneSecrets) error {
	probe := MinIO{
		AccessKey: minio.AccessKey, SecretKey: minio.SecretKey,
		APIEndpoint: "http://" + minioName + ":9000", WorkerEndpoint: "http://10.0.0.1:9000",
	}
	values, err := a.cfg.Values(dp, probe)
	if err != nil {
		return err
	}
	if a.cfg.Auth == AuthCognitoMock {
		values = overlay(values, apiServerCognito(a.cognitoSpec(), cognitoMockHost{ClusterIP: "10.0.0.2"}, true))
	}
	b, err := MarshalValues(values)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "mekostack-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "values.yaml")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	manifest, err := a.template(ctx, path)
	if err != nil {
		return fmt.Errorf("the chart does not render with these values: %w", err)
	}
	images, err := RenderedImages(manifest)
	if err != nil {
		return err
	}
	if err := CheckQuayOnly(images); err != nil {
		return err
	}
	support, err := RenderedSupport(manifest)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.support = support
	a.mu.Unlock()
	return nil
}

func (a *App) ensureNamespace(ctx context.Context) error {
	labels, exists, err := a.namespaceLabels(ctx, a.cfg.Namespace)
	if err != nil {
		return err
	}
	if exists {
		if labels[kubemeta.ManagedByLabel] != kubemeta.ManagedByValue {
			return fmt.Errorf("mekostack: namespace %s exists and is not a mekoqa namespace (no %s=%s label), refusing to deploy into it",
				a.cfg.Namespace, kubemeta.ManagedByLabel, kubemeta.ManagedByValue)
		}
		a.nsCreated = true
		return nil
	}
	if err := a.cluster.CreateNamespace(ctx, a.cfg.Namespace, a.namespaceLabelSet()); err != nil {
		return fmt.Errorf("create namespace %s: %w", a.cfg.Namespace, err)
	}
	a.nsCreated = true
	if a.cfg.TTL > 0 {
		expires := a.now().UTC().Add(a.cfg.TTL).Format(time.RFC3339)
		if err := a.cluster.AnnotateNamespace(ctx, a.cfg.Namespace, kubemeta.ExpiresAnnotation, expires); err != nil {
			return fmt.Errorf("annotate namespace %s: %w", a.cfg.Namespace, err)
		}
	}
	return nil
}

func (a *App) namespaceLabelSet() map[string]string {
	extra := map[string]string{kubemeta.ManagedByLabel: kubemeta.ManagedByValue}
	if v := kubemeta.LabelValue(a.cfg.RunID); v != "" {
		extra[kubemeta.RunLabel] = v
	}
	if v := kubemeta.LabelValue(a.cfg.Owner); v != "" {
		extra[kubemeta.OwnerLabel] = v
	}
	return kubemeta.TestLabels(extra)
}

// namespaceLabels reads a namespace's labels. A namespace that does not exist is
// a nil error with exists false.
func (a *App) namespaceLabels(ctx context.Context, name string) (labels map[string]string, exists bool, err error) {
	out, err := a.cluster.GetJSON(ctx, "namespace", name)
	if err != nil {
		if k8s.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read namespace %s: %w", name, err)
	}
	var ns struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(out), &ns); err != nil {
		return nil, false, fmt.Errorf("decode namespace %s: %w", name, err)
	}
	return ns.Metadata.Labels, true, nil
}

// noScaleDownPDBs keep the cluster autoscaler from draining a node that runs a
// pod of the stack. Scale-down evicts through the eviction API, which a budget
// of zero refuses, while a test's own pod kills and a rolling upgrade delete
// pods directly and are unaffected. Without them, amp-dev scaled nodes down
// under running tests and every MCP and gateway call in flight failed.
//
// One selects Deployment pods and one StatefulSet pods, the bundled database
// included. A bare pod (mekoqa-ysql, minio) counts as healthy but not as
// expected, so `selector: {}` left two evictions allowed. The autoscaler never
// moves a bare pod anyway.
var noScaleDownPDBs = []string{pdbOnLabel("mekoqa-no-scale-down", "pod-template-hash"), pdbOnLabel("mekoqa-no-scale-down-sts", "controller-revision-hash")}

func pdbOnLabel(name, key string) string {
	return fmt.Sprintf(`apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: %s
spec:
  maxUnavailable: 0
  selector:
    matchExpressions:
    - key: %s
      operator: Exists
`, name, key)
}

func (a *App) ensureNoScaleDown(ctx context.Context) error {
	for _, pdb := range noScaleDownPDBs {
		if err := a.kube.Apply(ctx, pdb); err != nil {
			return fmt.Errorf("apply the scale-down guard: %w", err)
		}
	}
	return nil
}

func (a *App) ensurePullSecret(ctx context.Context) error {
	auth := base64.StdEncoding.EncodeToString([]byte(a.cfg.QuayUser + ":" + a.cfg.QuayToken))
	cfgJSON, err := json.Marshal(map[string]any{"auths": map[string]any{"quay.io": map[string]string{
		"username": a.cfg.QuayUser, "password": a.cfg.QuayToken, "auth": auth,
	}}})
	if err != nil {
		return err
	}
	manifest := fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\ntype: kubernetes.io/dockerconfigjson\ndata:\n  .dockerconfigjson: %s\n",
		pullSecretName, base64.StdEncoding.EncodeToString(cfgJSON))
	if err := a.kube.Apply(ctx, manifest); err != nil {
		return fmt.Errorf("apply the Quay pull secret: %w", err)
	}
	return nil
}

func deployMinIO(ctx context.Context, kube *k8s.Kubectl, spec minio.Spec) (string, error) {
	srv, err := minio.Deploy(ctx, kube, spec)
	if err != nil {
		return "", err
	}
	return srv.Endpoint(), nil
}

func (a *App) setupMinIO(ctx context.Context) (MinIO, error) {
	endpoint, err := a.deployMinIO(ctx, a.kube, minio.Spec{Name: minioName, Bucket: a.cfg.S3Bucket, ServerImage: a.cfg.MinIOServerImage, ClientImage: a.cfg.MinIOClientImage, ReadyTimeout: a.cfg.ReadyTimeout})
	if err != nil {
		return MinIO{}, err
	}
	ip, err := a.kube.Get(ctx, "service", minioName, "{.spec.clusterIP}")
	if err != nil {
		return MinIO{}, fmt.Errorf("minio Service has no ClusterIP: %w", err)
	}
	if ip == "" || ip == "None" {
		return MinIO{}, fmt.Errorf("minio Service has no ClusterIP: %q", ip)
	}
	return MinIO{
		AccessKey: minio.AccessKey, SecretKey: minio.SecretKey,
		APIEndpoint:    endpoint,
		WorkerEndpoint: fmt.Sprintf("http://%s:%d", ip, minio.Port),
	}, nil
}

func (a *App) writeValues(values map[string]any) (string, error) {
	return a.writeValuesAs(values, a.cfg.Namespace+"-values.yaml")
}

func (a *App) writeValuesAs(values map[string]any, name string) (string, error) {
	b, err := MarshalValues(values)
	if err != nil {
		return "", err
	}
	dir := a.cfg.ValuesDir
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "mekoqa")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return "", fmt.Errorf("write values file: %w", err)
	}
	return path, nil
}

// installLangfuseFirst installs the release with no api-server pods and waits
// for langfuse's schema init before the api-server migrates. Run together, a
// langfuse DDL bumps the catalog version under golang-migrate's version write
// (TRUNCATE + INSERT into schema_migrations), YB refuses the INSERT with a
// catalog version mismatch, and the database is left at "Dirty database version 1".
// An api-server with the MEKO-831 recovery clears that and reruns migration 1, which is
// now retry-safe; an older one stays wedged, and a dirty version above 1 is never
// cleared, so the staging stays.
func (a *App) installLangfuseFirst(ctx context.Context, values map[string]any) error {
	staged, err := withAPIReplicas(values, 0)
	if err != nil {
		return err
	}
	path, err := a.writeValuesAs(staged, a.cfg.Namespace+"-values-langfuse-first.yaml")
	if err != nil {
		return err
	}
	a.log("mekostack: helm upgrade --install %s in %s with no api-server pods until langfuse has migrated", a.cfg.Release, a.cfg.Namespace)
	if err := a.upgrade(ctx, path); err != nil {
		return err
	}
	return a.until(ctx, a.cfg.ProvisionTimeout, "langfuse's schema init containers to finish", func(ctx context.Context) (bool, error) {
		return a.stepsDone(ctx, langfuseInit)
	})
}

// migrateWithOneReplica brings a fresh stack's api-server up as one pod before
// scaling it, so a single migrator runs. With several pods migrating at once the
// first fresh install wedged at "Dirty database version 1" on most attempts, a
// product bug a bring-up has to step around but an upgrade test must not. Builds with
// the MEKO-831 recovery clear that version, but not a dirty one above 1, so the single
// migrator stays.
func (a *App) migrateWithOneReplica(ctx context.Context, values map[string]any) error {
	if a.cfg.APIReplicas <= 1 {
		return nil
	}
	staged, err := withAPIReplicas(values, 1)
	if err != nil {
		return err
	}
	path, err := a.writeValuesAs(staged, a.cfg.Namespace+"-values-one-api-server.yaml")
	if err != nil {
		return err
	}
	a.log("mekostack: migrating with one api-server pod before scaling to %d", a.cfg.APIReplicas)
	if err := a.upgrade(ctx, path); err != nil {
		return err
	}
	return a.waitWorkloads(ctx)
}

// withAPIReplicas copies values with apiServer.replicas set to n.
func withAPIReplicas(values map[string]any, n int) (map[string]any, error) {
	out := make(map[string]any, len(values))
	for k, v := range values {
		out[k] = v
	}
	api, ok := values["apiServer"].(map[string]any)
	if !ok {
		return nil, errors.New("mekostack: the values carry no apiServer block")
	}
	cp := make(map[string]any, len(api))
	for k, v := range api {
		cp[k] = v
	}
	cp["replicas"] = n
	out["apiServer"] = cp
	return out, nil
}

// afterInstall applies what an older chart has no value for: the api-server's
// afterInstall applies what an older chart has no value for: the api-server's
// S3_ENDPOINT, the rag workers' AWS key pair, the LLM env of the api-server and
// mcp-server, and the Cognito mock wiring. A chart with them takes them as
// values and this does nothing. Each change rolls the pods once.
func (a *App) afterInstall(ctx context.Context, m MinIO, cognito cognitoMockHost) error {
	a.mu.Lock()
	support := a.support
	a.mu.Unlock()
	if a.cfg.Auth == AuthCognitoMock && !support.APIServerExtras {
		if err := a.patchAPIServerCognito(ctx, cognito); err != nil {
			return err
		}
	}
	if !support.S3EndpointEnv {
		if err := a.patchS3Endpoint(ctx, m); err != nil {
			return err
		}
	}
	if a.cfg.MockLLM() && !support.ExtraEnv {
		if err := a.patchLLMEnv(ctx); err != nil {
			return err
		}
	}
	if !a.cfg.RagWorkers || support.RagAWSKeys {
		return nil
	}
	return a.patchRagKeys(ctx, m)
}

func (a *App) patchS3Endpoint(ctx context.Context, m MinIO) error {
	env := []map[string]string{{"name": "S3_ENDPOINT", "value": m.APIEndpoint}}
	if a.cfg.S3PublicEndpoint != "" {
		env = append(env, map[string]string{"name": "S3_PUBLIC_ENDPOINT", "value": a.cfg.S3PublicEndpoint})
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
		"containers": []any{map[string]any{"name": apiDeployment, "env": env}},
	}}}})
	if err != nil {
		return err
	}
	if err := a.kube.Patch(ctx, "deployment", apiDeployment, "strategic", string(patch)); err != nil {
		return fmt.Errorf("point the api-server at MinIO: %w", err)
	}
	if err := a.kube.RolloutStatus(ctx, "deployment", apiDeployment, a.cfg.RolloutTimeout); err != nil {
		return fmt.Errorf("api-server rollout after the S3 endpoint change: %w", err)
	}
	return nil
}

func (a *App) patchRagKeys(ctx context.Context, m MinIO) error {

	secret := fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\nstringData:\n  AWS_ACCESS_KEY_ID: %q\n  AWS_SECRET_ACCESS_KEY: %q\n",
		ragSecretName, m.AccessKey, m.SecretKey)
	if err := a.kube.Apply(ctx, secret); err != nil {
		return fmt.Errorf("add the S3 key pair to %s: %w", ragSecretName, err)
	}
	stamp := a.now().UTC().Format(time.RFC3339)
	restart := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"mekoqa/restarted-at":%q}}}}}`, stamp)
	for _, w := range ragWorkers {
		if err := a.kube.Patch(ctx, "deployment", w, "strategic", restart); err != nil {
			return fmt.Errorf("restart %s: %w", w, err)
		}
	}
	for _, w := range ragWorkers {
		if err := a.kube.RolloutStatus(ctx, "deployment", w, a.cfg.RolloutTimeout); err != nil {
			return fmt.Errorf("rollout of %s: %w", w, err)
		}
	}
	return nil
}

// waitReady is the readiness a running pod does not give: the init steps done,
// the api-server answering health, and one request that needs the schema, since a
// missed migration still answers health 200 and fails everything else.
func (a *App) waitReady(ctx context.Context) error {
	if err := a.until(ctx, a.cfg.ReadyTimeout, "the init containers to finish", a.initDone); err != nil {
		return err
	}
	if err := a.connect(ctx); err != nil {
		return err
	}
	api := a.API()
	if err := a.until(ctx, a.cfg.ReadyTimeout, "/api/v1/health to report ok", func(ctx context.Context) (bool, error) {
		h, err := api.Health(ctx)
		if err != nil {
			return false, err
		}
		if !h.Healthy() {
			return false, fmt.Errorf("health body %+v", h)
		}
		return true, nil
	}); err != nil {
		return err
	}
	if a.cognitoMock() {
		if err := a.until(ctx, a.cfg.ReadyTimeout, "the default account to log in through the Cognito mock", func(ctx context.Context) (bool, error) {
			if err := a.ensureDefaultAccount(ctx); err != nil {
				return false, err
			}
			return true, nil
		}); err != nil {
			return err
		}
		api = a.API()
	}
	if err := a.until(ctx, a.cfg.ReadyTimeout, "an authenticated request that touches the schema (GET /api/v1/datapacks)", func(ctx context.Context) (bool, error) {
		_, err := api.ListDatapacks(ctx)
		if st := mekoapi.StatusOf(err); st >= 400 && st < 500 && st != 429 {
			return false, &fatalError{err}
		}
		return err == nil, err
	}); err != nil {
		return err
	}
	if a.cognitoMock() {
		return nil
	}
	return a.liftTierCaps(ctx)
}

// liftTierCaps moves the dev account every request runs as off the free tier.
// Free tier counts datapack creations over the account's lifetime and refuses
// the 11th with a 402, which one shared stack's tests exceed. The request above
// is what created the account.
func (a *App) liftTierCaps(ctx context.Context) error {
	out, err := a.ysql(ctx, `UPDATE meko_system.users SET user_type = 'enterprise' WHERE cognito_sub = 'dev-local' RETURNING 1`)
	if err != nil {
		return fmt.Errorf("lift the dev account's tier caps: %w", err)
	}
	if n := returnedRows(out); n != 1 {
		return fmt.Errorf("lift the dev account's tier caps: updated %d rows, want 1", n)
	}
	return nil
}

// returnedRows counts the rows of an UPDATE ... RETURNING 1, skipping the
// command tag psql may print after them.
func returnedRows(out string) int {
	n := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == "1" {
			n++
		}
	}
	return n
}

type fatalError struct{ error }

func (e *fatalError) Unwrap() error { return e.error }

// until polls fn until it reports done, fails fatally, or timeout passes. An
// error from fn is kept and reported if the wait times out.
func (a *App) until(ctx context.Context, timeout time.Duration, what string, fn func(context.Context) (bool, error)) error {
	deadline := a.now().Add(timeout)
	var last error
	for {
		done, err := fn(ctx)
		var fatal *fatalError
		if errors.As(err, &fatal) {
			return fmt.Errorf("waiting for %s: %w", what, fatal.error)
		}
		if done {
			return nil
		}
		if err != nil {
			last = err
		}
		if !a.now().Before(deadline) {
			if last != nil {
				return fmt.Errorf("timed out after %s waiting for %s: last error: %v", timeout, what, last)
			}
			return fmt.Errorf("timed out after %s waiting for %s", timeout, what)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s: %w", what, ctx.Err())
		case <-time.After(a.pollEvery):
		}
	}
}

// initDone is true once every pod of each step's Deployment has finished the init
// container, and the api-server's extensions step logged its success line.
func (a *App) initDone(ctx context.Context) (bool, error) {
	return a.stepsDone(ctx, initContainers)
}

func (a *App) stepsDone(ctx context.Context, steps []initStep) (bool, error) {
	for _, st := range steps {
		out, err := a.kube.GetSelector(ctx, "pods", st.selector,
			`{range .items[*]}{range .status.initContainerStatuses[*]}{.name}{"="}{.state.terminated.exitCode}{"\n"}{end}{end}`)
		if err != nil {
			return false, err
		}
		seen, ok := 0, true
		for _, line := range strings.Split(out, "\n") {
			name, code, found := strings.Cut(strings.TrimSpace(line), "=")
			if !found || name != st.container {
				continue
			}
			seen++
			if code != "0" {
				ok = false
			}
		}
		if seen == 0 {
			return false, fmt.Errorf("no pod of %s reports init container %s yet", st.deployment, st.container)
		}
		if !ok {
			return false, fmt.Errorf("init container %s of %s has not finished", st.container, st.deployment)
		}
		if st.marker != "" {
			logs, err := a.kube.Logs(ctx, "deployment/"+st.deployment, st.container, 200)
			if err != nil {
				return false, err
			}
			if !strings.Contains(logs, st.marker) {
				return false, fmt.Errorf("init container %s exited 0 without logging %q", st.container, st.marker)
			}
		}
	}
	return true, nil
}

func (a *App) connect(ctx context.Context) error {
	api, err := a.reach(ctx, apiService, apiServicePort, "app="+apiDeployment, apiPodPort)
	if err != nil {
		return fmt.Errorf("reach the api-server: %w", err)
	}
	mcp, err := a.reach(ctx, mcpService, mcpServicePort, "app="+mcpService, mcpPodPort)
	if err != nil {
		return fmt.Errorf("reach the mcp-server: %w", err)
	}
	a.mu.Lock()
	a.apiURL, a.mcpURL = api, mcp+mcpPath
	a.mu.Unlock()
	return a.connectLLM(ctx)
}

// ClusterURLs are the api-server and mcp-server addresses a pod in the cluster
// uses for the stack in namespace, which is what load clients dial.
func ClusterURLs(namespace string) (api, mcp string) {
	return fmt.Sprintf("http://%s.%s.svc:%d", apiService, namespace, apiServicePort),
		fmt.Sprintf("http://%s.%s.svc:%d%s", mcpService, namespace, mcpServicePort, mcpPath)
}

// ClusterGatewayURL is the inference gateway's in-cluster address.
func ClusterGatewayURL(namespace string) string {
	return fmt.Sprintf("http://inference-gateway-service.%s.svc:8088", namespace)
}

// API is the REST client for the stack's default account: the one account of a
// stack with no identity provider, or the mock pool's default user.
func (a *App) API() *mekoapi.Client {
	c := a.newAPI(a.APIURL())
	if tok := a.defaultToken(); tok != "" {
		c = c.WithToken(tok)
	}
	return c
}

func (a *App) reach(ctx context.Context, svc string, svcPort int, podSelector string, podPort int) (string, error) {
	if a.cfg.Reach != ReachPortForward {
		var tried []string
		if ip, err := a.kube.Get(ctx, "service", svc, "{.spec.clusterIP}"); err == nil && ip != "" && ip != "None" {
			addr := net.JoinHostPort(ip, fmt.Sprint(svcPort))
			if a.dial(ctx, addr) == nil {
				return "http://" + addr, nil
			}
			tried = append(tried, addr)
		}
		if ip, err := a.kube.GetSelector(ctx, "pods", podSelector, "{.items[0].status.podIP}"); err == nil && ip != "" {
			addr := net.JoinHostPort(ip, fmt.Sprint(podPort))
			if a.dial(ctx, addr) == nil {
				return "http://" + addr, nil
			}
			tried = append(tried, addr)
		}
		a.log("mekostack: %s not reachable directly (tried %v), using a port-forward", svc, tried)
	}
	pf, err := a.kube.PortForward(ctx, "svc/"+svc, 0, svcPort)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.pfs = append(a.pfs, pf)
	a.mu.Unlock()
	return "http://" + pf.LocalAddr(), nil
}

func dialTCP(ctx context.Context, addr string) error {
	d := net.Dialer{Timeout: 2 * time.Second}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return c.Close()
}

func (a *App) Close() {
	a.mu.Lock()
	pfs := a.pfs
	a.pfs = nil
	a.mu.Unlock()
	for _, pf := range pfs {
		_ = pf.Close()
	}
}

type Workload struct {
	Kind    string
	Name    string
	Desired int
	Ready   int
}

func (a *App) Workloads(ctx context.Context) ([]Workload, error) {
	var all []Workload
	for _, kind := range []string{"deployments", "statefulsets"} {
		out, err := a.kube.GetSelector(ctx, kind, "",
			`{range .items[*]}{.metadata.name}{"\t"}{.spec.replicas}{"\t"}{.status.readyReplicas}{"\n"}{end}`)
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(out, "\n") {
			f := strings.Split(strings.TrimSpace(line), "\t")
			if len(f) < 2 || f[0] == "" {
				continue
			}
			w := Workload{Kind: strings.TrimSuffix(kind, "s"), Name: f[0]}
			fmt.Sscan(f[1], &w.Desired)
			if len(f) > 2 {
				fmt.Sscan(f[2], &w.Ready)
			}
			all = append(all, w)
		}
	}
	return all, nil
}

func (a *App) waitWorkloads(ctx context.Context) error {
	return a.until(ctx, a.cfg.ProvisionTimeout, "every Deployment and StatefulSet to be Ready", func(ctx context.Context) (bool, error) {
		ws, err := a.Workloads(ctx)
		if err != nil {
			return false, err
		}
		if len(ws) == 0 {
			return false, errors.New("the release has no workloads yet")
		}
		nr := NotReady(ws)
		if len(nr) == 0 {
			return true, nil
		}
		names := make([]string, len(nr))
		for i, w := range nr {
			names[i] = fmt.Sprintf("%s/%s %d/%d", w.Kind, w.Name, w.Ready, w.Desired)
			if w.Name == apiDeployment {
				if err := a.migrationsWedged(ctx); err != nil {
					return false, &fatalError{err}
				}
			}
		}
		return false, fmt.Errorf("not ready: %s", strings.Join(names, ", "))
	})
}

// migrationsWedged reports the api-server's migrator unable to get past a migration,
// which no amount of waiting clears. Two shapes:
//
//   - It refuses a dirty version. On a fresh stack that is golang-migrate's postgres
//     driver on YB: its version write TRUNCATEs schema_migrations, which YB commits
//     outside the transaction, then the INSERT fails with a catalog version mismatch and
//     the table is left empty.
//   - A build with the MEKO-831 recovery clears a dirty version 1 itself, so that string
//     never appears; a migration 1 that then fails again shows instead as the "was
//     interrupted" warning followed by "migration failed" in the same log. A warning
//     followed by a success is a recovery that worked and is not reported.
func (a *App) migrationsWedged(ctx context.Context) error {
	logs, err := a.kube.Logs(ctx, "deployment/"+apiDeployment, "migrate", 50)
	if err != nil {
		return nil
	}
	rerun := false
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "Dirty database version") {
			return fmt.Errorf("api-server migrations wedged on a dirty version "+
				"(golang-migrate postgres driver on YugabyteDB): %s", strings.TrimSpace(line))
		}
		if strings.Contains(line, "data migration 1 was interrupted") {
			rerun = true
		}
		if rerun && strings.Contains(line, `msg="migration failed"`) {
			return fmt.Errorf("api-server data migration 1 keeps failing after its dirty version "+
				"was cleared and rerun: %s", strings.TrimSpace(line))
		}
	}
	return nil
}

func NotReady(ws []Workload) []Workload {
	var out []Workload
	for _, w := range ws {
		if w.Ready < w.Desired {
			out = append(out, w)
		}
	}
	return out
}

// Extensions lists the extensions installed in the stack's database.
func (a *App) Extensions(ctx context.Context) ([]string, error) {
	out, err := a.ysql(ctx, "SELECT extname FROM pg_extension ORDER BY 1")
	if err != nil {
		return nil, err
	}
	var exts []string
	for _, line := range strings.Split(out, "\n") {
		if l := strings.TrimSpace(line); l != "" {
			exts = append(exts, l)
		}
	}
	return exts, nil
}

// DatapacksWithoutLangfuseProject lists the datapacks that have no Langfuse
// project mapping. Every MCP call scoped to such a datapack fails with "No
// Langfuse project found" (MEKO-621).
func (a *App) DatapacksWithoutLangfuseProject(ctx context.Context) ([]string, error) {
	out, err := a.ysql(ctx, "SELECT d.datapack_id FROM meko_system.datapacks d"+
		" LEFT JOIN meko_system.langfuse_project_mapping m ON m.datapack_id = d.datapack_id"+
		" WHERE m.datapack_id IS NULL ORDER BY 1")
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		if l := strings.TrimSpace(line); l != "" {
			ids = append(ids, l)
		}
	}
	return ids, nil
}

// UniverseUsage is the api-server's own count of datapacks per the universe
// table next to the datapacks that exist. The counter goes up when a create
// takes a slot and down when a delete gives it back, and a create that finds no
// slot answers 500.
type UniverseUsage struct {
	Counted   int
	Max       int
	Datapacks int
}

func (a *App) UniverseUsage(ctx context.Context) (UniverseUsage, error) {
	out, err := a.ysql(ctx, "SELECT coalesce(sum(num_datapacks_created),0), coalesce(sum(max_datapacks),0),"+
		" (SELECT count(*) FROM meko_system.datapacks) FROM meko_system.yb_universes")
	if err != nil {
		return UniverseUsage{}, err
	}
	var u UniverseUsage
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d|%d|%d", &u.Counted, &u.Max, &u.Datapacks); err != nil {
		return UniverseUsage{}, fmt.Errorf("read the universe counters from %q: %w", strings.TrimSpace(out), err)
	}
	return u, nil
}

// ysql runs one statement against the stack's database and returns its unaligned
// output. The bundled one is reached with ysqlsh from its tserver pod, any other
// from a pod of the stack that has ysqlsh. The statement must not contain a
// double quote.
func (a *App) ysql(ctx context.Context, query string) (string, error) {
	if _, ok := a.externalBacking(); ok {
		return a.ysqlViaPod(ctx, query)
	}
	pod, err := a.kube.GetSelector(ctx, "pods", tserverSelector, "{.items[0].metadata.name}")
	if err != nil || pod == "" {
		return "", fmt.Errorf("find a tserver pod (%s): %v", tserverSelector, err)
	}
	cmd := fmt.Sprintf(`%s -h yb-tservers.%s.svc.cluster.local. -p 5433 -U yugabyte -d yugabyte -v ON_ERROR_STOP=1 -tAc "%s"`,
		ysqlsh, a.cfg.Namespace, query)
	res, err := a.kube.ForPod(pod, tserverContainer).Run(ctx, cmd)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("ysqlsh in %s exited %d: %s", pod, res.ExitCode, tail(res.Stderr, 500))
	}
	return res.Stdout, nil
}

func (a *App) YSQL(ctx context.Context, query string) (string, error) { return a.ysql(ctx, query) }

func (a *App) Versions(ctx context.Context) (product, database string) {
	if b, ok := a.externalBacking(); ok {
		return a.imageTag(ctx, "deployment", apiDeployment), a.backingDatabaseTag(ctx, b)
	}
	return a.imageTag(ctx, "deployment", apiDeployment), a.imageTag(ctx, "statefulset", "yb-tserver")
}

func (a *App) imageTag(ctx context.Context, kind, name string) string {
	img, err := a.kube.Get(ctx, kind, name, "{.spec.template.spec.containers[0].image}")
	if err != nil {
		return ""
	}
	if i := strings.LastIndexByte(img, ':'); i > strings.LastIndexByte(img, '/') {
		return img[i+1:]
	}
	return ""
}
