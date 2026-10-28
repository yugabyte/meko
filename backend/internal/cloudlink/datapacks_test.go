package cloudlink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A row as Meko Cloud's GET /datapacks gives it, database credentials included.
const cloudRow = `{"datapack_id":"%s","datapack_name":"%s","description":"","grant":"%s",
	"created_at":"%s","updated_at":"2026-10-26T03:12:11Z","memory_count":3,"datapack_owner_name":"A",
	"datapack_ysql_username":"dp_user","datapack_ysql_connection_string":"postgresql://db.example.com/dp"}`

func row(id, name, grant, created string) string {
	return fmt.Sprintf(cloudRow, id, name, grant, created)
}

// ListDatapacks sends the key, asks for every row, keeps only the fields Meko shows, and turns each
// way Meko Cloud can fail into its code, never with the key in the message.
func TestListDatapacks(t *testing.T) {
	// The hang and stall handlers can still be running when the next request comes in.
	var mu sync.Mutex
	var gotQuery, gotAuth string
	reply := func(w http.ResponseWriter, code int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotQuery, gotAuth = r.URL.RawQuery, r.Header.Get("Authorization")
		mu.Unlock()
		switch r.URL.Path {
		case "/ok/datapacks":
			reply(w, 200, `{"data":[`+row("a1", "mine", "owner", "2026-10-01T00:00:00Z")+`,`+
				row("b2", "theirs", "viewer", "2026-10-02T00:00:00.5Z")+`],"error":null}`)
		case "/refused/datapacks":
			reply(w, 401, `{"data":null,"error":{"code":"UNAUTHORIZED","message":"Invalid token"}}`)
		case "/forbidden/datapacks":
			reply(w, 403, `nope`)
		case "/envelope/datapacks":
			reply(w, 500, `{"data":null,"error":{"code":"INTERNAL","message":"Something broke."}}`)
		case "/paged/datapacks":
			reply(w, 200, `{"data":{"items":[],"has_more":false}}`)
		case "/html/datapacks":
			reply(w, 200, `<html>`)
		case "/hang/datapacks":
			<-r.Context().Done()
		case "/stall/datapacks":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[`))
			_ = http.NewResponseController(w).Flush()
			<-r.Context().Done()
		default:
			reply(w, 404, `not found`)
		}
	}))
	defer ts.Close()
	ctx := context.Background()

	rows, err := ListDatapacks(ctx, ts.URL+"/ok/", key)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	sentAuth, sentQuery := gotAuth, gotQuery
	mu.Unlock()
	if sentAuth != "Bearer "+key || sentQuery != "" {
		t.Fatalf("sent Authorization %q, query %q", sentAuth, sentQuery)
	}
	if len(rows) != 2 || rows[0].DatapackID != "a1" || rows[1].Grant != "viewer" || rows[0].WebURL != nil {
		t.Fatalf("rows %+v", rows)
	}
	if b, _ := json.Marshal(rows); strings.Contains(string(b), "ysql") || strings.Contains(string(b), "postgresql") {
		t.Fatalf("Cloud's database credentials came through: %s", b)
	}

	for _, tc := range []struct{ path, code, says string }{
		{"/refused/", CodeKeyRefused, "refused the key"},
		{"/forbidden/", CodeKeyRefused, "refused the key"},
		{"/envelope/", CodeListFailed, "Something broke"},
		{"/missing/", CodeListFailed, "answered 404"},
		{"/paged/", CodeListFailed, "isn't a list"},
		{"/html/", CodeListFailed, "isn't Meko's API"},
	} {
		_, err = ListDatapacks(ctx, ts.URL+tc.path, key)
		var le *CallError
		if !errors.As(err, &le) || le.Code != tc.code || !strings.Contains(err.Error(), tc.says) ||
			strings.Contains(err.Error(), key) {
			t.Errorf("%s: %v", tc.path, err)
		}
	}

	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_, err = ListDatapacks(short, ts.URL+"/hang/", key)
	var le *CallError
	if !errors.As(err, &le) || le.Code != CodeUnreachable {
		t.Fatalf("a server that hangs: %v", err)
	}
	stalled, cancelStall := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancelStall()
	_, err = ListDatapacks(stalled, ts.URL+"/stall/", key)
	if !errors.As(err, &le) || le.Code != CodeUnreachable || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("a reply cut short by the time limit: %v", err)
	}
	ts.CloseClientConnections()
	_, err = ListDatapacks(ctx, "http://127.0.0.1:1/api/v1", key)
	if !errors.As(err, &le) || le.Code != CodeUnreachable || strings.Contains(err.Error(), key) {
		t.Fatalf("nothing listening: %v", err)
	}
}

// Datapacks answers linked false until the link is on, then the rows newest first, each with its
// link in the web app; a link with no API URL says so.
func TestServiceDatapacks(t *testing.T) {
	s, _ := newService(t)
	var asked []string
	s.List = func(_ context.Context, api, k string) ([]Datapack, error) {
		asked = append(asked, api+" "+k)
		return []Datapack{
			{DatapackID: "old", CreatedAt: "2026-01-01T00:00:00Z"},
			{DatapackID: "bad", CreatedAt: "yesterday"},
			{DatapackID: "new", CreatedAt: "2026-10-01T00:00:00.123Z"},
		}, nil
	}
	ctx := context.Background()
	got, err := s.Datapacks(ctx)
	if err != nil || got.Linked || got.Datapacks == nil || len(got.Datapacks) != 0 || len(asked) != 0 {
		t.Fatalf("unlinked: %+v, %v, asked %v", got, err, asked)
	}

	if _, err = s.Link(ctx, Update{Key: ptr(key)}); err != nil {
		t.Fatal(err)
	}
	got, err = s.Datapacks(ctx)
	if err != nil || !got.Linked || *got.APIURL != DefaultWebURL+"/api/v1" || *got.WebURL != DefaultWebURL {
		t.Fatalf("linked: %+v, %v", got, err)
	}
	if asked[0] != DefaultWebURL+"/api/v1 "+key {
		t.Fatalf("asked %v", asked)
	}
	order := make([]string, 0, len(got.Datapacks))
	for _, d := range got.Datapacks {
		order = append(order, d.DatapackID)
	}
	if strings.Join(order, ",") != "new,old,bad" || *got.Datapacks[0].WebURL != DefaultWebURL+"/datapacks/new" {
		t.Fatalf("rows %+v", got.Datapacks)
	}

	// Another server with no web app or API URL can't be listed.
	if _, err = s.Link(ctx, Update{URL: ptr("https://meko.example.com/mcp"), Key: ptr(key)}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Datapacks(ctx)
	var le *CallError
	if !errors.As(err, &le) || le.Code != CodeNoAPIURL {
		t.Fatalf("no API URL: %v", err)
	}
	// With an API URL but no web app, the rows have no link.
	if _, err = s.Link(ctx, Update{APIURL: ptr("https://api.example.com/api/v1"), Key: ptr(key)}); err != nil {
		t.Fatal(err)
	}
	got, err = s.Datapacks(ctx)
	if err != nil || got.WebURL != nil || got.Datapacks[0].WebURL != nil {
		t.Fatalf("no web app: %+v, %v", got, err)
	}
}
