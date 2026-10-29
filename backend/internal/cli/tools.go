package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yugabyte/meko/backend/internal/auth"
	"github.com/yugabyte/meko/backend/internal/cloudlink"
	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/version"
)

// cliAgent is the agent id of the CLI's MCP calls.
const cliAgent = "meko_cli"

// cliConversation is the conversation id the CLI's calls carry (Meko Cloud's tools require one):
// the same for every call, derived from "meko-cli" as a session id would be.
var cliConversation = func() string {
	sum := sha256.Sum256([]byte("meko-cli"))
	return hex.EncodeToString(sum[:16])
}()

// toolTimeout bounds one CLI tool call: a search of a large datapack takes about a second.
const toolTimeout = time.Minute

// toolFlags are the flags and argument of the tool commands and token.
type toolFlags struct {
	text, datapack, source string
	files                  []string
	limit                  int
	json, force            bool
	cloud, local           bool // create_datapack's --cloud, datapacks' --local
	noWait                 bool
	out                    string // export's -o
}

// toolError is a tool's in-band error reply.
type toolError struct{ code, detail string }

func (e *toolError) Error() string {
	if e.detail == "" {
		return e.code
	}
	return e.code + ": " + e.detail
}

// toolSession is the CLI's MCP session with the running server, as the admin.
type toolSession struct{ s *sdk.ClientSession }

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// connectTools opens the CLI's MCP session with the running server, as the admin.
func connectTools(ctx context.Context, l paths.Layout) (*toolSession, error) {
	port, err := readInt(l.Port)
	if err != nil {
		return nil, fmt.Errorf("can't find the server's port: %w", err)
	}
	token, err := auth.ReadToken(l.Token)
	if err != nil {
		return nil, err
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "meko-cli", Version: version.Version}, nil)
	s, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:   fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
		HTTPClient: &http.Client{Transport: bearer{token}},
	}, nil)
	if err != nil {
		return nil, err
	}
	return &toolSession{s}, nil
}

