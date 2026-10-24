package store

import (
	"context"
	"testing"
	"time"
)

func TestOwnerAndSessions(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	if o, err := s.Owner(ctx); err != nil || o != nil {
		t.Fatalf("a new database has an owner: %+v %v", o, err)
	}
	// An imported owner keeps its hash and two-step secret, and only fills an empty table.
	if ok, err := s.ImportOwner(ctx, Owner{Username: "ana", PWScrypt: "h1", TOTPSecret: "S1"}); err != nil || !ok {
		t.Fatalf("import: %v %v", ok, err)
	}
	if ok, err := s.ImportOwner(ctx, Owner{Username: "bo", PWScrypt: "h2"}); err != nil || ok {
		t.Fatalf("second import: %v %v", ok, err)
	}
	o, err := s.Owner(ctx)
	if err != nil || *o != (Owner{Username: "ana", PWScrypt: "h1", TOTPSecret: "S1"}) {
		t.Fatalf("owner %+v %v", o, err)
	}

	now := time.Now()
	for _, h := range []string{"a", "b"} {
		if err = s.CreateAuthSession(ctx, h, "ua", "10.0.0.1", now, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	exp, ok, err := s.AuthSessionExpiry(ctx, "a")
	if err != nil || !ok || exp.Sub(now.Add(time.Hour)).Abs() > time.Microsecond {
		t.Fatalf("expiry %v %v %v", exp, ok, err)
	}
	if err = s.RenewAuthSession(ctx, "a", now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// A password change keeps the browser that made it.
	if err = s.SetPassword(ctx, "h3", "a"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.AuthSessionExpiry(ctx, "b"); ok {
		t.Fatal("another browser stayed signed in after a password change")
	}
	if _, ok, _ := s.AuthSessionExpiry(ctx, "a"); !ok {
		t.Fatal("the browser that changed the password was signed out")
	}

	// Enrolling, confirming and turning off two-step sign-in.
	if err = s.DisableTOTP(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTOTPPending(ctx, "S2"); err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmTOTP(ctx); err != nil {
		t.Fatal(err)
	}
	if o, _ = s.Owner(ctx); o.TOTPSecret != "S2" || o.TOTPPending != "" || o.PWScrypt != "h3" {
		t.Fatalf("after confirm %+v", o)
	}

	// A new login (a setup link) replaces the old one, turns two-step off and signs everyone out.
	if err = s.SetOwner(ctx, "cy", "h4"); err != nil {
		t.Fatal(err)
	}
	if o, _ = s.Owner(ctx); *o != (Owner{Username: "cy", PWScrypt: "h4"}) {
		t.Fatalf("after setup %+v", o)
	}
	if _, ok, _ := s.AuthSessionExpiry(ctx, "a"); ok {
		t.Fatal("a browser stayed signed in after a new login")
	}
	if err = s.DeleteAuthSession(ctx, "missing"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetSetting(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteSetting(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Setting(ctx, "k"); ok {
		t.Fatal("setting still there")
	}
}
