// Package env holds environment providers. The provider interfaces are defined
// canonically in core (their signatures reference core types such as EnvSpec and
// Seed, and core drives them, so they must live there to keep imports acyclic);
// env re-exports them so callers spell them env.Environment / env.EnvProvider.
package env

import "github.com/yugabyte/yb-amp/test/functional/framework/core"

type (
	Environment      = core.Environment
	EnvProvider      = core.EnvProvider
	EnvHandles       = core.EnvHandles
	ArtifactSink     = core.ArtifactSink
	DetectedVersions = core.DetectedVersions
)
