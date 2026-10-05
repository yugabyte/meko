// The Voyager migration surface (ac-voyager.md) end to end on a real control
// plane, which only a hand-run Docker runbook had covered: a source Postgres in
// the control plane's namespace, connectivity check, create, assess, provision
// target, migrate, verify the rows on the amp target, and end the migration.
// The source password goes in and must never come back out of any read.
package features

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	voyagerSourceRows  = 5000
	voyagerSourceTable = "amp_voyager_src"
	voyagerSourceUser  = "postgres"
	voyagerSourceDB    = "postgres"
	voyagerSourcePort  = 5432
	voyagerSourceReady = 5 * time.Minute
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_voyager_migration",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "gapcoverage"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Voyager migration end to end: a source Postgres pod with a seeded table; the connectivity check reports it reachable with a server version and an unreachable host as reachable=false without an HTTP error; createMigration answers 201 in state created; assess reaches assessed and the assessment reads back; provision-target reaches target_ready with a cluster, branch and writer under the run's pool; migrate reaches completed; the target endpoint serves every seeded row with the right checksum; end migration reaches ended; the migration is in the list; and no read of the migration ever carries the source password.",
		Params:      []string{"functional", "functional.voyager", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     90 * time.Minute,
		Run:         runAmpVoyagerMigration,
	})
}

func runAmpVoyagerMigration(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		v := &voyagerRun{a: a}
		return []phase{
			{"source_postgres", v.source},
			{"migration_create", v.create},
			{"migration_assess", v.assess},
			{"migration_provision_target", v.provisionTarget},
			{"migration_migrate", v.migrate},
			{"migration_verify", v.verify},
			{"migration_end", v.end},
		}
	})
}

type voyagerRun struct {
	a        *ampFeatureRun
	cl       *controller.Client
	kube     *k8s.Kubectl
	pod      string
	password string
	src      controller.MigrationSource
	mig      controller.Migration
	phase    time.Duration
}

func (v *voyagerRun) source(ctx context.Context) error {
	cl, err := v.a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	v.cl = cl
	if _, err := cl.ListMigrations(ctx, 1, 0); err != nil {
		if s := apiErrorStatus(err); s == http.StatusNotFound || s == http.StatusMethodNotAllowed || s == http.StatusNotImplemented {
			v.a.f.t.Skip("this controller has no /migrations surface: " + err.Error())
			return nil
		}
		return fmt.Errorf("GET /migrations: %w", err)
	}
	v.kube = v.a.f.ampKube()
	if v.kube == nil {
		v.a.f.t.Skip("the control plane is reached by a direct URL with no kube access, so no source Postgres can be stood up next to it")
		return nil
	}
	v.phase = v.a.f.t.Options.GetDuration("functional.voyager.phase_timeout", 25*time.Minute)
	image := v.a.f.t.Options.GetString("functional.voyager.source_image", "postgres:16-alpine")
	v.pod = "ampqa-src-" + v.a.f.suffix()
	v.password = "ampqa-src-" + v.a.f.suffix() + "-secret"
	if err := v.kube.Apply(ctx, sourcePostgresManifest(v.kube.Namespace(), v.pod, image, v.password)); err != nil {
		return core.AsEnvFailure(fmt.Errorf("apply the source Postgres pod: %w", err))
	}
	v.a.f.register("delete source postgres", func(ctx context.Context) error {
		_ = v.kube.Delete(ctx, "service", v.pod)
		return v.kube.Delete(ctx, "pod", v.pod)
	})
	if err := v.kube.WaitFor(ctx, "pod/"+v.pod, "condition=Ready", voyagerSourceReady); err != nil {
		return core.AsEnvFailure(fmt.Errorf("source Postgres pod %s not ready: %w", v.pod, err))
	}
	psql := fmt.Sprintf("psql -v ON_ERROR_STOP=1 -U %s -d %s -c %%s", voyagerSourceUser, voyagerSourceDB)
	seed := fmt.Sprintf("CREATE TABLE %s (id int primary key, v bigint, note text); INSERT INTO %s SELECT g, g*7, 'row-'||g FROM generate_series(1,%d) g;", voyagerSourceTable, voyagerSourceTable, voyagerSourceRows)
	in := v.kube.ForPod(v.pod, "")
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		res, err := in.Run(ctx, fmt.Sprintf(psql, shellQuote(seed)))
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = fmt.Errorf("seed the source: %w: %s", err, strings.TrimSpace(res.Stderr))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	if lastErr != nil {
		return core.AsEnvFailure(lastErr)
	}
	v.src = controller.MigrationSource{
		DBType: "postgresql", Host: v.pod + "." + v.kube.Namespace() + ".svc.cluster.local", Port: voyagerSourcePort,
		Username: voyagerSourceUser, Password: v.password, Database: voyagerSourceDB, Schemas: []string{"public"},
	}
	return nil
}

