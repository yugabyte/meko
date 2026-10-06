// Endpoint passwords (meko PR #1272) and the Service-DNS client address (PR #1376), checked by psql between compute pods.
package features

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	clientAccessAuthTimeout = 3 * time.Minute
	// A cold node takes up to ~19 minutes to join; past that the replica is not coming up.
	clientAccessReaderBudget = 25 * time.Minute
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_endpoint_client_access",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Endpoint credentials and client address: a writer create without a password, with one under 8 characters, a read_only create carrying one, and a with_writer cluster create without one are refused with 400. A writer answers to exactly the password it was created with (a wrong one and the old well-known cloud_admin are refused), a read_only endpoint answers to its writer's password, a fork's writer answers to the password its parent's catalog holds and not to the one its create carried (the catalog stays the authority so forking cannot mint a new login to the parent's data, meko#1648 and meko#1662), the parent is unchanged, and neither plaintext appears in the endpoint, task or pod/ConfigMap/Secret objects, and the writer stores a SCRAM-SHA-256 verifier with password_encryption scram-sha-256, which appears in no task, task detail or subtask either. GET .../endpoints/{id}/ca tells the truth: with compute TLS off it reports no bundle needed and the compute refuses TLS, with it on the bundle alone passes sslmode=verify-full against verify_name. Each cluster_ip endpoint records its Service DNS as its address, and that address and the password both survive a compute restart and an add_branch_extensions redeploy.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     50 * time.Minute,
		Run:         runAmpEndpointClientAccess,
	})
}

type clientAccessRun struct {
	a      *ampFeatureRun
	cl     *controller.Client
	kube   *k8s.Kubectl
	writer controller.Endpoint
	reader controller.Endpoint
	fork   controller.Endpoint
	pwMain string
	pwFork string
}

func runAmpEndpointClientAccess(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		r := &clientAccessRun{a: a}
		return []phase{
			{"password_validation", r.validation},
			{"writer_password", r.writerPassword},
			{"verifier_is_scram", r.verifierIsSCRAM},
			{"ca_bundle", r.caBundle},
			{"fork_inherits_parent_password", r.forkPassword},
			{"no_plaintext_at_rest", r.noPlaintext},
			{"address_survives_restart", r.survivesRestart},
			{"address_survives_extension_redeploy", r.survivesExtensionRedeploy},
			// Last: a replica never comes up beside a live writer (meko#833).
			{"reader_shares_writer_password", r.readerPassword},
		}
	})
}

func (r *clientAccessRun) validation(ctx context.Context) error {
	cl, err := r.a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	r.cl = cl
	if r.kube = r.a.f.ampKube(); r.kube == nil {
		r.a.f.t.Skip("no kubectl for the amp namespace: a password is only asked for off loopback, which needs a pod to connect from")
		return nil
	}
	sfx := r.a.f.suffix()
	r.pwMain = "Qa'm$" + sfx + "-1"
	r.pwFork = "Qa f " + sfx + "-2"

	proj := r.a.proj
	epPath := "/storage-pools/" + proj.poolID + "/clusters/" + proj.projID + "/branches/" + proj.branchID + "/endpoints"
	cases := []struct {
		what string
		path string
		body map[string]any
	}{
		{"a read_write endpoint with no password", epPath, map[string]any{"role": roleReadWrite}},
		{"a read_write endpoint with a 7-character password", epPath, map[string]any{"role": roleReadWrite, "password": "short12"}},
		{"a read_only endpoint carrying a password", epPath, map[string]any{"role": roleReadOnly, "password": r.pwMain}},
		{"a with_writer cluster with no password", "/storage-pools/" + proj.poolID + "/clusters", map[string]any{"name": "pw-none-" + sfx, "with_writer": true}},
		{"a cluster carrying a password without with_writer", "/storage-pools/" + proj.poolID + "/clusters", map[string]any{"name": "pw-nowriter-" + sfx, "password": r.pwMain}},
	}
	for _, tc := range cases {
		var task controller.Task
		status, hdr, err := cl.PostRaw(ctx, tc.path, tc.body, &task)
		if apiErrorStatus(err) == http.StatusBadRequest {
			continue
		}
		if err == nil {
			if ref := controller.TaskRef(task, hdr.Get("Location")); ref != "" {
				_, _ = cl.WaitTask(ctx, ref)
			}
		}
		return fmt.Errorf("creating %s answered %d (%v), want 400: an endpoint nobody holds the password to, or one silently given a different one, is the failure meko#1272 closed", tc.what, status, err)
	}
	return nil
}

