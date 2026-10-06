package mekostack

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/cognitomock"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
	"github.com/yugabyte/meko/meko/test/functional/framework/kubemeta"
)

// AuthMode is who the api-server believes.
type AuthMode string

const (
	// AuthNone is the BYOC default: no identity provider, every request runs as
	// one dev account and a Bearer header is ignored.
	AuthNone AuthMode = "none"
	// AuthCognitoMock runs the api-server's Cognito path against a mock pool
	// deployed beside it, so a test can have several accounts.
	AuthCognitoMock AuthMode = "cognito_mock"
)

const (
	cognitoClientSecretName = "meko-api-server-cognito"
	cognitoCAMount          = "/etc/cognito-mock"
	cognitoCAVolume         = "cognito-mock-ca"

	defaultAccountEmail    = "default@mekoqa.test"
	defaultAccountPassword = "Mekoqa-default-1!"
	accountPassword        = "Mekoqa-account-1!"
	accountDomain          = "mekoqa.test"
)

// Account is a user of the stack's mock pool with its account row created and
// lifted off the free tier.
type Account struct {
	Email    string
	Password string
	Sub      string
	// Token is an access token, valid for the mock's TTL.
	Token string
}

type cognitoMockHost struct {
	ClusterIP string
}

func (a *App) cognitoMock() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.auth == AuthCognitoMock
}

func (a *App) AuthMode() AuthMode {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.auth == "" {
		return AuthNone
	}
	return a.auth
}

// apiServerCognito is the values overlay that points the api-server's Cognito
// client and JWKS fetch at the mock. extras is false for a chart that predates
// apiServer.hostAliases and the other pod additions, which are then applied as
// a patch after the install.
func apiServerCognito(spec cognitomock.Spec, host cognitoMockHost, extras bool) map[string]any {
	api := map[string]any{
		"cognito": map[string]any{"enabled": true},
		"config": map[string]any{
			"COGNITO_REGION":    spec.Region,
			"COGNITO_POOL_ID":   spec.PoolID,
			"COGNITO_CLIENT_ID": spec.ClientID,
			"COGNITO_DOMAIN":    "",
		},
	}
	if extras {
		pod := cognitoPodAdditions(spec, host)
		api["hostAliases"] = pod.hostAliases
		api["extraEnv"] = pod.env
		api["extraVolumes"] = pod.volumes
		api["extraVolumeMounts"] = pod.mounts
	}
	return map[string]any{"apiServer": api}
}

type podAdditions struct {
	hostAliases, env, volumes, mounts []any
}

func cognitoPodAdditions(spec cognitomock.Spec, host cognitoMockHost) podAdditions {
	return podAdditions{
		hostAliases: []any{map[string]any{"ip": host.ClusterIP, "hostnames": []any{cognitomock.Host(spec.Region)}}},
		env:         []any{map[string]any{"name": "SSL_CERT_FILE", "value": cognitoCAMount + "/" + cognitomock.CAFile}},
		volumes: []any{map[string]any{"name": cognitoCAVolume, "secret": map[string]any{
			"secretName": cognitomock.SecretName,
			"items":      []any{map[string]any{"key": cognitomock.CAFile, "path": cognitomock.CAFile}},
		}}},
		mounts: []any{map[string]any{"name": cognitoCAVolume, "mountPath": cognitoCAMount, "readOnly": true}},
	}
}

// overlay lays over on base. Maps merge, and the pod additions several
// overlays contribute to (env, volumes, mounts, host aliases) are appended:
// replacing them dropped the LLM mock's OPENAI_BASE_URL from the api-server
// whenever the Cognito mock added its CA path.
func overlay(base, over map[string]any) map[string]any {
	out := make(map[string]any, len(base))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		bm, bok := out[k].(map[string]any)
		om, ook := v.(map[string]any)
		if bok && ook {
			out[k] = overlay(bm, om)
			continue
		}
		bl, blok := out[k].([]any)
		ol, olok := v.([]any)
		if blok && olok && appendedLists[k] {
			out[k] = append(append([]any{}, bl...), ol...)
			continue
		}
		out[k] = v
	}
	return out
}

var appendedLists = map[string]bool{"extraEnv": true, "extraVolumes": true, "extraVolumeMounts": true, "hostAliases": true}

func (a *App) cognitoSpec() cognitomock.Spec {
	spec := a.cfg.CognitoMock
	spec.Pod = a.cfg.Mocks
	return spec.WithDefaults()
}

