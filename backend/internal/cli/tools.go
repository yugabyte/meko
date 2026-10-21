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

// toolFlags are the flags and argument of search, add, datapacks and token.
type toolFlags struct {
	text, datapack, source string
	limit                  int
	json, mcp              bool
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

// call calls a tool as the CLI's agent and returns its reply's text, or its in-band error.
func (t *toolSession) call(ctx context.Context, name string, args map[string]any) (string, error) {
	args["conversation_id"] = cliConversation
	if _, ok := args["agent_id"]; !ok && name != "datapack_list" {
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

// runTool runs search, add or datapacks against the running server.
func runTool(l paths.Layout, cmd string, f toolFlags, stdin io.Reader, stdout, stderr io.Writer) int {
	up, err := running(l.Lock)
	if err != nil {
		return fail(stderr, err)
	}
	if !up {
		fmt.Fprintf(stderr, "meko: Meko isn't running (data in %s). Start it with meko start.\n", l.Root)
		return exitNotRunning
	}
	ctx, cancel := context.WithTimeout(context.Background(), toolTimeout)
	defer cancel()
	t, err := connectTools(ctx, l)
	if err != nil {
		return fail(stderr, fmt.Errorf("the server is running, but %w", err))
	}
	defer func() { _ = t.s.Close() }()
	switch cmd {
	case "datapacks":
		err = toolDatapacks(ctx, t, f, stdout)
	case "add":
		err = toolAdd(ctx, t, f, stdin, stdout)
	case "search":
		err = toolSearch(ctx, t, f, stdout)
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
	if len(r.Results.Memory) == 0 && (r.Results.Conversation == nil || len(r.Results.Conversation.Hits) == 0) {
		fmt.Fprintln(stdout, "Nothing found.")
		return nil
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
