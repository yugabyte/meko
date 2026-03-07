// Package apiclient provides an HTTP client for the meko API server.
package apiclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client wraps an HTTP client pointed at the meko API server.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// New creates a Client targeting the given host and port.
func New(host string, port int) *Client {
	return &Client{
		baseURL: fmt.Sprintf("http://%s:%d/api/v1", host, port),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// APIResponse is the standard envelope returned by the API server.
type APIResponse struct {
	Error *APIError       `json:"error"`
	Data  json.RawMessage `json:"data"`
}

// APIError represents a structured API error.
type APIError struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Details []string `json:"details,omitempty"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// do performs an HTTP request and returns the parsed data payload.
func (c *Client) do(method, path string, body any) (json.RawMessage, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, c.baseURL+path, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to reach API server at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var apiResp APIResponse
	if err := json.Unmarshal(respBytes, &apiResp); err != nil {
		return nil, fmt.Errorf(
			"unexpected response from API server: %s", string(respBytes))
	}

	if apiResp.Error != nil {
		return nil, apiResp.Error
	}

	return apiResp.Data, nil
}

// CreateDatapack creates a new datapack via the API.
func (c *Client) CreateDatapack(
	name, seedKB, embedModel, inferModel string,
) (json.RawMessage, error) {
	body := map[string]string{
		"name":                name,
		"seed_knowledge_base": seedKB,
	}
	if embedModel != "" {
		body["embed_model"] = embedModel
	}
	if inferModel != "" {
		body["inference_model"] = inferModel
	}
	return c.do(http.MethodPost, "/datapacks", body)
}

// ListDatapacks returns all datapacks.
func (c *Client) ListDatapacks() (json.RawMessage, error) {
	return c.do(http.MethodGet, "/datapacks", nil)
}

// DescribeDatapack returns details about a specific datapack.
func (c *Client) DescribeDatapack(name string) (json.RawMessage, error) {
	return c.do(http.MethodGet, "/datapacks/"+name, nil)
}

// DeleteDatapack deletes a datapack by name.
func (c *Client) DeleteDatapack(name string) (json.RawMessage, error) {
	return c.do(http.MethodDelete, "/datapacks/"+name, nil)
}

// DatapackStatus returns status info for a datapack.
func (c *Client) DatapackStatus(name string) (json.RawMessage, error) {
	return c.do(http.MethodGet, "/datapacks/"+name+"/status", nil)
}

// CreateAgent creates a new agent in a datapack.
func (c *Client) CreateAgent(
	datapackName, agentName, metadata string,
) (json.RawMessage, error) {
	body := map[string]string{
		"agent_name": agentName,
	}
	if metadata != "" {
		body["agent_metadata"] = metadata
	}
	return c.do(
		http.MethodPost, "/datapacks/"+datapackName+"/agents", body)
}

// ListAgents returns all agents for a datapack.
func (c *Client) ListAgents(
	datapackName string,
) (json.RawMessage, error) {
	return c.do(
		http.MethodGet, "/datapacks/"+datapackName+"/agents", nil)
}

// DeleteAgent removes an agent from a datapack.
func (c *Client) DeleteAgent(
	datapackName, agentName string,
) (json.RawMessage, error) {
	return c.do(
		http.MethodDelete,
		"/datapacks/"+datapackName+"/agents/"+agentName, nil)
}

// AddKnowledgeBase adds a knowledge base to a datapack.
func (c *Client) AddKnowledgeBase(
	datapackName, url string,
) (json.RawMessage, error) {
	body := map[string]string{"url": url}
	return c.do(
		http.MethodPost,
		"/datapacks/"+datapackName+"/knowledge-bases", body)
}

// ListKnowledgeBases returns all knowledge bases for a datapack.
func (c *Client) ListKnowledgeBases(
	datapackName string,
) (json.RawMessage, error) {
	return c.do(
		http.MethodGet,
		"/datapacks/"+datapackName+"/knowledge-bases", nil)
}

// DeleteKnowledgeBase removes a knowledge base from a datapack.
func (c *Client) DeleteKnowledgeBase(
	datapackName, url string,
) (json.RawMessage, error) {
	body := map[string]string{"url": url}
	return c.do(
		http.MethodDelete,
		"/datapacks/"+datapackName+"/knowledge-bases", body)
}
