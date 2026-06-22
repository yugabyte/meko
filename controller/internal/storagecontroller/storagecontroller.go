// Package storagecontroller manages the storage controller: a placement-group-
// global service (amp-controller.md §5.1) the amp controller runs as a
// managed subprocess (§11.1) and is the only caller of. It builds the
// process.Spec the supervisor runs, and a thin HTTP client for the
// timeline-management API the controller drives (issue #94).
//
// API-first (§3) governs the controller's own user-facing surface, not the
// storage controller's wire: this client is internal plumbing, never exposed to
// users.
package storagecontroller

import (
	"time"

	"github.com/yugabyte/yb-amp/controller/internal/config"
	"github.com/yugabyte/yb-amp/controller/internal/process"
)

// Spec builds the process.Spec for the managed storage controller from cfg. The
// readiness probe hits the controller's /ready endpoint, so the supervisor only
// reports it ready once it is actually serving.
//
// Argument shape mirrors how yb_light_local launches the binary today
// (control_plane/src/storage_controller.rs): --listen for the HTTP address,
// --database-url for the universe connection, and --dev to permit running
// without auth in dev/local. Production auth tokens are layered on later as the
// managed-service lifecycle (§11) fills in.
func Spec(cfg config.StorageControllerConfig) process.Spec {
	args := []string{"--listen", cfg.ListenAddr}
	if cfg.DatabaseURL != "" {
		args = append(args, "--database-url", cfg.DatabaseURL)
	}
	if cfg.Dev {
		args = append(args, "--dev")
	}
	args = append(args, cfg.ExtraArgs...)

	return process.Spec{
		Name:         "storage_controller",
		Path:         resolveBin(cfg.BinPath),
		Args:         args,
		Ready:        NewClient(cfg.ListenAddr).Ready,
		ReadyTimeout: cfg.StartTimeout,
		StopTimeout:  cfg.StopTimeout,
		Restart: process.Backoff{
			Initial:    time.Second,
			Max:        30 * time.Second,
			ResetAfter: time.Minute,
		},
	}
}