func (v *voyagerRun) create(ctx context.Context) error {
	if v.pod == "" {
		return nil
	}
	conn, err := v.cl.CheckMigrationConnectivity(ctx, v.src)
	if err != nil {
		return fmt.Errorf("connectivity check against the source: %w", err)
	}
	if !conn.Reachable {
		msg := ""
		if conn.Error != nil {
			msg = *conn.Error
		}
		return fmt.Errorf("the controller reports the source %s unreachable: %s", v.src.Host, msg)
	}
	if conn.ServerVersion == nil || *conn.ServerVersion == "" {
		return fmt.Errorf("a reachable source reported no server_version")
	}
	bad := v.src
	bad.Host = "no-such-host-" + v.a.f.suffix() + ".invalid"
	unreach, err := v.cl.CheckMigrationConnectivity(ctx, bad)
	if err != nil {
		return fmt.Errorf("a connectivity check against an unresolvable host must be HTTP 200 with reachable=false, got: %w", err)
	}
	if unreach.Reachable || unreach.Error == nil || *unreach.Error == "" {
		return fmt.Errorf("an unresolvable source reported reachable=%v error=%v", unreach.Reachable, unreach.Error)
	}

	mig, err := v.cl.CreateMigration(ctx, controller.CreateMigrationRequest{Name: "mig-" + v.a.f.suffix(), Source: v.src})
	if err != nil {
		return fmt.Errorf("create migration: %w", err)
	}
	if mig.ID == "" || mig.State != controller.MigrationCreated {
		return fmt.Errorf("createMigration returned id %q state %q, want a fresh id in state created", mig.ID, mig.State)
	}
	v.mig = mig
	return v.noPasswordLeak(ctx)
}

func (v *voyagerRun) assess(ctx context.Context) error {
	if v.mig.ID == "" {
		return nil
	}
	if _, _, err := v.cl.StartMigrationAssessment(ctx, v.mig.ID); err != nil {
		if voyagerNotConfigured(err) {
			v.a.f.t.Skip("this control plane has no Voyager job image configured, so no migration can run: " + err.Error())
			v.mig.ID = ""
			return nil
		}
		return fmt.Errorf("start the assessment: %w", err)
	}
	if err := v.waitState(ctx, controller.MigrationAssessed, controller.MigrationAssessing); err != nil {
		return err
	}
	report, err := v.cl.GetMigrationAssessment(ctx, v.mig.ID)
	if err != nil {
		return fmt.Errorf("read the assessment: %w", err)
	}
	if len(report) == 0 || string(report) == "null" {
		return fmt.Errorf("the assessment reads back empty after the migration reached assessed")
	}
	v.a.f.logf("assessment: %s", truncateFor(report, 400))
	return nil
}

