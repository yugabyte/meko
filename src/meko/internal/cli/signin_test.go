package cli

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/cloudlink"
	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/secrets"
	"github.com/yugabyte/meko/src/meko/internal/store"
)

// serve refuses Meko Cloud sign-in while Meko Cloud isn't linked: no one could sign in.
func TestSignInNeedsLink(t *testing.T) {
	dir := t.TempDir()
	st := cloudlink.Store{File: filepath.Join(dir, "cloud.toml"), Secrets: secrets.NewFiles(dir)}
	cfg := config.Default()
	if err := signInNeedsLink(cfg, st); err != nil {
		t.Fatalf("signin none: %v", err)
	}
	cfg.Web.SignIn = config.SignInCloud
	if err := signInNeedsLink(cfg, st); !errors.Is(err, errSignInUnlinked) {
		t.Fatalf("signin cloud, unlinked: %v", err)
	}
	svc := &cloudlink.Service{Store: st, Check: func(context.Context, string, string) error { return nil },
		CheckAPI: func(context.Context, string) error { return nil }}
	key := testCloudKey
	if _, err := svc.Link(context.Background(), cloudlink.Update{Key: &key}); err != nil {
		t.Fatal(err)
	}
	if err := signInNeedsLink(cfg, st); err != nil {
		t.Fatalf("signin cloud, linked: %v", err)
	}
}

// A link change made with no server running ends the browsers' sessions too.
func TestEndSessions(t *testing.T) {
	ctx := context.Background()
	l := paths.New(t.TempDir())
	if err := endSessions(ctx, l); err != nil {
		t.Fatalf("no database yet: %v", err)
	}
	st, err := store.Open(l.DB)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateSession(ctx, "sha", time.Now().Add(time.Hour), "", ""); err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	if err = endSessions(ctx, l); err != nil {
		t.Fatal(err)
	}
	if st, err = store.Open(l.DB); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if ok, err := st.SessionValid(ctx, "sha", time.Now()); ok || err != nil {
		t.Fatalf("session still valid after endSessions: %v", err)
	}
}