func (r *clientAccessRun) auth(ep controller.Endpoint, password string) (ampstack.EndpointAuth, error) {
	host, port, err := r.serviceAddr(ep)
	if err != nil {
		return ampstack.EndpointAuth{}, err
	}
	return ampstack.EndpointAuth{Host: host, Port: port, User: "cloud_admin", Password: password, Database: "postgres"}, nil
}

// serviceAddr also asserts the recorded address is the Service DNS name, not a pod IP.
func (r *clientAccessRun) serviceAddr(ep controller.Endpoint) (string, int, error) {
	want := ampstack.EndpointWorkloadName(ep.ID) + "." + r.kube.Namespace() + ".svc.cluster.local"
	host, p, err := net.SplitHostPort(strings.TrimSpace(ep.Address))
	if err != nil {
		return "", 0, fmt.Errorf("endpoint %s records address %q, want %s:<port>: %w", ep.ID, ep.Address, want, err)
	}
	if host != want {
		return "", 0, fmt.Errorf("cluster_ip endpoint %s records address %q, want its Service DNS %s: a pod IP changes on every reschedule and silently invalidates the connection string a customer was given (CLOUDGA-36993)", ep.ID, ep.Address, want)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		return "", 0, fmt.Errorf("endpoint %s address %q has a non-numeric port", ep.ID, ep.Address)
	}
	return host, port, nil
}

