// The endpoint user split (the endpoint chart's userSplit, set fleet-wide by the
// controller's --deploy-endpoint-user-split): the supervisor runs the pageserver
// and safekeeper as the storage user and compute_ctl and Postgres as the compute
// user, so a compromised Postgres holds no YugabyteDB credential and cannot drive
// the storage daemons.
//
// The split is toggled on this test's own endpoint by running helm in the
// controller pod against the endpoint's release, with the chart the controller
// itself deploys and --reuse-values. Flipping the controller's flag instead would
// change every endpoint created on the shared control plane while other tests
// run, and the framework has no way to re-render a running shared controller.
// The endpoint is the test's, so nothing needs restoring: the cluster-delete
// cascade removes it. A control plane already deploying split endpoints is
// detected, and then the endpoint has to come up split without the toggle.
package features

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	// The chart's userSplit defaults, which the endpoint image's users match.
	splitStorageUID = 1000
	splitComputeUID = 1001

	splitTable      = "amp_user_split"
	splitProbeTable = "amp_user_split_probe"
	splitRows       = 200
	splitDeployWait = 10 * time.Minute

	splitControllerFlag = "AMP_DEPLOY_ENDPOINT_USER_SPLIT"
	lfcStateDefaultPort = 9896
	// A well-formed endpoint id that is not this endpoint's.
	splitForeignEndpoint = "00000000-0000-4000-8000-0000000051a7"
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_endpoint_user_split",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamCompute,
		Description: "The endpoint user split (chart userSplit, controller --deploy-endpoint-user-split). An endpoint switched to split users serves its rows, runs pageserver and safekeeper as uid 1000 and compute_ctl and Postgres as uid 1001. Postgres as superuser, through COPY FROM PROGRAM, runs as uid 1001 outside the storage group, cannot read the YugabyteDB client key, the pageserver's config or its environment, and has no YB_*, COMPUTE_PASSWORD or YB password variable or value in its own; pg_read_binary_file on the key is refused. From the compute user, POST /v1/write_token and PUT /v1/failpoints on the pageserver and the safekeeper get 403 while GET /v1/status answers, and a PUT of another endpoint's LFC state to the supervisor's LFC state service gets 403. LFC offload and autoprewarm still work through that service across a compute restart. Switched back, the endpoint runs on uid 1000 again and serves every row written under either mode.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     45 * time.Minute,
		Run:         runAmpEndpointUserSplit,
	})
}

func runAmpEndpointUserSplit(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		r := &splitRun{a: a}
		return []phase{
			{"endpoint_and_controller_flag_agree", r.seed},
			{"switch_to_split_users", r.splitOn},
			{"compute_holds_no_storage_credential", r.noCredential},
			{"compute_cannot_drive_storage", r.noControl},
			{"lfc_prewarm_through_the_state_service", r.lfcThroughProxy},
			{"switch_back_to_one_user", r.splitOff},
		}
	})
}

type splitRun struct {
	a    *ampFeatureRun
	kube *k8s.Kubectl
	ep   controller.Endpoint
	rows int
	// Read as root in the endpoint container, so the compute-side checks know
	// what they are looking for and that it exists.
	store storageSecrets
}

type storageSecrets struct {
	keyPath     string
	passwordEnv string
	password    string
	psPID       string
	psDir       string
	psPort      string
	skPort      string
	lfcPort     string
}

func (r *splitRun) release() string { return ampstack.EndpointWorkloadName(r.ep.ID) }

