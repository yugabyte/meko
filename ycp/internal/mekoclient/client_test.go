package mekoclient

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func client(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL, "tok", srv.Client())
}

// The api-server wraps every payload in {"data":…}. Decoding that straight into
// the payload struct yields zero values instead of an error, which is how the
// agent once enrolled with an empty environment id (meko#399) — so the unwrap is
// asserted, not assumed.
func TestListUnwrapsTheDataEnvelope(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"datapack_id":"id-1","datapack_name":"team-a"}]}`))
	})
	got, err := c.ListDatapacks(t.Context())
	if err != nil {
		t.Fatalf("ListDatapacks: %v", err)
	}
	if len(got) != 1 || got[0].ID != "id-1" || got[0].Name != "team-a" {
		t.Fatalf("ListDatapacks = %+v", got)
	}
}

// An error in the envelope is a failure whatever the status line says. Trusting
// the status alone is the other half of the same trap: a 200 carrying an error
// object would decode into an empty payload and read as an empty world.
func TestEnvelopeErrorOn200IsAnError(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":null,"error":{"code":"NOT_FOUND","message":"nope"}}`))
	})
	_, err := c.ListDatapacks(t.Context())
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *Error", err)
	}
	if apiErr.Code != "NOT_FOUND" {
		t.Errorf("code = %q, want NOT_FOUND", apiErr.Code)
	}
}

// The machine-readable code is what callers branch on, so it has to survive the
// status: a 402 and a 400 are both rejections but only one of them is permanent.
func TestErrorCarriesStatusAndCode(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"code":"FREE_TIER_DATAPACK_LIMIT","message":"upgrade"}}`))
	})
	_, err := c.CreateDatapack(t.Context(), CreateDatapackRequest{Name: "x"})
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *Error", err)
	}
	if apiErr.StatusCode != http.StatusPaymentRequired || apiErr.Code != "FREE_TIER_DATAPACK_LIMIT" {
		t.Errorf("error = %+v", apiErr)
	}
}

// A body that is not the envelope at all — a proxy's HTML error page in front of
// the api-server — must fail rather than decode to nothing.
func TestNonEnvelopeBodyIsAnError(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>502 Bad Gateway</html>`))
	})
	if _, err := c.ListDatapacks(t.Context()); err == nil {
		t.Fatal("a non-JSON body must be an error")
	}
}

// A verb with nothing to return must not demand a payload: PATCH and DELETE
// reply with an empty data field.
func TestVerbsWithNoPayloadSucceedOnAnEmptyEnvelope(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"data":null}`))
	})
	if err := c.UpdateDatapack(t.Context(), "id-1", UpdateDatapackRequest{Description: "d"}); err != nil {
		t.Errorf("UpdateDatapack: %v", err)
	}
	if err := c.DeleteDatapack(t.Context(), "id-1"); err != nil {
		t.Errorf("DeleteDatapack: %v", err)
	}
}

// The legacy request (no limit) is what ListDatapacks sends, and a bare array is
// the complete set.
func TestListDatapacksSendsTheLegacyRequest(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("first request query = %q, want none", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"data":[{"datapack_id":"a","datapack_name":"one"},{"datapack_id":"b","datapack_name":"two"}]}`))
	})
	got, err := c.ListDatapacks(t.Context())
	if err != nil {
		t.Fatalf("ListDatapacks: %v", err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("ListDatapacks = %+v", got)
	}
}