func (a *App) setupCognitoMock(ctx context.Context, clientSecret string) (cognitoMockHost, error) {
	spec := a.cognitoSpec()
	spec.ClientSecret = clientSecret
	spec.ReadyTimeout = a.cfg.ReadyTimeout
	mat, err := cognitomock.NewMaterial(cognitomock.Host(spec.Region), a.now(), a.rand)
	if err != nil {
		return cognitoMockHost{}, err
	}
	dep, err := a.deployCognito(ctx, a.kube, spec, mat)
	if err != nil {
		return cognitoMockHost{}, err
	}
	secret := fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\n  labels:\n    %s: %q\nstringData:\n  cognito-client-secret: %q\n",
		cognitoClientSecretName, kubemeta.TestLabel, kubemeta.TestLabelValue, clientSecret)
	if err := a.kube.Apply(ctx, secret); err != nil {
		return cognitoMockHost{}, fmt.Errorf("apply the Cognito client secret: %w", err)
	}
	a.mu.Lock()
	a.auth = AuthCognitoMock
	a.mu.Unlock()
	return cognitoMockHost{ClusterIP: dep.ClusterIP}, nil
}

func deployCognito(ctx context.Context, kube *k8s.Kubectl, spec cognitomock.Spec, mat cognitomock.Material) (cognitomock.Deployed, error) {
	return cognitomock.Deploy(ctx, kube, spec, mat)
}

// patchAPIServerCognito gives the api-server what apiServer.hostAliases and the
// extra* values would, on a chart that has none of them. The rollout is the
// caller's to wait for.
func (a *App) patchAPIServerCognito(ctx context.Context, host cognitoMockHost) error {
	pod := cognitoPodAdditions(a.cognitoSpec(), host)
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
		"hostAliases": pod.hostAliases,
		"volumes":     pod.volumes,
		"containers": []any{map[string]any{
			"name": apiDeployment, "env": pod.env, "volumeMounts": pod.mounts,
		}},
	}}}})
	if err != nil {
		return err
	}
	if err := a.kube.Patch(ctx, "deployment", apiDeployment, "strategic", string(patch)); err != nil {
		return fmt.Errorf("point the api-server at the Cognito mock: %w", err)
	}
	if err := a.kube.RolloutStatus(ctx, "deployment", apiDeployment, a.cfg.RolloutTimeout); err != nil {
		return fmt.Errorf("api-server rollout after the Cognito change: %w", err)
	}
	return nil
}

func (a *App) detectAuth(ctx context.Context) {
	if _, err := a.kube.Get(ctx, "service", cognitomock.Name, "{.metadata.name}"); err == nil {
		a.mu.Lock()
		a.auth = AuthCognitoMock
		a.mu.Unlock()
	}
}

func (a *App) cognitoControl(ctx context.Context) (*cognitomock.Client, error) {
	a.mu.Lock()
	c := a.ctl
	a.mu.Unlock()
	if c != nil {
		return c, nil
	}
	url, err := a.reach(ctx, cognitomock.Name, cognitomock.CtlPort, "app="+cognitomock.Name, cognitomock.CtlPort)
	if err != nil {
		return nil, fmt.Errorf("reach the Cognito mock: %w", err)
	}
	c = cognitomock.NewClient(url)
	a.mu.Lock()
	a.ctl = c
	a.mu.Unlock()
	return c, nil
}

func (a *App) CognitoControl(ctx context.Context) (*cognitomock.Client, error) {
	if !a.cognitoMock() {
		return nil, fmt.Errorf("needs env.mekostack.auth=%s, this stack's is %s", AuthCognitoMock, a.AuthMode())
	}
	return a.cognitoControl(ctx)
}

var emailSafe = regexp.MustCompile(`^[A-Za-z0-9._+-]+@[A-Za-z0-9.-]+$`)

func (a *App) ensureAccount(ctx context.Context, email, password, first, last string) (Account, error) {
	if !emailSafe.MatchString(email) {
		return Account{}, fmt.Errorf("email %q is not one this framework will put in SQL", email)
	}
	ctl, err := a.cognitoControl(ctx)
	if err != nil {
		return Account{}, err
	}
	u, err := ctl.UpsertUser(ctx, cognitomock.UserSpec{Email: email, Password: password, FirstName: first, LastName: last})
	if err != nil {
		return Account{}, err
	}
	res, err := a.API().Login(ctx, email, password)
	if err != nil {
		return Account{}, fmt.Errorf("log %s in: %w", email, err)
	}
	if res.AccessToken == "" {
		return Account{}, fmt.Errorf("log %s in: answered a challenge %q and no token", email, res.Challenge)
	}
	return Account{Email: email, Password: password, Sub: u.Sub, Token: res.AccessToken}, nil
}

