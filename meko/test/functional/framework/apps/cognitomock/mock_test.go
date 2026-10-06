package cognitomock

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/cognitomock/server"
)

type running struct {
	spec    Spec
	mat     Material
	tlsPort int
	ctlPort int
	cancel  context.CancelFunc
	done    chan error
	ctl     *Client
	https   *http.Client
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startMock(t *testing.T, spec Spec, dir string, mat *Material) *running {
	t.Helper()
	spec = spec.WithDefaults()
	r := &running{spec: spec, tlsPort: freePort(t), ctlPort: freePort(t), done: make(chan error, 1)}
	if mat == nil {
		m, err := NewMaterial(spec.Host(), time.Now(), nil)
		if err != nil {
			t.Fatal(err)
		}
		mat = &m
	}
	r.mat = *mat
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	for path, content := range map[string][]byte{certFile: r.mat.CertPEM, keyFile: r.mat.KeyPEM} {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pool, err := server.New(server.Config{
		Region: spec.Region, PoolID: spec.PoolID, ClientID: spec.ClientID, ClientSecret: spec.ClientSecret,
		TokenTTL: spec.TokenTTL, ConfirmationCode: ConfirmationCode, Key: r.mat.Key,
		StatePath: filepath.Join(dir, "state", "users.json"), Logf: t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() {
		r.done <- pool.Serve(ctx, server.Listeners{
			PublicAddr:  fmt.Sprintf("127.0.0.1:%d", r.tlsPort),
			ControlAddr: fmt.Sprintf("127.0.0.1:%d", r.ctlPort),
			CertFile:    certFile, KeyFile: keyFile,
		})
	}()
	t.Cleanup(r.stop)
	r.ctl = NewClient(fmt.Sprintf("http://127.0.0.1:%d", r.ctlPort))
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(r.mat.CAPEM)
	r.https = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: spec.Host()},
	}}
	deadline := time.Now().Add(10 * time.Second)
	for r.ctl.Health(context.Background()) != nil {
		if time.Now().After(deadline) {
			t.Fatal("mock did not come up")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return r
}

func (r *running) stop() {
	if r.cancel != nil {
		r.cancel()
		<-r.done
		r.cancel = nil
	}
}

type cognitoErr struct {
	Type    string `json:"__type"`
	Message string `json:"message"`
}

func (r *running) call(t *testing.T, op string, in map[string]any) (map[string]any, string) {
	t.Helper()
	b, _ := json.Marshal(in)
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("https://%s:%d/", r.spec.Host(), r.tlsPort), bytes.NewReader(b))
	req.Header.Set("X-Amz-Target", "AWSCognitoIdentityProviderService."+op)
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	r.https.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, fmt.Sprintf("127.0.0.1:%d", r.tlsPort))
	}
	resp, err := r.https.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var e cognitoErr
		_ = json.Unmarshal(raw, &e)
		if hdr := resp.Header.Get("x-amzn-ErrorType"); hdr != e.Type {
			t.Errorf("%s: error header %q, body type %q", op, hdr, e.Type)
		}
		return nil, e.Type
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: %v: %s", op, err, raw)
	}
	return out, ""
}

