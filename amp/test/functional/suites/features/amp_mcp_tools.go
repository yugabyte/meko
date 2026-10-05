// The MCP server (ac-mcp.md) is the agent-facing surface and had no test: the
// tool catalog, the input schemas, the async contract (mutations hand back the
// controller's Task, the caller polls get_task) and the deliberate absence of
// user tools. Driven over the streamable-HTTP transport through a port-forward
// to the chart's amp-controller-mcp Service, with the arguments built from each
// tool's own inputSchema so a renamed parameter fails here, not in an agent.
package features

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	mcpProtocolVersion = "2025-06-18"
	mcpMinTools        = 33
	mcpForwardTimeout  = 2 * time.Minute
	mcpReadyTimeout    = 4 * time.Minute
	// mcpPodSelector is the chart's own label for the MCP deployment's pods
	// (mcpSelectorLabels in _helpers.tpl).
	mcpPodSelector = "app=amp-controller-mcp"
	mcpCallTimeout = 60 * time.Second
)

// mcpExpectedTools must all be in the catalog; mcpAbsentTools must not.
var (
	mcpExpectedTools = []string{
		"create_storage_pool", "list_storage_pools", "create_cluster", "delete_cluster",
		"create_branch", "delete_branch", "create_endpoint", "resize_endpoint", "delete_endpoint",
		"get_task", "create_migration", "get_migration", "start_migration_assessment", "start_migration", "end_migration",
	}
	mcpAsyncTools  = []string{"create_cluster", "delete_cluster", "create_branch", "delete_branch", "create_endpoint", "resize_endpoint", "delete_endpoint"}
	mcpAbsentTools = []string{"create_user", "list_users"}
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_mcp_tools",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "gapcoverage"},
		OwningTeam:  core.TeamControlPlane,
		Description: "The MCP server over its http transport: /healthz answers, initialize negotiates, tools/list carries at least 30 tools including every resource and migration tool and neither user tool, every tool has an object inputSchema, every asynchronous mutation's description points the caller at get_task; then create_branch is called with arguments derived from its schema, returns the controller's Task, get_task polls it to succeeded, and the branch exists through the REST API; a call with a missing required argument is an error, not a branch.",
		Params:      []string{"functional", "functional.mcp", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     20 * time.Minute,
		Run:         runAmpMCPTools,
	})
}

func runAmpMCPTools(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"mcp_tools", a.mcpTools}}
	})
}

// mcpClient is a minimal streamable-HTTP MCP client: one session, JSON-RPC
// over POST, answers either as JSON or as a single SSE event.
type mcpClient struct {
	url     string
	hc      *http.Client
	session string
	next    int
}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type mcpCallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

func (m *mcpClient) rpc(ctx context.Context, method string, params any, out any) error {
	m.next++
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.next, "method": method, "params": params})
	if err != nil {
		return err
	}
	return m.post(ctx, body, out)
}

func (m *mcpClient) notify(ctx context.Context, method string) error {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	return m.post(ctx, body, nil)
}

func (m *mcpClient) post(ctx context.Context, body []byte, out any) error {
	cctx, cancel := context.WithTimeout(ctx, mcpCallTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, m.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if m.session != "" {
		req.Header.Set("Mcp-Session-Id", m.session)
	}
	resp, err := m.hc.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", m.url, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		m.session = sid
	}
	if resp.StatusCode == http.StatusAccepted && out == nil {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("POST %s: HTTP %d: %s", m.url, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out == nil {
		return nil
	}
	payload := mcpPayload(resp.Header.Get("Content-Type"), data)
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return fmt.Errorf("decode MCP reply %q: %w", strings.TrimSpace(string(payload)), err)
	}
	if env.Error != nil {
		return &mcpError{Code: env.Error.Code, Message: env.Error.Message}
	}
	return json.Unmarshal(env.Result, out)
}

type mcpError struct {
	Code    int
	Message string
}

func (e *mcpError) Error() string { return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message) }

// mcpPayload lifts the JSON out of an SSE frame when the server streamed it.
func mcpPayload(contentType string, data []byte) []byte {
	if !strings.Contains(contentType, "text/event-stream") {
		return data
	}
	var last []byte
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "data:") {
			last = []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	return last
}

