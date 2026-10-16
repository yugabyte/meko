package mekostack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/yugabyte/meko/meko/test/functional/framework/kubemeta"
)

const (
	ModeBundled = "bundled"

	backingRecord = "mekoqa-backing"
	backingSecret = "mekoqa-backing-credentials"
	ysqlPod       = "mekoqa-ysql"
	ysqlContainer = "ysqlsh"

	backingDB = "yugabyte"

	backingAnswerWithin = 2 * time.Minute
)

var requiredExtensions = []string{"vector", "pg_dist_rag", "mage"}

// Backing is a YugabyteDB the stack uses instead of the bundled one. Host and
// Port are what the stack's pods dial. Replication and Tservers are 0 when the
// deployer does not know them.
type Backing struct {
	Mode        string
	Describe    string
	Host        string
	Port        int
	User        string
	Password    string
	SSLMode     string
	Namespace   string
	Replication int
	Tservers    int
	// Placement is the tserver count per zone name, nil when unknown.
	Placement map[string]int
}

func (b Backing) bundled() bool {
	return b.Mode == ModeBundled || (b.Mode == "" && b.Host == "")
}

func (b Backing) validate() error {
	var errs []error
	if b.Host == "" {
		errs = append(errs, errors.New("the backing database has no host"))
	}
	if b.Port <= 0 {
		errs = append(errs, fmt.Errorf("the backing database port is %d", b.Port))
	}
	// meko-mcp-server's connection string names the user literally.
	if b.User != "" && b.User != "yugabyte" {
		errs = append(errs, fmt.Errorf("the backing database user %q is not supported: the chart's mcp-server connects as yugabyte", b.User))
	}
	return errors.Join(errs...)
}

func (b Backing) user() string {
	if b.User == "" {
		return "yugabyte"
	}
	return b.User
}

func (b Backing) sslMode() string {
	if b.SSLMode == "" {
		return "disable"
	}
	return b.SSLMode
}

func (b Backing) dsn(password string) string {
	return fmt.Sprintf("postgresql://%s:%s@%s:%d/%s", b.user(), url.QueryEscape(password), b.Host, b.Port, backingDB)
}

func (b Backing) valuesFor(v map[string]any, password string, ragEnabled bool) {
	port := strconv.Itoa(b.Port)
	v["yugabyte"] = map[string]any{"enabled": false}
	api := v["apiServer"].(map[string]any)
	api["config"] = mergeMaps(api["config"], map[string]any{
		"DB_HOST": b.Host, "DB_PORT": port, "DB_USER_NAME": b.user(), "DB_SSL_MODE": b.sslMode(),
		"SYSTEM_DB_HOST": b.Host, "SYSTEM_DB_PORT": port, "SYSTEM_DB_USER_NAME": b.user(), "SYSTEM_DB_SSL_MODE": b.sslMode(),
		"LANGFUSE_DB_HOST": b.Host, "LANGFUSE_DB_SSL_MODE": b.sslMode(),
	})
	api["staticArgs"] = mergeMaps(api["staticArgs"], map[string]any{"langfuseDbPort": port, "langfuseDbUser": b.user()})
	mcp := v["mekoMcpServer"].(map[string]any)
	mcp["config"] = mergeMaps(mcp["config"], map[string]any{
		"DB_HOST": b.Host, "DB_PORT": port, "SYSTEM_DB_HOST": b.Host, "SYSTEM_DB_PORT": port,
	})
	v["dataPlaneSecrets"].(map[string]any)["dbPassword"] = password
	if ragEnabled {
		v["ragAgent"].(map[string]any)["dbConnectionString"] = b.dsn(password)
	}
}

func mergeMaps(base any, over map[string]any) map[string]any {
	out := map[string]any{}
	if m, ok := base.(map[string]any); ok {
		for k, val := range m {
			out[k] = val
		}
	}
	for k, val := range over {
		out[k] = val
	}
	return out
}

func (b Backing) record() map[string]string {
	return map[string]string{
		"mode": b.Mode, "describe": b.Describe, "host": b.Host, "port": strconv.Itoa(b.Port),
		"user": b.user(), "sslmode": b.sslMode(), "dbname": backingDB, "namespace": b.Namespace,
		"replication_factor": strconv.Itoa(b.Replication), "tservers": strconv.Itoa(b.Tservers),
		"placement": formatPlacement(b.Placement),
	}
}