// call calls a tool, as the CLI's agent where the tool takes one (the datapack tools don't), and
// returns its reply's text, or its in-band error.
func (t *toolSession) call(ctx context.Context, name string, args map[string]any) (string, error) {
	args["conversation_id"] = cliConversation
	if _, ok := args["agent_id"]; !ok && !strings.HasPrefix(name, "datapack_") {
		args["agent_id"] = cliAgent
	}
	res, err := t.s.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "", err
	}
	text := ""
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*sdk.TextContent); ok {
			text = tc.Text
		}
	}
	if res.IsError {
		return "", errors.New(text)
	}
	var e struct {
		Error  any    `json:"error"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal([]byte(text), &e) == nil && e.Error != nil {
		return "", &toolError{fmt.Sprint(e.Error), e.Detail}
	}
	return text, nil
}

// The prefixes that pick a datapack's side: the linked Meko Cloud's, or this Meko's.
const (
	cloudPrefix = "cloud:"
	localPrefix = "local:"
)

// splitSide parses a datapack's name or id, with cloud: or local: before it to pick the side.
func splitSide(named string) (side, rest string) {
	if rest, ok := strings.CutPrefix(named, cloudPrefix); ok {
		return "cloud", rest
	}
	if rest, ok := strings.CutPrefix(named, localPrefix); ok {
		return "local", rest
	}
	return "", named
}

// target is the datapack a command names: this Meko's, or the linked Meko Cloud's (cloud). The zero
// value is this Meko's default datapack.
type target struct {
	cloud    bool
	id, name string
}

// noun names the datapack in a message, saying when it's Meko Cloud's.
func (d target) noun() string {
	if d.cloud {
		return "Meko Cloud datapack " + d.name
	}
	return "datapack " + d.name
}

// spec names the datapack for a later command: cloud:<id> for Meko Cloud's, so it isn't looked up
// again.
func (d target) spec() string {
	if d.cloud {
		return cloudPrefix + d.id
	}
	return d.id
}

// tools is a command's way to the datapacks it names: the running server's MCP session, and the
// linked Meko Cloud, opened only when the command needs it.
type tools struct {
	*toolSession
	l        paths.Layout
	cloud    *cloudSide
	cloudErr error
	opened   bool
}

func newTools(t *toolSession, l paths.Layout) *tools { return &tools{toolSession: t, l: l} }

func (t *tools) close() { _ = t.s.Close() }

// cloudSide is the way to the linked Meko Cloud's datapacks, opened on first use.
func (t *tools) cloudSide() (*cloudSide, error) {
	if !t.opened {
		t.cloud, t.cloudErr = openCloud(t.l)
		t.opened = true
	}
	return t.cloud, t.cloudErr
}

// cloudCall calls one of the running server's /api/v1 routes for a Meko Cloud datapack, and returns
// its reply's data as text, as a tool's reply is.
func (t *tools) cloudCall(ctx context.Context, method, path string, body any) (string, error) {
	c, err := t.cloudSide()
	if err != nil {
		return "", err
	}
	data, err := c.api(ctx, method, path, body)
	return string(data), err
}

// tool calls a tool on a datapack: this Meko's through /mcp, or the linked Meko Cloud's through the
// server's route that runs it there (path, under the datapack's), with the same arguments.
func (t *tools) tool(ctx context.Context, d target, name, path string, args map[string]any) (string, error) {
	if !d.cloud {
		return t.call(ctx, name, args)
	}
	delete(args, "datapack_id")
	args["agent_id"], args["conversation_id"] = cliAgent, cliConversation
	return t.cloudCall(ctx, http.MethodPost, cloudDatapackPath(d.id, path), args)
}

// resolve finds the datapack named: "" is this Meko's default. A name or id is looked up among this
// Meko's datapacks, then, while Meko Cloud is linked, among its, so a name both have is this Meko's.
// cloud: or local: before it looks on that side only, and cloud:<id> is taken as it is (Meko Cloud
// says when it's wrong), so the prompt's use doesn't list Meko Cloud's datapacks on every line.
func (t *tools) resolve(ctx context.Context, named string) (target, error) {
	if named == "" {
		return target{}, nil
	}
	side, rest := splitSide(named)
	if rest == "" {
		return target{}, fmt.Errorf("name the datapack after %s: (meko-cli datapacks lists them)", side)
	}
	if side != "cloud" {
		text, err := t.call(ctx, "datapack_list", map[string]any{})
		if err != nil {
			return target{}, err
		}
		var rows []struct {
			ID   string `json:"datapack_id"`
			Name string `json:"datapack_name"`
		}
		if err := json.Unmarshal([]byte(text), &rows); err != nil {
			return target{}, err
		}
		for _, r := range rows {
			if strings.EqualFold(r.ID, rest) || r.Name == rest {
				return target{id: r.ID, name: r.Name}, nil
			}
		}
	}
	if side == "local" {
		return target{}, fmt.Errorf("no datapack is named %q here (meko-cli datapacks --local lists them)", rest)
	}
	c, err := t.cloudSide()
	if err != nil {
		return target{}, err
	}
	if _, perr := uuid.Parse(rest); perr == nil && side == "cloud" {
		return target{cloud: true, id: rest, name: rest}, nil
	}
	rows, err := c.datapacks(ctx)
	if side == "" && errors.Is(err, errNotLinked) {
		return target{}, fmt.Errorf("no datapack is named %q (meko-cli datapacks lists them)", rest)
	}
	if err != nil {
		return target{}, err
	}
	var found []target
	for _, d := range rows {
		if strings.EqualFold(d.DatapackID, rest) || d.DatapackName == rest {
			found = append(found, target{cloud: true, id: d.DatapackID, name: d.DatapackName})
		}
	}
	switch {
	case len(found) == 1:
		return found[0], nil
	case len(found) > 1:
		return target{}, fmt.Errorf(
			"%d Meko Cloud datapacks are named %q: give its id (meko-cli datapacks --json lists them)", len(found), rest)
	case side == "cloud":
		return target{}, fmt.Errorf("no Meko Cloud datapack is named %q (meko-cli datapacks --json lists them "+
			"under cloud)", rest)
	}
	return target{}, fmt.Errorf("no datapack is named %q here or in Meko Cloud (meko-cli datapacks lists them)", rest)
}

// defaultDatapackID is the default datapack's id, or "" once it has been deleted (Meko makes it
// only on first start).
func (t *toolSession) defaultDatapackID(ctx context.Context) (string, error) {
	text, err := t.call(ctx, "datapack_describe", map[string]any{})
	if te := (*toolError)(nil); errors.As(err, &te) && te.code == "datapack_id_required" {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var d struct {
		ID string `json:"datapack_id"`
	}
	return d.ID, json.Unmarshal([]byte(text), &d)
}

// runTool runs one of meko-cli's tool commands (search, add_memory, datapacks, create_datapack,
// delete_datapack, upload, upload_status, export) against the running server.
func runTool(l paths.Layout, prog, cmd string, f toolFlags, stdin io.Reader, stdout, stderr io.Writer) int {
	up, err := running(l.Lock)
	if err != nil {
		return failAs(prog, stderr, err)
	}
	if !up {
		fmt.Fprintf(stderr, "%s: Meko isn't running (data in %s). Start it with meko start\n", prog, l.Root)
		return exitNotRunning
	}
	timeout := toolTimeout
	if cmd == "upload" || cmd == "export" {
		timeout = uploadTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	session, err := connectTools(ctx, l)
	if err != nil {
		return failAs(prog, stderr, fmt.Errorf("the server is running, but %w", err))
	}
	t := newTools(session, l)
	defer t.close()
	switch cmd {
	case "datapacks":
		err = toolDatapacks(ctx, t.toolSession, l, f, stdout, stderr)
	case "add_memory":
		err = toolAdd(ctx, t, f, stdin, stdout)
	case "search":
		err = toolSearch(ctx, t, f, stdout)
	case "create_datapack":
		err = toolCreateDatapack(ctx, t, f, stdout)
	case "delete_datapack":
		err = toolDeleteDatapack(ctx, t, f, stdin, stdout)
	case "upload":
		err = toolUpload(ctx, t, f, stdout)
	case "upload_status":
		err = toolUploadStatus(ctx, t, f, stdout)
	case "export":
		err = toolExport(ctx, t, f, stdout)
	}
	if errors.Is(err, errDeclined) {
		fmt.Fprintln(stderr, "Nothing was deleted")
		return exitError
	}
	if te := (*toolError)(nil); errors.As(err, &te) && te.code == "datapack_id_required" {
		err = errors.New("there's no default datapack (it was deleted): pass --datapack NAME|ID " +
			"(meko-cli datapacks lists them)")
	}
	if err != nil {
		return failAs(prog, stderr, err)
	}
	return exitOK
}

// toolDatapacks lists this Meko's datapacks and, while Meko Cloud is linked and --local isn't given,
// the linked Meko Cloud's, by name: just names and descriptions, fitted to the terminal. Where each
// lives, whether it's shared and its id are --json's. A Meko Cloud that fails leaves a warning after
// the local rows: they're still right.
func toolDatapacks(ctx context.Context, t *toolSession, l paths.Layout, f toolFlags, stdout, stderr io.Writer) error {
	text, err := t.call(ctx, "datapack_list", map[string]any{})
	if err != nil || (f.json && f.local) {
		fmt.Fprintln(stdout, text)
		return err
	}
	var rows []struct {
		Name        string `json:"datapack_name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		return err
	}
	var cloud []cloudlink.Datapack
	var cloudErr error
	linked := false
	if !f.local {
		var c *cloudSide
		if c, cloudErr = openCloud(l); cloudErr == nil {
			cloud, cloudErr = c.datapacks(ctx)
		}
		linked = cloudErr == nil
		if errors.Is(cloudErr, errNotLinked) {
			cloudErr = nil
		} else if cloudErr != nil {
			cloudErr = fmt.Errorf("couldn't list Meko Cloud's datapacks: %w", cloudErr)
		}
	}
	if f.json {
		out := map[string]any{"local": json.RawMessage(text), "cloud": map[string]any{"linked": linked,
			"datapacks": append([]cloudlink.Datapack{}, cloud...)}}
		if cloudErr != nil {
			out["cloud"] = map[string]string{"error": cloudErr.Error()}
		}
		b, _ := json.Marshal(out)
		fmt.Fprintln(stdout, string(b))
		return nil
	}
	local := make([]nameDesc, 0, len(rows))
	for _, r := range rows {
		local = append(local, nameDesc{r.Name, r.Description})
	}
	list := datapackList(local, cloud)
	if len(list) == 0 && cloudErr == nil {
		fmt.Fprintln(stdout, "No datapacks")
	}
	fmt.Fprint(stdout, nameDescTable(list, termWidth(stdout)))
	if cloudErr != nil {
		fmt.Fprintf(stderr, "%s: %v (meko-cli datapacks --local skips Meko Cloud)\n", progMekoCLI, cloudErr)
	}
	return nil
}