func (a *ampFeatureRun) mcpTools(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	kube := a.f.ampKube()
	if kube == nil {
		a.f.t.Skip("the control plane is reached by a direct URL with no kube access, so the MCP Service cannot be port-forwarded")
		return nil
	}
	svc := a.f.t.Options.GetString("functional.mcp.service", "amp-controller-mcp")
	port := a.f.t.Options.GetInt("functional.mcp.port", 8000)
	if _, err := kube.Get(ctx, "service", svc, "{.metadata.name}"); err != nil {
		a.f.t.Skip(fmt.Sprintf("no Service %s in namespace %s (mcp.enabled=false on this control plane): %v", svc, kube.Namespace(), err))
		return nil
	}
	// The Service exists, so the release asked for an MCP server; a pod that never
	// serves is that release failing, not this test's environment. Waiting first
	// also keeps the port-forward error from standing in for the real reason.
	if err := mcpPodReady(ctx, kube, mcpReadyTimeout); err != nil {
		return err
	}
	fctx, cancel := context.WithTimeout(ctx, mcpForwardTimeout)
	pf, err := kube.PortForward(fctx, "svc/"+svc, 0, port)
	cancel()
	if err != nil {
		return core.AsEnvFailure(fmt.Errorf("port-forward the MCP Service: %w", err))
	}
	defer pf.Close()
	base := "http://" + pf.LocalAddr()
	hc := &http.Client{Timeout: mcpCallTimeout}

	if err := mcpHealthz(ctx, hc, base); err != nil {
		return err
	}
	m := &mcpClient{url: base + "/mcp", hc: hc}
	var initOut struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	initParams := map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "ampqa", "version": "1"},
	}
	if err := m.rpc(ctx, "initialize", initParams, &initOut); err != nil {
		m.url = base + "/mcp/"
		if err2 := m.rpc(ctx, "initialize", initParams, &initOut); err2 != nil {
			return fmt.Errorf("MCP initialize failed on /mcp (%v) and /mcp/ (%v)", err, err2)
		}
	}
	if initOut.ProtocolVersion == "" {
		return fmt.Errorf("initialize returned no protocolVersion")
	}
	a.f.logf("MCP server %s %s, protocol %s, session %q", initOut.ServerInfo.Name, initOut.ServerInfo.Version, initOut.ProtocolVersion, a.f.t.Mask(m.session))
	if err := m.notify(ctx, "notifications/initialized"); err != nil {
		return fmt.Errorf("notifications/initialized: %w", err)
	}

	var list struct {
		Tools []mcpTool `json:"tools"`
	}
	if err := m.rpc(ctx, "tools/list", map[string]any{}, &list); err != nil {
		return fmt.Errorf("tools/list: %w", err)
	}
	tools := map[string]mcpTool{}
	for _, t := range list.Tools {
		tools[t.Name] = t
	}
	if err := mcpCatalogVerdict(tools); err != nil {
		return err
	}
	a.f.logf("%d tools listed", len(tools))

	pool, err := cl.GetStoragePool(ctx, a.proj.poolID)
	if err != nil {
		return fmt.Errorf("read the pool: %w", err)
	}
	branch := "mcp-" + a.f.suffix()
	known := map[string]any{
		"pool_id": a.proj.poolID, "storage_pool_id": a.proj.poolID,
		"cluster_id": a.proj.projID, "project_id": a.proj.projID,
		"parent_branch_id": a.proj.branchID, "branch_id": a.proj.branchID,
		"owner_user_id": pool.OwnerUserID, "user_id": pool.OwnerUserID,
		"name": branch, "branch_name": branch,
	}
	args, err := mcpArgs(tools["create_branch"], known)
	if err != nil {
		return err
	}
	var task controller.Task
	if err := mcpCallJSON(ctx, m, "create_branch", args, &task); err != nil {
		return fmt.Errorf("tools/call create_branch %v: %w", args, err)
	}
	if task.ID == "" || task.Kind == "" {
		return fmt.Errorf("create_branch returned no Task (id %q kind %q); the async contract is the controller's Task, unchanged", task.ID, task.Kind)
	}
	var done controller.Task
	err = a.f.t.WaitFor("get_task to report the branch created", 5*time.Minute, 2*time.Second, func() (bool, error) {
		targs, err := mcpArgs(tools["get_task"], map[string]any{"task_id": task.ID, "id": task.ID})
		if err != nil {
			return false, err
		}
		if err := mcpCallJSON(ctx, m, "get_task", targs, &done); err != nil {
			return false, err
		}
		return done.Status == controller.TaskSucceeded || done.Status == controller.TaskFailed, nil
	})
	if err != nil {
		return fmt.Errorf("polling get_task(%s): %w", task.ID, err)
	}
	if done.Status != controller.TaskSucceeded {
		msg := ""
		if done.Error != nil {
			msg = *done.Error
		}
		return fmt.Errorf("the create_branch task %s failed: %s", task.ID, msg)
	}
	if _, err := findBranchCtl(ctx, cl, a.proj.poolID, a.proj.projID, branch); err != nil {
		return fmt.Errorf("the branch create_branch reported succeeded is not in the REST listing: %w", err)
	}

	// A required argument left out is a tool error, not a branch.
	delete(args, "name")
	delete(args, "branch_name")
	var res mcpCallResult
	err = m.rpc(ctx, "tools/call", map[string]any{"name": "create_branch", "arguments": args}, &res)
	if err == nil && !res.IsError {
		return fmt.Errorf("create_branch without its name argument returned a non-error result: %s", mcpText(res))
	}
	return nil
}

