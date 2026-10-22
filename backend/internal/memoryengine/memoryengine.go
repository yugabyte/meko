// Package memoryengine runs and calls Meko's memory engine: mem0 in a Node child process
// (backend/memory_engine) that serve supervises. It finds the Node binary and the built memory engine, builds the
// child's spec, and is the Go client of the memory engine's API on run/mem_eng.sock.
package memoryengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/meko/backend/internal/envelope"
	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/supervisor"
)

// Name is the memory engine's child name in status and its log file's.
const Name = "memory_engine"

// MinNode is the oldest Node the memory engine runs on.
var MinNode = [3]int{22, 13, 0}

// Options say where the memory engine and its Node are. Empty means look in the usual places.
type Options struct {
	Node string // the node binary
	Dir  string // the built memory engine: the folder holding package.json and dist/main.js
	// TestEmbedder, for tests only, swaps the model for a deterministic hashing embedder.
	TestEmbedder string
	// Models is a folder of model files shipped with the release (models/ beside bin/), which the
	// engine reads without downloading anything. Empty means download into the data folder's
	// models/ (a build from source).
	Models string
}

// FindModels is the release's bundled models folder, <bin>/../models, or "" when there's none.
func FindModels() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if real, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = real
	}
	dir := filepath.Join(filepath.Dir(exe), "..", "models")
	if fi, serr := os.Stat(dir); serr != nil || !fi.IsDir() {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	return abs
}

// Errors the memory engine client returns.
var (
	// ErrUnavailable: no memory engine is answering (not found, not started, crashed, stopped).
	ErrUnavailable = errors.New("the memory engine isn't running")
)

// NotReadyError: the memory engine runs but can't serve yet (loading its model) or at all (an error it
// reports, such as a changed embedding model).
type NotReadyError struct{ Message string }

func (e *NotReadyError) Error() string { return e.Message }

// APIError is a failure the memory engine reported for one request.
type APIError struct {
	Status        int
	Code, Message string
}

func (e *APIError) Error() string { return e.Message }

// FindNode picks the Node binary: o.Node alone if it's set (configured explicitly, it replaces the
// search); otherwise the one shipped next to meko (<bin>/../node/bin/node), then node on PATH.
// It must be MinNode or newer. It doesn't check whose Node it found: AgentK's own is used only if
// meko is installed inside AgentK's app folder or AgentK's node leads PATH, which packaging
// avoids.
func FindNode(o Options) (string, error) {
	var candidates []string
	if o.Node != "" {
		candidates = []string{o.Node}
	} else {
		if exe, err := os.Executable(); err == nil {
			candidates = append(candidates, filepath.Join(filepath.Dir(exe), "..", "node", "bin", "node"))
		}
		if p, err := exec.LookPath("node"); err == nil {
			candidates = append(candidates, p)
		}
	}
	lastErr := errors.New("no node binary found. Install Node " + versionString(MinNode) +
		" or newer, or set [memory_engine] node in config.toml or MEKO_NODE")
	for _, c := range candidates {
		if _, err := os.Stat(c); err != nil {
			continue
		}
		v, err := nodeVersion(c)
		if err != nil {
			lastErr = fmt.Errorf("can't run %s: %w", c, err)
			continue
		}
		if older(v, MinNode) {
			lastErr = fmt.Errorf("%s is Node %s; the memory engine needs %s or newer",
				c, versionString(v), versionString(MinNode))
			continue
		}
		return c, nil
	}
	return "", lastErr
}

func nodeVersion(node string) ([3]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, node, "--version").Output() //nolint:gosec // G204: the node binary we pick
	if err != nil {
		return [3]int{}, err
	}
	var v [3]int
	parts := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(string(out)), "v"), ".", 3)
	for i := range parts {
		if v[i], err = strconv.Atoi(strings.SplitN(parts[i], "-", 2)[0]); err != nil {
			return [3]int{}, fmt.Errorf("can't read its version %q", strings.TrimSpace(string(out)))
		}
	}
	return v, nil
}

func older(v, minimum [3]int) bool {
	for i := range v {
		if v[i] != minimum[i] {
			return v[i] < minimum[i]
		}
	}
	return false
}

func versionString(v [3]int) string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

// FindDir picks the built memory engine: o.Dir if set, else <bin>/../memory_engine (the source tree's layout,
// where make build puts bin/meko next to memory_engine/). It must hold dist/main.js.
func FindDir(o Options) (string, error) {
	dir := o.Dir
	if dir == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(filepath.Dir(exe), "..", "memory_engine")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(dir, "dist", "main.js")); err != nil {
		return "", fmt.Errorf("the memory engine isn't built in %s (run make memory_engine), or set [memory_engine] dir in "+
			"config.toml or MEKO_MEMORY_ENGINE_DIR", dir)
	}
	return dir, nil
}

