// Package proc spawns and stops the background processes that make up a local
// yb-amp deployment (pageserver, safekeeper, controller, endpoint_storage),
// applying the common log-file and pidfile conventions to each.
//
// It is a focused Go port of control_plane/src/background_process.rs. The
// launcher does not itself stay running, so it records each spawned process by
// storing its pid in a "<name>.pid" file. Depending on the binary, that file is
// either written by the child itself (PidExpect) or created and locked by the
// launcher and inherited by the child (PidCreate). The stored pid is later used
// to stop the service.
//
// This is the minimal yb-amp port: no TLS and Trust auth only.
package proc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// These constants control the loop used to poll for process start. The loop
// polls every retryInterval; once a second it prints a dot to show progress,
// and after noticeAfterRetries polls it prints a "still waiting" notice.
const (
	retryInterval      = 100 * time.Millisecond
	dotEveryRetries    = 10
	noticeAfterRetries = 50
)

// PidMode selects how the "<name>.pid" file is managed for a spawned process.
type PidMode int

const (
	// PidExpect: the child creates and flock-locks its own pidfile. The
	// launcher only waits for the file to appear and be locked. Used by the
	// Neon storage binaries (pageserver, safekeeper) which follow the pidfile
	// convention themselves.
	PidExpect PidMode = iota
	// PidCreate: the launcher creates the pidfile, takes an advisory flock on
	// it, and passes the open descriptor to the child so the child inherits the
	// lock. Used for binaries that do not manage a pidfile themselves.
	PidCreate
)

// StatusCheck reports whether a freshly spawned process is ready. It returns
// nil once the process passes its readiness check, or a non-nil error while the
// process is not yet ready (the launcher keeps polling until the timeout).
type StatusCheck func() error

// StartProcess launches binPath as a background child process.
//
// It opens "<dataDir>/<name>.log" (append) for the child's combined
// stdout+stderr, scrubs the environment down to the curated allowlist plus
// extraEnv (see buildChildEnv), sets the child's working directory to dataDir,
// and spawns it. It then polls both statusCheck and the pidfile every
// retryInterval until they pass or timeout elapses. On any failure the child is
// SIGKILLed and reaped before returning; on success the child is left running.
func StartProcess(
	name string,
	dataDir string,
	binPath string,
	args []string,
	extraEnv map[string]string,
	pidMode PidMode,
	statusCheck StatusCheck,
	timeout time.Duration,
) error {
	info, err := os.Stat(dataDir)
	if err != nil {
		return fmt.Errorf("stat datadir %q: %w", dataDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("`dataDir` must be a directory: %q", dataDir)
	}

	pidFilePath := filepath.Join(dataDir, name+".pid")

	logPath := filepath.Join(dataDir, name+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s log file %q: %w", name, logPath, err)
	}
	defer logFile.Close()

	cmd := exec.Command(binPath, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Dir = dataDir
	cmd.Env = buildChildEnv(extraEnv)

	// For PidCreate, create+lock the pidfile up front and hand its descriptor
	// to the child so the child inherits the flock.
	var pidFile *os.File
	if pidMode == PidCreate {
		pidFile, err = createLockedPidFile(pidFilePath)
		if err != nil {
			return err
		}
		cmd.ExtraFiles = []*os.File{pidFile}
	}

	if err := cmd.Start(); err != nil {
		if pidFile != nil {
			pidFile.Close()
		}
		return fmt.Errorf("could not spawn %s, see log file %q for details: %w", name, logPath, err)
	}
	pid := cmd.Process.Pid

	// Scopeguard: SIGKILL & reap the child unless we hand off successfully.
	started := false
	defer func() {
		if pidFile != nil {
			pidFile.Close() // child keeps the inherited fd (and lock) alive
		}
		if started {
			return
		}
		fmt.Println("SIGKILL & wait the started process")
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// For PidCreate, record the child's pid in the pidfile we locked so the
	// status check (and later StopProcess) can find it.
	if pidFile != nil {
		if err := writePid(pidFile, pid); err != nil {
			return err
		}
	}

	retries := int(timeout / retryInterval)
	for i := 0; i < retries; i++ {
		ok, err := processStarted(pid, pidFilePath, statusCheck)
		if err != nil {
			return fmt.Errorf("error starting process %q: %w", name, err)
		}
		if ok {
			fmt.Printf("\n%s started and passed status check, pid: %d\n", name, pid)
			started = true
			return nil
		}
		if i == noticeAfterRetries {
			fmt.Printf("\n%s has not started yet, continuing to wait", name)
		}
		if i%dotEveryRetries == 0 {
			fmt.Print(".")
		}
		time.Sleep(retryInterval)
	}
	fmt.Println()
	return fmt.Errorf("%s did not start+pass status checks within %s", name, timeout)
}

// processStarted returns true once the status check passes AND the pidfile is
// locked by exactly our spawned child. Mirrors process_started in
// background_process.rs.
func processStarted(pid int, pidFilePath string, statusCheck StatusCheck) (bool, error) {
	if statusCheck != nil {
		if err := statusCheck(); err != nil {
			// Not ready yet; keep polling.
			return false, nil
		}
	}
	kind, filePid, err := readPidFile(pidFilePath)
	if err != nil {
		return false, err
	}
	switch kind {
	case pidLocked:
		return filePid == pid, nil
	default: // pidNotExist, pidNotHeld
		return false, nil
	}
}
