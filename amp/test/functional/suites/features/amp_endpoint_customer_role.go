// The endpoint customer role (AMP-15; the endpoint chart's endpoint.customerRole,
// set fleet-wide by the controller's --deploy-endpoint-customer-role): clients log
// in as a non-superuser member of compute_ctl's privileged role instead of
// cloud_admin, which keeps no password and is reachable over loopback only.
//
// The role is switched on this test's own endpoint by running helm in the
// controller pod against the endpoint's release, as the user-split test does,
// because flipping the controller's flag would change every endpoint on the
// shared control plane while other tests run. Each switch also clears
// endpoint.password: the controller's own re-renders never carry the create-time
// password, so this is the render a real fleet roll produces, and the one that
// makes the supervisor hand cloud_admin's password over rather than re-apply it.
//
// Logins are checked from the endpoint's pod to its Service address, off
// loopback, where pg_hba asks for the password. The admin-side reads go through
// the harness's port-forward, which arrives over loopback trust as cloud_admin
// and so keeps working after cloud_admin loses its password. A control plane
// already deploying a customer role is detected; the endpoint then has to come up
// converted, and the switching phases are not run.
package features

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	custControllerFlag = "AMP_DEPLOY_ENDPOINT_CUSTOMER_ROLE"
	// The customer role this test switches its endpoint to, and one a customer
	// made themselves under a name the setting then points at.
	custRole      = "ampqa_customer"
	custTakenRole = "ampqa_taken"
	// compute_ctl's default --privileged-role-name, the group the role joins.
	custGroupRole  = "yb_amp_superuser"
	custTable      = "amp_customer_role"
	custRows       = 100
	custDeployWait = 10 * time.Minute
	// The supervisor retries for a minute before it gives up; past that the
	// answer is not coming.
	custRemoteAuthWait = 3 * time.Minute
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_endpoint_customer_role",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamCompute,
		Description: "The endpoint customer role (AMP-15; chart endpoint.customerRole, controller --deploy-endpoint-customer-role). Off, a writer's clients log in as cloud_admin with the password it was created with, cloud_admin is a superuser and no customer role exists. Pointed at a role the customer created themselves, the supervisor reports remote-auth failed and changes nothing: the role joins no group, the privileged group is not created, and cloud_admin keeps its verifier and still logs in. Pointed at a new name on a re-render that carries no password, the role logs in from off loopback with the same password, holding cloud_admin's former SCRAM verifier verbatim, while cloud_admin's password is NULL and its login is refused; the role is a LOGIN, non-superuser member of yb_amp_superuser, remote-auth says already_set, and every row survives. Connected as the role, COPY TO PROGRAM, pg_read_file and ALTER SYSTEM are refused, while creating a table and a role, and reading and writing a table cloud_admin owns, work. A compute restart keeps all of it. A fork's writer, created without a password and rendered without the setting, inherits it: the role logs in with the parent's password and cloud_admin is refused. Switched off again, nothing reverts: remote-auth says unset, cloud_admin has no password and is refused, and the role still logs in.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     50 * time.Minute,
		Run:         runAmpEndpointCustomerRole,
	})
}

func runAmpEndpointCustomerRole(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		r := &customerRoleRun{a: a}
		return []phase{
			{"default_login_is_cloud_admin", r.baseline},
			{"a_customers_own_role_is_not_taken_over", r.taken},
			{"switch_on_hands_the_password_over", r.switchOn},
			{"customer_role_cannot_reach_the_machine", r.confined},
			{"restart_keeps_it", r.restart},
			{"fork_inherits_the_conversion", r.fork},
			{"switch_off_keeps_it", r.switchOff},
		}
	})
}

type customerRoleRun struct {
	a    *ampFeatureRun
	cl   *controller.Client
	kube *k8s.Kubectl
	ep   controller.Endpoint
	// role is the login clients use once the endpoint is converted: custRole, or
	// the controller's own when it already deploys one (preset).
	role   string
	preset bool
	pw     string
	// verifier is cloud_admin's stored SCRAM verifier before the switch, which
	// the role must hold verbatim after it. Kept out of every log line and error.
	verifier string
	rows     int
}

func (r *customerRoleRun) release() string { return ampstack.EndpointWorkloadName(r.ep.ID) }

