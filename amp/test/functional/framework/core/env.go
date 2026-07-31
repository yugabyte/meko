package core

import (
	"context"

	"github.com/yugabyte/amp/test/functional/framework/exec"
)

// EnvHandles are the concrete identifiers of a live environment, used for
// collection and chaos.
type EnvHandles struct {
	Namespace  string
	Project    string
	Endpoint   string
	Containers []string
}

// ArtifactSink receives collected blobs (logs, cores, configs) during teardown.
type ArtifactSink interface {
	Add(name, kind string, content []byte) error
}

// Environment is a handle to a running yb-amp target. Produced by an
// EnvProvider.
type Environment interface {
	ControllerURL() string // "" for shapes without a controller
	Versions() DetectedVersions
	Exec() exec.RemoteExecutor
	Handles() EnvHandles
	Collect(ctx context.Context, into ArtifactSink) error
}

// EnvProvider provisions, attaches to, and tears down environments.
//
// Canonically defined here (rather than in the env package) because its method
// signatures reference core types (EnvSpec, Seed); the env package re-exports it
// so callers can spell it env.EnvProvider.
type EnvProvider interface {
	// Provision stands up an environment. ctx bounds the provisioning WORK only
	// (it typically carries a provision-timeout the caller cancels the instant
	// Provision returns). Any resource that must outlive the call — port-forwards,
	// SSH sessions, background processes owned by the returned Environment — MUST
	// therefore be created under a provider/env-owned lifetime rooted in
	// context.Background(), never under ctx, or it dies the moment provisioning
	// returns. Such resources are released in Teardown (and provider Close for
	// anything shared across environments).
	Provision(ctx context.Context, spec EnvSpec, seed *Seed) (Environment, error)
	Teardown(ctx context.Context, env Environment) error
	Attach(ctx context.Context, ref string) (Environment, error)
}
