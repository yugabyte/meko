// Package mcp is a minimal, real MCP-protocol server for a single datapack. It
// exposes JSON-RPC 2.0 over HTTP (initialize, tools/list, tools/call) with two
// tools — memory_add and memory_list — backed by the datapack's database.
package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

const protocolVersion = "2024-11-05"

// Memory is one stored memory in a datapack.
type Memory struct {
	ID        int64     `json:"id"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

// Store persists a datapack's memories.
type Store interface {
	Add(ctx context.Context, text string) (Memory, error)
	List(ctx context.Context) ([]Memory, error)
	Close() error
}

// MemStore is an in-memory Store (dev/tests).
type MemStore struct {
	mu   sync.Mutex
	seq  int64
	rows []Memory
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore { return &MemStore{} }

func (m *MemStore) Add(_ context.Context, text string) (Memory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	row := Memory{ID: m.seq, Text: text, CreatedAt: time.Now().UTC()}
	m.rows = append(m.rows, row)
	return row, nil
}

func (m *MemStore) List(_ context.Context) ([]Memory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Memory(nil), m.rows...), nil
}

func (m *MemStore) Close() error { return nil }

// ---- JSON-RPC 2.0 ----

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Handler builds the MCP HTTP handler over a store.
func Handler(store Store) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	mux.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeRPC(w, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
			return
		}
		resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
		switch req.Method {
		case "initialize":
			resp.Result = map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "meko-datapack-mcp", "version": "0.1.0"},
			}
		case "tools/list":
			resp.Result = map[string]any{"tools": toolDefs()}
		case "tools/call":
			result, rerr := callTool(r.Context(), store, req.Params)
			if rerr != nil {
				resp.Error = rerr
			} else {
				resp.Result = result
			}
		default:
			resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
		}
		writeRPC(w, resp)
	})

	return mux
}

func toolDefs() []map[string]any {
	return []map[string]any{
		{
			"name":        "memory_add",
			"description": "Store a memory in this datapack.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"text": map[string]any{"type": "string"}},
				"required":   []string{"text"},
			},
		},
		{
			"name":        "memory_list",
			"description": "List memories stored in this datapack.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
}

func callTool(ctx context.Context, store Store, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string `json:"name"`
		Arguments struct {
			Text string `json:"text"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid params"}
	}
	switch p.Name {
	case "memory_add":
		if p.Arguments.Text == "" {
			return nil, &rpcError{Code: -32602, Message: "text is required"}
		}
		m, err := store.Add(ctx, p.Arguments.Text)
		if err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		return toolResult("stored memory #" + itoa(m.ID)), nil
	case "memory_list":
		rows, err := store.List(ctx)
		if err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		b, _ := json.Marshal(rows)
		return toolResult(string(b)), nil
	default:
		return nil, &rpcError{Code: -32602, Message: "unknown tool: " + p.Name}
	}
}

func toolResult(text string) map[string]any {
	return map[string]any{"content": []toolContent{{Type: "text", Text: text}}}
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
