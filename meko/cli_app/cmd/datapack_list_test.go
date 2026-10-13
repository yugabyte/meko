package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cli_app/internal/apiclient"
)

// fakeAPI serves GET /api/v1/datapacks. pages maps a cursor ("" for the first
// request) to the data payload returned for it.
func fakeAPI(t *testing.T, pages map[string]string) (*apiclient.Client, *[]string) {
	t.Helper()
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/datapacks" {
			t.Errorf("path = %s", r.URL.Path)
		}
		queries = append(queries, r.URL.RawQuery)
		data, ok := pages[r.URL.Query().Get("cursor")]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			if _, err := w.Write([]byte(`{"error":{"code":"INVALID_CURSOR","message":"bad cursor"}}`)); err != nil {
				t.Errorf("write: %v", err)
			}
			return
		}
		if _, err := w.Write([]byte(`{"data":` + data + `}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return apiclient.NewFromBaseURL(srv.URL), &queries
}

const (
	rowA = `{"datapack_id":"id-a","datapack_name":"alpha","created_at":"2026-09-01T10:00:00.123Z"}`
	rowB = `{"datapack_id":"id-b","datapack_name":"beta","created_at":"2026-09-02T10:00:00Z"}`
	rowC = `{"datapack_id":"id-c","datapack_name":"gamma","created_at":"2026-09-03T10:00:00Z"}`
)

func run(t *testing.T, c *apiclient.Client, opts datapackListOptions) string {
	t.Helper()
	var buf bytes.Buffer
	if err := runDatapackList(c, opts, &buf, io.Discard); err != nil {
		t.Fatalf("runDatapackList: %v", err)
	}
	return buf.String()
}

// The table reads datapack_name and datapack_id, the fields the API returns.
func TestListPrintsNameAndID(t *testing.T) {
	c, queries := fakeAPI(t, map[string]string{
		"": `{"items":[` + rowA + `],"has_more":false,"next_cursor":"","limit":50}`,
	})
	out := run(t, c, datapackListOptions{Limit: 50})
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "id-a") {
		t.Fatalf("output lacks name or id:\n%s", out)
	}
	if strings.Contains(out, "Next page") {
		t.Errorf("last page printed a next-page hint:\n%s", out)
	}
	if (*queries)[0] != "fields=full&limit=50" {
		t.Errorf("query = %q, want fields=full&limit=50", (*queries)[0])
	}
}

// One page by default, with the command for the next one.
func TestListPrintsOnePageAndTheNextCommand(t *testing.T) {
	c, queries := fakeAPI(t, map[string]string{
		"": `{"items":[` + rowA + `],"has_more":true,"next_cursor":"cur1","limit":1}`,
	})
	out := run(t, c, datapackListOptions{Limit: 1, Search: "al"})
	if len(*queries) != 1 {
		t.Fatalf("requests = %v, want 1", *queries)
	}
	if (*queries)[0] != "fields=full&limit=1&q=al" {
		t.Errorf("query = %q", (*queries)[0])
	}
	want := "meko datapack list --search 'al' --limit 1 --cursor 'cur1'"
	if !strings.Contains(out, want) {
		t.Fatalf("output lacks %q:\n%s", want, out)
	}
}

// --all follows next_cursor to the end.
func TestListAllPagesToTheEnd(t *testing.T) {
	c, queries := fakeAPI(t, map[string]string{
		"":   `{"items":[` + rowA + `],"has_more":true,"next_cursor":"c1","limit":1}`,
		"c1": `{"items":[` + rowB + `],"has_more":true,"next_cursor":"c2","limit":1}`,
		"c2": `{"items":[` + rowC + `],"has_more":false,"next_cursor":"","limit":1}`,
	})
	out := run(t, c, datapackListOptions{Limit: 1, All: true})
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if !strings.Contains(out, name) {
			t.Errorf("output lacks %s:\n%s", name, out)
		}
	}
	if len(*queries) != 3 {
		t.Errorf("requests = %v, want 3", *queries)
	}
	if strings.Contains(out, "Next page") {
		t.Errorf("--all printed a next-page hint:\n%s", out)
	}
}

// --json prints one page as an array. The next-page command goes to stderr.
func TestListJSONIsAnArrayAndTheNextPageGoesToStderr(t *testing.T) {
	c, queries := fakeAPI(t, map[string]string{
		"": `{"items":[` + rowA + `],"has_more":true,"next_cursor":"cur1","limit":1}`,
	})
	var out, errOut bytes.Buffer
	if err := runDatapackList(c, datapackListOptions{Limit: 1, JSON: true}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("stdout is not a JSON array: %v\n%s", err, out.String())
	}
	if len(rows) != 1 || rows[0]["datapack_id"] != "id-a" {
		t.Fatalf("rows = %v", rows)
	}
	if strings.Contains(out.String(), "has_more") || strings.Contains(out.String(), "Next page") {
		t.Fatalf("stdout has the page envelope or the hint:\n%s", out.String())
	}
	want := "meko datapack list --limit 1 --json --cursor 'cur1'"
	if !strings.Contains(errOut.String(), want) {
		t.Fatalf("stderr lacks %q:\n%s", want, errOut.String())
	}
	if (*queries)[0] != "fields=full&limit=1" {
		t.Errorf("query = %q", (*queries)[0])
	}
}

// --all --json prints every page as one array.
func TestListAllJSONIsOneArray(t *testing.T) {
	c, _ := fakeAPI(t, map[string]string{
		"":   `{"items":[` + rowA + `],"has_more":true,"next_cursor":"c1","limit":1}`,
		"c1": `{"items":[` + rowB + `],"has_more":false,"next_cursor":"","limit":1}`,
	})
	out := run(t, c, datapackListOptions{Limit: 1, All: true, JSON: true})
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("output is not a JSON array: %v\n%s", err, out)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
}

// An old server ignores limit and returns a bare array: the complete list,
// printed whole with no next-page hint, and --search applied locally.
func TestListOldServerBareArrayIsTheCompleteList(t *testing.T) {
	c, queries := fakeAPI(t, map[string]string{
		"": `[` + rowA + `,` + rowB + `,` + rowC + `]`,
	})
	out := run(t, c, datapackListOptions{Limit: 1})
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if !strings.Contains(out, name) {
			t.Errorf("output lacks %s:\n%s", name, out)
		}
	}
	if strings.Contains(out, "Next page") {
		t.Errorf("complete list printed a next-page hint:\n%s", out)
	}

	out = run(t, c, datapackListOptions{Limit: 50, Search: "ET"})
	if !strings.Contains(out, "beta") || strings.Contains(out, "alpha") || strings.Contains(out, "gamma") {
		t.Errorf("search not applied to the old server's list:\n%s", out)
	}
	if len(*queries) != 2 {
		t.Errorf("requests = %v, want 2", *queries)
	}
}

// With --all, a bare array from an old replica mid-loop is the complete set and
// replaces the pages read before it.
func TestListAllBareArrayMidLoopReplacesEarlierPages(t *testing.T) {
	c, _ := fakeAPI(t, map[string]string{
		"":   `{"items":[` + rowA + `],"has_more":true,"next_cursor":"c1","limit":1}`,
		"c1": `[` + rowA + `,` + rowB + `]`,
	})
	out := run(t, c, datapackListOptions{Limit: 1, All: true, JSON: true})
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("output is not a JSON array: %v\n%s", err, out)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (no duplicate of alpha)", len(rows))
	}
}

// A page error fails the command rather than printing a partial list.
func TestListAllFailsOnAPageError(t *testing.T) {
	c, _ := fakeAPI(t, map[string]string{
		"": `{"items":[` + rowA + `],"has_more":true,"next_cursor":"missing","limit":1}`,
	})
	var buf bytes.Buffer
	err := runDatapackList(c, datapackListOptions{Limit: 1, All: true}, &buf, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "INVALID_CURSOR") {
		t.Fatalf("error = %v, want INVALID_CURSOR", err)
	}
	if buf.Len() != 0 {
		t.Errorf("printed output despite the error:\n%s", buf.String())
	}
}

func TestListRejectsANonPositiveLimit(t *testing.T) {
	c, queries := fakeAPI(t, nil)
	if err := runDatapackList(c, datapackListOptions{Limit: 0}, &bytes.Buffer{}, io.Discard); err == nil {
		t.Fatal("want an error for --limit 0")
	}
	if len(*queries) != 0 {
		t.Errorf("requests = %v, want none", *queries)
	}
}