func (r *customerRoleRun) baseline(ctx context.Context) error {
	r.kube = r.a.f.ampKube()
	if r.kube == nil {
		r.a.f.t.Skip("no kube handle for the amp namespace: the role is switched through helm in the controller pod and logins are checked from the endpoint pod, off loopback")
		return nil
	}
	cl, err := r.a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	r.cl = cl
	r.pw = "Qa c$" + r.a.f.suffix() + "-1"
	if r.ep, err = createEndpointReq(ctx, cl, r.a.proj.poolID, r.a.proj.projID, r.a.proj.branchID,
		controller.CreateEndpointRequest{Role: roleReadWrite, Password: r.pw}); err != nil {
		return fmt.Errorf("create the writer with its own password: %w", err)
	}

	acfg := ampstack.FromConfig(r.a.f.t.Options)
	flag, err := r.kube.Get(ctx, "deployment", acfg.ControllerName,
		fmt.Sprintf(`{.spec.template.spec.containers[?(@.name==%q)].env[?(@.name==%q)].value}`, acfg.ControllerContainer, custControllerFlag))
	if err != nil {
		return fmt.Errorf("read %s on the controller Deployment %s: %w", custControllerFlag, acfg.ControllerName, err)
	}
	if flag = strings.TrimSpace(flag); flag != "" {
		r.preset, r.role = true, flag
		r.a.f.logf("amp_endpoint_customer_role: the controller already deploys customer role %q, so the endpoint must come up converted and the switching phases are not run", flag)
		if err := r.logsIn(ctx, r.ep, r.role, "the writer created by a controller deploying a customer role, as that role"); err != nil {
			return err
		}
		if err := r.refused(ctx, r.ep, "cloud_admin", "the same writer as cloud_admin"); err != nil {
			return err
		}
		return r.writeRows(ctx, nil, "the freshly created endpoint")
	}
	r.role = custRole

	if err := r.logsIn(ctx, r.ep, "cloud_admin", "the writer as cloud_admin, with the password it was created with"); err != nil {
		return err
	}
	admin, err := r.a.f.endpointSQL(ctx, r.ep)
	if err != nil {
		return err
	}
	defer admin.Close()
	if err := expectString(ctx, admin, "true", "cloud_admin's rolsuper before the switch",
		"SELECT rolsuper::text FROM pg_roles WHERE rolname = 'cloud_admin'"); err != nil {
		return err
	}
	if err := expectInt(ctx, admin, 0, "roles named "+r.role+" before the switch",
		"SELECT count(*) FROM pg_roles WHERE rolname = $1", r.role); err != nil {
		return err
	}
	if r.verifier, err = admin.QueryString(ctx, "SELECT rolpassword FROM pg_authid WHERE rolname = 'cloud_admin'"); err != nil {
		return fmt.Errorf("read cloud_admin's stored password: %w", err)
	}
	if !strings.HasPrefix(r.verifier, "SCRAM-SHA-256$") {
		return fmt.Errorf("cloud_admin's stored password is not a SCRAM-SHA-256 verifier, so there is nothing a customer role could take over")
	}
	return r.writeRows(ctx, nil, "the freshly created endpoint")
}

// taken points the setting at a role the customer made while they were
// superuser. The supervisor must leave it alone: converging it would hand its
// password holder the group's privileges and lock out cloud_admin's clients.
func (r *customerRoleRun) taken(ctx context.Context) error {
	if r.preset {
		r.a.f.logf("amp_endpoint_customer_role: not run, the endpoint was created converted")
		return nil
	}
	if err := r.adminExec(ctx, "make the customer's own role "+custTakenRole,
		"DROP ROLE IF EXISTS "+custTakenRole,
		"CREATE ROLE "+custTakenRole+" LOGIN PASSWORD 'the customer''s own'",
	); err != nil {
		return err
	}
	if err := r.setRole(ctx, custTakenRole); err != nil {
		return err
	}
	if err := r.expectRemoteAuth(ctx, "failed", "pointed at the customer's own role "+custTakenRole); err != nil {
		return err
	}
	if err := r.logsIn(ctx, r.ep, "cloud_admin", "cloud_admin, after the setting named a role the supervisor did not create"); err != nil {
		return err
	}
	admin, err := r.a.f.endpointSQL(ctx, r.ep)
	if err != nil {
		return err
	}
	defer admin.Close()
	if err := expectInt(ctx, admin, 0, "groups "+custTakenRole+" was made a member of",
		"SELECT count(*) FROM pg_auth_members m JOIN pg_roles r ON r.oid = m.member WHERE r.rolname = $1", custTakenRole); err != nil {
		return err
	}
	if err := expectInt(ctx, admin, 0, "privileged group roles created", "SELECT count(*) FROM pg_roles WHERE rolname = $1", custGroupRole); err != nil {
		return err
	}
	if err := expectString(ctx, admin, "false", custTakenRole+"'s rolsuper", "SELECT rolsuper::text FROM pg_roles WHERE rolname = $1", custTakenRole); err != nil {
		return err
	}
	return r.verifierIs(ctx, admin, "cloud_admin", true, "cloud_admin, after the setting named a role the supervisor did not create")
}

