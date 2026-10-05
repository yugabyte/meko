package yugabytedk8s

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
	"github.com/yugabyte/meko/meko/test/functional/framework/kubemeta"
	"github.com/yugabyte/meko/meko/test/functional/framework/universe"
)

// Universe is a running yugabyted universe.
type Universe struct {
	p       *Provider
	spec    universe.Spec
	ns      string
	image   string
	kube    *k8s.Kubectl
	regions map[string]string
}

var (
	_ universe.Universe     = (*Universe)(nil)
	_ universe.XCluster     = (*Universe)(nil)
	_ universe.ReadReplicas = (*Universe)(nil)
	_ universe.Backups      = (*Universe)(nil)
	_ universe.PITR         = (*Universe)(nil)
	_ universe.Upgrades     = (*Universe)(nil)
	_ universe.Gflags       = (*Universe)(nil)
	_ universe.NodeControl  = (*Universe)(nil)
)

func (u *Universe) Name() string                { return u.spec.Name }
func (u *Universe) Spec() universe.Spec         { return u.spec }
func (u *Universe) Provider() universe.Provider { return universe.Yugabyted }
func (u *Universe) Namespace() string           { return u.ns }
func (u *Universe) Image() string               { return u.image }
func (u *Universe) log(f string, a ...any)      { u.p.log(f, a...) }
func (u *Universe) ClusterAddr() (string, int) {
	return u.ysqlService(universe.Primary) + "." + u.ns + ".svc.cluster.local.", ysqlPort
}

// Conn is the first ready primary pod's IP, which the coordinator can dial when
// pod IPs are routable to it. The password is empty, the pods run with auth off.
func (u *Universe) Conn() universe.ConnInfo {
	ci := universe.ConnInfo{Port: ysqlPort, User: "yugabyte", Database: "yugabyte", SSLMode: "disable"}
	if ip, err := u.kube.GetSelector(context.Background(), "pods", labelSelector(u.selector(universe.Primary, "")), "{.items[0].status.podIP}"); err == nil {
		ci.Host = strings.TrimSpace(ip)
	}
	return ci
}