// mcpPodReady waits for the MCP deployment's pod to report Ready and, when it
// does not, fails with what the pod actually says. The reason matters: an image
// tag the registry does not carry is a packaging defect, and it reaches a caller
// as an unexplained "pod is not running" from the port-forward otherwise.
func mcpPodReady(ctx context.Context, kube *k8s.Kubectl, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		ready, err := kube.GetSelector(ctx, "pod", mcpPodSelector,
			`{.items[*].status.containerStatuses[*].ready}`)
		if err == nil && strings.Contains(ready, "true") {
			return nil
		}
		last = mcpPodTrouble(ctx, kube)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("the release deployed an MCP Service but no MCP pod became ready within %s: %s", timeout, last)
}

// mcpPodTrouble is the pod's own account of why it is not up.
func mcpPodTrouble(ctx context.Context, kube *k8s.Kubectl) string {
	names, err := kube.GetSelector(ctx, "pod", mcpPodSelector, "{.items[*].metadata.name}")
	if err != nil || strings.TrimSpace(names) == "" {
		return "no pod matches the MCP deployment's selector"
	}
	pod := strings.Fields(names)[0]
	if t := kube.PodTrouble(ctx, pod); strings.TrimSpace(t) != "" {
		return pod + ": " + t
	}
	state, _ := kube.Get(ctx, "pod", pod, `{.status.phase}{" "}{.status.containerStatuses[*].state}`)
	return pod + ": " + state
}

func mcpHealthz(ctx context.Context, hc *http.Client, base string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("GET /healthz on the MCP server: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "ok") {
		return fmt.Errorf("MCP /healthz answered HTTP %d %q, want 200 with status ok", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// mcpCatalogVerdict holds the catalog shape ac-mcp.md documents.
func mcpCatalogVerdict(tools map[string]mcpTool) error {
	if len(tools) < mcpMinTools {
		return fmt.Errorf("tools/list returned %d tools, want at least %d", len(tools), mcpMinTools)
	}
	for _, name := range mcpExpectedTools {
		if _, ok := tools[name]; !ok {
			return fmt.Errorf("tool %s is missing from the catalog", name)
		}
	}
	for _, name := range mcpAbsentTools {
		if _, ok := tools[name]; ok {
			return fmt.Errorf("tool %s is in the catalog; ac-mcp.md leaves user management out on purpose", name)
		}
	}
	for name, t := range tools {
		var schema struct {
			Type string `json:"type"`
		}
		if len(t.InputSchema) == 0 || json.Unmarshal(t.InputSchema, &schema) != nil || schema.Type != "object" {
			return fmt.Errorf("tool %s has no object inputSchema: %s", name, string(t.InputSchema))
		}
		if strings.TrimSpace(t.Description) == "" {
			return fmt.Errorf("tool %s has no description", name)
		}
	}
	for _, name := range mcpAsyncTools {
		if !strings.Contains(tools[name].Description, "get_task") {
			return fmt.Errorf("the asynchronous tool %s does not tell the caller to poll get_task: %q", name, tools[name].Description)
		}
	}
	for _, name := range []string{"start_migration_assessment", "start_migration", "end_migration"} {
		if !strings.Contains(tools[name].Description, "get_migration") {
			return fmt.Errorf("%s does not point the caller at get_migration, the surface that reports the migration's progress: %q", name, tools[name].Description)
		}
	}
	return nil
}

// mcpArgs fills a tool's required properties from known values, so the test
// follows the schema instead of guessing parameter names.
func mcpArgs(t mcpTool, known map[string]any) (map[string]any, error) {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
		return nil, fmt.Errorf("decode inputSchema of %s: %w", t.Name, err)
	}
	args := map[string]any{}
	for _, name := range schema.Required {
		v, ok := known[name]
		if !ok {
			return nil, fmt.Errorf("tool %s requires argument %q the test has no value for; schema: %s", t.Name, name, string(t.InputSchema))
		}
		args[name] = v
	}
	// Optional properties the test has a value for and that select the resource.
	for name := range schema.Properties {
		if _, set := args[name]; set {
			continue
		}
		if v, ok := known[name]; ok && strings.HasSuffix(name, "_id") {
			args[name] = v
		}
	}
	return args, nil
}

// mcpCallJSON calls a tool and decodes the JSON it returned, from
// structuredContent when present, else from the first text block.
func mcpCallJSON(ctx context.Context, m *mcpClient, name string, args map[string]any, out any) error {
	var res mcpCallResult
	if err := m.rpc(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &res); err != nil {
		return err
	}
	if res.IsError {
		return fmt.Errorf("tool error: %s", mcpText(res))
	}
	if len(res.StructuredContent) > 0 {
		if err := json.Unmarshal(res.StructuredContent, out); err == nil {
			return nil
		}
		var wrapped struct {
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(res.StructuredContent, &wrapped) == nil && len(wrapped.Result) > 0 {
			return json.Unmarshal(wrapped.Result, out)
		}
	}
	text := mcpText(res)
	if err := json.Unmarshal([]byte(text), out); err != nil {
		return fmt.Errorf("tool %s returned text that is not the JSON the caller must parse: %q", name, text)
	}
	return nil
}

func mcpText(res mcpCallResult) string {
	for _, c := range res.Content {
		if c.Type == "text" {
			return c.Text
		}
	}
	return ""
}