func (r *splitRun) seed(ctx context.Context) error {
	r.kube = r.a.f.ampKube()
	if r.kube == nil {
		r.a.f.t.Skip("no kube handle for the amp namespace: the split is toggled through helm in the controller pod and checked inside the endpoint pod")
		return nil
	}
	cl, err := r.a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	if r.ep, err = createEndpoint(ctx, cl, r.a.proj.poolID, r.a.proj.projID, r.a.proj.branchID); err != nil {
		return err
	}
	if err := r.writeRows(ctx, "the freshly created endpoint"); err != nil {
		return err
	}

	acfg := ampstack.FromConfig(r.a.f.t.Options)
	flag, err := r.kube.Get(ctx, "deployment", acfg.ControllerName,
		fmt.Sprintf(`{.spec.template.spec.containers[?(@.name==%q)].env[?(@.name==%q)].value}`, acfg.ControllerContainer, splitControllerFlag))
	if err != nil {
		return fmt.Errorf("read %s on the controller Deployment %s: %w", splitControllerFlag, acfg.ControllerName, err)
	}
	split, err := r.deployedSplit(ctx)
	if err != nil {
		return err
	}
	if want := strings.TrimSpace(flag) == "true"; split != want {
		return fmt.Errorf("the controller runs with %s=%q but deployed endpoint %s with userSplit %t", splitControllerFlag, strings.TrimSpace(flag), r.ep.ID, split)
	}
	r.a.f.logf("amp_endpoint_user_split: controller %s=%q, endpoint %s created with userSplit %t", splitControllerFlag, strings.TrimSpace(flag), r.ep.ID, split)
	return r.assertUIDs(ctx, split)
}

// deployedSplit reads the rendered Deployment: only the split runs the
// container as root.
func (r *splitRun) deployedSplit(ctx context.Context) (bool, error) {
	u, err := r.kube.Get(ctx, "deployment", r.release(),
		fmt.Sprintf(`{.spec.template.spec.containers[?(@.name==%q)].securityContext.runAsUser}`, endpointPodContainer))
	if err != nil {
		return false, fmt.Errorf("read the endpoint container's runAsUser on Deployment %s: %w", r.release(), err)
	}
	return strings.TrimSpace(u) == "0", nil
}

func (r *splitRun) setSplit(ctx context.Context, on bool) error {
	acfg := ampstack.FromConfig(r.a.f.t.Options)
	ctl := r.kube.ForPod("deployment/"+acfg.ControllerName, acfg.ControllerContainer)
	cmd := fmt.Sprintf(`helm upgrade %s "${AMP_DEPLOY_CHART:?}" --namespace %s --reuse-values --set userSplit.enabled=%t --wait --timeout %s 2>&1`,
		r.release(), r.kube.Namespace(), on, splitDeployWait)
	r.a.f.logf("amp_endpoint_user_split: in the controller pod: %s", cmd)
	res, err := ctl.Run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("exec helm in the controller pod: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("helm upgrade of %s with userSplit.enabled=%t exited %d: %s", r.release(), on, res.ExitCode, strings.TrimSpace(res.Stdout+res.Stderr))
	}
	roller := &kubeComputeRoller{kube: r.kube, timeout: splitDeployWait}
	if err := roller.waitReady(ctx, r.ep.ID); err != nil {
		return fmt.Errorf("endpoint %s is not ready after switching userSplit.enabled=%t: %w", r.ep.ID, on, err)
	}
	got, err := r.deployedSplit(ctx)
	if err != nil {
		return err
	}
	if got != on {
		if on {
			r.a.f.t.Skip("the endpoint chart this controller deploys has no userSplit value: setting it rendered no root container")
			return nil
		}
		return fmt.Errorf("after helm set userSplit.enabled=false the endpoint container still runs as root")
	}
	return nil
}

func (r *splitRun) splitOn(ctx context.Context) error {
	split, err := r.deployedSplit(ctx)
	if err != nil {
		return err
	}
	if !split {
		if err := r.setSplit(ctx, true); err != nil {
			return err
		}
	}
	if err := r.assertUIDs(ctx, true); err != nil {
		return err
	}
	return r.writeRows(ctx, "the endpoint switched to split users")
}

