package callhome

import (
	"context"
	"net/http"
	"time"
)

// The envelope's commands for meko uninstall and meko destroy, each sent as it ends.
const (
	UninstallCommand = "uninstall"
	DestroyCommand   = "destroy" // removes the data folder, and leaves meko installed
)

// Uninstall is what meko uninstall and meko destroy report, so an install removed before its next
// heartbeat still says how far it got: only enums, versions and the first commands' names.
type Uninstall struct {
	Schema         int    `json:"schema"`
	Purge          bool   `json:"purge"`          // uninstall --purge: the data folder went too
	WithExtensions bool   `json:"withExtensions"` // destroy --with-extensions
	Version        string `json:"version"`
	Platform       string `json:"platform"`
	Arch           string `json:"arch"`
	Service        string `json:"service"`            // launchd, systemd or none
	FirstDay       string `json:"firstDay,omitempty"` // the UTC day of the install's first heartbeat
	// FirstCommands are the first commands after a first install (the uninstall or destroy last,
	// while there was room), even when a heartbeat sent them all; none for an install that wasn't
	// tracked.
	FirstCommands []Command `json:"firstCommands"`
}

// UninstallEnvelope is an uninstall's or destroy's body: the heartbeat's envelope, with Uninstall
// as its payload.
type UninstallEnvelope struct {
	CallhomeType string    `json:"callhome_type"` // meko
	Command      string    `json:"command"`       // uninstall or destroy
	Timestamp    string    `json:"timestamp"`
	InstallID    string    `json:"install_id"`
	Version      string    `json:"version"`
	Payload      Uninstall `json:"payload"`
}

// SendUninstall posts an uninstall's or destroy's event (command is UninstallCommand or
// DestroyCommand) to url, giving up after FailureTimeout.
func SendUninstall(ctx context.Context, client *http.Client, url, command, installID string, u Uninstall,
	now time.Time) error {
	u.Schema = 1
	if u.FirstCommands == nil {
		u.FirstCommands = []Command{}
	}
	return sendEvent(ctx, client, url, u.Version, UninstallEnvelope{CallhomeType: "meko", Command: command,
		Timestamp: Timestamp(now), InstallID: installID, Version: u.Version, Payload: u})
}
