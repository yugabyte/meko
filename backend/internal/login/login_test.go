package login

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/yugabyte/meko/backend/internal/auth"
	"github.com/yugabyte/meko/backend/internal/store"
)

func newService(t *testing.T) (*Service, *time.Time) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "meko.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err = st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s := New(st)
	s.now = func() time.Time { return now }
	return s, &now
}

func TestSetupAndLogin(t *testing.T) {
	s, now := newService(t)
	ctx := context.Background()
	revoked := 0
	s.OnRevoke(func() { revoked++ })

	if _, err := s.Login(ctx, "ana", "a long password", "", "10.0.0.1", ""); !errors.Is(err, ErrBadLogin) {
		t.Fatalf("login with no owner: %v", err)
	}
	token, err := s.IssueSetupLink(ctx)
	if err != nil || !s.SetupOutstanding(ctx) {
		t.Fatalf("setup link: %v", err)
	}
	var check CheckError
	if _, err = s.Setup(ctx, token, "ana", "short", "10.0.0.1", ""); !errors.As(err, &check) {
		t.Fatalf("a short password: %v", err)
	}
	if _, err = s.Setup(ctx, "not-the-token", "ana", "a long password", "10.0.0.1", ""); !errors.Is(err, ErrBadSetupLink) {
		t.Fatalf("a wrong link: %v", err)
	}
	cookie, err := s.Setup(ctx, token, "ana", "a long password", "10.0.0.1", "ua")
	if err != nil || cookie == "" || revoked != 1 {
		t.Fatalf("setup: %q %v (revoked %d)", cookie, err, revoked)
	}
	if _, err = s.Setup(ctx, token, "ana", "a long password", "10.0.0.1", ""); !errors.Is(err, ErrBadSetupLink) {
		t.Fatalf("a used link: %v", err)
	}
	if ok, _, serr := s.Session(ctx, cookie); !ok || serr != nil {
		t.Fatalf("the setup's session: %v %v", ok, serr)
	}

	// Sessions slide forward at most hourly, and run out.
	*now = now.Add(2 * time.Hour)
	if ok, renewed, _ := s.Session(ctx, cookie); !ok || !renewed {
		t.Fatalf("after 2 h: ok %v renewed %v", ok, renewed)
	}
	if ok, renewed, _ := s.Session(ctx, cookie); !ok || renewed {
		t.Fatalf("right after a renewal: ok %v renewed %v", ok, renewed)
	}
	*now = now.Add(SessionTTL + time.Minute)
	if ok, _, _ := s.Session(ctx, cookie); ok {
		t.Fatal("an expired session is signed in")
	}

	if _, err = s.Login(ctx, "ana", "wrong password!", "", "10.0.0.1", ""); !errors.Is(err, ErrBadLogin) {
		t.Fatalf("a wrong password: %v", err)
	}
	cookie, err = s.Login(ctx, "ana", "a long password", "", "10.0.0.1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Logout(ctx, cookie); err != nil || s.Live(ctx, Hash(cookie)) {
		t.Fatalf("logout: %v", err)
	}
}

func TestTwoStepAndPassword(t *testing.T) {
	s, now := newService(t)
	ctx := context.Background()
	token, _ := s.IssueSetupLink(ctx)
	mine, err := s.Setup(ctx, token, "ana", "a long password", "10.0.0.1", "")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := s.Login(ctx, "ana", "a long password", "", "10.0.0.2", "")

	secret, url, err := s.EnrollTOTP(ctx)
	if err != nil || url == "" {
		t.Fatal(err)
	}
	if err = s.ConfirmTOTP(ctx, "000000"); !errors.Is(err, ErrBadTOTP) && auth.TOTPCode(secret, *now) != "000000" {
		t.Fatalf("a wrong code confirmed: %v", err)
	}
	if err = s.ConfirmTOTP(ctx, auth.TOTPCode(secret, *now)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Login(ctx, "ana", "a long password", "", "10.0.0.3", ""); !errors.Is(err, ErrTOTPRequired) {
		t.Fatalf("no code: %v", err)
	}
	// The code that confirmed can't sign in: each code is used once.
	used := auth.TOTPCode(secret, *now)
	if _, err = s.Login(ctx, "ana", "a long password", used, "10.0.0.3", ""); !errors.Is(err, ErrBadTOTP) {
		t.Fatalf("a replayed code: %v", err)
	}
	*now = now.Add(30 * time.Second)
	if _, err = s.Login(ctx, "ana", "a long password", auth.TOTPCode(secret, *now), "10.0.0.3", ""); err != nil {
		t.Fatalf("the next code: %v", err)
	}

	if err = s.ChangePassword(ctx, "not it", "another long one", mine); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("a wrong current password: %v", err)
	}
	if err = s.ChangePassword(ctx, "a long password", "another long one", mine); err != nil {
		t.Fatal(err)
	}
	if !s.Live(ctx, Hash(mine)) || s.Live(ctx, Hash(other)) {
		t.Fatal("a password change didn't keep this browser and sign out the others")
	}
	*now = now.Add(30 * time.Second)
	if err = s.DisableTOTP(ctx, auth.TOTPCode(secret, *now)); err != nil {
		t.Fatal(err)
	}
	if o, _ := s.Owner(ctx); o.TOTPSecret != "" {
		t.Fatal("two-step sign-in still on")
	}
}

func TestRateLimitAndImport(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	hash, _ := auth.HashPassword("a long password")
	if ok, err := s.ImportOwner(ctx, store.Owner{Username: "ana", PWScrypt: hash}); !ok || err != nil {
		t.Fatalf("import: %v %v", ok, err)
	}
	for range 5 {
		_, _ = s.Login(ctx, "ana", "wrong password!", "", "10.0.0.1", "")
	}
	if _, err := s.Login(ctx, "ana", "a long password", "", "10.0.0.1", ""); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("after 5 failures: %v", err)
	}
	// A setup link still gets a locked-out owner in.
	token, _ := s.IssueSetupLink(ctx)
	if _, err := s.Setup(ctx, token, "ana", "a new long password", "10.0.0.1", ""); err != nil {
		t.Fatalf("setup while limited: %v", err)
	}
}

// Sign-ins that arrive together get no more guesses than ones one at a time, and only a few run
// scrypt at once.
func TestLoginBurst(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	hash, _ := auth.HashPassword("a long password")
	if _, err := s.ImportOwner(ctx, store.Owner{Username: "ana", PWScrypt: hash}); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 20)
	for range 20 {
		go func() {
			_, err := s.Login(ctx, "ana", "wrong password!", "", "10.0.0.1", "")
			errs <- err
		}()
	}
	guesses := 0
	for range 20 {
		err := <-errs
		switch {
		case errors.Is(err, ErrBadLogin):
			guesses++
		case errors.Is(err, ErrRateLimited), errors.Is(err, ErrBusy):
		default:
			t.Fatalf("unexpected: %v", err)
		}
	}
	if guesses == 0 || guesses > 5 {
		t.Fatalf("%d guesses got checked, want 1 to 5", guesses)
	}
	if _, err := s.Login(ctx, "ana", "a long password", "", "10.0.0.2", ""); err != nil {
		t.Fatalf("another client: %v", err)
	}
}