func (r *splitRun) splitOff(ctx context.Context) error {
	if err := r.setSplit(ctx, false); err != nil {
		return err
	}
	if err := r.assertUIDs(ctx, false); err != nil {
		return err
	}
	c, err := r.a.f.endpointSQL(ctx, r.ep)
	if err != nil {
		return fmt.Errorf("connect the endpoint switched back to one user: %w", err)
	}
	defer c.Close()
	p, err := computeShell(ctx, c, "id -u")
	if err != nil {
		return err
	}
	if strings.TrimSpace(p) != strconv.Itoa(splitStorageUID) {
		return fmt.Errorf("Postgres runs as uid %q after the split was switched off, want %d: the endpoint is not back on one user", strings.TrimSpace(p), splitStorageUID)
	}
	return r.writeRows(ctx, "the endpoint switched back to one user")
}

// writeRows checks every row written so far and adds a batch, so each mode
// reads what the previous one wrote.
func (r *splitRun) writeRows(ctx context.Context, where string) error {
	c, err := r.a.f.endpointSQL(ctx, r.ep)
	if err != nil {
		return fmt.Errorf("connect %s: %w", where, err)
	}
	defer c.Close()
	if r.rows == 0 {
		if _, err := c.Exec(ctx, "CREATE TABLE "+splitTable+" (id int PRIMARY KEY, v text NOT NULL)"); err != nil {
			return fmt.Errorf("create %s on %s: %w", splitTable, where, err)
		}
	} else if err := r.checkRows(ctx, c, where); err != nil {
		return err
	}
	if _, err := c.Exec(ctx, "INSERT INTO "+splitTable+" SELECT g, md5(g::text) FROM generate_series($1::int + 1, $1::int + $2::int) g", r.rows, splitRows); err != nil {
		return fmt.Errorf("write to %s: %w", where, err)
	}
	r.rows += splitRows
	return r.checkRows(ctx, c, where)
}

func (r *splitRun) checkRows(ctx context.Context, c sqlConn, where string) error {
	n, err := c.QueryInt(ctx, "SELECT count(*) FROM "+splitTable+" WHERE v = md5(id::text)")
	if err != nil {
		return fmt.Errorf("read %s on %s: %w", splitTable, where, err)
	}
	if n != int64(r.rows) {
		return fmt.Errorf("%s serves %d intact rows of %s, want %d", where, n, splitTable, r.rows)
	}
	return nil
}

func (r *splitRun) endpointRoot(ctx context.Context) (*k8s.Kubectl, error) {
	pod, err := (&kubeComputeRoller{kube: r.kube}).podName(ctx, r.ep.ID)
	if err != nil {
		return nil, err
	}
	return r.kube.ForPod(pod, endpointPodContainer), nil
}

