package main

import (
	"os"
	"strings"
	"testing"
)

// --portal beats the config key, so the key stays empty on every pipeline run
// unless the resolved URL is written back. a test that reads
// it for the real release list would otherwise see nothing.
func TestRunWritesTheResolvedPortalURLBackIntoConfig(t *testing.T) {
	body, err := os.ReadFile("run.go")
	if err != nil {
		t.Fatalf("read run.go: %v", err)
	}
	if !strings.Contains(string(body), `cfg.Set("report.portal.url", portalURL)`) {
		t.Error("the resolved portal URL is no longer written back into the config, so a test that reads report.portal.url sees nothing on a --portal run")
	}
}
