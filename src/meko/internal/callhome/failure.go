package callhome

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// FailureCommand is the envelope's command for a failed meko install.
const FailureCommand = "install_failure"

// FailureTimeout is the longest a failed install waits to send its report.
const FailureTimeout = 3 * time.Second

// The steps a failed meko install reports (Failure.Step).
const (
	StepChecks  = "checks"  // before anything was written: the service manager, the port
	StepInstall = "install" // app/current, the meko command
	StepData    = "data"    // the data folder, its token and settings
	StepService = "service" // writing and starting the login service
	StepServer  = "server"  // waiting for the server to answer
)

// The error classes a failed meko install reports (Failure.Kind).
const (
	KindServiceManager = "service_manager" // no systemd user manager, or no login session for launchd
	KindPortInUse      = "port_in_use"     // the port the user chose is another program's
	KindNoFreePort     = "no_free_port"    // the port and the ones after it are all taken
	KindStartFailed    = "start_failed"    // the server printed an error and didn't start
	KindStartTimeout   = "start_timeout"   // the server didn't answer in time
	KindRolledBack     = "rolled_back"     // an upgrade's server failed, and the old version is back
	KindError          = "error"           // anything else
)

// Failure is what a failed meko install reports: only enums and versions, never an error's text,
// a path or a host name.
type Failure struct {
	Schema   int    `json:"schema"`
	Step     string `json:"step"`
	Kind     string `json:"kind"`
	Version  string `json:"version"`
	From     string `json:"from,omitempty"` // the version an upgrade replaced
	First    bool   `json:"first"`          // nothing was installed before
	Platform string `json:"platform"`
	Arch     string `json:"arch"`
	Service  string `json:"service"` // launchd, systemd or none
}

// FailureEnvelope is a failure report's body: the heartbeat's envelope, with Failure as its payload.
type FailureEnvelope struct {
	CallhomeType string  `json:"callhome_type"` // meko
	Command      string  `json:"command"`       // install_failure
	Timestamp    string  `json:"timestamp"`
	InstallID    string  `json:"install_id"`
	Version      string  `json:"version"`
	Payload      Failure `json:"payload"`
}

// SendFailure posts a failed install's report to url, giving up after FailureTimeout.
func SendFailure(ctx context.Context, client *http.Client, url, installID string, f Failure, now time.Time) error {
	f.Schema = 1
	body, err := json.Marshal(FailureEnvelope{CallhomeType: "meko", Command: FailureCommand,
		Timestamp: Timestamp(now), InstallID: installID, Version: f.Version, Payload: f})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, FailureTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "meko/"+f.Version)
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	_ = res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("the call home service answered %d", res.StatusCode)
	}
	return nil
}
