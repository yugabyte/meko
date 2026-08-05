package migration

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCheckConnectivityRejectsInvalidPort(t *testing.T) {
	for _, port := range []int{0, -1, 99999} {
		result := CheckConnectivity(context.Background(), SourceConfig{
			Host:     "db.internal",
			Port:     port,
			Username: "app_user",
			Password: "secret",
			Database: "prod",
		})
		if result.Reachable {
			t.Fatalf("port %d: expected Reachable=false", port)
		}
		if result.Error == nil {
			t.Fatalf("port %d: expected an error message", port)
		}
		if !strings.Contains(*result.Error, "source database is not reachable") {
			t.Fatalf("port %d: message %q missing standard prefix", port, *result.Error)
		}
		if strings.Contains(*result.Error, "secret") {
			t.Fatalf("port %d: message %q leaked the password", port, *result.Error)
		}
	}
}

func TestFailedConnectivityRedactsPassword(t *testing.T) {
	source := SourceConfig{Password: "sup3r-secret"}
	result := failedConnectivity(source, errors.New(`failed to connect: password "sup3r-secret" rejected`))

	if result.Reachable {
		t.Fatal("expected Reachable=false")
	}
	if result.Error == nil {
		t.Fatal("expected an error message")
	}
	if strings.Contains(*result.Error, "sup3r-secret") {
		t.Fatalf("message %q leaked the password", *result.Error)
	}
	if !strings.Contains(*result.Error, "[REDACTED]") {
		t.Fatalf("message %q did not redact the password", *result.Error)
	}
}

func TestFailedConnectivityWithoutPassword(t *testing.T) {
	result := failedConnectivity(SourceConfig{}, errors.New("host not found"))
	if result.Reachable || result.Error == nil {
		t.Fatal("expected an unreachable result with a message")
	}
	if !strings.Contains(*result.Error, "host not found") {
		t.Fatalf("message %q dropped the underlying error", *result.Error)
	}
}