func backingFromRecord(data map[string]string) Backing {
	atoi := func(k string) int { n, _ := strconv.Atoi(data[k]); return n }
	return Backing{
		Mode: data["mode"], Describe: data["describe"], Host: data["host"], Port: atoi("port"),
		User: data["user"], SSLMode: data["sslmode"], Namespace: data["namespace"],
		Replication: atoi("replication_factor"), Tservers: atoi("tservers"), Placement: parsePlacement(data["placement"]),
	}
}

func formatPlacement(p map[string]int) string {
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + strconv.Itoa(p[k])
	}
	return strings.Join(parts, ",")
}

func parsePlacement(s string) map[string]int {
	if s == "" {
		return nil
	}
	out := map[string]int{}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(part, "=")
		if n, err := strconv.Atoi(v); ok && err == nil {
			out[k] = n
		}
	}
	return out
}

func bundledBacking() Backing {
	return Backing{Mode: ModeBundled, Describe: "the chart's bundled YugabyteDB", Replication: 1, Tservers: 1}
}

// Backing is what the stack's database was built as. A stack with no record is
// the bundled one.
func (a *App) Backing(ctx context.Context) (Backing, error) {
	a.mu.Lock()
	b := a.backing
	a.mu.Unlock()
	if b != nil {
		return *b, nil
	}
	return a.loadBacking(ctx)
}

