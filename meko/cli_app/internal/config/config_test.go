package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withTempDir overrides Dir() indirectly by creating a temp directory
// and setting HOME so Dir() resolves there.
func withTempDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	return filepath.Join(tmp, "var", "meko")
}

func TestSaveAndLoadCredentials(t *testing.T) {
	withTempDir(t)

	want := &Credentials{
		AccessToken:  "acc-tok",
		IDToken:      "id-tok",
		RefreshToken: "ref-tok",
		ExpiresIn:    3600,
		Email:        "user@example.com",
		APIBaseURL:   "https://mekodev.com",
		LoggedInAt:   time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC),
	}

	if err := SaveCredentials(want); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}

	got, err := LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}

	if got.AccessToken != want.AccessToken {
		t.Errorf("AccessToken = %q, want %q", got.AccessToken, want.AccessToken)
	}
	if got.Email != want.Email {
		t.Errorf("Email = %q, want %q", got.Email, want.Email)
	}
	if got.APIBaseURL != want.APIBaseURL {
		t.Errorf("APIBaseURL = %q, want %q", got.APIBaseURL, want.APIBaseURL)
	}
	if got.ExpiresIn != want.ExpiresIn {
		t.Errorf("ExpiresIn = %d, want %d", got.ExpiresIn, want.ExpiresIn)
	}
}

func TestLoadCredentials_NotExist(t *testing.T) {
	withTempDir(t)

	got, err := LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil credentials when file absent, got %+v", got)
	}
}

func TestDeleteCredentials(t *testing.T) {
	withTempDir(t)

	creds := &Credentials{
		AccessToken: "tok",
		Email:       "x@example.com",
		APIBaseURL:  "https://mekodev.com",
		LoggedInAt:  time.Now(),
	}
	if err := SaveCredentials(creds); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}

	if err := DeleteCredentials(); err != nil {
		t.Fatalf("DeleteCredentials: %v", err)
	}

	got, err := LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials after delete: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil after delete, got %+v", got)
	}
}

func TestDeleteCredentials_NotExist(t *testing.T) {
	withTempDir(t)

	if err := DeleteCredentials(); err != nil {
		t.Fatalf("DeleteCredentials on missing file: %v", err)
	}
}

func TestSaveCredentials_FilePermissions(t *testing.T) {
	withTempDir(t)

	creds := &Credentials{
		AccessToken: "tok",
		Email:       "x@example.com",
		APIBaseURL:  "https://mekodev.com",
		LoggedInAt:  time.Now(),
	}
	if err := SaveCredentials(creds); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}

	info, err := os.Stat(credentialsPath())
	if err != nil {
		t.Fatalf("stat credentials: %v", err)
	}

	perm := info.Mode().Perm()
	if perm != 0600 {
		t.Errorf("file permissions = %o, want 0600", perm)
	}
}
