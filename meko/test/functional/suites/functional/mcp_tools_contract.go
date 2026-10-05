package functional

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mcpclient"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

// requiredTools are tools every meko-mcp-server release lists.
var requiredTools = []string{
	"memory_add", "memory_search", "conversation_create", "knowledgebase_search",
	"context_search", "datapack_list", "artifact_put",
}

// A PAT is validated on each tool call, and a server may cache a verdict for a
// moment, so a revoked token gets this long to be refused.
const revokeWindow = 60 * time.Second

func init() {
	core.Register(core.Test{
		Name:        "functional/mcp_tools_contract",
		Suite:       "functional",
		Category:    core.Functional,
		Tags:        tags(),
		OwningTeam:  core.TeamMCPServer,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Description: "Mint a PAT, open an MCP session with it, tools/list carries memory_add, memory_search, conversation_create, knowledgebase_search, context_search, datapack_list and artifact_put, and a tool call works. Revoke the PAT and the same call is refused.",
		Run:         mcpToolsContract,
	})
}

func mcpToolsContract(t *core.T) error {
	api := apiFor(t)

	var tok mekoapi.Token
	defer func() {
		if tok.Record.ID == "" {
			return
		}
		ctx, cancel := cleanupCtx()
		defer cancel()
		_ = api.RevokeToken(ctx, tok.Record.ID)
	}()
	if err := t.Step("mint_pat", func(h *core.StepHandle) error {
		var err error
		tok, err = api.CreateToken(t, mekoapi.CreateTokenRequest{Name: "mekoqa-" + t.ResourceSuffix(), TTLDays: 1})
		if err != nil {
			return err
		}
		h.Set("token_id", tok.Record.ID)
		if tok.Value == "" || tok.Record.ID == "" {
			return fmt.Errorf("the token response carries no value or id: %+v", tok.Record)
		}
		return nil
	}); err != nil {
		return err
	}

	live := mcpFor(t, tok.Value)
	defer func() {
		ctx, cancel := cleanupCtx()
		defer cancel()
		live.Close(ctx)
	}()
	if err := t.Step("initialize", func(h *core.StepHandle) error {
		res, err := live.Initialize(t)
		if err != nil {
			return err
		}
		h.Set("server", res.ServerInfo.Name+" "+res.ServerInfo.Version)
		h.Set("protocol", res.ProtocolVersion)
		return nil
	}); err != nil {
		return err
	}

	var args map[string]any
	if err := t.Step("tools_list", func(h *core.StepHandle) error {
		tools, err := live.ListTools(t)
		if err != nil {
			return err
		}
		h.Set("tools", len(tools))
		have := map[string]mcpclient.Tool{}
		for _, tl := range tools {
			have[tl.Name] = tl
		}
		var missing []string
		for _, want := range requiredTools {
			if _, ok := have[want]; !ok {
				missing = append(missing, want)
			}
		}
		if len(missing) > 0 {
			var names []string
			for n := range have {
				names = append(names, n)
			}
			sort.Strings(names)
			return fmt.Errorf("tools/list lacks %v, it lists %v", missing, names)
		}
		args = mcpclient.ArgsFromSchema(have["datapack_list"].InputSchema)
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("call_with_live_pat", func(h *core.StepHandle) error {
		res, err := live.CallTool(t, "datapack_list", args)
		if err != nil {
			return err
		}
		if res.IsError {
			return fmt.Errorf("datapack_list with arguments %v answered a tool error: %s", args, res.Text())
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("revoke", func(*core.StepHandle) error {
		return api.RevokeToken(t, tok.Record.ID)
	}); err != nil {
		return err
	}

	return t.Step("refused_after_revoke", func(h *core.StepHandle) error {
		start := time.Now()
		var last string
		err := t.WaitFor("the revoked PAT to be refused", revokeWindow, 2*time.Second, func() (bool, error) {
			refused, why, err := refusedWithRevokedPAT(t, mcpFor(t, tok.Value), live, args)
			last = why
			return refused, err
		})
		if err != nil {
			return fmt.Errorf("%w: last answer: %s", err, last)
		}
		t.Metric("mcp.revoked_pat_refused_seconds", time.Since(start).Seconds(), "s")
		h.Set("refused_by", last)
		return nil
	})
}

// refusedWithRevokedPAT tries the revoked token twice, on the session opened
// before the revoke and on a fresh one, and reports whether both are refused.
// A call that works means "not refused yet". Any other failure is an error.
func refusedWithRevokedPAT(ctx context.Context, fresh, old *mcpclient.Client, args map[string]any) (refused bool, why string, err error) {
	why, ok, err := tryRevoked(func() (mcpclient.CallResult, error) {
		return old.CallTool(ctx, "datapack_list", args)
	})
	if err != nil || !ok {
		return false, "session opened before the revoke: " + why, err
	}
	if _, initErr := fresh.Initialize(ctx); initErr != nil {
		if mcpclient.IsAuthRejection(initErr) {
			return true, "fresh session refused at initialize: " + initErr.Error(), nil
		}
		return false, "fresh session", fmt.Errorf("initialize with the revoked PAT failed for a reason other than authentication: %w", initErr)
	}
	defer fresh.Close(ctx)
	why, ok, err = tryRevoked(func() (mcpclient.CallResult, error) {
		return fresh.CallTool(ctx, "datapack_list", args)
	})
	return ok, "fresh session: " + why, err
}

// tryRevoked runs one call with the revoked token. ok means it was refused.
func tryRevoked(call func() (mcpclient.CallResult, error)) (why string, ok bool, err error) {
	res, callErr := call()
	switch {
	case callErr != nil && mcpclient.IsAuthRejection(callErr):
		return callErr.Error(), true, nil
	case callErr != nil:
		return callErr.Error(), false, fmt.Errorf("the call failed for a reason other than authentication: %w", callErr)
	case res.IsError && mcpclient.LooksLikeAuthFailure(res.Text()):
		return strings.TrimSpace(res.Text()), true, nil
	case res.IsError:
		return strings.TrimSpace(res.Text()), false, fmt.Errorf("the call answered a tool error that does not read as a refused credential: %s", res.Text())
	}
	return "the call still succeeds", false, nil
}
