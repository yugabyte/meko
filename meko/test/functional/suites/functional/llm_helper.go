package functional

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/llmmock"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mcpclient"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/stacktest"
)

const mockPoll = 2 * time.Second

// memStack is one account's datapack, PAT, MCP session and conversation, which
// the memory and chat tests drive. close removes all of it.
type memStack struct {
	t          *core.T
	api        *mekoapi.Client
	mcp        *mcpclient.Client
	llm        *llmmock.Client
	token      mekoapi.Token
	datapack   mekoapi.Datapack
	conversion string
}

func newMemStack(t *core.T) (*memStack, error) {
	s := &memStack{t: t, api: apiFor(t), llm: stacktest.LLM(t)}
	suffix := t.ResourceSuffix()
	var err error
	if s.datapack, err = createDatapack(t, s.api, "llm-"+suffix); err != nil {
		return s, fmt.Errorf("create a datapack: %w", err)
	}
	if stack := stackEnv(t).Stack(); stack != nil {
		if err := t.WaitFor("the datapack's Langfuse project", time.Minute, 3*time.Second, func() (bool, error) {
			return hasLangfuseProject(t, stack, s.datapack.ID)
		}); err != nil {
			return s, err
		}
	}
	if s.token, err = s.api.CreateToken(t, mekoapi.CreateTokenRequest{Name: "mekoqa-llm-" + suffix, TTLDays: 1}); err != nil {
		return s, fmt.Errorf("mint a PAT: %w", err)
	}
	s.mcp = mcpFor(t, s.token.Value)
	if _, err := s.mcp.Initialize(t); err != nil {
		return s, err
	}
	return s, nil
}

func (s *memStack) close() {
	ctx, cancel := cleanupCtx()
	defer cancel()
	if s.mcp != nil {
		s.mcp.Close(ctx)
	}
	if s.token.Record.ID != "" {
		_ = s.api.RevokeToken(ctx, s.token.Record.ID)
	}
	if s.datapack.ID != "" {
		_ = s.api.DeleteDatapack(ctx, s.datapack.ID)
	}
}

// tool calls an MCP tool and returns its text. A tool-level error and a JSON
// body carrying an "error" key both come back as toolError.
func (s *memStack) tool(name string, args map[string]any) (string, *toolError) {
	if _, ok := args["datapack_id"]; !ok {
		args["datapack_id"] = s.datapack.ID
	}
	res, err := s.mcp.CallTool(s.t, name, args)
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

type toolError struct{ tool, text, code string }

func (e *toolError) Error() string { return fmt.Sprintf("%s: %s", e.tool, strings.TrimSpace(e.text)) }

func (s *memStack) createConversation() error {
	text, terr := s.tool("conversation_create", map[string]any{"title": "mekoqa " + s.t.ResourceSuffix()})
	if terr != nil {
		return terr
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(text), &body); err != nil || body.ID == "" {
		return fmt.Errorf("conversation_create answered %q, want JSON with an id", text)
	}
	s.conversion = body.ID
	return nil
}

// waitCall waits until the mock has a call on route that holds marker, and
// returns every such call.
func waitCall(t *core.T, c *llmmock.Client, route, marker string, atLeast int, within time.Duration) ([]llmmock.Call, error) {
	var calls []llmmock.Call
	err := t.WaitFor(fmt.Sprintf("the LLM mock to see %d %s call(s) carrying %q", atLeast, route, marker), within, mockPoll, func() (bool, error) {
		var err error
		calls, err = c.Calls(context.Background(), llmmock.Filter{Route: route, Contains: marker})
		return len(calls) >= atLeast, err
	})
	return calls, err
}

// chatCalls are the mock's chat-style calls holding marker, whichever API the
// caller used (chat completions, responses, Converse).
func chatCalls(c *llmmock.Client, marker string) ([]llmmock.Call, error) {
	return c.Calls(context.Background(), llmmock.Filter{Route: llmmock.RouteChat, Contains: marker})
}

// createDatapack retries the 500 that concurrent creates get on a stack with one
// universe: AssignUniverse locks the universe row with FOR UPDATE SKIP LOCKED, so
// a create that finds it locked reports "no available universe with capacity".
func createDatapack(t *core.T, api *mekoapi.Client, name string) (mekoapi.Datapack, error) {
	var dp mekoapi.Datapack
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		dp, err = api.CreateDatapack(t, mekoapi.CreateDatapackRequest{Name: name})
		if err == nil || !strings.Contains(err.Error(), "no available universe with capacity") {
			if attempt > 0 {
				t.Metric("datapack.create_capacity_retries", float64(attempt), "count")
			}
			return dp, err
		}
		select {
		case <-t.Done():
			return dp, err
		case <-time.After(time.Duration(1+attempt) * time.Second):
		}
	}
	return dp, err
}
