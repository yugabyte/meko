package fetch_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/artifact/fetch"
)

func TestHTTPDownloads(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello world"))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	local, err := fetch.HTTP(context.Background(), srv.Client(), srv.URL, dest)
	if err != nil {
		t.Fatalf("HTTP: %v", err)
	}
	if local.Cached {
		t.Errorf("first fetch must not be reported cached")
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "hello world" {
		t.Fatalf("dest content = %q, %v", data, err)
	}
}

func TestHTTPStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := fetch.HTTP(context.Background(), srv.Client(), srv.URL, filepath.Join(t.TempDir(), "f"))
	if err == nil {
		t.Fatal("expected error on 404")
	}
}

func TestHTTPCacheHitSkipsRedownload(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte("0123456789"))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	if _, err := fetch.HTTP(context.Background(), srv.Client(), srv.URL, dest); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	local, err := fetch.HTTP(context.Background(), srv.Client(), srv.URL, dest)
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if !local.Cached {
		t.Errorf("second fetch should report a cache hit")
	}
}

func TestGithubReleaseResolvesAndDownloadsAsset(t *testing.T) {
	var (
		srv       *httptest.Server
		gotAuth   string
		assetHits int
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/releases/tags/v1.2.3", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"assets":[
			{"name":"tool-linux-amd64.tar.gz","url":"` + srv.URL + `/asset","browser_download_url":"` + srv.URL + `/browser-404"},
			{"name":"tool-darwin-amd64.tar.gz","url":"` + srv.URL + `/other","browser_download_url":"` + srv.URL + `/other-browser"}
		]}`))
	})
	mux.HandleFunc("/asset", func(w http.ResponseWriter, r *http.Request) {
		assetHits++
		if got := r.Header.Get("Authorization"); got != "Bearer secrettoken" {
			t.Errorf("asset request auth header = %q, want Bearer secrettoken", got)
		}
		if got := r.Header.Get("Accept"); got != "application/octet-stream" {
			t.Errorf("asset request Accept header = %q, want application/octet-stream", got)
		}
		_, _ = w.Write([]byte("binary-bytes"))
	})
	// The browser_download_url must NEVER be requested — it 404s for private repos.
	mux.HandleFunc("/browser-404", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("browser_download_url was requested; must use the asset API url")
		w.WriteHeader(http.StatusNotFound)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "tool.tar.gz")
	local, err := fetch.GithubRelease(context.Background(), srv.Client(),
		srv.URL+"/repos/org/repo/releases/tags/v1.2.3", "tool-linux-*.tar.gz", "secrettoken", dest)
	if err != nil {
		t.Fatalf("GithubRelease: %v", err)
	}
	if local.Size == 0 {
		t.Errorf("expected non-zero size")
	}
	if gotAuth != "Bearer secrettoken" {
		t.Errorf("release request auth header = %q, want Bearer secrettoken", gotAuth)
	}
	if assetHits != 1 {
		t.Errorf("asset endpoint hit %d times, want 1", assetHits)
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "binary-bytes" {
		t.Fatalf("dest content = %q, %v", data, err)
	}
}

func TestGithubReleaseNoMatchingAsset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"assets":[{"name":"other.zip","browser_download_url":"x"}]}`))
	}))
	defer srv.Close()

	_, err := fetch.GithubRelease(context.Background(), srv.Client(), srv.URL, "tool-linux-*.tar.gz", "", filepath.Join(t.TempDir(), "f"))
	if err == nil {
		t.Fatal("expected no-match error")
	}
}

func TestS3UsesInjectedRunner(t *testing.T) {
	var gotArgs []string
	dest := filepath.Join(t.TempDir(), "obj.bin")
	runner := func(ctx context.Context, args ...string) ([]byte, error) {
		gotArgs = args
		return nil, os.WriteFile(dest, []byte("s3-data"), 0o644)
	}
	local, err := fetch.S3(context.Background(), runner, "s3://bucket/key", dest)
	if err != nil {
		t.Fatalf("S3: %v", err)
	}
	if local.Size == 0 {
		t.Errorf("expected non-zero size")
	}
	wantArgs := []string{"s3", "cp", "s3://bucket/key", dest}
	if len(gotArgs) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", gotArgs, wantArgs)
	}
	for i := range wantArgs {
		if gotArgs[i] != wantArgs[i] {
			t.Errorf("args[%d] = %q, want %q", i, gotArgs[i], wantArgs[i])
		}
	}
}

func TestS3RunnerErrorPropagates(t *testing.T) {
	runner := func(ctx context.Context, args ...string) ([]byte, error) {
		return nil, errors.New("aws cli failed")
	}
	_, err := fetch.S3(context.Background(), runner, "s3://bucket/key", filepath.Join(t.TempDir(), "obj.bin"))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestFromLocalPathCopiesAndCaches(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.bin")
	if err := os.WriteFile(src, []byte("local-content"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "dest.bin")

	local, err := fetch.FromLocalPath(src, dest)
	if err != nil {
		t.Fatalf("FromLocalPath: %v", err)
	}
	if local.Cached {
		t.Errorf("first copy must not be cached")
	}
	local2, err := fetch.FromLocalPath(src, dest)
	if err != nil {
		t.Fatalf("second FromLocalPath: %v", err)
	}
	if !local2.Cached {
		t.Errorf("second copy should be a cache hit")
	}
}

func TestFromLocalPathMissingSource(t *testing.T) {
	_, err := fetch.FromLocalPath(filepath.Join(t.TempDir(), "nope"), filepath.Join(t.TempDir(), "dest"))
	if err == nil {
		t.Fatal("expected error for missing source")
	}
}
