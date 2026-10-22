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
	"strings"
	"time"

	"github.com/google/uuid"
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
	json, mcp, force       bool
	noWait                 bool
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

// datapackID is the datapack a --datapack flag names: an id as it is, a name looked up.
func (t *toolSession) datapackID(ctx context.Context, named string) (string, error) {
	if named == "" {
		return "", nil
	}
	if _, err := uuid.Parse(named); err == nil {
		return named, nil
	}
	text, err := t.call(ctx, "datapack_list", map[string]any{})
	if err != nil {
		return "", err
	}
	var rows []struct {
		ID   string `json:"datapack_id"`
		Name string `json:"datapack_name"`
	}
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		return "", err
	}
	for _, r := range rows {
		if r.Name == named {
			return r.ID, nil
		}
	}
	return "", fmt.Errorf("no datapack is named %q (meko datapacks lists them)", named)
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

// runTool runs a tool command (search, add_memory, datapacks, create_datapack, delete_datapack, upload,
// upload_status) against the running server.
func runTool(l paths.Layout, cmd string, f toolFlags, stdin io.Reader, stdout, stderr io.Writer) int {
	up, err := running(l.Lock)
	if err != nil {
		return fail(stderr, err)
	}
	if !up {
		fmt.Fprintf(stderr, "meko: Meko isn't running (data in %s). Start it with meko start.\n", l.Root)
		return exitNotRunning
	}
	timeout := toolTimeout
	if cmd == "upload" {
		timeout = uploadTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	t, err := connectTools(ctx, l)
	if err != nil {
		return fail(stderr, fmt.Errorf("the server is running, but %w", err))
	}
	defer func() { _ = t.s.Close() }()
	switch cmd {
	case "datapacks":
		err = toolDatapacks(ctx, t, f, stdout)
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
	}
	if errors.Is(err, errDeclined) {
		fmt.Fprintln(stderr, "Nothing was deleted.")
		return exitError
	}
	if te := (*toolError)(nil); errors.As(err, &te) && te.code == "datapack_id_required" {
		err = errors.New("there's no default datapack (it was deleted): pass --datapack NAME|ID " +
			"(meko datapacks lists them)")
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

func toolDatapacks(ctx context.Context, t *toolSession, f toolFlags, stdout io.Writer) error {
	text, err := t.call(ctx, "datapack_list", map[string]any{})
	if err != nil || f.json {
		fmt.Fprintln(stdout, text)
		return err
	}
	var rows []struct {
		ID          string `json:"datapack_id"`
		Name        string `json:"datapack_name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		return err
	}
	for _, r := range rows {
		line := r.ID + "  " + r.Name
		if r.Description != "" {
			line += "  (" + r.Description + ")"
		}
		fmt.Fprintln(stdout, line)
	}
	return nil
}

func toolCreateDatapack(ctx context.Context, t *toolSession, f toolFlags, stdout io.Writer) error {
	name := strings.TrimSpace(f.text)
	if name == "" {
		return errors.New("give the new datapack's name: meko create_datapack <name>")
	}
	reply, err := t.call(ctx, "datapack_create", map[string]any{"name": name})
	if err != nil || f.json {
		fmt.Fprintln(stdout, reply)
		return err
	}
	var r struct {
		ID   string `json:"datapack_id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(reply), &r); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Created datapack %s (%s).\n", r.Name, r.ID)
	return nil
}

// errDeclined means the user answered no to a command's question.
var errDeclined = errors.New("declined")

// toolDeleteDatapack deletes a datapack, named or by id, after asking (unless --force):
// everything in it goes for good.
func toolDeleteDatapack(ctx context.Context, t *toolSession, f toolFlags, stdin io.Reader, stdout io.Writer) error {
	named := strings.TrimSpace(f.text)
	if named == "" {
		return errors.New("name the datapack to delete: meko delete_datapack <name|id>")
	}
	id, err := t.datapackID(ctx, named)
	if err != nil {
		return err
	}
	if !f.force {
		fmt.Fprintf(stdout, "This deletes datapack %s and everything in it (memories, conversations, "+
			"artifacts) for good.\n", named)
		if def, derr := t.defaultDatapackID(ctx); derr == nil && def == id {
			fmt.Fprintln(stdout, "It's the default datapack: Meko doesn't make another, so search and "+
				"add_memory will need --datapack afterwards.")
		}
		if !confirm(stdin, stdout, "Delete it? [y/N] ") {
			return errDeclined
		}
		// The answer may have taken a while: the delete gets a whole timeout of its own.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), toolTimeout)
		defer cancel()
	}
	reply, err := t.call(ctx, "datapack_delete", map[string]any{"datapack_id": id})
	if err != nil || f.json {
		fmt.Fprintln(stdout, reply)
		return err
	}
	fmt.Fprintf(stdout, "Deleted datapack %s.\n", named)
	return nil
}

func toolAdd(ctx context.Context, t *toolSession, f toolFlags, stdin io.Reader, stdout io.Writer) error {
	text := f.text
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
	dp, err := t.datapackID(ctx, f.datapack)
	if err != nil {
		return err
	}
	reply, err := t.call(ctx, "memory_add", map[string]any{"text": strings.TrimSpace(text), "datapack_id": dp})
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
		fmt.Fprintf(stdout, "Added memory %s.\n", m.ID)
	}
	return nil
}

func toolSearch(ctx context.Context, t *toolSession, f toolFlags, stdout io.Writer) error {
	if strings.TrimSpace(f.text) == "" {
		return errors.New("give a question to search for: meko search \"<question>\"")
	}
	dp, err := t.datapackID(ctx, f.datapack)
	if err != nil {
		return err
	}
	reply, err := t.call(ctx, "context_search", map[string]any{"query": f.text, "datapack_id": dp,
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
		fmt.Fprintln(stdout, "Nothing found.")
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
