package storagecontroller

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ReportedVersion runs `<bin> --version` (bin resolved per resolveBin) and
// returns its trimmed output — the bundled storage_controller's self-reported
// version. Best-effort: a short timeout bounds it, and any error is returned to
// the caller (which treats the version as unavailable).
func ReportedVersion(bin string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, resolveBin(bin), "--version").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// resolveBin locates the storage controller binary given the configured bin
// value.
//
// A value containing a path separator (absolute, or explicitly relative like
// ./storage_controller) is used verbatim. A bare name is resolved against the
// directory of the running controller executable — the bundle layout that
// `make controller-bundle` and the deploy/controller container produce, where controller and
// storage_controller sit side by side — so the default works with no
// configuration even when the bundle is not on PATH. If no such sibling
// exists, the bare name is returned unchanged so exec falls back to its normal
// PATH lookup.
func resolveBin(bin string) string {
	if strings.ContainsRune(bin, filepath.Separator) {
		return bin
	}
	exe, err := os.Executable()
	if err != nil {
		return bin
	}
	if sibling := filepath.Join(filepath.Dir(exe), bin); isFile(sibling) {
		return sibling
	}
	return bin
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