// datapackList is this Meko's datapacks and Meko Cloud's in one list, by name ignoring case. It's
// stable, so this Meko's datapack comes first when Meko Cloud has one of the same name, as
// --datapack picks it.
func datapackList(local []nameDesc, cloud []cloudlink.Datapack) []nameDesc {
	list := slices.Clone(local)
	for _, d := range cloud {
		list = append(list, nameDesc{d.DatapackName, d.Description})
	}
	slices.SortStableFunc(list, func(a, b nameDesc) int {
		return strings.Compare(strings.ToLower(a.name), strings.ToLower(b.name))
	})
	return list
}

// toolCreateDatapack creates a datapack here, or with --cloud in the linked Meko Cloud, through its
// REST API.
func toolCreateDatapack(ctx context.Context, t *tools, f toolFlags, stdout io.Writer) error {
	name := strings.TrimSpace(f.text)
	if name == "" {
		return errors.New("give the new datapack's name: meko-cli create_datapack <name>")
	}
	var reply string
	where := "datapack"
	if f.cloud {
		var err error
		if reply, err = t.cloudCall(ctx, http.MethodPost, "/datapacks",
			map[string]string{"name": name, "location": "cloud"}); err != nil {
			return err
		}
		where = "Meko Cloud datapack"
	} else {
		var err error
		if reply, err = t.call(ctx, "datapack_create", map[string]any{"name": name}); err != nil {
			fmt.Fprintln(stdout, reply)
			return err
		}
	}
	if f.json {
		fmt.Fprintln(stdout, reply)
		return nil
	}
	var r struct {
		ID   string `json:"datapack_id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(reply), &r); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Created %s %s (%s)\n", where, r.Name, r.ID)
	return nil
}

// errDeclined means the user answered no to a command's question.
var errDeclined = errors.New("declined")

// toolDeleteDatapack deletes a datapack, named or by id, here or in the linked Meko Cloud, after asking
// (unless --force): everything in it goes for good.
func toolDeleteDatapack(ctx context.Context, t *tools, f toolFlags, stdin io.Reader, stdout io.Writer) error {
	named := strings.TrimSpace(f.text)
	if named == "" {
		return errors.New("name the datapack to delete: meko-cli delete_datapack <name|id>")
	}
	dp, err := t.resolve(ctx, named)
	if err != nil {
		return err
	}
	if !f.force {
		fmt.Fprintf(stdout, "This deletes %s and everything in it (memories, conversations, "+
			"artifacts) for good\n", dp.noun())
		if def, derr := t.defaultDatapackID(ctx); !dp.cloud && derr == nil && def == dp.id {
			fmt.Fprintln(stdout, "It's the default datapack: Meko doesn't make another, so search and "+
				"add_memory will need --datapack afterwards")
		}
		if !confirm(stdin, stdout, "Delete it? [y/N] ") {
			return errDeclined
		}
		// The answer may have taken a while: the delete gets a whole timeout of its own.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), toolTimeout)
		defer cancel()
	}
	var reply string
	if dp.cloud {
		if reply, err = t.cloudCall(ctx, http.MethodDelete, cloudDatapackPath(dp.id, ""), nil); err != nil {
			return err
		}
	} else if reply, err = t.call(ctx, "datapack_delete", map[string]any{"datapack_id": dp.id}); err != nil {
		fmt.Fprintln(stdout, reply)
		return err
	}
	if f.json {
		fmt.Fprintln(stdout, reply)
		return nil
	}
	fmt.Fprintf(stdout, "Deleted %s\n", dp.noun())
	return nil
}

func toolAdd(ctx context.Context, t *tools, f toolFlags, stdin io.Reader, stdout io.Writer) error {
	text := f.text
	if (text == "" || text == "-") && atPrompt {
		return errors.New(`give the memory's text: add_memory "<text>"`)
	}
	if text == "" || text == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		text = string(b)
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("nothing to add: give the text, or pipe it in")
	}
	dp, err := t.resolve(ctx, f.datapack)
	if err != nil {
		return err
	}
	reply, err := t.tool(ctx, dp, "memory_add", "/memories",
		map[string]any{"text": strings.TrimSpace(text), "datapack_id": dp.id})
	if err != nil || f.json {
		fmt.Fprintln(stdout, reply)
		return err
	}
	var r struct {
		Results []struct {
			ID string `json:"id"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(reply), &r); err != nil {
		return err
	}
	for _, m := range r.Results {
		if dp.cloud {
			fmt.Fprintf(stdout, "Added memory %s to %s\n", m.ID, dp.noun())
		} else {
			fmt.Fprintf(stdout, "Added memory %s\n", m.ID)
		}
	}
	return nil
}

func toolSearch(ctx context.Context, t *tools, f toolFlags, stdout io.Writer) error {
	if strings.TrimSpace(f.text) == "" {
		return errors.New("give a question to search for: meko-cli search \"<question>\"")
	}
	dp, err := t.resolve(ctx, f.datapack)
	if err != nil {
		return err
	}
	reply, err := t.tool(ctx, dp, "context_search", "/search", map[string]any{"query": f.text, "datapack_id": dp.id,
		"force_source": f.source, "limit": f.limit})
	if err != nil || f.json {
		fmt.Fprintln(stdout, reply)
		return err
	}
	var r struct {
		Results struct {
			KnowledgeBase []struct {
				ChunkText   string   `json:"chunk_text"`
				RerankScore *float64 `json:"rerank_score"`
				Distance    float64  `json:"distance"`
				DocumentID  string   `json:"document_id"`
			} `json:"knowledge_base"`
			Memory []struct {
				Memory    string   `json:"memory"`
				Score     *float64 `json:"score"`
				AgentID   string   `json:"agent_id"`
				CreatedAt string   `json:"created_at"`
			} `json:"memory"`
			Conversation *struct {
				Hits []struct {
					Content        string  `json:"content"`
					Distance       float64 `json:"distance"`
					ConversationID string  `json:"conversation_id"`
				} `json:"hits"`
			} `json:"conversation"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(reply), &r); err != nil {
		return err
	}
	if len(r.Results.Memory) == 0 && len(r.Results.KnowledgeBase) == 0 &&
		(r.Results.Conversation == nil || len(r.Results.Conversation.Hits) == 0) {
		fmt.Fprintln(stdout, "Nothing found")
		return nil
	}
	if len(r.Results.KnowledgeBase) > 0 {
		fmt.Fprintln(stdout, "Knowledge base (relevance):")
		for _, k := range r.Results.KnowledgeBase {
			score := 1 - k.Distance
			if k.RerankScore != nil {
				score = *k.RerankScore
			}
			fmt.Fprintf(stdout, "  %.2f  %s  (document %s)\n", score, oneLine(k.ChunkText), k.DocumentID)
		}
	}
	if len(r.Results.Memory) > 0 {
		fmt.Fprintln(stdout, "Memories (score):")
		for _, m := range r.Results.Memory {
			score := 0.0
			if m.Score != nil {
				score = *m.Score
			}
			fmt.Fprintf(stdout, "  %.2f  %s  (%s, %s)\n", score, oneLine(m.Memory), m.AgentID, day(m.CreatedAt))
		}
	}
	if r.Results.Conversation != nil && len(r.Results.Conversation.Hits) > 0 {
		fmt.Fprintln(stdout, "Past conversations (distance):")
		for _, h := range r.Results.Conversation.Hits {
			fmt.Fprintf(stdout, "  %.2f  %s  (conversation %s)\n", h.Distance, oneLine(h.Content), h.ConversationID)
		}
	}
	return nil
}

// oneLine is text on one line, cut to 120 characters.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 120 {
		return string(r[:119]) + "…"
	}
	return s
}

// day is the date part of an RFC 3339 time.
func day(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}