func (v *voyagerRun) provisionTarget(ctx context.Context) error {
	if v.mig.ID == "" {
		return nil
	}
	if _, _, err := v.cl.ProvisionMigrationTarget(ctx, v.mig.ID, controller.ProvisionMigrationTargetRequest{PoolID: v.a.proj.poolID}); err != nil {
		return fmt.Errorf("provision the target: %w", err)
	}
	if err := v.waitState(ctx, controller.MigrationTargetReady, controller.MigrationProvisioningTarget); err != nil {
		return err
	}
	mig, err := v.cl.GetMigration(ctx, v.mig.ID)
	if err != nil {
		return err
	}
	if mig.Target == nil || mig.Target.ClusterID == "" || mig.Target.EndpointID == "" {
		return fmt.Errorf("target_ready but the migration carries no target: %+v", mig.Target)
	}
	if mig.Target.PoolID != v.a.proj.poolID {
		return fmt.Errorf("the target was provisioned in pool %s, want the requested %s", mig.Target.PoolID, v.a.proj.poolID)
	}
	v.mig = mig
	v.a.f.register("delete migration target cluster", func(ctx context.Context) error {
		_, e := v.cl.DeleteClusterAndWait(ctx, mig.Target.PoolID, mig.Target.ClusterID)
		if apiErrorStatus(e) == http.StatusNotFound {
			return nil
		}
		return e
	})
	return v.noPasswordLeak(ctx)
}

func (v *voyagerRun) migrate(ctx context.Context) error {
	if v.mig.ID == "" {
		return nil
	}
	if _, _, err := v.cl.StartMigration(ctx, v.mig.ID); err != nil {
		return fmt.Errorf("start the migration: %w", err)
	}
	return v.waitState(ctx, controller.MigrationCompleted, controller.MigrationMigrating)
}

func (v *voyagerRun) verify(ctx context.Context) error {
	if v.mig.ID == "" {
		return nil
	}
	target := v.mig.Target
	cluster, err := v.cl.GetCluster(ctx, target.PoolID, target.ClusterID)
	if err != nil {
		return fmt.Errorf("read the target cluster: %w", err)
	}
	ep, err := v.cl.GetEndpoint(ctx, target.PoolID, target.ClusterID, cluster.DefaultBranch.BranchID, target.EndpointID)
	if err != nil {
		return fmt.Errorf("read the target endpoint: %w", err)
	}
	c, err := v.a.f.endpointSQL(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect the target endpoint: %w", err)
	}
	defer c.Close()
	n, err := c.QueryInt(ctx, "SELECT count(*) FROM "+voyagerSourceTable)
	if err != nil {
		return fmt.Errorf("count %s on the target: %w", voyagerSourceTable, err)
	}
	if n != voyagerSourceRows {
		return fmt.Errorf("the target holds %d rows of %s, want %d", n, voyagerSourceTable, voyagerSourceRows)
	}
	bad, err := c.QueryInt(ctx, "SELECT count(*) FROM "+voyagerSourceTable+" WHERE v <> id::bigint*7 OR note <> 'row-'||id")
	if err != nil {
		return fmt.Errorf("checksum %s on the target: %w", voyagerSourceTable, err)
	}
	if bad != 0 {
		return fmt.Errorf("%d rows of %s arrived on the target with altered values", bad, voyagerSourceTable)
	}
	listed := false
	for offset := 0; ; offset += 100 {
		list, err := v.cl.ListMigrations(ctx, 100, offset)
		if err != nil {
			return fmt.Errorf("list migrations: %w", err)
		}
		for _, m := range list.Migrations {
			if m.ID == v.mig.ID {
				listed = true
			}
		}
		if listed || list.Count < 100 || offset+len(list.Migrations) >= list.Total {
			break
		}
	}
	if !listed {
		return fmt.Errorf("migration %s is not in GET /migrations", v.mig.ID)
	}
	return v.noPasswordLeak(ctx)
}

func (v *voyagerRun) end(ctx context.Context) error {
	if v.mig.ID == "" {
		return nil
	}
	if _, _, err := v.cl.EndMigration(ctx, v.mig.ID); err != nil {
		return fmt.Errorf("end the migration: %w", err)
	}
	return v.waitState(ctx, controller.MigrationEnded, controller.MigrationEnding)
}