func (r *customerRoleRun) switchOn(ctx context.Context) error {
	if r.preset {
		r.a.f.logf("amp_endpoint_customer_role: not run, the endpoint was created converted")
		return nil
	}
	if err := r.setRole(ctx, r.role); err != nil {
		return err
	}
	// A handover sets a password nobody sent: already_set, which gives a root a
	// 0/0 fork floor.
	if err := r.expectRemoteAuth(ctx, "already_set", "switched to "+r.role+" on a render without a password"); err != nil {
		return err
	}
	if err := r.logsIn(ctx, r.ep, r.role, "the customer role, with the password the writer was created with"); err != nil {
		return err
	}
	if err := r.refused(ctx, r.ep, "cloud_admin", "cloud_admin after the switch"); err != nil {
		return err
	}
	admin, err := r.a.f.endpointSQL(ctx, r.ep)
	if err != nil {
		return err
	}
	defer admin.Close()
	if err := r.converted(ctx, admin, "the switched endpoint"); err != nil {
		return err
	}
	if err := r.verifierIs(ctx, admin, r.role, true, "the customer role after the handover"); err != nil {
		return err
	}
	return r.checkRows(ctx, admin, "the switched endpoint")
}

func (r *customerRoleRun) confined(ctx context.Context) error {
	c, err := r.a.f.endpointSQLWith(ctx, r.ep, func(ci *pg.ConnInfo) { ci.User, ci.Password = r.role, r.pw })
	if err != nil {
		return fmt.Errorf("connect as %s: %w", r.role, err)
	}
	defer c.Close()
	if err := expectString(ctx, c, r.role, "current_user", "SELECT current_user::text"); err != nil {
		return err
	}
	if err := expectString(ctx, c, "false", r.role+"'s rolsuper", "SELECT rolsuper::text FROM pg_roles WHERE rolname = current_user"); err != nil {
		return err
	}
	// AMP-15 step 1: what reaches the pod and the compute spec beside it.
	for _, d := range []struct{ what, sql string }{
		{"COPY TO PROGRAM", "COPY (SELECT 1) TO PROGRAM 'id'"},
		{"pg_read_file", "SELECT pg_read_file('/etc/hostname')"},
		{"ALTER SYSTEM", "ALTER SYSTEM SET work_mem = '64MB'"},
	} {
		_, err := c.Exec(ctx, d.sql)
		if err == nil {
			return fmt.Errorf("%s as %s succeeded, want a permission error: the customer role still reaches the machine", d.what, r.role)
		}
		if msg := err.Error(); !strings.Contains(msg, "permission denied") && !strings.Contains(msg, "must be superuser") {
			return fmt.Errorf("%s as %s failed for a reason other than permissions, so it does not show the role is confined: %w", d.what, r.role, err)
		}
	}
	// ...and what an owner of their own data needs.
	for _, a := range []struct{ what, sql string }{
		{"create a table in public", "CREATE TABLE IF NOT EXISTS ampqa_customer_owned (id int)"},
		{"write the table it owns", "INSERT INTO ampqa_customer_owned VALUES (1)"},
		{"drop a role it may have made", "DROP ROLE IF EXISTS ampqa_customer_child"},
		{"create a role", "CREATE ROLE ampqa_customer_child"},
		{"see every session", "SELECT count(*) FROM pg_stat_activity"},
	} {
		if _, err := c.Exec(ctx, a.sql); err != nil {
			return fmt.Errorf("%s as %s: %w", a.what, r.role, err)
		}
	}
	// The rows so far belong to cloud_admin, as everything on an endpoint from
	// before the switch does: pg_read_all_data / pg_write_all_data reach them.
	return r.writeRows(ctx, c, "the endpoint, as "+r.role)
}

