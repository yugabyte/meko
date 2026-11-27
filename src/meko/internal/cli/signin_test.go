package cli

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/server"
	"github.com/yugabyte/meko/src/meko/internal/store"
)

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

// meko ui opens this computer's own address while remote access asks other computers to sign in,
// so the owner here never meets the sign-in page; else the server's link.
func TestLocalUILink(t *testing.T) {
	t.Setenv("MEKO_PORT", "7123") // another test's server may have set it
	l := paths.New(t.TempDir())
	remote := &server.WebStatus{URL: "https://my-meko.example:7123", Access: config.AccessRemote}
	link := remote.URL + "/"
	if got := localUILink(l, remote, link); got != link {
		t.Errorf("no config, so no sign-in: %q", got)
	}
	if err := os.WriteFile(l.Config, []byte("[server]\nport = 7123\n\n[web]\nsignin = \"cloud\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := localUILink(l, remote, link); got != "http://127.0.0.1:7123/" {
		t.Errorf("remote with sign-in: %q", got)
	}
	local := &server.WebStatus{URL: "http://127.0.0.1:7123", Access: config.AccessLocal}
	if got := localUILink(l, local, local.URL+"/"); got != local.URL+"/" {
		t.Errorf("local access: %q", got)
	}
}