// waitState polls get_migration until want, failing on `failed` or a state
// outside the two the phase may show.
func (v *voyagerRun) waitState(ctx context.Context, want, during string) error {
	var last controller.Migration
	err := v.a.f.t.WaitFor("migration to reach "+want, v.phase, 10*time.Second, func() (bool, error) {
		m, err := v.cl.GetMigration(ctx, v.mig.ID)
		if err != nil {
			return false, nil
		}
		last = m
		switch m.State {
		case want:
			return true, nil
		case controller.MigrationFailed:
			return false, fmt.Errorf("migration %s failed while heading for %s: %s (%s)", m.ID, want, m.Detail, describeRuns(m.Runs))
		case during, v.mig.State:
			return false, nil
		}
		return false, fmt.Errorf("migration %s is in state %q while heading for %s via %s: %s", m.ID, m.State, want, during, m.Detail)
	})
	if err != nil {
		return fmt.Errorf("%w (last: state=%s detail=%q runs=%s)", err, last.State, last.Detail, describeRuns(last.Runs))
	}
	v.mig = last
	return nil
}

// noPasswordLeak reads the migration and the list raw and searches for the
// source password and any password key.
func (v *voyagerRun) noPasswordLeak(ctx context.Context) error {
	for _, path := range []string{"/migrations/" + v.mig.ID, "/migrations"} {
		var raw json.RawMessage
		if _, err := v.cl.GetRaw(ctx, path, nil, &raw); err != nil {
			return fmt.Errorf("GET %s: %w", path, err)
		}
		body := string(raw)
		if strings.Contains(body, v.password) {
			return fmt.Errorf("GET %s returns the source password", path)
		}
		if strings.Contains(body, `"password"`) {
			return fmt.Errorf("GET %s carries a password field: %s", path, truncateFor(raw, 300))
		}
	}
	return nil
}

// voyagerNotConfigured is the deployment saying it cannot launch a Voyager job
// at all, as opposed to a job that ran and failed. Only this is a skip.
func voyagerNotConfigured(err error) bool {
	if s := apiErrorStatus(err); s == http.StatusNotImplemented {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, phrase := range []string{"voyager image", "voyager_image", "not configured", "no job runner", "unsupported"} {
		if strings.Contains(msg, phrase) {
			return true
		}
	}
	return false
}

func describeRuns(runs []controller.MigrationRun) string {
	if len(runs) == 0 {
		return "no runs"
	}
	parts := make([]string, 0, len(runs))
	for _, r := range runs {
		s := fmt.Sprintf("%s#%d=%s", r.Operation, r.Attempt, r.Status)
		if r.ErrorSummary != nil && *r.ErrorSummary != "" {
			s += " (" + *r.ErrorSummary + ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "; ")
}

func truncateFor(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sourcePostgresManifest is a single-container Postgres behind a ClusterIP
// Service, so the controller and the Voyager job resolve it by DNS.
func sourcePostgresManifest(namespace, name, image, password string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
    app: %[1]s
    ampqa-managed-by: functional
spec:
  restartPolicy: Never
  containers:
    - name: postgres
      image: %[3]s
      env:
        - name: POSTGRES_PASSWORD
          value: %[4]q
        - name: POSTGRES_HOST_AUTH_METHOD
          value: md5
      args: ["-c", "wal_level=logical"]
      ports:
        - containerPort: %[5]d
      readinessProbe:
        exec:
          command: ["pg_isready", "-U", "%[6]s"]
        periodSeconds: 3
      resources:
        requests:
          cpu: 250m
          memory: 512Mi
        limits:
          memory: 1Gi
---
apiVersion: v1
kind: Service
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
    ampqa-managed-by: functional
spec:
  selector:
    app: %[1]s
  ports:
    - port: %[5]d
      targetPort: %[5]d
`, name, namespace, image, password, voyagerSourcePort, voyagerSourceUser)
}