func labelSelector(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func (u *Universe) create(ctx context.Context) error {
	if err := u.p.cluster.CreateNamespace(ctx, u.ns, u.p.namespaceLabels()); err != nil {
		return fmt.Errorf("create namespace %s: %w", u.ns, err)
	}
	if u.p.cfg.TTL > 0 {
		exp := u.p.now().UTC().Add(u.p.cfg.TTL).Format(time.RFC3339)
		if err := u.p.cluster.AnnotateNamespace(ctx, u.ns, kubemeta.ExpiresAnnotation, exp); err != nil {
			return err
		}
	}
	if m, err := u.p.pullSecretManifest(u.ns); err != nil {
		return err
	} else if m != "" {
		if err := u.kube.Apply(ctx, m); err != nil {
			return fmt.Errorf("apply the pull secret: %w", err)
		}
	}
	for _, svc := range u.services() {
		m, err := render(svc)
		if err != nil {
			return err
		}
		if err := u.kube.Apply(ctx, m); err != nil {
			return fmt.Errorf("apply service: %w", err)
		}
	}
	if err := u.applyCluster(ctx, u.spec.Primary()); err != nil {
		return err
	}
	if err := u.waitReady(ctx, u.spec.Primary().Tservers()); err != nil {
		return err
	}
	if err := u.configurePlacement(ctx); err != nil {
		return err
	}
	if err := u.waitServers(ctx, u.spec.Primary().Tservers()); err != nil {
		return err
	}
	for _, c := range u.spec.ReadReplicas() {
		if err := u.AddReadReplica(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

func (u *Universe) applyCluster(ctx context.Context, c universe.ClusterSpec) error {
	cm, err := u.configMap(c)
	if err != nil {
		return err
	}
	docs := []map[string]any{cm}
	for _, z := range c.Zones() {
		docs = append(docs, u.statefulSet(c, z, c.Placement[z]))
	}
	m, err := render(docs...)
	if err != nil {
		return err
	}
	if err := u.kube.Apply(ctx, m); err != nil {
		return fmt.Errorf("apply the %s cluster: %w", c.Type, err)
	}
	return nil
}

// pods lists pod names, optionally of one cluster role.
func (u *Universe) pods(ctx context.Context, role string) ([]string, error) {
	sel := labelSelector(map[string]string{labelApp: appName, labelUniv: u.spec.Name})
	if role != "" {
		sel += "," + labelRole + "=" + role
	}
	out, err := u.kube.GetSelector(ctx, "pods", sel, `{range .items[*]}{.metadata.name}{"\n"}{end}`)
	if err != nil {
		return nil, err
	}
	names := strings.Fields(out)
	sort.Strings(names)
	return names, nil
}

func (u *Universe) waitReady(ctx context.Context, want int) error {
	return u.waitRoleReady(ctx, universe.Primary, want)
}

func (u *Universe) waitRoleReady(ctx context.Context, role universe.ClusterType, want int) error {
	deadline := time.Now().Add(u.p.cfg.ReadyTimeout)
	var last string
	for {
		out, err := u.kube.GetSelector(ctx, "pods", labelSelector(u.selector(role, "")),
			`{range .items[*]}{range .status.conditions[?(@.type=="Ready")]}{.status}{"\n"}{end}{end}`)
		if err == nil {
			ready := strings.Count(out, "True")
			last = fmt.Sprintf("%d of %d primary pods ready", ready, want)
			if ready >= want {
				return nil
			}
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("universe %s: %s after %s%s", u.spec.Name, last, u.p.cfg.ReadyTimeout, u.diagnose(ctx))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (u *Universe) diagnose(ctx context.Context) string {
	out, err := u.kube.GetSelector(ctx, "pods", "",
		`{range .items[*]}{.metadata.name}{" "}{.status.phase}{range .status.containerStatuses[*]}{" "}{.state.waiting.reason}{end}{"\n"}{end}`)
	if err != nil {
		return ""
	}
	return "\npods:\n" + strings.TrimSpace(out)
}

func (u *Universe) waitServers(ctx context.Context, want int) error {
	deadline := time.Now().Add(u.p.cfg.ReadyTimeout)
	var last string
	for {
		out, err := u.SQL(ctx, "", "", "SELECT count(*) FROM yb_servers()")
		if err == nil {
			n, _ := strconv.Atoi(strings.TrimSpace(out))
			last = fmt.Sprintf("yb_servers() lists %d, want %d", n, want)
			if n >= want {
				return nil
			}
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("universe %s: %s", u.spec.Name, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// Nodes lists the universe's pods.
func (u *Universe) Nodes(ctx context.Context) ([]universe.Node, error) {
	out, err := u.kube.GetSelector(ctx, "pods", labelSelector(map[string]string{labelApp: appName, labelUniv: u.spec.Name}),
		`{range .items[*]}{.metadata.name}{"\t"}{.metadata.labels.yb-cluster}{"\t"}{.metadata.labels.yb-zone}{"\t"}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}`)
	if err != nil {
		return nil, err
	}
	var nodes []universe.Node
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 4 || f[0] == "" {
			continue
		}
		n := universe.Node{Name: f[0], Zone: f[2], Tserver: true, Ready: f[3] == "True", Cluster: universe.Primary}
		if f[1] == "read-replica" {
			n.Cluster = universe.ReadReplica
		} else {
			n.Master = strings.HasSuffix(f[0], "-0") || true
		}
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	return nodes, nil
}

// SQL runs one statement with ysqlsh in a pod, the statement on stdin so it can
// hold any quoting.
func (u *Universe) SQL(ctx context.Context, node, database, statement string) (string, error) {
	if node == "" {
		names, err := u.pods(ctx, "primary")
		if err != nil || len(names) == 0 {
			return "", fmt.Errorf("no pod of %s to run SQL in: %v", u.spec.Name, err)
		}
		node = names[0]
	}
	if database == "" {
		database = "yugabyte"
	}
	cmd := fmt.Sprintf(`%s -h "$(hostname -f)" -p %d -U yugabyte -d %s -v ON_ERROR_STOP=1 -tA`, ysqlsh, ysqlPort, shellQuote(database))
	res, err := u.kube.ForPod(node, container).RunStdin(ctx, strings.NewReader(statement), cmd)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("ysqlsh in %s exited %d: %s", node, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// sh runs a shell command in a pod and returns its combined output.
func (u *Universe) sh(ctx context.Context, pod, cmd string) (string, error) {
	res, err := u.kube.ForPod(pod, container).Run(ctx, "cd /home/yugabyte && "+cmd)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return res.Stdout, fmt.Errorf("%s exited %d: %s%s", firstWord(cmd), res.ExitCode, strings.TrimSpace(res.Stderr), tailOf(res.Stdout))
	}
	return res.Stdout, nil
}

func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

func tailOf(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 600 {
		s = "..." + s[len(s)-600:]
	}
	if s == "" {
		return ""
	}
	return "\n" + s
}

// Teardown deletes the universe's namespace and with it the volumes.
func (u *Universe) Teardown(ctx context.Context) error {
	return deleteNamespace(ctx, u.p.cluster, u.ns)
}
