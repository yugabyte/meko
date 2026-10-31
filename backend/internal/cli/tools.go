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

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yugabyte/meko/backend/internal/auth"
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

// target is the datapack a command names, by id and name, and whether it's Meko Cloud's: while Meko
// Cloud is linked, /mcp serves only its datapacks. The zero value is this Meko's default datapack.
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

// tools is a command's way to the datapacks: the running server's MCP session, which serves the
// linked Meko Cloud's datapacks while it's linked (docs/cloud.md).
type tools struct {
	*toolSession
	l      paths.Layout
	linked *bool
}

func newTools(t *toolSession, l paths.Layout) *tools { return &tools{toolSession: t, l: l} }

func (t *tools) close() { _ = t.s.Close() }

// isLinked says whether the running server is linked to Meko Cloud, asked once; false when it can't
// say. It only words the messages: /mcp picks the side.
func (t *tools) isLinked(ctx context.Context) bool {
	if t.linked == nil {
		linked := false
		if c, err := newClient(t.l); err == nil {
			var st struct {
				Linked bool `json:"linked"`
			}
			if c.send(ctx, http.MethodGet, "/api/v1/settings/meko-cloud", nil, &st) == nil {
				linked = st.Linked
			}
		}
		t.linked = &linked
	}
	return *t.linked
}

// datapackRow is a datapack as datapack_list and GET /api/v1/datapacks list it.
type datapackRow struct {
	ID          string `json:"datapack_id"`
	Name        string `json:"datapack_name"`
	Description string `json:"description"`
}

// pick finds the datapack named among rows, by id or name; what lists them goes in the errors.
func pick(rows []datapackRow, named, lists string) (datapackRow, error) {
	var found []datapackRow
	for _, r := range rows {
		if strings.EqualFold(r.ID, named) {
			return r, nil
		}
		if r.Name == named {
			found = append(found, r)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return datapackRow{}, fmt.Errorf("no datapack is named %q (%s lists them)", named, lists)
	}
	return datapackRow{}, fmt.Errorf("%d datapacks are named %q: give its id (%s --json lists them)", len(found),
		named, lists)
}

// resolve finds the datapack named, by name or id, among the ones /mcp lists: this Meko's, or while
// Meko Cloud is linked, its. "" is the default datapack.
func (t *tools) resolve(ctx context.Context, named string) (target, error) {
	cloud := t.isLinked(ctx)
	if named == "" {
		return target{cloud: cloud}, nil
	}
	text, err := t.call(ctx, "datapack_list", map[string]any{})
	if err != nil {
		return target{}, err
	}
	var rows []datapackRow
	if err = json.Unmarshal([]byte(text), &rows); err != nil {
		return target{}, err
	}
	r, err := pick(rows, named, "meko-cli datapacks")
	return target{cloud: cloud, id: r.ID, name: r.Name}, err
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
		err = toolDatapacks(ctx, t.toolSession, f, stdout)
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
	te := (*toolError)(nil)
	switch {
	case errors.As(err, &te) && te.code == "datapack_id_required":
		err = errors.New("there's no default datapack (it was deleted): pass --datapack NAME|ID " +
			"(meko-cli datapacks lists them)")
	case errors.As(err, &te) && strings.HasPrefix(te.code, "cloud_"):
		// Meko Cloud wasn't reached, or refused the key: its detail says what to do.
		err = errors.New(strings.TrimSuffix(te.detail, "."))
	}
	if err != nil {
		return failAs(prog, stderr, err)
	}
	return exitOK
}

// toolDatapacks lists the datapacks /mcp lists (the linked Meko Cloud's while it's linked), by
// name: just names and descriptions, fitted to the terminal. Ids are --json's.
func toolDatapacks(ctx context.Context, t *toolSession, f toolFlags, stdout io.Writer) error {
	text, err := t.call(ctx, "datapack_list", map[string]any{})
	if err != nil || f.json {
		fmt.Fprintln(stdout, text)
		return err
	}
	var rows []datapackRow
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		return err
	}
	list := make([]nameDesc, 0, len(rows))
	for _, r := range rows {
		list = append(list, nameDesc{r.Name, r.Description})
	}
	slices.SortStableFunc(list, func(a, b nameDesc) int {
		return strings.Compare(strings.ToLower(a.name), strings.ToLower(b.name))
	})
	if len(list) == 0 {
		fmt.Fprintln(stdout, "No datapacks")
	}
	fmt.Fprint(stdout, nameDescTable(list, termWidth(stdout)))
	return nil
}

// toolCreateDatapack creates a datapack through datapack_create: in the linked Meko Cloud while
// it's linked, else here.
func toolCreateDatapack(ctx context.Context, t *tools, f toolFlags, stdout io.Writer) error {
	name := strings.TrimSpace(f.text)
	if name == "" {
		return errors.New("give the new datapack's name: meko-cli create_datapack <name>")
	}
	reply, err := t.call(ctx, "datapack_create", map[string]any{"name": name})
	if err != nil {
		fmt.Fprintln(stdout, reply)
		return err
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
	fmt.Fprintf(stdout, "Created %s (%s)\n", target{cloud: t.isLinked(ctx), name: r.Name}.noun(), r.ID)
	return nil
}

// errDeclined means the user answered no to a command's question.
var errDeclined = errors.New("declined")

// toolDeleteDatapack deletes a datapack, named or by id, through /mcp, after asking
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
	reply, err := t.call(ctx, "datapack_delete", map[string]any{"datapack_id": dp.id})
	if err != nil {
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
	reply, err := t.call(ctx, "memory_add", map[string]any{"text": strings.TrimSpace(text), "datapack_id": dp.id})
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
	reply, err := t.call(ctx, "context_search", map[string]any{"query": f.text, "datapack_id": dp.id,
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