func (r *customerRoleRun) restart(ctx context.Context) error {
	c, ok, err := r.a.restartEndpointCompute(ctx, r.ep)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	c.Close()
	if !r.preset {
		if err := r.expectRemoteAuth(ctx, "already_set", "restarted"); err != nil {
			return err
		}
	}
	if err := r.logsIn(ctx, r.ep, r.role, "the customer role after a compute restart"); err != nil {
		return err
	}
	if err := r.refused(ctx, r.ep, "cloud_admin", "cloud_admin after a compute restart"); err != nil {
		return err
	}
	admin, err := r.a.f.endpointSQL(ctx, r.ep)
	if err != nil {
		return err
	}
	defer admin.Close()
	if err := r.converted(ctx, admin, "the restarted endpoint"); err != nil {
		return err
	}
	return r.checkRows(ctx, admin, "the restarted endpoint")
}

// fork shows the catalog, not the setting, carries the conversion: the fork's
// writer is rendered by the controller, which (unless preset) names no role.
func (r *customerRoleRun) fork(ctx context.Context) error {
	name := "cust-fork-" + r.a.f.suffix()
	if _, err := r.cl.CreateBranchAndWait(ctx, r.a.proj.poolID, r.a.proj.projID, controller.CreateBranchRequest{
		Name: name, ParentBranchID: r.a.proj.branchID,
	}); err != nil {
		return fmt.Errorf("fork the converted branch: %w", err)
	}
	b, err := findBranchCtl(ctx, r.cl, r.a.proj.poolID, r.a.proj.projID, name)
	if err != nil {
		return err
	}
	ep, err := createEndpointReq(ctx, r.cl, r.a.proj.poolID, r.a.proj.projID, b.BranchID,
		controller.CreateEndpointRequest{Role: roleReadWrite})
	if err != nil {
		return fmt.Errorf("create the fork's writer without a password: %w", err)
	}
	if err := r.logsIn(ctx, ep, r.role, "the fork's writer as the customer role, with the parent's password"); err != nil {
		return err
	}
	if err := r.refused(ctx, ep, "cloud_admin", "the fork's writer as cloud_admin"); err != nil {
		return err
	}
	admin, err := r.a.f.endpointSQL(ctx, ep)
	if err != nil {
		return err
	}
	defer admin.Close()
	return r.converted(ctx, admin, "the fork's writer")
}

func (r *customerRoleRun) switchOff(ctx context.Context) error {
	if r.preset {
		r.a.f.logf("amp_endpoint_customer_role: not run, the endpoint was created converted")
		return nil
	}
	if err := r.setRole(ctx, ""); err != nil {
		return err
	}
	// Back on cloud_admin as the login, which has no password and is sent none.
	if err := r.expectRemoteAuth(ctx, "unset", "switched off again"); err != nil {
		return err
	}
	if err := r.refused(ctx, r.ep, "cloud_admin", "cloud_admin after the setting was switched off"); err != nil {
		return err
	}
	if err := r.logsIn(ctx, r.ep, r.role, "the customer role after the setting was switched off"); err != nil {
		return err
	}
	admin, err := r.a.f.endpointSQL(ctx, r.ep)
	if err != nil {
		return err
	}
	defer admin.Close()
	if err := r.converted(ctx, admin, "the endpoint switched off again"); err != nil {
		return err
	}
	return r.checkRows(ctx, admin, "the endpoint switched off again")
}

// adminExec runs each statement as cloud_admin over the harness's loopback
// connection, one at a time.
func (r *customerRoleRun) adminExec(ctx context.Context, what string, stmts ...string) error {
	admin, err := r.a.f.endpointSQL(ctx, r.ep)
	if err != nil {
		return err
	}
	defer admin.Close()
	for _, stmt := range stmts {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
	}
	return nil
}

// converted asserts the catalog state the switch leaves: the role a LOGIN,
// non-superuser member of the group, and cloud_admin with no password.
func (r *customerRoleRun) converted(ctx context.Context, admin sqlConn, where string) error {
	if err := expectString(ctx, admin, "false|true", r.role+"'s rolsuper|rolcanlogin on "+where,
		"SELECT rolsuper::text || '|' || rolcanlogin::text FROM pg_roles WHERE rolname = $1", r.role); err != nil {
		return err
	}
	if err := expectString(ctx, admin, "true", r.role+" a member of "+custGroupRole+" on "+where,
		"SELECT pg_has_role($1, $2, 'member')::text", r.role, custGroupRole); err != nil {
		return err
	}
	return expectString(ctx, admin, "true", "cloud_admin's password is NULL on "+where,
		"SELECT (rolpassword IS NULL)::text FROM pg_authid WHERE rolname = 'cloud_admin'")
}

