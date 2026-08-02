// Package registry seeds and reads the storage layer's per-branch state
// (amp_storage.branch_state) for the control plane. It is reached in-process by
// the controller's task framework; it serves no HTTP surface of its own.
package registry

// BranchCreateRecord records how to materialize a branch whose id + name are
// allocated but whose pageserver/safekeeper don't exist yet.
type BranchCreateRecord struct {
	BranchName       string
	PgVersion        int
	AncestorBranchID *string
	StartLSN         string
	// PgSettings merge over the ancestor's inherited set, the caller's winning.
	// Nil inherits unchanged.
	PgSettings map[string]string
}
