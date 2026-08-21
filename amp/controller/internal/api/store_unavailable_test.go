package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/config"
	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/db"
	"github.com/yugabyte/amp/controller/internal/task"
)

// storeFailBackend fails every read and every write with a fixed error,
// delegating the rest to MemBackend, to drive the classification the API applies
// to an error escaping a controller.
type storeFailBackend struct {
	*core.MemBackend
	err error
}

func (b storeFailBackend) GetUser(context.Context, uuid.UUID) (core.User, error) {
	return core.User{}, fmt.Errorf("get user: %w", b.err)
}

func (b storeFailBackend) CreateStoragePool(context.Context, uuid.UUID, string) (core.StoragePool, error) {
	return core.StoragePool{}, fmt.Errorf("create storage pool: %w", b.err)
}

// An unreachable state store is a retryable 503, not the catch-all 500 a caller
// cannot tell from a controller bug (#886). A read is always replayable; a
// mutation only when the failure provably preceded the statement reaching the
// store, since one that died mid-statement may have committed.
func TestStoreUnavailableStatusCodes(t *testing.T) {
	// A pgx connection attempt that never reached the tserver — the failure the
	// controller reported as a 500.
	_, dialErr := pgconn.Connect(context.Background(), "postgres://yugabyte@127.0.0.1:1/yb_storage")
	if dialErr == nil {
		t.Skip("something is listening on 127.0.0.1:1")
	}

	cases := []struct {
		name     string
		err      error
		wantRead int
		wantWrit int
	}{
		{
			name:     "connection attempt failed",
			err:      dialErr,
			wantRead: http.StatusServiceUnavailable,
			wantWrit: http.StatusServiceUnavailable,
		},
		{
			name:     "connection died under the statement",
			err:      &net.OpError{Op: "write", Net: "tcp", Err: errors.New("broken pipe")},
			wantRead: http.StatusServiceUnavailable,
			// The write may have committed before the connection dropped, so
			// telling the caller to retry could double-apply it.
			wantWrit: http.StatusInternalServerError,
		},
		{
			name: "connection attempt failed after a durable write",
			err:  db.AfterWrite(dialErr),
			// The store says a write in this request already landed, so neither a
			// read nor a mutation may be replayed on the strength of the error type.
			wantRead: http.StatusInternalServerError,
			wantWrit: http.StatusInternalServerError,
		},
		{
			name:     "an ordinary controller error",
			err:      errors.New("boom"),
			wantRead: http.StatusInternalServerError,
			wantWrit: http.StatusInternalServerError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := newTestServerBackend(t, storeFailBackend{core.NewMemBackend(), tc.err}, api.Provisioning{})
			ctx := context.Background()

			read, err := client.GetUserWithResponse(ctx, uuid.New())
			if err != nil {
				t.Fatalf("GetUser: %v", err)
			}
			assertStatus(t, read.StatusCode(), tc.wantRead, read.Body, read.HTTPResponse.Header)

			write, err := client.CreateStoragePoolWithResponse(ctx, api.CreateStoragePoolRequest{OwnerUserId: uuid.New(), Name: "pool"})
			if err != nil {
				t.Fatalf("CreateStoragePool: %v", err)
			}
			assertStatus(t, write.StatusCode(), tc.wantWrit, write.Body, write.HTTPResponse.Header)
		})
	}
}

// assertStatus checks the status, the error code in the envelope, and that a 503
// tells the caller when to come back.
func assertStatus(t *testing.T, got, want int, body []byte, header http.Header) {
	t.Helper()
	if got != want {
		t.Fatalf("status = %d, want %d; body = %s", got, want, body)
	}
	wantCode := "internal"
	if want == http.StatusServiceUnavailable {
		wantCode = "service_unavailable"
	}
	if code := errorCode(t, body); code != wantCode {
		t.Errorf("error code = %q, want %q", code, wantCode)
	}
	if retry := header.Get("Retry-After"); (retry != "") != (want == http.StatusServiceUnavailable) {
		t.Errorf("Retry-After = %q on a %d", retry, want)
	}
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var env api.Error
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode error envelope %s: %v", body, err)
	}
	return env.Error.Code
}

// A store outage must not spend the ERROR level: #886 is partly about a backing
// restart flooding the logs, which leaves ERROR useless as "look at the
// controller". The 503 logs at WARN — both the classification line and the
// access-log line the request middleware writes.
func TestStoreUnavailableLogsBelowError(t *testing.T) {
	_, dialErr := pgconn.Connect(context.Background(), "postgres://yugabyte@127.0.0.1:1/yb_storage")
	if dialErr == nil {
		t.Skip("something is listening on 127.0.0.1:1")
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tasks := task.NewMemStore()
	srv := api.NewServer(&config.Config{ListenAddr: ":0"},
		storeFailBackend{core.NewMemBackend(), dialErr}, task.NewService(tasks), api.Provisioning{}, nil, logger)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	client, err := api.NewClientWithResponses(ts.URL + "/api/v1")
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}

	resp, err := client.GetUserWithResponse(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if resp.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode())
	}
	logs := buf.String()
	if strings.Contains(logs, "level=ERROR") {
		t.Errorf("a 503 logged at ERROR:\n%s", logs)
	}
	if !strings.Contains(logs, "state store unavailable") {
		t.Errorf("no line names the cause:\n%s", logs)
	}
}
