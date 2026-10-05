package stacktest

import (
	"fmt"
	"sync"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/cognitomock"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mcpclient"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekostack"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

// Account is a user of the stack's mock Cognito pool, with its own account row
// and its own datapacks. Every client it hands out carries the leak hook.
type Account struct {
	Email    string
	Password string
	// ID is the account's user id in the product, as /auth/me reports it.
	ID string
	// API calls as the account, with its access token.
	API *mekoapi.Client

	t    *core.T
	mu   sync.Mutex
	pats []string
}

// Accounts creates n accounts for t on a stack running the Cognito mock, each
// off the free tier, and skips t on a stack that has none. The returned
// function revokes every PAT the accounts minted. Accounts are named after the
// test, so two tests never share one.
func Accounts(t *core.T, n int) ([]*Account, func()) {
	stack := ClusterEnv(t).Stack()
	if stack.AuthMode() != mekostack.AuthCognitoMock {
		t.Skipf("needs env.mekostack.auth=%s, this stack's is %s", mekostack.AuthCognitoMock, stack.AuthMode())
	}
	made, err := stack.NewAccounts(t, "qa-"+t.ResourceSuffix(), n)
	if err != nil {
		t.Fatalf("create %d accounts: %v", n, err)
	}
	accts := make([]*Account, len(made))
	for i, m := range made {
		a := &Account{Email: m.Email, Password: m.Password, t: t}
		a.API = Anonymous(t).WithToken(m.Token)
		me, err := a.API.Me(t)
		if err != nil {
			t.Fatalf("%s: GET /auth/me with its own token: %v", m.Email, err)
		}
		a.ID = me.AccountID
		accts[i] = a
	}
	return accts, func() {
		for _, a := range accts {
			a.revokePATs()
		}
	}
}

func Anonymous(t *core.T) *mekoapi.Client { return API(t).WithToken("") }

// Control is the mock pool's control API, for a test that needs a token the
// product would never issue: expired, from another pool, for a user in a state
// that cannot log in.
func Control(t *core.T) *cognitomock.Client {
	stack := ClusterEnv(t).Stack()
	if stack.AuthMode() != mekostack.AuthCognitoMock {
		t.Skipf("needs env.mekostack.auth=%s, this stack's is %s", mekostack.AuthCognitoMock, stack.AuthMode())
	}
	ctl, err := stack.CognitoControl(t)
	if err != nil {
		t.Fatalf("reach the Cognito mock's control API: %v", err)
	}
	return ctl
}

// PAT mints a personal access token for the account with scopes, empty meaning
// all of them. It is revoked when the function Accounts returned runs.
func (a *Account) PAT(scopes ...string) mekoapi.Token {
	tok, err := a.API.CreateToken(a.t, mekoapi.CreateTokenRequest{
		Name: fmt.Sprintf("mekoqa-%s-%d", a.t.ResourceSuffix(), len(a.pats)), TTLDays: 1, Scopes: scopes,
	})
	if err != nil {
		a.t.Fatalf("%s: mint a PAT %v: %v", a.Email, scopes, err)
	}
	a.mu.Lock()
	a.pats = append(a.pats, tok.Record.ID)
	a.mu.Unlock()
	return tok
}

func (a *Account) WithPAT(pat string) *mekoapi.Client { return a.API.WithToken(pat) }

func (a *Account) MCP(pat string) *mcpclient.Client { return MCP(a.t, pat) }

func (a *Account) revokePATs() {
	a.mu.Lock()
	ids := a.pats
	a.pats = nil
	a.mu.Unlock()
	ctx, cancel := CleanupCtx()
	defer cancel()
	for _, id := range ids {
		_ = a.API.RevokeToken(ctx, id)
	}
}