// Spec is the memory engine's child spec: node dist/main.js in the memory engine's folder, with the sockets, the
// internal token and the data folder's paths in its environment, healthy once its API answers.
func Spec(node, dir string, l paths.Layout, token string, o Options) supervisor.Spec {
	env := []string{
		"MEKO_MEMORY_ENGINE_SOCKET=" + l.MemoryEngineSock,
		"MEKO_STORE_SOCKET=" + l.StoreSock,
		"MEKO_INTERNAL_TOKEN=" + token,
		"MEKO_MEMORY_ENGINE_STATE_DIR=" + l.MemoryEngineDir,
		"MEM0_TELEMETRY=false",
		"MEM0_DIR=" + l.MemoryEngineDir,
	}
	if o.Models != "" {
		// The release's own copy: read only, and nothing is fetched (it works offline).
		env = append(env, "MEKO_MODELS_DIR="+o.Models, "MEKO_MODELS_OFFLINE=1")
	} else {
		env = append(env, "MEKO_MODELS_DIR="+l.ModelsDir)
	}
	if o.TestEmbedder != "" {
		env = append(env, "MEKO_MEMORY_ENGINE_TEST_EMBEDDER="+o.TestEmbedder)
	}
	c := NewClient(l.MemoryEngineSock, token)
	return supervisor.Spec{
		Name:    Name,
		Path:    node,
		Args:    []string{filepath.Join(dir, "dist", "main.js")},
		Env:     env,
		Dir:     dir,
		LogFile: l.MemoryEngineLog,
		PidFile: filepath.Join(l.ChildrenDir, Name),
		Health: func(ctx context.Context) error {
			_, err := c.Health(ctx)
			return err
		},
		StartTimeout: time.Minute,
	}
}

// Client calls the memory engine's API.
type Client struct {
	token string
	http  *http.Client
}

// NewClient is a client for the memory engine listening on socket.
func NewClient(socket, token string) *Client {
	return &Client{token: token, http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}}
}

// Embedder is the model the memory engine runs.
type Embedder struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Dims     int    `json:"dims"`
}

// Health is what the memory engine says about itself: loading (its model), ready, or error (with why).
type Health struct {
	State    string   `json:"state"`
	Error    string   `json:"error,omitempty"`
	Embedder Embedder `json:"embedder"`
}

// Health asks the memory engine how it is. It fails with ErrUnavailable if nothing accepts the connection,
// and with ctx's error if the memory engine accepts it but doesn't answer before ctx ends.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	err := c.call(ctx, http.MethodGet, "/healthz", nil, &h)
	return h, err
}

func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://memory-engine"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	var env struct {
		Data  json.RawMessage `json:"data"`
		Error *envelope.Error `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("the memory engine answered %s with no JSON", resp.Status)
	}
	if env.Error != nil {
		if env.Error.Code == "MEMORY_ENGINE_NOT_READY" {
			return &NotReadyError{Message: env.Error.Message}
		}
		return &APIError{Status: resp.StatusCode, Code: env.Error.Code, Message: env.Error.Message}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

// Message is one turn of a verbatim add.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name,omitempty"`
}

// AddRequest stores messages as memories, verbatim.
type AddRequest struct {
	DatapackID     string         `json:"datapack_id"`
	UserID         string         `json:"user_id"`
	AgentID        string         `json:"agent_id,omitempty"`
	ConversationID string         `json:"conversation_id,omitempty"`
	Messages       []Message      `json:"messages"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

// Added is one memory an add made.
type Added struct {
	ID      string  `json:"id"`
	Memory  string  `json:"memory"`
	Event   string  `json:"event"`
	ActorID *string `json:"actor_id"`
	Role    string  `json:"role"`
}

// Add stores each non-system message as a memory.
func (c *Client) Add(ctx context.Context, r AddRequest) ([]Added, error) {
	var out struct {
		Results []Added `json:"results"`
	}
	err := c.call(ctx, http.MethodPost, "/v1/add", r, &out)
	return out.Results, err
}

// SearchRequest searches a datapack's memories.
type SearchRequest struct {
	DatapackID     string `json:"datapack_id"`
	UserID         string `json:"user_id"`
	AgentID        string `json:"agent_id,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	Query          string `json:"query"`
	Limit          int    `json:"limit,omitempty"`
}

// Memory is a memory in prod Meko's reply shape.
type Memory struct {
	ID                 string         `json:"id"`
	Memory             string         `json:"memory"`
	Hash               string         `json:"hash,omitempty"`
	Metadata           map[string]any `json:"metadata"`
	Score              *float64       `json:"score,omitempty"`
	RawSimilarityScore *float64       `json:"raw_similarity_score,omitempty"`
	CreatedAt          string         `json:"created_at,omitempty"`
	UpdatedAt          string         `json:"updated_at,omitempty"`
	UserID             string         `json:"user_id,omitempty"`
	AgentID            string         `json:"agent_id,omitempty"`
	RunID              string         `json:"run_id,omitempty"`
	ActorID            string         `json:"actor_id,omitempty"`
	Role               string         `json:"role,omitempty"`
	AttributedTo       string         `json:"attributed_to,omitempty"`
	ExpirationDate     string         `json:"expiration_date,omitempty"`
}

// Search is the datapack's best memories for a query, best first, with Meko's 0-1 score and the
// cosine as raw_similarity_score.
func (c *Client) Search(ctx context.Context, r SearchRequest) ([]Memory, error) {
	var out struct {
		Results []Memory `json:"results"`
	}
	err := c.call(ctx, http.MethodPost, "/v1/search", r, &out)
	return out.Results, err
}

// Update replaces a memory's text (re-embedding it).
func (c *Client) Update(ctx context.Context, datapack, id, text string) error {
	return c.call(ctx, http.MethodPost, "/v1/update",
		map[string]string{"datapack_id": datapack, "memory_id": id, "text": text}, nil)
}

// Delete deletes a memory.
func (c *Client) Delete(ctx context.Context, datapack, id string) error {
	return c.call(ctx, http.MethodPost, "/v1/delete", map[string]string{"datapack_id": datapack, "memory_id": id}, nil)
}

// MaxEmbed is the most texts one Embed call takes.
const MaxEmbed = 256

// Embed embeds texts with the memory engine's model.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	var out struct {
		Vectors [][]float32 `json:"vectors"`
	}
	err := c.call(ctx, http.MethodPost, "/v1/embed", map[string][]string{"texts": texts}, &out)
	return out.Vectors, err
}