func (r *running) jwks(t *testing.T) *rsa.PublicKey {
	t.Helper()
	r.https.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, fmt.Sprintf("127.0.0.1:%d", r.tlsPort))
	}
	resp, err := r.https.Get(fmt.Sprintf("https://%s:%d/%s/.well-known/jwks.json", r.spec.Host(), r.tlsPort, r.spec.PoolID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var set struct {
		Keys []struct{ Kid, Kty, Alg, Use, N, E string } `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil || len(set.Keys) != 1 {
		t.Fatalf("jwks: %v %+v", err, set)
	}
	k := set.Keys[0]
	if k.Kty != "RSA" || k.Use != "sig" || k.Alg != "RS256" || k.Kid == "" {
		t.Fatalf("jwk %+v", k)
	}
	nb, _ := base64.RawURLEncoding.DecodeString(k.N)
	eb, _ := base64.RawURLEncoding.DecodeString(k.E)
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(new(big.Int).SetBytes(eb).Int64())}
}

// verify checks an RS256 token the way the api-server's validator does and
// returns its claims.
func verify(pub *rsa.PublicKey, token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%d parts", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return nil, err
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	return claims, json.Unmarshal(body, &claims)
}

func TestTokensVerifyAgainstTheServedJWKS(t *testing.T) {
	r := startMock(t, Spec{}, t.TempDir(), nil)
	ctx := context.Background()
	u, err := r.ctl.UpsertUser(ctx, UserSpec{Email: "a@mekoqa.test", Password: "Passw0rd!x", FirstName: "Ann", LastName: "Lee"})
	if err != nil || u.Sub == "" || u.Status != "CONFIRMED" {
		t.Fatalf("upsert: %+v %v", u, err)
	}
	pub := r.jwks(t)
	tok, err := r.ctl.Mint(ctx, MintSpec{Email: "a@mekoqa.test"})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := verify(pub, tok)
	if err != nil {
		t.Fatalf("signature: %v", err)
	}
	if claims["iss"] != r.spec.Issuer() || claims["token_use"] != "access" || claims["sub"] != u.Sub || claims["username"] != "a@mekoqa.test" {
		t.Fatalf("claims %v", claims)
	}
	if exp, _ := claims["exp"].(float64); exp < float64(time.Now().Add(23*time.Hour).Unix()) {
		t.Fatalf("exp %v is not about a day out", exp)
	}

	i := -5
	expired, _ := r.ctl.Mint(ctx, MintSpec{Email: "a@mekoqa.test", ExpiresIn: &i})
	c, err := verify(pub, expired)
	if err != nil || c["exp"].(float64) > float64(time.Now().Unix()) {
		t.Fatalf("expired token: %v %v", c, err)
	}
	other, _ := r.ctl.Mint(ctx, MintSpec{Email: "a@mekoqa.test", Issuer: "https://cognito-idp.us-east-1.amazonaws.com/us-east-1_Other"})
	if c, err := verify(pub, other); err != nil || c["iss"] == r.spec.Issuer() {
		t.Fatalf("other-pool token: %v %v", c, err)
	}
	id, _ := r.ctl.Mint(ctx, MintSpec{Email: "a@mekoqa.test", TokenUse: "id"})
	if c, err := verify(pub, id); err != nil || c["email"] != "a@mekoqa.test" || c["given_name"] != "Ann" || c["aud"] != r.spec.ClientID {
		t.Fatalf("id token: %v %v", c, err)
	}

	parts := strings.Split(tok, ".")
	forged := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 256))
	if _, err := verify(pub, forged); err == nil {
		t.Fatal("a token with a different signature verified")
	}
}

func TestSignupConfirmLoginAndTheirErrors(t *testing.T) {
	r := startMock(t, Spec{}, t.TempDir(), nil)
	cid := r.spec.ClientID
	email := "new@mekoqa.test"

	if _, e := r.call(t, "SignUp", map[string]any{"ClientId": cid, "Username": email, "Password": "weak"}); e != "InvalidPasswordException" {
		t.Fatalf("weak password: %q", e)
	}
	out, e := r.call(t, "SignUp", map[string]any{
		"ClientId": cid, "Username": email, "Password": "Passw0rd!x",
		"UserAttributes": []map[string]string{{"Name": "email", "Value": email}, {"Name": "given_name", "Value": "Nia"}},
	})
	if e != "" || out["UserSub"] == "" || out["UserConfirmed"] != false {
		t.Fatalf("signup: %v %q", out, e)
	}
	if _, e := r.call(t, "SignUp", map[string]any{"ClientId": cid, "Username": strings.ToUpper(email), "Password": "Passw0rd!x"}); e != "UsernameExistsException" {
		t.Fatalf("duplicate signup: %q", e)
	}
	login := func(pw string) (map[string]any, string) {
		return r.call(t, "InitiateAuth", map[string]any{
			"AuthFlow": "USER_PASSWORD_AUTH", "ClientId": cid,
			"AuthParameters": map[string]string{"USERNAME": email, "PASSWORD": pw},
		})
	}
	if _, e := login("Passw0rd!x"); e != "UserNotConfirmedException" {
		t.Fatalf("login before confirm: %q", e)
	}
	if _, e := r.call(t, "ConfirmSignUp", map[string]any{"ClientId": cid, "Username": email, "ConfirmationCode": "000000"}); e != "CodeMismatchException" {
		t.Fatalf("wrong code: %q", e)
	}
	if _, e := r.call(t, "ConfirmSignUp", map[string]any{"ClientId": cid, "Username": email, "ConfirmationCode": ConfirmationCode}); e != "" {
		t.Fatalf("confirm: %q", e)
	}
	if _, e := login("nope"); e != "NotAuthorizedException" {
		t.Fatalf("wrong password: %q", e)
	}
	if _, e := r.call(t, "InitiateAuth", map[string]any{
		"AuthFlow": "USER_PASSWORD_AUTH", "ClientId": cid,
		"AuthParameters": map[string]string{"USERNAME": "ghost@mekoqa.test", "PASSWORD": "Passw0rd!x"},
	}); e != "NotAuthorizedException" {
		t.Fatalf("unknown user: %q", e)
	}
	res, e := login("Passw0rd!x")
	if e != "" {
		t.Fatalf("login: %q", e)
	}
	ar := res["AuthenticationResult"].(map[string]any)
	pub := r.jwks(t)
	if _, err := verify(pub, ar["AccessToken"].(string)); err != nil {
		t.Fatalf("access token: %v", err)
	}
	idc, err := verify(pub, ar["IdToken"].(string))
	if err != nil || idc["email"] != email || idc["email_verified"] != true || idc["given_name"] != "Nia" {
		t.Fatalf("id token: %v %v", idc, err)
	}

	rt := ar["RefreshToken"].(string)
	refreshed, e := r.call(t, "InitiateAuth", map[string]any{
		"AuthFlow": "REFRESH_TOKEN_AUTH", "ClientId": cid, "AuthParameters": map[string]string{"REFRESH_TOKEN": rt},
	})
	if e != "" || refreshed["AuthenticationResult"].(map[string]any)["AccessToken"] == "" {
		t.Fatalf("refresh: %v %q", refreshed, e)
	}
	if _, e := r.call(t, "GlobalSignOut", map[string]any{"AccessToken": ar["AccessToken"]}); e != "" {
		t.Fatalf("sign out: %q", e)
	}
	if _, e := r.call(t, "InitiateAuth", map[string]any{
		"AuthFlow": "REFRESH_TOKEN_AUTH", "ClientId": cid, "AuthParameters": map[string]string{"REFRESH_TOKEN": rt},
	}); e != "NotAuthorizedException" {
		t.Fatalf("refresh after sign out: %q", e)
	}
}

func TestAdminCreatedUserMustSetAPassword(t *testing.T) {
	r := startMock(t, Spec{}, t.TempDir(), nil)
	cid, pool := r.spec.ClientID, r.spec.PoolID
	email := "invited@mekoqa.test"
	out, e := r.call(t, "AdminCreateUser", map[string]any{
		"UserPoolId": pool, "Username": email, "TemporaryPassword": "Temp0rary!pw", "MessageAction": "SUPPRESS",
		"UserAttributes": []map[string]string{{"Name": "email", "Value": email}, {"Name": "email_verified", "Value": "true"}},
	})
	if e != "" {
		t.Fatalf("create: %q", e)
	}
	if _, e := r.call(t, "AdminCreateUser", map[string]any{"UserPoolId": pool, "Username": email, "TemporaryPassword": "Temp0rary!pw"}); e != "UsernameExistsException" {
		t.Fatalf("create twice: %q", e)
	}
	got, e := r.call(t, "AdminGetUser", map[string]any{"UserPoolId": pool, "Username": email})
	if e != "" || got["UserStatus"] != "FORCE_CHANGE_PASSWORD" {
		t.Fatalf("get: %v %q", got, e)
	}
	sub := ""
	for _, a := range out["User"].(map[string]any)["Attributes"].([]any) {
		if m := a.(map[string]any); m["Name"] == "sub" {
			sub = m["Value"].(string)
		}
	}
	listed, e := r.call(t, "ListUsers", map[string]any{"UserPoolId": pool, "Filter": fmt.Sprintf("sub = %q", sub), "Limit": 1})
	if e != "" || len(listed["Users"].([]any)) != 1 || listed["Users"].([]any)[0].(map[string]any)["Username"] != email {
		t.Fatalf("list by sub: %v %q", listed, e)
	}
	if _, e := r.call(t, "AdminGetUser", map[string]any{"UserPoolId": "us-east-1_Wrong", "Username": email}); e != "ResourceNotFoundException" {
		t.Fatalf("wrong pool: %q", e)
	}

	ch, e := r.call(t, "InitiateAuth", map[string]any{
		"AuthFlow": "USER_PASSWORD_AUTH", "ClientId": cid,
		"AuthParameters": map[string]string{"USERNAME": email, "PASSWORD": "Temp0rary!pw"},
	})
	if e != "" || ch["ChallengeName"] != "NEW_PASSWORD_REQUIRED" || ch["Session"] == "" {
		t.Fatalf("challenge: %v %q", ch, e)
	}
	respond := func(session, pw string) (map[string]any, string) {
		return r.call(t, "RespondToAuthChallenge", map[string]any{
			"ChallengeName": "NEW_PASSWORD_REQUIRED", "ClientId": cid, "Session": session,
			"ChallengeResponses": map[string]string{"USERNAME": email, "NEW_PASSWORD": pw, "userAttributes.given_name": "Ivy"},
		})
	}
	if _, e := respond("not-a-session", "Passw0rd!x"); e != "NotAuthorizedException" {
		t.Fatalf("bad session: %q", e)
	}
	if _, e := respond(ch["Session"].(string), "weak"); e != "InvalidPasswordException" {
		t.Fatalf("weak new password: %q", e)
	}
	done, e := respond(ch["Session"].(string), "Passw0rd!x")
	if e != "" {
		t.Fatalf("respond: %q", e)
	}
	idc, err := verify(r.jwks(t), done["AuthenticationResult"].(map[string]any)["IdToken"].(string))
	if err != nil || idc["given_name"] != "Ivy" || idc["email"] != email {
		t.Fatalf("id token after challenge: %v %v", idc, err)
	}
}

func TestClientSecretIsEnforcedWhenConfigured(t *testing.T) {
	r := startMock(t, Spec{ClientSecret: "s3cret"}, t.TempDir(), nil)
	cid := r.spec.ClientID
	if _, err := r.ctl.UpsertUser(context.Background(), UserSpec{Email: "s@mekoqa.test", Password: "Passw0rd!x"}); err != nil {
		t.Fatal(err)
	}
	auth := func(hash string) string {
		params := map[string]string{"USERNAME": "s@mekoqa.test", "PASSWORD": "Passw0rd!x"}
		if hash != "" {
			params["SECRET_HASH"] = hash
		}
		_, e := r.call(t, "InitiateAuth", map[string]any{"AuthFlow": "USER_PASSWORD_AUTH", "ClientId": cid, "AuthParameters": params})
		return e
	}
	if e := auth(""); e != "NotAuthorizedException" {
		t.Fatalf("no hash: %q", e)
	}
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write([]byte("s@mekoqa.test" + cid))
	if e := auth(base64.StdEncoding.EncodeToString(mac.Sum(nil))); e != "" {
		t.Fatalf("right hash: %q", e)
	}
	if _, e := r.call(t, "InitiateAuth", map[string]any{"AuthFlow": "USER_PASSWORD_AUTH", "ClientId": "other", "AuthParameters": map[string]string{}}); e != "ResourceNotFoundException" {
		t.Fatalf("unknown client: %q", e)
	}
}

func TestUsersSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	first := startMock(t, Spec{}, dir, nil)
	u, err := first.ctl.UpsertUser(context.Background(), UserSpec{Email: "keep@mekoqa.test", Password: "Passw0rd!x"})
	if err != nil {
		t.Fatal(err)
	}
	first.stop()
	second := startMock(t, Spec{}, dir, &first.mat)
	users, err := second.ctl.Users(context.Background())
	if err != nil || len(users) != 1 || users[0].Sub != u.Sub {
		t.Fatalf("after restart: %+v %v", users, err)
	}
}

func TestUnknownOperationAndUserErrors(t *testing.T) {
	r := startMock(t, Spec{}, t.TempDir(), nil)
	if _, e := r.call(t, "AdminDisableUser", map[string]any{}); e != "InvalidActionException" {
		t.Fatalf("unimplemented op: %q", e)
	}
	if _, e := r.call(t, "ForgotPassword", map[string]any{"ClientId": r.spec.ClientID, "Username": "ghost@mekoqa.test"}); e != "UserNotFoundException" {
		t.Fatalf("forgot password for nobody: %q", e)
	}
	if _, err := r.ctl.Mint(context.Background(), MintSpec{Email: "ghost@mekoqa.test"}); err == nil {
		t.Fatal("minted a token for a user that does not exist")
	}
}

func TestAChangedEmailSignsInOnceVerified(t *testing.T) {
	r := startMock(t, Spec{}, t.TempDir(), nil)
	cid := r.spec.ClientID
	old, changed := "before@mekoqa.test", "after@mekoqa.test"
	if _, e := r.call(t, "SignUp", map[string]any{"ClientId": cid, "Username": old, "Password": "Passw0rd!x"}); e != "" {
		t.Fatal(e)
	}
	if _, e := r.call(t, "ConfirmSignUp", map[string]any{"ClientId": cid, "Username": old, "ConfirmationCode": ConfirmationCode}); e != "" {
		t.Fatal(e)
	}
	login := func(name string) string {
		_, e := r.call(t, "InitiateAuth", map[string]any{"AuthFlow": "USER_PASSWORD_AUTH", "ClientId": cid,
			"AuthParameters": map[string]string{"USERNAME": name, "PASSWORD": "Passw0rd!x"}})
		return e
	}
	res, _ := r.call(t, "InitiateAuth", map[string]any{"AuthFlow": "USER_PASSWORD_AUTH", "ClientId": cid,
		"AuthParameters": map[string]string{"USERNAME": old, "PASSWORD": "Passw0rd!x"}})
	tok := res["AuthenticationResult"].(map[string]any)["AccessToken"]
	if _, e := r.call(t, "UpdateUserAttributes", map[string]any{"AccessToken": tok,
		"UserAttributes": []map[string]string{{"Name": "email", "Value": changed}, {"Name": "email_verified", "Value": "false"}}}); e != "" {
		t.Fatal(e)
	}
	if e := login(changed); e == "" {
		t.Error("an unverified new email signed in")
	}
	if _, e := r.call(t, "VerifyUserAttribute", map[string]any{"AccessToken": tok, "AttributeName": "email", "Code": ConfirmationCode}); e != "" {
		t.Fatal(e)
	}
	if e := login(changed); e != "" {
		t.Errorf("the verified new email does not sign in: %q", e)
	}
}
