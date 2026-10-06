package functional

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mcpclient"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/stacktest"
)

// user is an account driving the stack the two ways a customer does: REST as
// the web app with its login token, and MCP as an agent with a PAT.
type user struct {
	t    *core.T
	acct *stacktest.Account
	api  *mekoapi.Client
	pat  mekoapi.Token
	mcp  *mcpclient.Client
}

func newUser(t *core.T, a *stacktest.Account) (*user, error) {
	u := &user{t: t, acct: a, api: a.API, pat: a.PAT()}
	u.mcp = a.MCP(u.pat.Value)
	if _, err := u.mcp.Initialize(t); err != nil {
		return u, fmt.Errorf("%s: MCP initialize: %w", a.Email, err)
	}
	return u, nil
}

func (u *user) close() {
	if u == nil || u.mcp == nil {
		return
	}
	ctx, cancel := cleanupCtx()
	defer cancel()
	u.mcp.Close(ctx)
}

// call runs an MCP tool and returns its text. A tool error, an isError result
// and a JSON body with an "error" key all come back as *toolError, with the
// text a client would show.
func (u *user) call(name string, args map[string]any) (string, *toolError) {
	res, err := u.mcp.CallTool(u.t, name, args)
	if err != nil {
		return "", &toolError{tool: name, text: err.Error()}
	}
	if res.IsError {
		return res.Text(), &toolError{tool: name, text: res.Text()}
	}
	var body map[string]any
	if json.Unmarshal([]byte(res.Text()), &body) == nil {
		if e, ok := body["error"]; ok {
			return res.Text(), &toolError{tool: name, text: res.Text(), code: fmt.Sprint(e)}
		}
	}
	return res.Text(), nil
}

func (u *user) callJSON(name string, args map[string]any, out any) error {
	text, terr := u.call(name, args)
	if terr != nil {
		return terr
	}
	if err := json.Unmarshal([]byte(text), out); err != nil {
		return fmt.Errorf("%s answered %q, which is not the JSON the tool documents: %w", name, truncate(text, 300), err)
	}
	return nil
}

func (u *user) conversation(datapack, title string) (string, error) {
	var conv struct {
		ID string `json:"id"`
	}
	if err := u.callJSON("conversation_create", map[string]any{"datapack_id": datapack, "title": title}, &conv); err != nil {
		return "", err
	}
	if conv.ID == "" {
		return "", fmt.Errorf("conversation_create on %s answered no id", datapack)
	}
	return conv.ID, nil
}

// rest sends a request as the account's login and decodes the envelope's data
// into out (nil to discard). Any status but want is an error carrying the body.
func (u *user) rest(method, path string, body any, want int, out any) error {
	return restAs(u.t, u.api, method, path, body, want, out)
}

func restAs(t *core.T, api *mekoapi.Client, method, path string, body any, want int, out any) error {
	r, err := api.Raw(t, method, path, body)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if r.Status != want {
		return fmt.Errorf("%s %s: HTTP %d %q, want %d: %s", method, path, r.Status, r.Code, want, truncateBody(r.Body))
	}
	if out == nil {
		return nil
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(r.Body, &env); err != nil {
		return fmt.Errorf("%s %s: decode the envelope: %w: %s", method, path, err, truncateBody(r.Body))
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("%s %s: decode data: %w: %s", method, path, err, truncateBody(env.Data))
	}
	return nil
}

// refused checks a request is turned down with status and, when set, code,
// and that the answer leaks nothing named in secrets.
func refused(t *core.T, what string, api *mekoapi.Client, method, path string, body any, status int, code string, secrets ...string) error {
	r, err := api.Raw(t, method, path, body)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if err := wantStatus(what, r, status, code); err != nil {
		return err
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(string(r.Body), s) {
			return fmt.Errorf("%s: the refusal names %q, which the caller may not see: %s", what, s, truncateBody(r.Body))
		}
	}
	return nil
}

// toolRefused checks an MCP tool turns the call down and its text names none of secrets.
func toolRefused(u *user, what, tool string, args map[string]any, secrets ...string) error {
	text, terr := u.call(tool, args)
	if terr == nil {
		return fmt.Errorf("%s: %s answered without an error: %s", what, tool, truncate(text, 300))
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(terr.text, s) {
			return fmt.Errorf("%s: %s refused, but its answer names %q: %s", what, tool, s, truncate(terr.text, 300))
		}
	}
	if strings.Contains(terr.text, "Traceback") || strings.Contains(terr.text, "invalid literal") || strings.Contains(terr.text, "not JSON serializable") {
		return fmt.Errorf("%s: %s refused with an internal error a client cannot act on: %s", what, tool, truncate(terr.text, 300))
	}
	return nil
}

// eventually polls cond for what, the way a client polls a page until the
// write it made shows. The bound is the product's, read from config.
func eventually(t *core.T, what string, within time.Duration, cond func() (bool, error)) error {
	return t.WaitFor(what, within, 2*time.Second, cond)
}

// visibleWithin is how long a write may take to show in a read of another
// path (a conversation in Langfuse, a memory count).
func visibleWithin(t *core.T) time.Duration {
	return t.Options.GetDuration("functional.visible_within", 60*time.Second)
}

func idsOf[T any](items []T, id func(T) string) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = id(it)
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

const (
	get    = http.MethodGet
	post   = http.MethodPost
	patch  = http.MethodPatch
	del    = http.MethodDelete
	httpOK = http.StatusOK
)

func shareWith(t *core.T, owner, other *user, datapack, grant string) error {
	inv, err := owner.api.ShareDatapack(t, datapack, other.acct.Email, grant)
	if err != nil {
		return fmt.Errorf("share %s with %s as %s: %w", datapack, other.acct.Email, grant, err)
	}
	if _, err := other.api.ActOnInvite(t, inv.InviteID, "accept"); err != nil {
		return fmt.Errorf("%s accepts invite %s: %w", other.acct.Email, inv.InviteID, err)
	}
	return nil
}

// ownedDatapack creates a datapack for u and waits for its Langfuse project,
// without which every MCP call on it fails (MEKO-621).
func ownedDatapack(t *core.T, u *user, name string) (mekoapi.Datapack, error) {
	dp, err := createDatapack(t, u.api, name)
	if err != nil {
		return dp, fmt.Errorf("create datapack %s: %w", name, err)
	}
	if stack := stackEnv(t).Stack(); stack != nil {
		err = t.WaitFor("the Langfuse project of "+name, time.Minute, 3*time.Second, func() (bool, error) {
			return hasLangfuseProject(t, stack, dp.ID)
		})
	}
	return dp, err
}

func deleteDatapack(u *user, id string) {
	if u == nil || id == "" {
		return
	}
	ctx, cancel := cleanupCtx()
	defer cancel()
	_ = u.api.DeleteDatapack(ctx, id)
}

func jsonUnmarshal(text string, out any) error {
	if err := json.Unmarshal([]byte(text), out); err != nil {
		return fmt.Errorf("decode %q: %w", truncate(text, 200), err)
	}
	return nil
}
