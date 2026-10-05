package yugabytedk8s

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/universe"
)

const base = "--base_dir=/data"

func (u *Universe) firstPrimaryPod() string {
	sts, _ := u.firstPod()
	return sts + "-0"
}

// locations are the cloud.region.zone constraints of a cluster's zones.
func (u *Universe) locations(c universe.ClusterSpec) []string {
	var out []string
	for _, z := range c.Zones() {
		out = append(out, u.cloudLocation(z))
	}
	return out
}

// configurePlacement sets the replication factor and spreads replicas over the
// primary's zones, from the first node.
func (u *Universe) configurePlacement(ctx context.Context) error {
	c := u.spec.Primary()
	if c.Tservers() < 2 && c.RF == 1 {
		return nil
	}
	cmd := fmt.Sprintf("bin/yugabyted configure data_placement %s --rf=%d", base, c.RF)
	if zones := c.Zones(); len(zones) >= 3 && c.RF <= len(zones) {
		cmd += " --fault_tolerance=zone --constraint_value=" + strings.Join(u.locations(c), ",")
	}
	if _, err := u.sh(ctx, u.firstPrimaryPod(), cmd); err != nil {
		return fmt.Errorf("configure data placement: %w", err)
	}
	return nil
}

// AddReadReplica starts the read replica nodes, then declares the cluster.
func (u *Universe) AddReadReplica(ctx context.Context, c universe.ClusterSpec) error {
	c.Type = universe.ReadReplica
	c = withRR(c, u.spec.Primary().Node)
	if err := (universe.Spec{Name: u.spec.Name, Clusters: []universe.ClusterSpec{u.spec.Primary(), c}}).Validate(); err != nil {
		return err
	}
	u.spec.Clusters = append(u.spec.Clusters, c)
	if err := u.applyCluster(ctx, c); err != nil {
		return err
	}
	if err := u.waitRoleReady(ctx, universe.ReadReplica, c.Tservers()); err != nil {
		return err
	}
	cmd := fmt.Sprintf("bin/yugabyted configure_read_replica new %s --rf=%d --data_placement_constraint=%s", base, c.RF, u.constraints(c))
	if _, err := u.sh(ctx, u.firstPrimaryPod(), cmd); err != nil {
		return fmt.Errorf("configure the read replica: %w", err)
	}
	return u.waitServers(ctx, u.spec.Primary().Tservers()+c.Tservers())
}

// constraints is cloud.region.zone:replicas per zone.
func (u *Universe) constraints(c universe.ClusterSpec) string {
	var out []string
	for _, z := range c.Zones() {
		out = append(out, fmt.Sprintf("%s:%d", u.cloudLocation(z), c.Placement[z]))
	}
	return strings.Join(out, ",")
}

// A read replica with no node shape of its own gets the primary's.
func withRR(c universe.ClusterSpec, primary universe.NodeSpec) universe.ClusterSpec {
	if c.RF == 0 {
		c.RF = 1
	}
	c.Node = c.Node.Or(primary)
	return c
}

// RemoveReadReplica deletes the read replica cluster and its pods.
func (u *Universe) RemoveReadReplica(ctx context.Context) error {
	if len(u.spec.ReadReplicas()) == 0 {
		return nil
	}
	if _, err := u.sh(ctx, u.firstPrimaryPod(), "bin/yugabyted configure_read_replica delete "+base); err != nil {
		return fmt.Errorf("delete the read replica cluster: %w", err)
	}
	for _, c := range u.spec.ReadReplicas() {
		for _, z := range c.Zones() {
			if err := u.kube.Delete(ctx, "statefulset", stsName(universe.ReadReplica, z)); err != nil {
				return err
			}
		}
	}
	pvcs, _ := u.kube.GetSelector(ctx, "pvc", labelRole+"=read-replica", "{.items[*].metadata.name}")
	for _, p := range strings.Fields(pvcs) {
		_ = u.kube.Delete(ctx, "pvc", p)
	}
	kept := u.spec.Clusters[:0]
	for _, c := range u.spec.Clusters {
		if c.Type != universe.ReadReplica {
			kept = append(kept, c)
		}
	}
	u.spec.Clusters = kept
	return nil
}