func (a *App) loadBacking(ctx context.Context) (Backing, error) {
	raw, err := a.kube.GetJSON(ctx, "configmap", backingRecord)
	if err != nil {
		if strings.Contains(err.Error(), "NotFound") {
			return bundledBacking(), nil
		}
		return Backing{}, fmt.Errorf("read the stack's database record: %w", err)
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if strings.TrimSpace(raw) == "" || json.Unmarshal([]byte(raw), &cm) != nil || len(cm.Data) == 0 {
		return bundledBacking(), nil
	}
	b := backingFromRecord(cm.Data)
	a.mu.Lock()
	a.backing = &b
	a.mu.Unlock()
	return b, nil
}

func (a *App) externalBacking() (Backing, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.backing == nil || a.backing.bundled() {
		return Backing{}, false
	}
	return *a.backing, true
}

func (a *App) recordBacking(ctx context.Context, b Backing, password string) error {
	labels := kubemeta.TestLabels(nil)
	docs := []map[string]any{{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": backingRecord, "labels": labels},
		"data":     b.record(),
	}}
	if !b.bundled() {
		docs = append(docs, map[string]any{
			"apiVersion": "v1", "kind": "Secret",
			"metadata":   map[string]any{"name": backingSecret, "labels": labels},
			"stringData": map[string]string{"password": password},
		})
	}
	var sb strings.Builder
	for i, d := range docs {
		out, err := yaml.Marshal(d)
		if err != nil {
			return err
		}
		if i > 0 {
			sb.WriteString("---\n")
		}
		sb.Write(out)
	}
	if err := a.kube.Apply(ctx, sb.String()); err != nil {
		return fmt.Errorf("record the stack's database: %w", err)
	}
	return nil
}

func (a *App) ysqlPodManifest() (string, error) {
	cm := func(key string) map[string]any {
		return map[string]any{"configMapKeyRef": map[string]any{"name": backingRecord, "key": key}}
	}
	pod := map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": ysqlPod, "labels": kubemeta.TestLabels(map[string]string{"app": ysqlPod})},
		"spec": map[string]any{
			"terminationGracePeriodSeconds": 0,
			"imagePullSecrets":              []any{map[string]any{"name": pullSecretName}},
			"containers": []any{map[string]any{
				"name": ysqlContainer, "image": a.cfg.DatabaseImage,
				"command": []string{"sleep", "infinity"},
				"env": []any{
					map[string]any{"name": "PGHOST", "valueFrom": cm("host")},
					map[string]any{"name": "PGPORT", "valueFrom": cm("port")},
					map[string]any{"name": "PGUSER", "valueFrom": cm("user")},
					map[string]any{"name": "PGSSLMODE", "valueFrom": cm("sslmode")},
					map[string]any{"name": "PGDATABASE", "valueFrom": cm("dbname")},
					map[string]any{"name": "PGPASSWORD", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": backingSecret, "key": "password"}}},
				},
				"resources": map[string]any{"requests": map[string]any{"cpu": "50m", "memory": "64Mi"}},
			}},
		},
	}
	out, err := yaml.Marshal(pod)
	return string(out), err
}

func (a *App) ysqlHelper(ctx context.Context) error {
	a.ysqlMu.Lock()
	defer a.ysqlMu.Unlock()
	if a.ysqlUp {
		return nil
	}
	manifest, err := a.ysqlPodManifest()
	if err != nil {
		return err
	}
	if err := a.kube.Apply(ctx, manifest); err != nil {
		return fmt.Errorf("create the ysqlsh pod: %w", err)
	}
	if err := a.kube.WaitFor(ctx, "pod/"+ysqlPod, "condition=Ready", max(a.cfg.ReadyTimeout, time.Minute)); err != nil {
		return fmt.Errorf("the ysqlsh pod did not become Ready: %w", err)
	}
	a.ysqlUp = true
	return nil
}

func (a *App) ysqlViaPod(ctx context.Context, query string) (string, error) {
	for attempt := 0; ; attempt++ {
		if err := a.ysqlHelper(ctx); err != nil {
			return "", err
		}
		cmd := fmt.Sprintf(`%s -v ON_ERROR_STOP=1 -tAc "%s"`, ysqlsh, query)
		res, err := a.kube.ForPod(ysqlPod, ysqlContainer).Run(ctx, cmd)
		if err == nil && res.ExitCode == 0 {
			return res.Stdout, nil
		}
		if attempt == 0 && (err != nil && strings.Contains(err.Error(), "NotFound") || strings.Contains(res.Stderr, "NotFound")) {
			a.ysqlMu.Lock()
			a.ysqlUp = false
			a.ysqlMu.Unlock()
			continue
		}
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("ysqlsh in %s exited %d: %s", ysqlPod, res.ExitCode, tail(res.Stderr, 500))
	}
}

// checkBacking fails a wrong database build here, not as a crash-looping
// ensure-extensions init container minutes into the install.
func (a *App) checkBacking(ctx context.Context, b Backing) error {
	a.log("mekostack: checking the %s", b.Describe)
	query := "SELECT name FROM pg_available_extensions WHERE name IN ('" + strings.Join(requiredExtensions, "','") + "') ORDER BY 1"
	out, err := a.ysql(ctx, query)
	// A universe reports ready from inside its pods, before its Service has
	// endpoints, and the ClusterIP refuses until it does.
	for deadline := time.Now().Add(backingAnswerWithin); err != nil && strings.Contains(err.Error(), "Connection refused") && time.Now().Before(deadline); {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
		out, err = a.ysql(ctx, query)
	}
	if err != nil {
		return fmt.Errorf("the database at %s:%d does not answer: %w", b.Host, b.Port, err)
	}
	have := strings.Fields(out)
	var missing []string
	for _, want := range requiredExtensions {
		if !slices.Contains(have, want) {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the database at %s:%d offers no %s: it is not the meko database build (%s)", b.Host, b.Port, strings.Join(missing, ", "), a.cfg.DatabaseImage)
	}
	return nil
}

func (a *App) backingDatabaseTag(ctx context.Context, b Backing) string {
	if b.Namespace == "" {
		return ""
	}
	var img string
	for _, sel := range []string{tserverSelector, "app=yugabyted"} {
		out, err := a.kube.InNamespace(b.Namespace).GetSelector(ctx, "pods", sel, "{.items[0].spec.containers[0].image}")
		if err == nil && out != "" {
			img = out
			break
		}
	}
	if i := strings.LastIndexByte(img, ':'); i > strings.LastIndexByte(img, '/') {
		return img[i+1:]
	}
	return ""
}

func (cfg Config) dbPassword(dp DataPlaneSecrets) string {
	if cfg.Backing != nil && cfg.Backing.Password != "" {
		return cfg.Backing.Password
	}
	return dp.DBPassword
}

func (a *App) setupBacking(ctx context.Context, dp DataPlaneSecrets) error {
	b := bundledBacking()
	if a.cfg.Backing != nil && !a.cfg.Backing.bundled() {
		b = *a.cfg.Backing
		if b.Mode == "" {
			b.Mode = "external"
		}
	}
	a.mu.Lock()
	a.backing = &b
	a.mu.Unlock()
	if err := a.recordBacking(ctx, b, a.cfg.dbPassword(dp)); err != nil {
		return err
	}
	if b.bundled() {
		return nil
	}
	return a.checkBacking(ctx, b)
}
