// Package version holds the build's version and commit, set at build time:
//
//	go build -ldflags "-X github.com/yugabyte/meko/src/meko/internal/version.Version=0.1.0 \
//	  -X github.com/yugabyte/meko/src/meko/internal/version.Commit=abc1234"
package version

// Version is the release version, or "dev" for a local build.
var Version = "dev"

// Commit is the git commit the binary was built from, or "unknown".
var Commit = "unknown"