func (u *Universe) targetAddr(target universe.Universe) (string, error) {
	t, ok := target.(*Universe)
	if !ok {
		return "", universe.Unsupportedf(universe.Yugabyted, "xcluster", "the target is a %s universe, yugabyted replicates to yugabyted only", target.Provider())
	}
	return t.podFQDN(stsName(universe.Primary, t.spec.Primary().Zones()[0]), 0), nil
}

func replicationID(target universe.Universe, src string) string { return src + "-to-" + target.Name() }

// SetupXCluster replicates the databases to target in automatic mode, which
// carries DDL. The databases must be empty on the source, they are created empty
// on the target, so there is nothing to bootstrap.
func (u *Universe) SetupXCluster(ctx context.Context, target universe.Universe, databases []string) error {
	addr, err := u.targetAddr(target)
	if err != nil {
		return err
	}
	id := replicationID(target, u.spec.Name)
	for _, db := range databases {
		if _, err := target.SQL(ctx, "", "", fmt.Sprintf("CREATE DATABASE %s", db)); err != nil && !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("create %s on the target: %w", db, err)
		}
	}
	pod := u.firstPrimaryPod()
	if _, err := u.retry(ctx, pod, fmt.Sprintf("bin/yugabyted xcluster create_checkpoint %s --replication_id=%s --databases=%s --automatic_mode", base, id, strings.Join(databases, ","))); err != nil {
		return fmt.Errorf("xcluster create_checkpoint: %w", err)
	}
	if _, err := u.retry(ctx, pod, fmt.Sprintf("bin/yugabyted xcluster set_up %s --replication_id=%s --target_address=%s --bootstrap_done", base, id, addr)); err != nil {
		return fmt.Errorf("xcluster set_up: %w", err)
	}
	return nil
}

// XClusterStatus reads yugabyted's own report. Healthy is false when it names an
// error, the lag is the largest "lag" figure in it that parses.
func (u *Universe) XClusterStatus(ctx context.Context, target universe.Universe) (universe.XClusterStatus, error) {
	out, err := u.sh(ctx, u.firstPrimaryPod(), fmt.Sprintf("bin/yugabyted xcluster status %s --replication_id=%s", base, replicationID(target, u.spec.Name)))
	if err != nil {
		return universe.XClusterStatus{Detail: out}, err
	}
	return universe.XClusterStatus{Healthy: xclusterHealthy(out), Detail: strings.TrimSpace(out)}, nil
}

var (
	xclusterBad  = regexp.MustCompile(`(?i)\b(error|failed|failure|not running|unhealthy|not healthy|paused|stopped)\b`)
	xclusterGood = regexp.MustCompile(`(?i)\b(running|replicating|healthy)\b`)
)

func xclusterHealthy(out string) bool {
	return !xclusterBad.MatchString(out) && xclusterGood.MatchString(out)
}

func (u *Universe) TeardownXCluster(ctx context.Context, target universe.Universe) error {
	addr, err := u.targetAddr(target)
	if err != nil {
		return err
	}
	_, err = u.sh(ctx, u.firstPrimaryPod(), fmt.Sprintf("bin/yugabyted xcluster delete_replication %s --replication_id=%s --target_address=%s", base, replicationID(target, u.spec.Name), addr))
	return err
}

func s3URI(s universe.S3) string {
	return "s3://" + strings.Trim(s.Bucket+"/"+strings.Trim(s.Prefix, "/"), "/")
}