// verifierIs compares role's stored password with the one cloud_admin had
// before the switch, without either reaching an error message.
func (r *customerRoleRun) verifierIs(ctx context.Context, admin sqlConn, role string, want bool, what string) error {
	n, err := admin.QueryInt(ctx, "SELECT count(*) FROM pg_authid WHERE rolname = $1 AND rolpassword = $2", role, r.verifier)
	if err != nil {
		return fmt.Errorf("read %s's stored password: %w", role, err)
	}
	if got := n == 1; got != want {
		return fmt.Errorf("%s: stored password equals cloud_admin's verifier from before the switch: %t, want %t", what, got, want)
	}
	return nil
}

// setRole re-renders the endpoint's release with endpoint.customerRole set to
// role ("" for none) and no password, the shape of a controller re-render, and
// waits for the rolled pod.
func (r *customerRoleRun) setRole(ctx context.Context, role string) error {
	acfg := ampstack.FromConfig(r.a.f.t.Options)
	ctl := r.kube.ForPod("deployment/"+acfg.ControllerName, acfg.ControllerContainer)
	cmd := fmt.Sprintf(`helm upgrade %s "${AMP_DEPLOY_CHART:?}" --namespace %s --reuse-values --set-string endpoint.customerRole=%s --set-string endpoint.password= --wait --timeout %s 2>&1`,
		r.release(), r.kube.Namespace(), role, custDeployWait)
	r.a.f.logf("amp_endpoint_customer_role: in the controller pod: %s", cmd)
	res, err := ctl.Run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("exec helm in the controller pod: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("helm upgrade of %s with endpoint.customerRole=%q exited %d: %s", r.release(), role, res.ExitCode, strings.TrimSpace(res.Stdout+res.Stderr))
	}
	roller := &kubeComputeRoller{kube: r.kube, timeout: custDeployWait}
	if err := roller.waitReady(ctx, r.ep.ID); err != nil {
		return fmt.Errorf("endpoint %s is not ready after setting endpoint.customerRole=%q: %w", r.ep.ID, role, err)
	}
	got, err := r.kube.Get(ctx, "configmap", r.release(), "{.data.CUSTOMER_ROLE}")
	if err != nil {
		return fmt.Errorf("read CUSTOMER_ROLE from ConfigMap %s: %w", r.release(), err)
	}
	if got = strings.TrimSpace(got); got != role {
		if role != "" && got == "" {
			r.a.f.t.Skip("the endpoint chart this controller deploys has no endpoint.customerRole value: setting it rendered no CUSTOMER_ROLE")
			return nil
		}
		return fmt.Errorf("after helm set endpoint.customerRole=%q the endpoint's ConfigMap says CUSTOMER_ROLE=%q", role, got)
	}
	return nil
}