func (r *clientAccessRun) logsIn(ctx context.Context, from, to controller.Endpoint, password, what string) error {
	a, err := r.auth(to, password)
	if err != nil {
		return err
	}
	if err := ampstack.WaitEndpointAuth(ctx, r.kube, from.ID, a, clientAccessAuthTimeout, r.a.f.logf); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

func (r *clientAccessRun) refused(ctx context.Context, from, to controller.Endpoint, password, what string) error {
	a, err := r.auth(to, password)
	if err != nil {
		return err
	}
	out, err := ampstack.EndpointPsql(ctx, r.kube, from.ID, a, "select 1")
	if err == nil {
		return fmt.Errorf("%s: psql logged in and answered %q, want password authentication to fail", what, out)
	}
	if !strings.Contains(err.Error(), "password authentication failed") {
		return fmt.Errorf("%s: want a password authentication failure, got: %w", what, err)
	}
	return nil
}

func (r *clientAccessRun) refreshed(ctx context.Context, ep controller.Endpoint) (controller.Endpoint, error) {
	got, err := r.cl.GetEndpoint(ctx, r.a.proj.poolID, r.a.proj.projID, ep.BranchID, ep.ID)
	if err != nil {
		return ep, fmt.Errorf("read endpoint %s: %w", ep.ID, err)
	}
	return got, nil
}

func (r *clientAccessRun) writerPassword(ctx context.Context) error {
	ep, err := createEndpointReq(ctx, r.cl, r.a.proj.poolID, r.a.proj.projID, r.a.proj.branchID,
		controller.CreateEndpointRequest{Role: roleReadWrite, Password: r.pwMain})
	if err != nil {
		return fmt.Errorf("create the writer with its own password: %w", err)
	}
	r.writer = ep
	if err := r.logsIn(ctx, ep, ep, r.pwMain, "the writer with the password it was created with"); err != nil {
		return err
	}
	if err := r.refused(ctx, ep, ep, r.pwMain+"x", "the writer with a wrong password"); err != nil {
		return err
	}
	return r.refused(ctx, ep, ep, controller.DefaultEndpointPassword, "the writer with the pre-#1272 well-known cloud_admin password")
}

func (r *clientAccessRun) readerPassword(ctx context.Context) error {
	ep, err := r.createReader(ctx)
	if err != nil {
		return err
	}
	r.reader = ep
	if err := r.logsIn(ctx, ep, ep, r.pwMain, "the read_only endpoint with its writer's password"); err != nil {
		return err
	}
	if err := r.refused(ctx, ep, ep, controller.DefaultEndpointPassword, "the read_only endpoint with the well-known cloud_admin password"); err != nil {
		return err
	}
	return r.logsIn(ctx, ep, r.writer, r.pwMain, "the writer, reached from the reader's pod")
}

func (r *clientAccessRun) forkPassword(ctx context.Context) error {
	name := "pw-fork-" + r.a.f.suffix()
	if _, err := r.cl.CreateBranchAndWait(ctx, r.a.proj.poolID, r.a.proj.projID, controller.CreateBranchRequest{
		Name: name, ParentBranchID: r.a.proj.branchID,
	}); err != nil {
		return fmt.Errorf("fork the writer's branch: %w", err)
	}
	b, err := findBranchCtl(ctx, r.cl, r.a.proj.poolID, r.a.proj.projID, name)
	if err != nil {
		return err
	}
	ep, err := createEndpointReq(ctx, r.cl, r.a.proj.poolID, r.a.proj.projID, b.BranchID,
		controller.CreateEndpointRequest{Role: roleReadWrite, Password: r.pwFork})
	if err != nil {
		return fmt.Errorf("create the fork's writer: %w", err)
	}
	r.fork = ep
	if err := r.logsIn(ctx, r.writer, ep, r.pwMain, "the fork's writer with the parent's password it inherited in the catalog, from the parent writer's pod"); err != nil {
		return err
	}
	if err := r.refused(ctx, r.writer, ep, r.pwFork, "the fork's writer with the password its create carried, which the catalog's existing one outranks"); err != nil {
		return err
	}
	if err := r.logsIn(ctx, ep, r.writer, r.pwMain, "the parent writer with its own password after the fork came up"); err != nil {
		return err
	}
	return r.refused(ctx, ep, r.writer, r.pwFork, "the parent writer with the password the fork's create carried")
}

func (r *clientAccessRun) noPlaintext(ctx context.Context) error {
	secrets := map[string]string{
		"the writer's password":  r.pwMain,
		"the fork's password":    r.pwFork,
		"base64 of the writer's": base64.StdEncoding.EncodeToString([]byte(r.pwMain)),
		"base64 of the fork's":   base64.StdEncoding.EncodeToString([]byte(r.pwFork)),
	}
	scan := func(where, body string) error {
		for what, s := range secrets {
			if !strings.Contains(body, s) {
				continue
			}
			if s == scramVerifierPrefix {
				return fmt.Errorf("%s carries %s: the verifier belongs in pg_authid only and must never reach a task payload", where, what)
			}
			return fmt.Errorf("%s carries %s in the clear: the controller must keep only the pg_authid verifier", where, what)
		}
		return nil
	}
	for _, ep := range []controller.Endpoint{r.writer, r.fork} {
		var raw json.RawMessage
		path := "/storage-pools/" + r.a.proj.poolID + "/clusters/" + r.a.proj.projID + "/branches/" + ep.BranchID + "/endpoints/" + ep.ID
		if _, err := r.cl.GetRaw(ctx, path, nil, &raw); err != nil {
			return fmt.Errorf("GET endpoint %s: %w", ep.ID, err)
		}
		if err := scan("GET endpoint "+ep.ID, string(raw)); err != nil {
			return err
		}
		objs, err := r.kube.GetSelector(ctx, "pod,deployment,replicaset,configmap,secret",
			"app.kubernetes.io/instance="+ampstack.EndpointWorkloadName(ep.ID), "{@}")
		if err != nil {
			return fmt.Errorf("read the Kubernetes objects of endpoint %s: %w", ep.ID, err)
		}
		if err := scan("the pod/deployment/configmap/secret objects of endpoint "+ep.ID, objs); err != nil {
			return err
		}
	}
	secrets["a SCRAM verifier"] = scramVerifierPrefix
	var tasks json.RawMessage
	if _, err := r.cl.GetRaw(ctx, "/tasks", map[string][]string{"cluster_id": {r.a.proj.projID}, "limit": {"100"}}, &tasks); err != nil {
		return fmt.Errorf("list the cluster's tasks: %w", err)
	}
	if err := scan("GET /tasks for the cluster", string(tasks)); err != nil {
		return err
	}
	var list struct {
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(tasks, &list); err != nil {
		return fmt.Errorf("decode GET /tasks: %w", err)
	}
	for _, t := range list.Tasks {
		for _, path := range []string{"/tasks/" + t.ID, "/tasks/" + t.ID + "/subtasks"} {
			var raw json.RawMessage
			if _, err := r.cl.GetRaw(ctx, path, nil, &raw); err != nil {
				return fmt.Errorf("GET %s: %w", path, err)
			}
			if err := scan("GET "+path, string(raw)); err != nil {
				return err
			}
		}
	}
	return nil
}

// caBundle checks GET .../endpoints/{id}/ca against what the compute really
// offers: with compute TLS off it must say no bundle is needed, and with it on
// the bundle alone must be enough for sslmode=verify-full.
func (r *clientAccessRun) caBundle(ctx context.Context) error {
	var ca struct {
		Required   bool   `json:"required"`
		CACrt      string `json:"ca_crt"`
		VerifyName string `json:"verify_name"`
	}
	path := "/storage-pools/" + r.a.proj.poolID + "/clusters/" + r.a.proj.projID + "/branches/" + r.writer.BranchID + "/endpoints/" + r.writer.ID + "/ca"
	if _, err := r.cl.GetRaw(ctx, path, nil, &ca); err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	a, err := r.auth(r.writer, r.pwMain)
	if err != nil {
		return err
	}
	pod, err := ampstack.EndpointPodName(ctx, r.kube, r.writer.ID)
	if err != nil {
		return err
	}
	ex := r.kube.ForPod(pod, endpointPodContainer)
	psql := func(env string) (string, int, error) {
		res, err := ex.Run(ctx, fmt.Sprintf("%s PGPASSWORD=%s PGCONNECT_TIMEOUT=10 timeout 30 psql -h %s -p %d -U %s -d %s -tAc 'select ssl from pg_stat_ssl where pid = pg_backend_pid()' 2>&1",
			env, shellQuoteArg(a.Password), shellQuoteArg(a.Host), a.Port, a.User, a.Database))
		if err != nil {
			return "", 0, fmt.Errorf("exec psql in %s: %w", pod, err)
		}
		return strings.TrimSpace(res.Stdout), res.ExitCode, nil
	}
	if !ca.Required {
		if ca.CACrt != "" {
			return fmt.Errorf("GET %s says required=false but returns a %d-byte ca_crt", path, len(ca.CACrt))
		}
		out, code, err := psql("PGSSLMODE=require")
		if err != nil {
			return err
		}
		if code == 0 && out == "t" {
			r.a.f.logf("endpoint %s answers TLS with a publicly trusted certificate, so no bundle is needed", r.writer.ID)
			return nil
		}
		if !strings.Contains(out, "does not support SSL") {
			return fmt.Errorf("GET %s says no bundle is needed, but sslmode=require to the writer neither negotiated TLS nor was refused for lack of it: %s", path, out)
		}
		return nil
	}
	if !strings.Contains(ca.CACrt, "BEGIN CERTIFICATE") {
		return fmt.Errorf("GET %s says required=true but ca_crt is not a PEM certificate: %q", path, ca.CACrt)
	}
	if ca.VerifyName == "" {
		return fmt.Errorf("GET %s says required=true but names no verify_name to connect to", path)
	}
	bundle := "/tmp/ampqa-ca-" + r.a.f.suffix() + ".crt"
	if res, err := ex.RunStdin(ctx, strings.NewReader(ca.CACrt), "cat > "+bundle); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("write the CA bundle into pod %s: %v (%+v)", pod, err, res)
	}
	a.Host = ca.VerifyName
	out, code, err := psql("PGSSLMODE=verify-full PGSSLROOTCERT=" + bundle)
	if err != nil {
		return err
	}
	if code != 0 || out != "t" {
		return fmt.Errorf("sslmode=verify-full to %s with only the bundle GET %s returned failed (exit %d): %s", ca.VerifyName, path, code, out)
	}
	return nil
}

const scramVerifierPrefix = "SCRAM-SHA-256$"

func shellQuoteArg(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

func (r *clientAccessRun) verifierIsSCRAM(ctx context.Context) error {
	a, err := r.auth(r.writer, r.pwMain)
	if err != nil {
		return err
	}
	out, err := ampstack.EndpointPsql(ctx, r.kube, r.writer.ID, a,
		"select split_part(rolpassword, '$', 1) || ' ' || current_setting('password_encryption') from pg_authid where rolname = current_user")
	if err != nil {
		return fmt.Errorf("read the writer's stored verifier: %w", err)
	}
	if got := strings.TrimSpace(out); got != "SCRAM-SHA-256 scram-sha-256" {
		return fmt.Errorf("the writer stores its password as %q (verifier scheme, then password_encryption), want \"SCRAM-SHA-256 scram-sha-256\": an md5 verifier is a replayable hash of the password", got)
	}
	return nil
}

func (r *clientAccessRun) survivesRestart(ctx context.Context) error {
	before := r.writer.Address
	// No reconnect through restartEndpointCompute: off loopback it would log in with the harness password.
	roller := &kubeComputeRoller{kube: r.kube, timeout: restartReadyTimeout}
	pod, err := roller.podName(ctx, r.writer.ID)
	if err != nil {
		return err
	}
	if err := roller.deletePod(ctx, pod); err != nil {
		return fmt.Errorf("delete the writer's compute pod %s: %w", pod, err)
	}
	if err := roller.waitReady(ctx, r.writer.ID); err != nil {
		return fmt.Errorf("the writer's compute did not come back after the restart: %w", err)
	}
	got, err := r.refreshed(ctx, r.writer)
	if err != nil {
		return err
	}
	if got.Address != before {
		return fmt.Errorf("the writer's address moved from %q to %q across a compute restart: a customer's connection string must survive a reschedule", before, got.Address)
	}
	r.writer = got
	if err := r.logsIn(ctx, r.fork, r.writer, r.pwMain, "the restarted writer with its password, through the unchanged address"); err != nil {
		return err
	}
	return r.refused(ctx, r.fork, r.writer, controller.DefaultEndpointPassword, "the restarted writer with the well-known cloud_admin password")
}

func (r *clientAccessRun) survivesExtensionRedeploy(ctx context.Context) error {
	before := r.writer.Address
	if _, err := r.cl.AddBranchExtensionsAndWait(ctx, r.a.proj.poolID, r.a.proj.projID, r.a.proj.branchID,
		controller.AddBranchExtensionsRequest{Extensions: []string{"pgvector"}}); err != nil {
		if extensionsUnavailable(err) {
			r.a.f.logf("no extension catalog on this control plane, so the redeploy half is not run: %v", err)
			return nil
		}
		return fmt.Errorf("add pgvector to the writer's branch (redeploys its endpoints): %w", err)
	}
	got, err := r.refreshed(ctx, r.writer)
	if err != nil {
		return err
	}
	if got.Address != before {
		return fmt.Errorf("the writer moved from %q to %q when add_branch_extensions redeployed it: this is the CLOUDGA-36993 release blocker", before, got.Address)
	}
	r.writer = got
	if err := r.logsIn(ctx, r.fork, r.writer, r.pwMain, "the redeployed writer with its password"); err != nil {
		if a, aerr := r.auth(r.writer, controller.DefaultEndpointPassword); aerr == nil {
			if _, perr := ampstack.EndpointPsql(ctx, r.kube, r.fork.ID, a, "select 1"); perr == nil {
				return fmt.Errorf("after add_branch_extensions redeployed writer %s it refuses the password it was created with and accepts the well-known cloud_admin/cloud_admin: the redeploy renders no COMPUTE_PASSWORD and the supervisor re-applies its default: %w", r.writer.ID, err)
			}
		}
		return err
	}
	return r.refused(ctx, r.writer, r.writer, controller.DefaultEndpointPassword, "the redeployed writer with the well-known cloud_admin password")
}

func (r *clientAccessRun) createReader(ctx context.Context) (controller.Endpoint, error) {
	proj := r.a.proj
	task, loc, err := r.cl.CreateEndpoint(ctx, proj.poolID, proj.projID, proj.branchID, controller.CreateEndpointRequest{Role: roleReadOnly})
	if err != nil {
		return controller.Endpoint{}, fmt.Errorf("create a read_only endpoint on the writer's branch: %w", err)
	}
	ref := controller.TaskRef(task, loc)
	start := time.Now()
	wctx, cancel := context.WithTimeout(ctx, clientAccessReaderBudget)
	defer cancel()
	stuck := make(chan string, 1)
	go r.watchTokenWait(wctx, stuck)
	var done controller.Task
	var werr error
	waited := make(chan struct{})
	go func() { done, werr = r.cl.WaitTask(wctx, ref); close(waited) }()
	select {
	case <-waited:
	case pod := <-stuck:
		cancel()
		<-waited
		return controller.Endpoint{}, fmt.Errorf("read_only create %s never came up: its supervisor in %s is waiting for the branch write token its writer %s holds, so Postgres never starts (meko#833); %s",
			ref, pod, r.writer.ID, r.readerDiagnosis(ctx))
	}
	if werr == nil {
		eps, err := r.cl.ListAllEndpoints(ctx, proj.poolID, proj.projID, proj.branchID)
		if err != nil {
			return controller.Endpoint{}, fmt.Errorf("list the writer's branch endpoints: %w", err)
		}
		for _, e := range eps {
			if e.Role == roleReadOnly {
				return e, nil
			}
		}
		return controller.Endpoint{}, fmt.Errorf("read_only create %s succeeded but branch %s lists no read_only endpoint", ref, proj.branchID)
	}
	if ctx.Err() != nil {
		return controller.Endpoint{}, ctx.Err()
	}
	return controller.Endpoint{}, fmt.Errorf("read_only create %s on a branch with a live writer ended %q after %s (%v); %s",
		ref, done.Status, time.Since(start).Round(time.Second), werr, r.readerDiagnosis(ctx))
}

// watchTokenWait reports a replica pod still waiting on the write token after readerTokenGrace.
func (r *clientAccessRun) watchTokenWait(ctx context.Context, stuck chan<- string) {
	const readerTokenGrace = 5 * time.Minute
	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
		if time.Since(start) < readerTokenGrace {
			continue
		}
		eps, err := r.cl.ListAllEndpoints(ctx, r.a.proj.poolID, r.a.proj.projID, r.a.proj.branchID)
		if err != nil {
			continue
		}
		for _, e := range eps {
			if e.Role != roleReadOnly {
				continue
			}
			pod, err := ampstack.EndpointPodName(ctx, r.kube, e.ID)
			if err != nil {
				continue
			}
			logs, _ := r.kube.Logs(ctx, "pod/"+pod, "endpoint", 200)
			if strings.Contains(logs, "write token is held elsewhere") && !strings.Contains(logs, "acquired") {
				stuck <- pod
				return
			}
		}
	}
}

func (r *clientAccessRun) readerDiagnosis(ctx context.Context) string {
	eps, err := r.cl.ListAllEndpoints(ctx, r.a.proj.poolID, r.a.proj.projID, r.a.proj.branchID)
	if err != nil {
		return "endpoints unreadable: " + err.Error()
	}
	var out []string
	for _, e := range eps {
		if e.Role != roleReadOnly {
			continue
		}
		pod, err := ampstack.EndpointPodName(ctx, r.kube, e.ID)
		if err != nil {
			out = append(out, fmt.Sprintf("endpoint %s has no pod (%v)", e.ID, err))
			continue
		}
		phase, _ := r.kube.Get(ctx, "pod", pod, "{.status.phase} ready={.status.containerStatuses[*].ready} restarts={.status.containerStatuses[*].restartCount}")
		logs, _ := r.kube.Logs(ctx, "pod/"+pod, "endpoint", 8)
		out = append(out, fmt.Sprintf("endpoint %s pod %s %s; %s; last log lines: %s", e.ID, pod, phase, r.kube.PodTrouble(ctx, pod), stripANSI(strings.TrimSpace(logs))))
	}
	if len(out) == 0 {
		return "no read_only endpoint row exists"
	}
	return strings.Join(out, " | ")
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }
