package mekoapi

import (
	"context"
	"encoding/json"
	"io"
)

// Response is what Raw returns: the status whatever it is, and the body.
type Response struct {
	Status int
	Code   string
	Body   []byte
}

// Raw sends one request under /api/v1 and returns the status instead of
// turning a non-2xx into an error, for a test that asserts on refusals. The
// error hook still sees every 4xx and 5xx, so a leaked internal endpoint in a
// refusal still fails the test.
func (c *Client) Raw(ctx context.Context, method, path string, body any) (Response, error) {
	var payload []byte
	contentType := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return Response{}, err
		}
		payload, contentType = b, "application/json"
	}
	resp, err := c.send(ctx, method, path, contentType, payload)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
	if err != nil {
		return Response{}, err
	}
	out := Response{Status: resp.StatusCode, Body: raw}
	var env envelope
	_ = json.Unmarshal(raw, &env)
	if env.Error != nil {
		out.Code = env.Error.Code
	}
	if resp.StatusCode >= 400 && c.onError != nil {
		e := &Error{Method: method, Path: path, Status: resp.StatusCode, Code: out.Code, Body: truncate(string(raw), 512)}
		if env.Error != nil {
			e.Message = env.Error.Message
		}
		c.onError(e)
	}
	return out, nil
}