// pagedServer answers the first request and every cursor with the page the
// handler returns for it.
func pagedServer(t *testing.T, pages map[string]string) (*Client, *[]string) {
	t.Helper()
	var queries []string
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		cur := r.URL.Query().Get("cursor")
		if cur != "" {
			q := r.URL.Query()
			if q.Get("limit") != "1000" || q.Get("fields") != "full" || q.Get("sort") != "created" {
				t.Errorf("page query = %q", r.URL.RawQuery)
			}
		}
		body, ok := pages[cur]
		if !ok {
			t.Errorf("unexpected cursor %q", cur)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"INVALID_CURSOR","message":"bad"}}`))
			return
		}
		if strings.HasPrefix(body, "status:") {
			w.WriteHeader(http.StatusInternalServerError)
			body = strings.TrimPrefix(body, "status:")
		}
		_, _ = w.Write([]byte(body))
	})
	return c, &queries
}

// A server that pages the response is followed to the end, in order.
func TestListDatapacksFollowsThreePages(t *testing.T) {
	c, queries := pagedServer(t, map[string]string{
		"":     `{"data":{"items":[{"datapack_id":"1"},{"datapack_id":"2"}],"has_more":true,"next_cursor":"c1","limit":2}}`,
		"c1":   `{"data":{"items":[{"datapack_id":"3"},{"datapack_id":"4"}],"has_more":true,"next_cursor":"c/2+","limit":2}}`,
		"c/2+": `{"data":{"items":[{"datapack_id":"5"}],"has_more":false,"next_cursor":"","limit":2}}`,
	})
	got, err := c.ListDatapacks(t.Context())
	if err != nil {
		t.Fatalf("ListDatapacks: %v", err)
	}
	var ids []string
	for _, d := range got {
		ids = append(ids, d.ID)
	}
	if strings.Join(ids, ",") != "1,2,3,4,5" {
		t.Fatalf("ids = %v", ids)
	}
	if len(*queries) != 3 {
		t.Fatalf("requests = %v, want 3", *queries)
	}
}

// A failed page fails the whole list. Returning the pages before it would read
// as a complete inventory and the backend would collect everything after them.
func TestListDatapacksFailsOnAPageErrorInTheMiddle(t *testing.T) {
	c, _ := pagedServer(t, map[string]string{
		"":   `{"data":{"items":[{"datapack_id":"1"}],"has_more":true,"next_cursor":"c1","limit":1}}`,
		"c1": `status:{"error":{"code":"INTERNAL_ERROR","message":"db down"}}`,
		"c2": `{"data":{"items":[{"datapack_id":"3"}],"has_more":false,"next_cursor":"","limit":1}}`,
	})
	got, err := c.ListDatapacks(t.Context())
	if err == nil {
		t.Fatalf("ListDatapacks = %+v, want an error", got)
	}
	if got != nil {
		t.Errorf("partial result returned with the error: %+v", got)
	}
}

// An old replica mid-rollout ignores limit and answers with the bare array; that
// is the complete set and replaces whatever earlier pages returned.
func TestListDatapacksTreatsABareArrayMidLoopAsComplete(t *testing.T) {
	c, _ := pagedServer(t, map[string]string{
		"":   `{"data":{"items":[{"datapack_id":"1"}],"has_more":true,"next_cursor":"c1","limit":1}}`,
		"c1": `{"data":[{"datapack_id":"1"},{"datapack_id":"2"}]}`,
	})
	got, err := c.ListDatapacks(t.Context())
	if err != nil {
		t.Fatalf("ListDatapacks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListDatapacks = %+v, want the 2-row array", got)
	}
}

// Cursor bugs must fail rather than loop or stop early.
func TestListDatapacksRejectsABrokenCursorChain(t *testing.T) {
	for name, pages := range map[string]map[string]string{
		"empty cursor with has_more": {
			"": `{"data":{"items":[{"datapack_id":"1"}],"has_more":true,"next_cursor":""}}`,
		},
		"repeated cursor": {
			"":   `{"data":{"items":[{"datapack_id":"1"}],"has_more":true,"next_cursor":"c1"}}`,
			"c1": `{"data":{"items":[{"datapack_id":"2"}],"has_more":true,"next_cursor":"c1"}}`,
		},
		"object without items": {
			"": `{"data":{"has_more":false}}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := pagedServer(t, pages)
			if got, err := c.ListDatapacks(t.Context()); err == nil {
				t.Fatalf("ListDatapacks = %+v, want an error", got)
			}
		})
	}
}

// A streamed list that fails mid-way reaches the client as a truncated body. It
// must be an error, never the rows that arrived before the cut.
func TestListDatapacksTruncatedBodyIsAnError(t *testing.T) {
	for name, body := range map[string]string{
		"cut mid-row":   `{"data":[{"datapack_id":"a","datapack_name":"one"},{"datapack_id":"b","datap`,
		"cut after row": `{"data":[{"datapack_id":"a","datapack_name":"one"},`,
		"no closer":     `{"data":[{"datapack_id":"a","datapack_name":"one"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			c := client(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			})
			if got, err := c.ListDatapacks(t.Context()); err == nil {
				t.Fatalf("ListDatapacks = %+v, want an error", got)
			}
		})
	}
}

// The api-server closes the connection when a stream fails after the first
// byte. With chunked encoding the client then sees an unexpected EOF.
func TestListDatapacksConnectionCutMidStreamIsAnError(t *testing.T) {
	var setupErr string
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		flusher, ok := w.(http.Flusher)
		if !ok {
			setupErr = "response has no flusher"
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"datapack_id":"a","datapack_name":"one"},`))
		flusher.Flush()
		hj, ok := w.(http.Hijacker)
		if !ok {
			setupErr = "response has no hijacker"
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			setupErr = "hijack: " + err.Error()
			return
		}
		_ = conn.Close()
	})
	if got, err := c.ListDatapacks(t.Context()); err == nil {
		t.Fatalf("ListDatapacks = %+v, want an error", got)
	}
	if setupErr != "" {
		t.Fatal(setupErr)
	}
}
