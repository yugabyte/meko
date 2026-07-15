// Package version carries the controller's build identity. The values are
// stamped at build time via -ldflags -X (see the Makefile's
// controller-bundle-build target); an un-stamped build (e.g. `go run`) reports
// the "dev"/"unknown" defaults. The single source of truth for the semantic
// version is the repo-root VERSION file, which the Makefile reads into
// Version.
package version

// Stamped via -ldflags -X github.com/yugabyte/yb-amp/controller/internal/version.<var>=<value>.
var (
	// Version is the hybrid CalVer version from the repo-root VERSION file
	// (MAJOR.YYMMDD.PATCH, e.g. "0.260714.0").
	Version = "dev"
	// Commit is the short git commit the binary was built from.
	Commit = "unknown"
	// BuildDate is the UTC RFC-3339 build timestamp.
	BuildDate = "unknown"
)

// storageController is the bundled storage_controller's self-reported version,
// captured at startup via `storage_controller --version` (see SetStorageController).
// Empty when no managed storage_controller is running (e.g. the Docker driver).
// It is a sibling release stamped from the same VERSION file (Makefile BUILD_TAG).
var storageController string

// SetStorageController records the bundled storage_controller's version string.
// Called once at startup; safe to leave unset.
func SetStorageController(v string) { storageController = v }

// Info is the build identity as served by GET /api/v1/version and logged at startup.
type Info struct {
	Version           string `json:"version"`
	Commit            string `json:"git_commit"`
	BuildDate         string `json:"build_date"`
	StorageController string `json:"storage_controller,omitempty"`
}

// Get returns the stamped build identity.
func Get() Info {
	return Info{Version: Version, Commit: Commit, BuildDate: BuildDate, StorageController: storageController}
}
