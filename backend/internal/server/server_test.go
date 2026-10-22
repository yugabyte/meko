package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/backend/internal/envelope"
	"github.com/yugabyte/meko/backend/internal/supervisor"
)

const (
	token       = "secret-token"
	clientToken = "client-token"
)

// start runs a server on a free port and returns its base URL and log.
func start(t *testing.T) (*Server, string, *bytes.Buffer) {
	t.Helper()
	return startWith(t, "")
}

// startWith runs a server with an auth mode ("" for the default).
func startWith(t *testing.T, authMode string) (*Server, string, *bytes.Buffer) {
	t.Helper()
	ln, err := Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	port := tcpPort(t, ln)
	var logs bytes.Buffer
	s := New(Options{
		Info:        Info{Version: "test", DataDir: "/data", PID: 42, StartedAt: time.Now()},
		Port:        port,
		Token:       token,
		ClientToken: clientToken,
		Auth:        authMode,
		Logger:      slog.New(slog.NewJSONHandler(&logs, nil)),
		Children: func() []supervisor.Status {
			return []supervisor.Status{{Name: "memory_engine", State: supervisor.Running}}
		},
	})
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return s, fmt.Sprintf("http://127.0.0.1:%d", port), &logs
}

func tcpPort(t *testing.T, ln net.Listener) int {
	t.Helper()
	a, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("not a TCP address: %v", ln.Addr())
	}
	return a.Port
}

func get(t *testing.T, url string, header map[string]string, host string) (int, envelope.Response) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var env envelope.Response
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("%s: not an envelope: %s", url, body)
	}
	if env.Error == nil && strings.Contains(string(body), `"error"`) {
		t.Errorf("%s: error should be omitted on success: %s", url, body)
	}
	return resp.StatusCode, env
}

func TestAccess(t *testing.T) {
	_, base, _ := start(t)
	auth := map[string]string{"Authorization": "Bearer " + token}
	wrong := map[string]string{"Authorization": "Bearer nope"}
	browser := map[string]string{"Origin": "http://example.com"}
	browserAuth := map[string]string{"Origin": "http://example.com", "Authorization": "Bearer " + token}
	cases := []struct {
		name     string
		path     string
		header   map[string]string
		host     string
		want     int
		wantCode string
	}{
		{name: "healthz needs no token", path: "/healthz", want: 200},
		{name: "status needs the token", path: "/api/v1/status", want: 401, wantCode: "UNAUTHORIZED"},
		{name: "wrong token", path: "/api/v1/status", header: wrong, want: 401, wantCode: "UNAUTHORIZED"},
		{name: "status with token", path: "/api/v1/status", header: auth, want: 200},
		{name: "version with token", path: "/api/v1/version", header: auth, want: 200},
		{name: "unknown route is not found", path: "/api/v1/nope", header: auth, want: 404, wantCode: "NOT_FOUND"},
		{name: "browser request", path: "/healthz", header: browser, want: 403, wantCode: "FORBIDDEN"},
		{name: "browser request with token", path: "/api/v1/status", header: browserAuth, want: 403, wantCode: "FORBIDDEN"},
		{name: "foreign Host (DNS rebinding)", path: "/healthz", host: "evil.example.com", want: 403, wantCode: "FORBIDDEN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, env := get(t, base+tc.path, tc.header, tc.host)
			if status != tc.want {
				t.Fatalf("status %d, want %d (%+v)", status, tc.want, env.Error)
			}
			if tc.wantCode == "" && (env.Error != nil || env.Data == nil) {
				t.Errorf("want data, got %+v", env)
			}
			if tc.wantCode != "" && (env.Error == nil || env.Error.Code != tc.wantCode || env.Data != nil) {
				t.Errorf("want error %s, got %+v", tc.wantCode, env)
			}
		})
	}
}

// probe is a data route that reports the credential a request got in with.
func probe(s *Server) {
	s.e.GET("/api/v1/probe", func(c echo.Context) error { return envelope.OK(c, Credential(c)) })
}

func TestCredentials(t *testing.T) {
	admin := map[string]string{"Authorization": "Bearer " + token}
	client := map[string]string{"Authorization": "Bearer " + clientToken}
	wrong := map[string]string{"Authorization": "Bearer nope"}
	cases := []struct {
		name, auth, path string
		header           map[string]string
		want             int
		wantCred         string
	}{
		{name: "admin on a data route", path: "/api/v1/probe", header: admin, want: 200, wantCred: CredentialAdmin},
		{name: "client on a data route", path: "/api/v1/probe", header: client, want: 200, wantCred: CredentialClient},
		{name: "nothing on a data route", path: "/api/v1/probe", want: 401},
		{name: "wrong token on a data route", path: "/api/v1/probe", header: wrong, want: 401},
		{name: "client on status", path: "/api/v1/status", header: client, want: 403},
		{name: "client on version", path: "/api/v1/version", header: client, want: 403},
		{name: "auth off: nothing", auth: "none", path: "/api/v1/probe", want: 200, wantCred: CredentialNone},
		{name: "auth off: a wrong token is ignored", auth: "none", path: "/api/v1/probe", header: wrong, want: 200,
			wantCred: CredentialNone},
		{name: "auth off: the client token still counts", auth: "none", path: "/api/v1/probe", header: client,
			want: 200, wantCred: CredentialClient},
		{name: "auth off: status still needs the admin token", auth: "none", path: "/api/v1/status", want: 401},
		{name: "auth off: and the client token isn't enough", auth: "none", path: "/api/v1/status", header: client,
			want: 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, base, _ := startWith(t, tc.auth)
			probe(s)
			status, env := get(t, base+tc.path, tc.header, "")
			if status != tc.want {
				t.Fatalf("status %d, want %d (%+v)", status, tc.want, env.Error)
			}
			if tc.wantCred != "" && env.Data != tc.wantCred {
				t.Errorf("credential %v, want %s", env.Data, tc.wantCred)
			}
		})
	}
}

func TestUnauthorizedAsksForABearer(t *testing.T) {
	s, base, _ := start(t)
	probe(s)
	for _, path := range []string{"/api/v1/probe", "/api/v1/status"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s: status %d, WWW-Authenticate %q", path, resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
	}
}

func TestStatusReply(t *testing.T) {
	_, base, _ := start(t)
	_, env := get(t, base+"/api/v1/status", map[string]string{"Authorization": "Bearer " + token}, "")
	b, _ := json.Marshal(env.Data)
	var st StatusReply
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if st.PID != 42 || st.DataDir != "/data" || st.Version != "test" || st.Auth != "token" ||
		len(st.Children) != 1 || st.Children[0].State != supervisor.Running {
		t.Fatalf("status %+v", st)
	}
}

func TestInternalErrorIsGeneric(t *testing.T) {
	s, base, logs := start(t)
	s.e.GET("/api/v1/boom", func(echo.Context) error { return errors.New("open /secret/path: denied") })
	status, env := get(t, base+"/api/v1/boom", map[string]string{"Authorization": "Bearer " + token}, "")
	if status != 500 || env.Error == nil || env.Error.Code != "INTERNAL_ERROR" ||
		strings.Contains(env.Error.Message, "/secret") {
		t.Fatalf("status %d, %+v", status, env.Error)
	}
	if !strings.Contains(logs.String(), "/secret/path") {
		t.Errorf("the cause should be logged: %s", logs.String())
	}
}

func TestListenTakenPort(t *testing.T) {
	ln, err := Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := tcpPort(t, ln)
	_, err = Listen(port)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("port %d is already in use", port)) ||
		!strings.Contains(err.Error(), "--port") {
		t.Fatalf("got %v", err)
	}
}
