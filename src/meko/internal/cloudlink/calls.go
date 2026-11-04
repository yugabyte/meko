package cloudlink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// CallTimeout bounds one call to Meko Cloud made for a route: a search of a large datapack takes
// seconds.
const CallTimeout = 30 * time.Second

// maxCallBytes bounds a reply from Meko Cloud to a call.
const maxCallBytes = 8 << 20

// IsLinked says whether Meko Cloud is linked.
func (s *Service) IsLinked() (bool, error) {
	st, err := s.load()
	return st.Linked, err
}

// linked is the saved link a call goes through; a CodeNotLinked CallError when there's none.
func (s *Service) linked() (Link, error) {
	st, err := s.load()
	if err != nil {
		return Link{}, err
	}
	if !st.Linked {
		return Link{}, notLinked()
	}
	return st.Link, nil
}

func notLinked() error {
	return &CallError{Status: http.StatusConflict, Code: CodeNotLinked,
		msg: "This Meko isn't linked to Meko Cloud. Link it in Settings › Meko Cloud, or with meko cloud_link."}
}

// API calls the linked Meko Cloud's REST API with the key: method on path, which follows the API URL
// (such as /datapacks/<id>), with body as JSON when it isn't nil. It returns the reply's data. A
// failure is a *CallError: Meko Cloud's own 4xx keeps its status, code and message (a 401 is
// CodeKeyRefused, a 502); anything else is a 502.
func (s *Service) API(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	l, err := s.linked()
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(l.EffectiveAPIURL(), "/")
	if base == "" {
		return nil, &CallError{Code: CodeNoAPIURL, msg: "The Meko Cloud link has no API URL. " +
			"Set the web app or API URL in Settings › Meko Cloud, or meko cloud_link --api-url."}
	}
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	var r io.Reader
	if body != nil {
		b, merr := json.Marshal(body)
		if merr != nil {
			return nil, merr
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, r)
	if err != nil {
		return nil, &CallError{Code: CodeCallFailed, msg: "Meko Cloud's API URL isn't one Meko can call."}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+l.Key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	unreachable := func(err error) error {
		return &CallError{Code: CodeUnreachable,
			msg: "Couldn't reach Meko Cloud: " + strings.TrimSuffix(shortError(ctx, err).Error(), ".") + "."}
	}
	res, err := http.DefaultClient.Do(req) //nolint:gosec // G704: the link's API URL, checked when it was saved
	if err != nil {
		return nil, unreachable(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusUnauthorized {
		return nil, keyRefused()
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxCallBytes))
	if err != nil {
		return nil, unreachable(err)
	}
	var env struct {
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	jsonErr := json.Unmarshal(b, &env)
	switch {
	case jsonErr == nil && env.Error != nil && res.StatusCode/100 == 4:
		return nil, &CallError{Status: res.StatusCode, Code: env.Error.Code, msg: shorten(env.Error.Message)}
	case jsonErr == nil && env.Error != nil:
		return nil, &CallError{Code: CodeCallFailed,
			msg: "Meko Cloud: " + strings.TrimSuffix(shorten(env.Error.Message), ".") + "."}
	case res.StatusCode/100 != 2 || jsonErr != nil:
		return nil, &CallError{Code: CodeCallFailed, msg: "Meko Cloud answered " + res.Status + ", not like Meko's API."}
	}
	return env.Data, nil
}

// serverFailed is Meko Cloud's own failure (a 5xx) as CodeCallFailed, with the message its envelope
// holds, if any.
func serverFailed(status string, b []byte) error {
	var env struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &env) == nil && env.Error != nil && env.Error.Message != "" {
		return &CallError{Code: CodeCallFailed,
			msg: "Meko Cloud: " + strings.TrimSuffix(shorten(env.Error.Message), ".") + "."}
	}
	return &CallError{Code: CodeCallFailed, msg: "Meko Cloud answered " + status + "."}
}

func keyRefused() error {
	return &CallError{Code: CodeKeyRefused,
		msg: "Meko Cloud refused the key. Link it again in Settings › Meko Cloud, or with meko cloud_link."}
}

// Tool calls one of the linked Meko Cloud's MCP tools with the key, within CallTimeout, and returns
// its reply's text: an error a caller can act on ({"error", "detail"}) is a reply too, as on this
// Meko's /mcp. args must hold only arguments the tool takes, since Meko Cloud refuses others.
func (s *Service) Tool(ctx context.Context, name string, args map[string]any) (string, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	res, err := s.Forward(ctx, name, raw)
	if err != nil {
		return "", err
	}
	text := ""
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			text = tc.Text
		}
	}
	if res.IsError {
		return "", &CallError{Code: CodeCallFailed, msg: "Meko Cloud's " + name + " failed: " +
			strings.TrimSuffix(shorten(text), ".") + "."}
	}
	return text, nil
}

// Reply is the linked Meko Cloud's REST reply as it came: its status, content type and body.
type Reply struct {
	Status      int
	ContentType string
	Body        []byte
}

// Pass sends a request to the linked Meko Cloud's REST API as it came, with the key: method, the
// path after the API URL with its query (such as /datapacks?limit=10), and body (nil for none) of
// contentType. Meko Cloud's status, content type and body come back, with three exceptions: a JSON
// body loses its database and MCP credentials (withoutCredentials), which no caller of Meko's
// routes gets, a 401 is CodeKeyRefused, since the key is Meko's, not the caller's, and a 5xx is
// CodeCallFailed (serverFailed), as on Meko's other routes that call Meko Cloud. A request
// that doesn't reach Meko Cloud, or a reply over maxCallBytes, is a *CallError.
func (s *Service) Pass(ctx context.Context, method, pathQuery string, body io.Reader, contentType string) (Reply,
	error) {
	l, err := s.linked()
	if err != nil {
		return Reply{}, err
	}
	base := strings.TrimRight(l.EffectiveAPIURL(), "/")
	if base == "" {
		return Reply{}, &CallError{Code: CodeNoAPIURL, msg: "The Meko Cloud link has no API URL. " +
			"Set the web app or API URL in Settings › Meko Cloud, or meko cloud_link --api-url."}
	}
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, base+pathQuery, body)
	if err != nil {
		return Reply{}, &CallError{Code: CodeCallFailed, msg: "Meko Cloud's API URL isn't one Meko can call."}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+l.Key)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	unreachable := func(err error) error {
		return &CallError{Code: CodeUnreachable,
			msg: "Couldn't reach Meko Cloud: " + strings.TrimSuffix(shortError(ctx, err).Error(), ".") + "."}
	}
	res, err := http.DefaultClient.Do(req) //nolint:gosec // G704: the link's API URL, checked when it was saved
	if err != nil {
		return Reply{}, unreachable(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusUnauthorized {
		return Reply{}, keyRefused()
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxCallBytes+1))
	if err != nil {
		return Reply{}, unreachable(err)
	}
	if len(b) > maxCallBytes {
		return Reply{}, &CallError{Code: CodeCallFailed,
			msg: "Meko Cloud's reply is larger than the 8 MiB Meko passes on."}
	}
	if res.StatusCode >= 500 {
		return Reply{}, serverFailed(res.Status, b)
	}
	return Reply{Status: res.StatusCode, ContentType: res.Header.Get("Content-Type"), Body: withoutCredentials(b)},
		nil
}

// credentialKeys are the parts of a key's name that mark a credential in Meko Cloud's replies: a
// datapack's database user, password and connection string, and an MCP server's key.
var credentialKeys = []string{"password", "conn_str", "connection_string", "ysql", "db_access", "db_user", "api_key"}

func isCredentialKey(k string) bool {
	k = strings.ToLower(k)
	for _, c := range credentialKeys {
		if strings.Contains(k, c) {
			return true
		}
	}
	return false
}

// withoutCredentials is a JSON body with every credential (isCredentialKey) taken out, at any
// depth; a body that isn't JSON comes back as it is.
func withoutCredentials(b []byte) []byte {
	var v any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber() // numbers come back as they were written
	if d.Decode(&v) != nil {
		return b
	}
	if !dropCredentials(v) {
		return b
	}
	out, err := json.Marshal(v)
	if err != nil {
		return b
	}
	return out
}

// dropCredentials deletes the credentials in v, and says whether there were any.
func dropCredentials(v any) bool {
	dropped := false
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if isCredentialKey(k) {
				delete(x, k)
				dropped = true
			} else if dropCredentials(e) {
				dropped = true
			}
		}
	case []any:
		for _, e := range x {
			if dropCredentials(e) {
				dropped = true
			}
		}
	}
	return dropped
}

// PostUpload posts a file to an upload link that a knowledge base's upload-url gave: its fields,
// then the file, as multipart/form-data, with no key. Any 2xx is the file received: this Meko's link
// answers 204, as Meko Cloud's presigned S3 POST does by default.
func PostUpload(ctx context.Context, url string, fields map[string]string, name string, data io.Reader) error {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return err
		}
	}
	fw, err := w.CreateFormFile("file", name)
	if err != nil {
		return err
	}
	if _, err = io.Copy(fw, data); err != nil {
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: the upload link the knowledge base gave
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return nil
	}
	var env struct {
		Error *struct{ Message string } `json:"error"`
	}
	if json.NewDecoder(resp.Body).Decode(&env) == nil && env.Error != nil {
		return errors.New(env.Error.Message)
	}
	return fmt.Errorf("the upload answered %s", resp.Status)
}
