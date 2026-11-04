package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/cloudlink"
	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/extproxy"
	"github.com/yugabyte/meko/src/meko/internal/secrets"
)

const cloudKey = "mko_tkn_cloud_0123456789wxyz"

func cloudService(t *testing.T) *cloudlink.Service {
	t.Helper()
	dir := t.TempDir()
	return &cloudlink.Service{
		Store:    cloudlink.Store{File: filepath.Join(dir, "cloud.toml"), Secrets: secrets.NewFiles(dir)},
		Check:    func(context.Context, string, string) error { return nil },
		CheckAPI: func(context.Context, string) error { return nil },
	}
}

func send(t *testing.T, method, url, payload string, header map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	code, b, _ := body(t, req)
	return code, b
}

// On the API listener the link's routes take only the admin token (meko cloud_link): an agent's
// client token can't read it or point it elsewhere.
func TestCloudRoutesOnTheAPIListener(t *testing.T) {
	ln, err := Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{Info: Info{Version: "test"}, Port: tcpPort(t, ln), Token: token, ClientToken: clientToken,
		Logger: slog.New(slog.DiscardHandler), Cloud: cloudService(t)})
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	base := fmt.Sprintf("http://127.0.0.1:%d", tcpPort(t, ln))
	admin := map[string]string{"Authorization": "Bearer " + token}

	code, b := send(t, http.MethodPut, base+cloudRoute, `{"api_key":"`+cloudKey+`"}`,
		map[string]string{"Authorization": "Bearer " + clientToken})
	if code != 403 {
		t.Fatalf("PUT with the client token: %d %s", code, b)
	}
	code, b = send(t, http.MethodPut, base+cloudRoute, `{"api_key":"two words"}`, admin)
	if code != 400 || !strings.Contains(b, "VALIDATION_ERROR") || !strings.Contains(b, "no spaces") {
		t.Fatalf("a refused change: %d %s", code, b)
	}
	code, b = send(t, http.MethodPut, base+cloudRoute, `{"api_key":"`+cloudKey+`"}`, admin)
	if code != 200 || !strings.Contains(b, `"linked":true`) || !strings.Contains(b, `"key_hint":"wxyz"`) ||
		strings.Contains(b, cloudKey) {
		t.Fatalf("PUT with the admin token: %d %s", code, b)
	}
}

// On the web listener the owner reads and changes the link, and each running extension hears of a
// change on its socket.
func TestCloudRoutesOnTheWebListener(t *testing.T) {
	sock := filepath.Join(shortTemp(t), "web.sock")
	heard := make(chan string, 4)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	record := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		heard <- r.Method + " " + r.URL.Path + " " + string(b) + " origin=" + r.Header.Get("Origin")
	}
	ext := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(record)}
	go func() { _ = ext.Serve(ln) }()
	t.Cleanup(func() { _ = ext.Close() })

	cloud := cloudService(t)
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := extproxy.New("agentk", "AgentK", func() string { return sock }, slog.New(slog.DiscardHandler))
	cloud.Changed = func(ctx context.Context) { _ = p.Notify(ctx, "cloud") }
	s := New(Options{Info: Info{Version: "test"}, Port: tcpPort(t, ln2), Logger: slog.New(slog.DiscardHandler),
		Cloud: cloud, Web: &Web{Access: config.AccessLocal,
			Extensions: func() []*extproxy.Proxy { return []*extproxy.Proxy{p} }}})
	go func() { _ = s.Serve(ln2) }()
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	web := fmt.Sprintf("http://127.0.0.1:%d", tcpPort(t, ln2))

	code, b := send(t, http.MethodGet, web+cloudRoute, "", nil)
	if code != 200 || !strings.Contains(b, `"set":false`) {
		t.Fatalf("GET: %d %s", code, b)
	}
	code, b = send(t, http.MethodPut, web+cloudRoute, `{"api_key":"`+cloudKey+`"}`, map[string]string{"Origin": web})
	if code != 200 || strings.Contains(b, cloudKey) {
		t.Fatalf("PUT: %d %s", code, b)
	}
	select {
	case got := <-heard:
		if got != `POST /_meko/changed {"what":"cloud"} origin=` {
			t.Fatalf("the extension heard %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the extension heard nothing")
	}
	code, b = send(t, http.MethodDelete, web+cloudRoute, "", map[string]string{"Origin": web})
	if code != 200 || !strings.Contains(b, `"linked":false`) {
		t.Fatalf("DELETE: %d %s", code, b)
	}
	code, _ = send(t, http.MethodDelete, web+cloudRoute, "", map[string]string{"Origin": "http://example.com"})
	if code != 403 {
		t.Fatalf("another site's page: %d", code)
	}
}
