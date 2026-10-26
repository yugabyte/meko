package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/backend/internal/cloudlink"
	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/extproxy"
	"github.com/yugabyte/meko/backend/internal/secrets"
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

// The Cloud datapacks route takes the client token too, never sends the key, and answers a listing
// Meko Cloud didn't make with a 502 and its code; on the web listener it takes the owner.
func TestCloudDatapacksRoute(t *testing.T) {
	cloud := cloudService(t)
	var fail error
	cloud.List = func(_ context.Context, _, k string) ([]cloudlink.Datapack, error) {
		if k != cloudKey {
			t.Errorf("listed with key %q", k)
		}
		if fail != nil {
			return nil, fail
		}
		return []cloudlink.Datapack{{DatapackID: "dp-1", DatapackName: "shared one", Grant: "viewer",
			CreatedAt: "2026-10-26T03:12:11Z"}}, nil
	}
	ln, err := Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{Info: Info{Version: "test"}, Port: tcpPort(t, ln), Token: token, ClientToken: clientToken,
		Logger: slog.New(slog.DiscardHandler), Cloud: cloud})
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	base := fmt.Sprintf("http://127.0.0.1:%d", tcpPort(t, ln))
	client := map[string]string{"Authorization": "Bearer " + clientToken}

	if code, b := send(t, http.MethodGet, base+cloudDatapacksRoute, "", nil); code != 401 {
		t.Fatalf("no token: %d %s", code, b)
	}
	code, b := send(t, http.MethodGet, base+cloudDatapacksRoute, "", client)
	if code != 200 || strings.TrimSpace(b) != `{"data":{"linked":false,"datapacks":[]}}` {
		t.Fatalf("unlinked: %d %s", code, b)
	}
	if _, err = cloud.Link(context.Background(), cloudlink.Update{Key: ptr(cloudKey)}); err != nil {
		t.Fatal(err)
	}
	code, b = send(t, http.MethodGet, base+cloudDatapacksRoute, "", client)
	if code != 200 || !strings.Contains(b, `"linked":true`) || strings.Contains(b, cloudKey) ||
		!strings.Contains(b, `"web_url":"https://cloud.mekodata.ai/datapacks/dp-1"`) ||
		!strings.Contains(b, `"grant":"viewer"`) {
		t.Fatalf("linked, with the client token: %d %s", code, b)
	}
	if code, b = send(t, http.MethodGet, base+cloudDatapacksRoute, "",
		map[string]string{"Authorization": "Bearer " + token}); code != 200 {
		t.Fatalf("with the admin token: %d %s", code, b)
	}
	fail = &cloudlink.ListError{Code: cloudlink.CodeKeyRefused}
	code, b = send(t, http.MethodGet, base+cloudDatapacksRoute, "", client)
	if code != 502 || !strings.Contains(b, `"code":"CLOUD_KEY_REFUSED"`) {
		t.Fatalf("a refused key: %d %s", code, b)
	}
	fail = errors.New("boom " + cloudKey)
	code, b = send(t, http.MethodGet, base+cloudDatapacksRoute, "", client)
	if code != 500 || strings.Contains(b, cloudKey) {
		t.Fatalf("an internal error: %d %s", code, b)
	}
	fail = nil

	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	w := New(Options{Info: Info{Version: "test"}, Port: tcpPort(t, ln2), Logger: slog.New(slog.DiscardHandler),
		Cloud: cloud, Web: &Web{Access: config.AccessLocal}})
	go func() { _ = w.Serve(ln2) }()
	t.Cleanup(func() { _ = w.Shutdown(context.Background()) })
	web := fmt.Sprintf("http://127.0.0.1:%d", tcpPort(t, ln2))
	if code, b = send(t, http.MethodGet, web+cloudDatapacksRoute, "", nil); code != 200 ||
		!strings.Contains(b, `"dp-1"`) {
		t.Fatalf("web listener, local access: %d %s", code, b)
	}
	if code, _ = send(t, http.MethodGet, web+cloudDatapacksRoute, "",
		map[string]string{"Origin": "http://example.com"}); code != 403 {
		t.Fatalf("another site's page: %d", code)
	}
}

func ptr(s string) *string { return &s }
