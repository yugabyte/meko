package store

import (
	"context"
	"testing"
	"time"
)

func TestSessions(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	valid := func(sha string, at time.Time) bool {
		t.Helper()
		ok, err := s.SessionValid(ctx, sha, at)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if err := s.CreateSession(ctx, "a", now.Add(time.Hour), "Firefox", "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, "b", now.Add(2*time.Hour), "", ""); err != nil {
		t.Fatal(err)
	}
	if !valid("a", now) || !valid("b", now) || valid("c", now) {
		t.Fatal("a and b should be valid, an unknown one not")
	}
	if valid("a", now.Add(time.Hour)) || !valid("b", now.Add(time.Hour)) {
		t.Fatal("a should have expired at its expiry, b not")
	}
	if err := s.DeleteSession(ctx, "a"); err != nil || valid("a", now) {
		t.Fatalf("a should be gone after DeleteSession: %v", err)
	}
	if err := s.DeleteSession(ctx, "a"); err != nil {
		t.Fatalf("deleting a gone session: %v", err)
	}
	if err := s.DeleteAllSessions(ctx); err != nil || valid("b", now) {
		t.Fatalf("b should be gone after DeleteAllSessions: %v", err)
	}
}

func TestCreateSessionDropsExpired(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, "old", time.Now().Add(-time.Minute), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, "new", time.Now().Add(time.Hour), "", ""); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.r.QueryRowContext(ctx, "SELECT count(*) FROM auth_sessions").Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d, %v; want only the new session", n, err)
	}
}