// NewAccounts creates n accounts named <prefix>-1 ... <prefix>-n, each lifted to
// the enterprise tier. Free tier counts datapack creations over the account's
// lifetime and refuses the 11th.
func (a *App) NewAccounts(ctx context.Context, prefix string, n int) ([]Account, error) {
	if !a.cognitoMock() {
		return nil, fmt.Errorf("needs env.mekostack.auth=%s, this stack's is %s", AuthCognitoMock, a.AuthMode())
	}
	out := make([]Account, 0, n)
	emails := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		email := fmt.Sprintf("%s-%d@%s", prefix, i, accountDomain)
		acct, err := a.ensureAccount(ctx, email, accountPassword, "Mekoqa", fmt.Sprintf("Account%d", i))
		if err != nil {
			return nil, err
		}
		out = append(out, acct)
		emails = append(emails, email)
	}
	if err := a.liftAccounts(ctx, emails...); err != nil {
		return nil, err
	}
	if err := a.waitProvisioned(ctx, emails...); err != nil {
		return nil, err
	}
	return out, nil
}

// waitProvisioned waits for each account's default datapack and its Langfuse
// project. A login creates the account row at once and provisions the rest in
// a goroutine, so an invariant check run right after would see datapacks that
// are only mid-provisioning as missing a project.
func (a *App) waitProvisioned(ctx context.Context, emails ...string) error {
	quoted := make([]string, len(emails))
	for i, e := range emails {
		quoted[i] = "'" + e + "'"
	}
	q := fmt.Sprintf("SELECT count(*) FROM meko_system.users u"+
		" JOIN meko_system.datapacks d ON d.user_id = u.user_id"+
		" JOIN meko_system.langfuse_project_mapping m ON m.datapack_id = d.datapack_id"+
		" WHERE u.user_email IN (%s)", strings.Join(quoted, ", "))
	return a.until(ctx, a.cfg.ReadyTimeout, "the accounts' default datapacks to have Langfuse projects", func(ctx context.Context) (bool, error) {
		out, err := a.ysql(ctx, q)
		if err != nil {
			return false, err
		}
		var n int
		if _, err := fmt.Sscan(strings.TrimSpace(out), &n); err != nil {
			return false, fmt.Errorf("read the provisioned count from %q: %w", out, err)
		}
		if n < len(emails) {
			return false, fmt.Errorf("%d of %d accounts have a default datapack with a Langfuse project", n, len(emails))
		}
		return true, nil
	})
}

func (a *App) liftAccounts(ctx context.Context, emails ...string) error {
	quoted := make([]string, len(emails))
	for i, e := range emails {
		quoted[i] = "'" + e + "'"
	}
	q := fmt.Sprintf("UPDATE meko_system.users SET user_type = 'enterprise' WHERE user_email IN (%s) RETURNING 1", strings.Join(quoted, ", "))
	out, err := a.ysql(ctx, q)
	if err != nil {
		return fmt.Errorf("lift the accounts' tier caps: %w", err)
	}
	if n := returnedRows(out); n != len(emails) {
		return fmt.Errorf("lift the accounts' tier caps: updated %d rows, want %d", n, len(emails))
	}
	return nil
}

// ensureDefaultAccount is the account every request of a Cognito stack runs as
// unless a test asks for its own, the role the dev account has without a pool.
func (a *App) ensureDefaultAccount(ctx context.Context) error {
	acct, err := a.ensureAccount(ctx, defaultAccountEmail, defaultAccountPassword, "Default", "Account")
	if err != nil {
		if st := mekoapi.StatusOf(err); st >= 400 && st < 500 {
			return &fatalError{err}
		}
		return err
	}
	a.mu.Lock()
	a.token = acct.Token
	a.mu.Unlock()
	if err := a.liftAccounts(ctx, defaultAccountEmail); err != nil {
		return err
	}
	return a.waitProvisioned(ctx, defaultAccountEmail)
}

func (a *App) defaultToken() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.token
}

func (a *App) cognitoExtrasOK() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.support.APIServerExtras
}