func (r *customerRoleRun) auth(ep controller.Endpoint, user string) (ampstack.EndpointAuth, error) {
	host, p, err := net.SplitHostPort(strings.TrimSpace(ep.Address))
	if err != nil {
		return ampstack.EndpointAuth{}, fmt.Errorf("endpoint %s records address %q, want host:port: %w", ep.ID, ep.Address, err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		return ampstack.EndpointAuth{}, fmt.Errorf("endpoint %s address %q has a non-numeric port", ep.ID, ep.Address)
	}
	return ampstack.EndpointAuth{Host: host, Port: port, User: user, Password: r.pw, Database: "postgres"}, nil
}

// logsIn waits for user to log in to ep with the test's password from ep's own
// pod, through its Service: off loopback, so pg_hba asks for the password.
func (r *customerRoleRun) logsIn(ctx context.Context, ep controller.Endpoint, user, what string) error {
	a, err := r.auth(ep, user)
	if err != nil {
		return err
	}
	if err := ampstack.WaitEndpointAuth(ctx, r.kube, ep.ID, a, clientAccessAuthTimeout, r.a.f.logf); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

func (r *customerRoleRun) refused(ctx context.Context, ep controller.Endpoint, user, what string) error {
	a, err := r.auth(ep, user)
	if err != nil {
		return err
	}
	out, err := ampstack.EndpointPsql(ctx, r.kube, ep.ID, a, "select 1")
	if err == nil {
		return fmt.Errorf("%s: psql logged in with the endpoint's password and answered %q, want password authentication to fail", what, out)
	}
	if !strings.Contains(err.Error(), "password authentication failed") {
		return fmt.Errorf("%s: want a password authentication failure, got: %w", what, err)
	}
	return nil
}

// expectRemoteAuth waits for the supervisor's GET /v1/remote-auth to settle and
// requires want. The route is served only where the chart gives the supervisor
// a port; without one the outcome is not checked.
func (r *customerRoleRun) expectRemoteAuth(ctx context.Context, want, when string) error {
	var state string
	err := pollUntil(ctx, custRemoteAuthWait, 3*time.Second, func() (bool, error) {
		pod, err := (&kubeComputeRoller{kube: r.kube}).podName(ctx, r.ep.ID)
		if err != nil {
			return false, nil
		}
		out, err := runIn(ctx, r.kube.ForPod(pod, endpointPodContainer), "read the supervisor's remote-auth outcome", custRemoteAuthScript)
		if err != nil {
			return false, nil
		}
		state, err = parseRemoteAuth(out)
		if err != nil {
			return false, err
		}
		return state != "pending", nil
	})
	if state == "no-api" {
		r.a.f.logf("amp_endpoint_customer_role: the supervisor serves no settings API (SUPERVISOR_HTTP_PORT unset), so the remote-auth outcome %s is not checked", when)
		return nil
	}
	if err != nil {
		return fmt.Errorf("the supervisor's remote-auth outcome %s never settled (last %q): %w", when, state, err)
	}
	if state != want {
		return fmt.Errorf("the supervisor's remote-auth outcome %s is %q, want %q", when, state, want)
	}
	return nil
}

// custRemoteAuthScript prints the body of GET /v1/remote-auth on one line, or
// "no-api" when the supervisor serves no API. bash, for /dev/tcp.
const custRemoteAuthScript = `port="${SUPERVISOR_HTTP_PORT:-}"
[ -n "$port" ] || { echo no-api; exit 0; }
exec 3<>"/dev/tcp/127.0.0.1/$port" || exit 7
printf 'GET /v1/remote-auth HTTP/1.0\r\nHost: 127.0.0.1\r\n\r\n' >&3
tr -d '\r' <&3 | sed -n '/^$/,$p' | tr -d '\n'; echo`

// parseRemoteAuth reads custRemoteAuthScript's output: the route's JSON body's
// state, or "no-api".
func parseRemoteAuth(out string) (string, error) {
	out = strings.TrimSpace(out)
	if out == "no-api" {
		return out, nil
	}
	var body struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &body); err != nil || body.State == "" {
		return "", fmt.Errorf("GET /v1/remote-auth answered %q, want a JSON object with a state", out)
	}
	return body.State, nil
}

// writeRows checks every row written so far and adds a batch, through c, or
// the admin connection when c is nil.
func (r *customerRoleRun) writeRows(ctx context.Context, c sqlConn, where string) error {
	if c == nil {
		admin, err := r.a.f.endpointSQL(ctx, r.ep)
		if err != nil {
			return fmt.Errorf("connect %s: %w", where, err)
		}
		defer admin.Close()
		c = admin
	}
	if r.rows == 0 {
		if _, err := c.Exec(ctx, "CREATE TABLE "+custTable+" (id int PRIMARY KEY, v text NOT NULL)"); err != nil {
			return fmt.Errorf("create %s on %s: %w", custTable, where, err)
		}
	} else if err := r.checkRows(ctx, c, where); err != nil {
		return err
	}
	if _, err := c.Exec(ctx, "INSERT INTO "+custTable+" SELECT g, md5(g::text) FROM generate_series($1::int + 1, $1::int + $2::int) g", r.rows, custRows); err != nil {
		return fmt.Errorf("write to %s on %s: %w", custTable, where, err)
	}
	r.rows += custRows
	return r.checkRows(ctx, c, where)
}

func (r *customerRoleRun) checkRows(ctx context.Context, c sqlConn, where string) error {
	return expectInt(ctx, c, int64(r.rows), "intact rows of "+custTable+" on "+where,
		"SELECT count(*) FROM "+custTable+" WHERE v = md5(id::text)")
}

func expectString(ctx context.Context, c sqlConn, want, what, sql string, args ...any) error {
	got, err := c.QueryString(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("read %s: %w", what, err)
	}
	if got != want {
		return fmt.Errorf("%s is %q, want %q", what, got, want)
	}
	return nil
}

func expectInt(ctx context.Context, c sqlConn, want int64, what, sql string, args ...any) error {
	got, err := c.QueryInt(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("read %s: %w", what, err)
	}
	if got != want {
		return fmt.Errorf("%s: %d, want %d", what, got, want)
	}
	return nil
}
