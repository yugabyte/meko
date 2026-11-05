package cli

import (
	"bytes"
	"strings"
	"testing"
)

// A bad flag is a usage error (exit 2, pointing at the help); --help alone exits 0.
func TestCloudSyncFlags(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runCloudSync([]string{"--bogus"}, &out, &errOut); code != exitUsage ||
		!strings.Contains(errOut.String(), "See meko cloud_sync --help") {
		t.Errorf("--bogus: exit %d, stderr %q", code, errOut.String())
	}
	out.Reset()
	code := runCloudSync([]string{"--help"}, &out, &errOut)
	if code != exitOK || !strings.Contains(out.String(), "cloud_sync") {
		t.Errorf("--help: exit %d, %q", code, out.String())
	}
	if code := runCloudSync(nil, &out, &errOut); code != exitUsage {
		t.Errorf("no datapack: exit %d", code)
	}
}
