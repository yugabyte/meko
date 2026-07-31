// Package exec defines the remote-execution contract. Concrete transports
// (SSH, kubectl, docker) arrive in later milestones; M0 ships the interface
// only so the environment contract can reference it.
package exec

import "context"

// RemoteExecutor runs commands and copies files on a remote target — an SSH
// host, a k8s pod, or a docker container. Every environment exposes one.
type RemoteExecutor interface {
	Run(ctx context.Context, cmd string) (Result, error)
	Copy(ctx context.Context, localPath, remotePath string) error
}

// Result is the outcome of a single remote command. Captured output is bounded;
// Truncated reports whether stdout or stderr hit the capture cap.
type Result struct {
	ExitCode  int
	Stdout    string
	Stderr    string
	Truncated bool
}