// runIn runs script with bash, which /dev/tcp needs, in the endpoint container.
func runIn(ctx context.Context, k *k8s.Kubectl, what, script string) (string, error) {
	res, err := k.Run(ctx, "echo "+base64.StdEncoding.EncodeToString([]byte(script))+" | base64 -d | bash")
	if err != nil {
		return "", fmt.Errorf("%s: %w", what, err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("%s exited %d: %s", what, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

const daemonUIDsScript = `for p in /proc/[0-9]*; do
  c=$(cat "$p/comm" 2>/dev/null) || continue
  case "$c" in pageserver|safekeeper|compute_ctl|postgres)
    echo "$c $(sed -n 's/^Uid:[[:space:]]*\([0-9]*\).*/\1/p' "$p/status" 2>/dev/null)";;
  esac
done; exit 0`

// assertUIDs requires each daemon to be running and every process of it on the
// expected uid. Polled because the compute starts after the pod reports ready.
func (r *splitRun) assertUIDs(ctx context.Context, split bool) error {
	want := map[string]int{"pageserver": splitStorageUID, "safekeeper": splitStorageUID, "compute_ctl": splitStorageUID, "postgres": splitStorageUID}
	if split {
		want["compute_ctl"], want["postgres"] = splitComputeUID, splitComputeUID
	}
	var got map[string][]int
	err := pollUntil(ctx, 2*time.Minute, 3*time.Second, func() (bool, error) {
		k, err := r.endpointRoot(ctx)
		if err != nil {
			return false, err
		}
		out, err := runIn(ctx, k, "list the endpoint's daemons", daemonUIDsScript)
		if err != nil {
			return false, err
		}
		got = map[string][]int{}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			comm, uid, ok := strings.Cut(strings.TrimSpace(line), " ")
			if n, err := strconv.Atoi(uid); ok && err == nil {
				got[comm] = append(got[comm], n)
			}
		}
		for comm := range want {
			if len(got[comm]) == 0 {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("endpoint %s is not running all of pageserver, safekeeper, compute_ctl and postgres (saw %v): %w", r.ep.ID, got, err)
	}
	for comm, uid := range want {
		if slices.ContainsFunc(got[comm], func(u int) bool { return u != uid }) {
			return fmt.Errorf("with userSplit %t, %s runs as uid %v, want only %d", split, comm, got[comm], uid)
		}
	}
	return nil
}

func (r *splitRun) readStorageSecrets(ctx context.Context) error {
	k, err := r.endpointRoot(ctx)
	if err != nil {
		return err
	}
	out, err := runIn(ctx, k, "read the storage settings as root", `
pw="${YB_PASSWORD_ENV:-YB_PASSWORD}"
printf 'key=%s\n' "${YB_SSLKEY:-}"
printf 'pwenv=%s\n' "$pw"
for p in /proc/[0-9]*; do [ "$(cat "$p/comm" 2>/dev/null)" = pageserver ] && { printf 'pspid=%s\n' "${p#/proc/}"; break; }; done
printf 'psdir=%s/pageserver\n' "${RUN_DIR:-/data}"
printf 'psport=%s\n' "${PAGESERVER_HTTP_PORT:-9898}"
printf 'skport=%s\n' "${SAFEKEEPER_HTTP_PORT:-7676}"
printf 'lfcport=%s\n' "${LFC_STATE_PORT:-`+strconv.Itoa(lfcStateDefaultPort)+`}"
exit 0`)
	if err != nil {
		return err
	}
	kv := parseKV(out)
	s := storageSecrets{keyPath: kv["key"], passwordEnv: kv["pwenv"], psPID: kv["pspid"], psDir: kv["psdir"], psPort: kv["psport"], skPort: kv["skport"], lfcPort: kv["lfcport"]}
	if s.psPID == "" || s.psDir == "" {
		return fmt.Errorf("found no pageserver process in endpoint %s's pod", r.ep.ID)
	}
	// Kept out of every log line and error.
	pw, err := runIn(ctx, k, "read the YB password variable as root", `printenv "${YB_PASSWORD_ENV:-YB_PASSWORD}"; exit 0`)
	if err != nil {
		return err
	}
	s.password = strings.TrimSpace(pw)
	// Their existence makes a refusal below a permission, not a missing file.
	files := []string{s.psDir + "/pageserver.toml"}
	if s.keyPath != "" {
		files = append(files, s.keyPath)
	}
	for _, f := range files {
		if _, err := runIn(ctx, k, "stat "+f+" as root", "test -s "+shellQuote(f)); err != nil {
			return fmt.Errorf("%s is not a non-empty file in the endpoint container, so a refusal to the compute would prove nothing: %w", f, err)
		}
	}
	r.store = s
	return nil
}

func (r *splitRun) noCredential(ctx context.Context) error {
	if err := r.readStorageSecrets(ctx); err != nil {
		return err
	}
	s := r.store
	if s.keyPath == "" {
		r.a.f.logf("amp_endpoint_user_split: this backing gives endpoints no YB client key (YB_SSLKEY unset), so only the password, config and environment are checked")
	}
	if s.password == "" {
		r.a.f.logf("amp_endpoint_user_split: %s is empty in the endpoint container, so there is no password value to look for", s.passwordEnv)
	}

	c, err := r.a.f.endpointSQL(ctx, r.ep)
	if err != nil {
		return err
	}
	defer c.Close()
	super, err := c.QueryString(ctx, "SELECT rolsuper::text FROM pg_roles WHERE rolname = current_user")
	if err != nil {
		return fmt.Errorf("read whether the test's role is a superuser: %w", err)
	}
	if super != "true" {
		return fmt.Errorf("the test's endpoint role is not a superuser, so it cannot stand in for a compromised Postgres")
	}

	script := fmt.Sprintf(`echo "uid=$(id -u)"
echo "groups=$(id -G)"
cat /proc/%s/environ >/dev/null 2>&1; echo "ps_environ_rc=$?"
cat %s >/dev/null 2>&1; echo "ps_toml_rc=$?"
`, s.psPID, shellQuote(s.psDir+"/pageserver.toml"))
	if s.keyPath != "" {
		script += fmt.Sprintf("cat %s >/dev/null 2>&1; echo \"key_rc=$?\"\n", shellQuote(s.keyPath))
	}
	script += `echo "env=$(env | base64 -w0)"`
	out, err := computeShell(ctx, c, script)
	if err != nil {
		return err
	}
	kv := parseKV(out)

	if kv["uid"] != strconv.Itoa(splitComputeUID) {
		return fmt.Errorf("Postgres's COPY FROM PROGRAM runs as uid %q, want the compute user %d", kv["uid"], splitComputeUID)
	}
	if slices.Contains(strings.Fields(kv["groups"]), strconv.Itoa(splitStorageUID)) {
		return fmt.Errorf("Postgres runs with groups %q, which include the storage group %d that can read the YB client key", kv["groups"], splitStorageUID)
	}
	for _, f := range []struct{ key, what string }{
		{"ps_environ_rc", "the pageserver's environment (/proc/" + s.psPID + "/environ), which holds the YB connection"},
		{"ps_toml_rc", "pageserver.toml, which holds the YB connection and the control secret"},
		{"key_rc", "the YB client key " + s.keyPath},
	} {
		rc, ok := kv[f.key]
		if !ok && (f.key != "key_rc" || s.keyPath != "") {
			return fmt.Errorf("the compute-side probe printed no %s: %q", f.key, out)
		}
		if ok && rc == "0" {
			return fmt.Errorf("Postgres as uid %d read %s", splitComputeUID, f.what)
		}
	}

	envRaw, err := base64.StdEncoding.DecodeString(kv["env"])
	if err != nil {
		return fmt.Errorf("decode the compute's environment: %w", err)
	}
	var leaked []string
	for _, line := range strings.Split(string(envRaw), "\n") {
		name, _, _ := strings.Cut(line, "=")
		if strings.HasPrefix(name, "YB_") || name == "COMPUTE_PASSWORD" || name == s.passwordEnv {
			leaked = append(leaked, name)
		}
	}
	if len(leaked) > 0 {
		return fmt.Errorf("Postgres's environment carries %v, which the supervisor strips from the compute", leaked)
	}
	if s.password != "" && strings.Contains(string(envRaw), s.password) {
		return fmt.Errorf("the value of %s appears in Postgres's environment under another name", s.passwordEnv)
	}

	if s.keyPath != "" {
		_, err := c.QueryString(ctx, "SELECT length(pg_read_binary_file($1))::text", s.keyPath)
		if err == nil {
			return fmt.Errorf("pg_read_binary_file read the YB client key %s as superuser", s.keyPath)
		}
		if !strings.Contains(err.Error(), "ermission denied") {
			return fmt.Errorf("pg_read_binary_file on the YB client key failed for a reason other than permissions, so it does not show the key is protected: %w", err)
		}
	}
	return nil
}

func (r *splitRun) noControl(ctx context.Context) error {
	s := r.store
	c, err := r.a.f.endpointSQL(ctx, r.ep)
	if err != nil {
		return err
	}
	defer c.Close()
	type req struct {
		name, port, method, path string
		want                     int
	}
	reqs := []req{
		{"ps_status", s.psPort, "GET", "/v1/status", 200},
		{"ps_write_token", s.psPort, "POST", "/v1/write_token", 403},
		{"ps_failpoints", s.psPort, "PUT", "/v1/failpoints", 403},
		{"sk_status", s.skPort, "GET", "/v1/status", 200},
		{"sk_write_token", s.skPort, "POST", "/v1/write_token", 403},
		{"sk_failpoints", s.skPort, "PUT", "/v1/failpoints", 403},
		{"lfc_foreign_write", s.lfcPort, "PUT", "/v1/lfc_state/" + splitForeignEndpoint, 403},
	}
	script := `st() {
  exec 3<>"/dev/tcp/127.0.0.1/$2" 2>/dev/null || { echo "$1=refused"; return; }
  printf '%s %s HTTP/1.0\r\nHost: 127.0.0.1\r\nContent-Length: 0\r\n\r\n' "$3" "$4" >&3
  read -r line <&3
  set -- "$1" $line
  echo "$1=${3%$'\r'}"
  exec 3>&-
}
`
	for _, q := range reqs {
		script += fmt.Sprintf("st %s %s %s %s\n", q.name, q.port, q.method, q.path)
	}
	out, err := computeShell(ctx, c, script)
	if err != nil {
		return err
	}
	kv := parseKV(out)
	for _, q := range reqs {
		if got := kv[q.name]; got != strconv.Itoa(q.want) {
			return fmt.Errorf("%s %s on 127.0.0.1:%s from Postgres as uid %d answered %q, want %d (all: %v)", q.method, q.path, q.port, splitComputeUID, got, q.want, kv)
		}
	}
	return nil
}

func (r *splitRun) lfcThroughProxy(ctx context.Context) error {
	sum, pages, offloading, err := r.a.lfcSeedAndOffload(ctx, r.ep)
	if err != nil {
		return err
	}
	if !offloading {
		r.a.f.logf("amp_endpoint_user_split: LFC phase not run: %s", lfcOffloadOffReason)
		return nil
	}
	k, err := r.endpointRoot(ctx)
	if err != nil {
		return err
	}
	status, err := runIn(ctx, k, "read this endpoint's LFC state from the state service", fmt.Sprintf(`exec 3<>/dev/tcp/127.0.0.1/%s || exit 7
printf 'GET /v1/lfc_state/%s HTTP/1.0\r\nHost: 127.0.0.1\r\n\r\n' >&3
read -r line <&3; set -- $line; echo "${2%%$'\r'}"`, r.store.lfcPort, r.ep.ID))
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "200" {
		return fmt.Errorf("after an offload the LFC state service answers %q for endpoint %s's state, want 200: the offload did not go through it", strings.TrimSpace(status), r.ep.ID)
	}
	if err := r.a.lfcPrewarmAfterRestart(ctx, r.ep, sum, pages); err != nil {
		return err
	}
	return r.assertUIDs(ctx, true)
}

// computeShell runs script with bash as Postgres's own OS user, through COPY
// FROM PROGRAM, and returns its combined output. Both directions go through
// base64: the script needs no quoting, and COPY's text format would otherwise
// unescape backslashes in the output.
func computeShell(ctx context.Context, c sqlConn, script string) (string, error) {
	prog := "echo " + base64.StdEncoding.EncodeToString([]byte(script)) + " | base64 -d | bash 2>&1 | base64 -w0; echo"
	for _, stmt := range []string{
		"CREATE TABLE IF NOT EXISTS " + splitProbeTable + " (id bigserial PRIMARY KEY, line text)",
		"TRUNCATE " + splitProbeTable,
		"COPY " + splitProbeTable + " (line) FROM PROGRAM '" + prog + "'",
	} {
		if _, err := c.Exec(ctx, stmt); err != nil {
			return "", fmt.Errorf("run a shell as Postgres through COPY FROM PROGRAM: %w", err)
		}
	}
	enc, err := c.QueryString(ctx, "SELECT coalesce(string_agg(line, '' ORDER BY id), '') FROM "+splitProbeTable)
	if err != nil {
		return "", fmt.Errorf("read the shell's output: %w", err)
	}
	out, err := base64.StdEncoding.DecodeString(strings.TrimSpace(enc))
	if err != nil {
		return "", fmt.Errorf("decode the shell's output: %w", err)
	}
	return string(out), nil
}

func parseKV(s string) map[string]string {
	kv := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			kv[k] = v
		}
	}
	return kv
}