// s3Env is the environment yugabyted's backup tool reads its credentials and
// endpoint from.
func s3Env(s universe.S3) string {
	env := fmt.Sprintf("AWS_ACCESS_KEY_ID=%s AWS_SECRET_ACCESS_KEY=%s AWS_DEFAULT_REGION=%s", shellQuote(s.AccessKey), shellQuote(s.SecretKey), shellQuote(orDefault(s.Region, "us-east-1")))
	if s.Endpoint != "" {
		env += " AWS_ENDPOINT=" + shellQuote(s.Endpoint) + " AWS_HOST_BASE=" + shellQuote(strings.TrimPrefix(strings.TrimPrefix(s.Endpoint, "http://"), "https://")) + " PATH_STYLE_ACCESS=true"
	}
	return env
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// Backup runs yugabyted backup into the object store and waits for it.
func (u *Universe) Backup(ctx context.Context, database string, to universe.S3) (universe.Backup, error) {
	b := universe.Backup{ID: u.spec.Name + "-" + database, Database: database, Location: s3URI(to), At: u.p.now()}
	out, err := u.sh(ctx, u.firstPrimaryPod(), fmt.Sprintf("%s bin/yugabyted backup %s --database=%s --cloud_storage_uri=%s", s3Env(to), base, shellQuote(database), shellQuote(b.Location)))
	if err != nil {
		return b, fmt.Errorf("yugabyted backup: %w", err)
	}
	return b, u.waitTask(ctx, "backup", out, to, database, b.Location)
}

// Restore runs yugabyted restore of a backup into this universe.
func (u *Universe) Restore(ctx context.Context, b universe.Backup, from universe.S3) error {
	out, err := u.sh(ctx, u.firstPrimaryPod(), fmt.Sprintf("%s bin/yugabyted restore %s --database=%s --cloud_storage_uri=%s", s3Env(from), base, shellQuote(b.Database), shellQuote(b.Location)))
	if err != nil {
		return fmt.Errorf("yugabyted restore: %w", err)
	}
	return u.waitTask(ctx, "restore", out, from, b.Database, b.Location)
}

// waitTask polls `--status` until yugabyted reports the task done or failed.
func (u *Universe) waitTask(ctx context.Context, verb, first string, s universe.S3, db, uri string) error {
	deadline := time.Now().Add(15 * time.Minute)
	out := first
	for {
		low := strings.ToLower(out)
		switch {
		case strings.Contains(low, " is complete"):
			return nil
		case strings.Contains(low, "fail"), strings.Contains(low, "error"), strings.Contains(low, "abort"), strings.Contains(low, "no "+verb+" task"), strings.Contains(low, "not found"):
			return fmt.Errorf("%s: %s", verb, strings.TrimSpace(out))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s still not done after 15m: %s", verb, strings.TrimSpace(out))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
		var err error
		out, err = u.sh(ctx, u.firstPrimaryPod(), fmt.Sprintf("%s bin/yugabyted %s %s --database=%s --cloud_storage_uri=%s --status", s3Env(s), verb, base, shellQuote(db), shellQuote(uri)))
		if err != nil {
			out = err.Error()
		}
	}
}

func (u *Universe) EnablePITR(ctx context.Context, database string, retention time.Duration) error {
	days := int(retention.Hours() / 24)
	if days < 1 {
		days = 1
	}
	_, err := u.sh(ctx, u.firstPrimaryPod(), fmt.Sprintf("bin/yugabyted configure point_in_time_recovery %s --enable --retention %dd --database %s", base, days, shellQuote(database)))
	return err
}

func (u *Universe) RecoverTo(ctx context.Context, database string, at time.Time) error {
	ts := at.UTC().Format("2006-01-02 03:04:05 PM")
	_, err := u.sh(ctx, u.firstPrimaryPod(), fmt.Sprintf("bin/yugabyted restore %s --recover_to_point_in_time %s --database %s", base, shellQuote(ts), shellQuote(database)))
	return err
}

// SetGflags rewrites the clusters' flag files and rolls the pods by changing the
// pod template, one StatefulSet at a time.
func (u *Universe) SetGflags(ctx context.Context, master, tserver map[string]string) error {
	u.spec.MasterGflags = merge(u.spec.MasterGflags, master)
	u.spec.TServerGflags = merge(u.spec.TServerGflags, tserver)
	for _, c := range u.spec.Clusters {
		if err := u.applyCluster(ctx, c); err != nil {
			return err
		}
		if err := u.rollout(ctx, c); err != nil {
			return err
		}
	}
	return u.waitServers(ctx, u.allTservers())
}

func (u *Universe) allTservers() int {
	n := 0
	for _, c := range u.spec.Clusters {
		n += c.Tservers()
	}
	return n
}

func (u *Universe) rollout(ctx context.Context, c universe.ClusterSpec) error {
	for _, z := range c.Zones() {
		if err := u.kube.RolloutStatus(ctx, "statefulset", stsName(c.Type, z), u.p.cfg.ReadyTimeout); err != nil {
			return err
		}
	}
	return nil
}

// Upgrade swaps the image on every StatefulSet, primary first, waits for each
// roll, then finalises from the first node.
func (u *Universe) Upgrade(ctx context.Context, imageOrVersion string) error {
	img := imageOrVersion
	if !strings.Contains(img, "/") {
		img = u.p.imageFor(universe.Spec{Version: imageOrVersion})
	}
	for _, c := range u.spec.Clusters {
		for _, z := range c.Zones() {
			name := stsName(c.Type, z)
			patch := fmt.Sprintf(`{"spec":{"template":{"spec":{"containers":[{"name":%q,"image":%q}]}}}}`, container, img)
			if err := u.kube.Patch(ctx, "statefulset", name, "strategic", patch); err != nil {
				return err
			}
			if err := u.kube.RolloutStatus(ctx, "statefulset", name, u.p.cfg.ReadyTimeout); err != nil {
				return err
			}
		}
	}
	u.image = img
	if err := u.waitServers(ctx, u.allTservers()); err != nil {
		return err
	}
	for _, step := range []string{"ysql_catalog", "finalize_new_version"} {
		if _, err := u.sh(ctx, u.firstPrimaryPod(), "bin/yugabyted upgrade "+step+" "+base); err != nil {
			return fmt.Errorf("yugabyted upgrade %s: %w", step, err)
		}
	}
	return nil
}

// KillNode deletes the pod without a grace period, its StatefulSet brings it back.
func (u *Universe) KillNode(ctx context.Context, node string) error {
	return u.kube.DeleteNow(ctx, node)
}

// StopNode scales the node's StatefulSet to its ordinal, so only the highest
// ordinal of a zone can be stopped.
func (u *Universe) StopNode(ctx context.Context, node string) error {
	sts, ord, err := splitPod(node)
	if err != nil {
		return err
	}
	cur, err := u.kube.Get(ctx, "statefulset", sts, "{.spec.replicas}")
	if err != nil {
		return err
	}
	if n, _ := strconv.Atoi(cur); n != ord+1 {
		return universe.Unsupportedf(universe.Yugabyted, "stop a node", "%s is not the highest ordinal of %s (%s replicas), a StatefulSet can only drop its last pod", node, sts, cur)
	}
	return u.kube.Patch(ctx, "statefulset", sts, "merge", fmt.Sprintf(`{"spec":{"replicas":%d}}`, ord))
}

func (u *Universe) StartNode(ctx context.Context, node string) error {
	sts, ord, err := splitPod(node)
	if err != nil {
		return err
	}
	return u.kube.Patch(ctx, "statefulset", sts, "merge", fmt.Sprintf(`{"spec":{"replicas":%d}}`, ord+1))
}

func splitPod(pod string) (string, int, error) {
	i := strings.LastIndexByte(pod, '-')
	if i < 0 {
		return "", 0, fmt.Errorf("%q is not a StatefulSet pod name", pod)
	}
	n, err := strconv.Atoi(pod[i+1:])
	if err != nil {
		return "", 0, fmt.Errorf("%q is not a StatefulSet pod name", pod)
	}
	return pod[:i], n, nil
}

// retry runs a yugabyted command up to four times, since it refuses while the
// universe is still settling after it came up.
func (u *Universe) retry(ctx context.Context, pod, cmd string) (string, error) {
	var out string
	var err error
	for i := 0; i < 4; i++ {
		if out, err = u.sh(ctx, pod, cmd); err == nil {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
	return out, err
}
