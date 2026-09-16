package task

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/deploy"
	"github.com/yugabyte/amp/controller/internal/extensions"
)

// resolveBranchExtensions computes a branch's externalized-extension mount set:
// its own opted-in names unioned with the catalog defaults, resolved to pinned
// image references. With no catalog it returns nil — the endpoint ships only
// image-baked extensions (Docker driver / feature off). Unknown names are dropped
// by the catalog, so a stale entry can't wedge the deploy.
func resolveBranchExtensions(ctx context.Context, catalog *extensions.Service, state StateStore, branchID uuid.UUID) ([]deploy.Extension, error) {
	if catalog == nil {
		return nil, nil
	}
	// uuid.Nil for the vestigial cluster id: every backend addresses the branch by
	// its primary key alone and ignores the argument.
	opted, err := state.BranchExtensions(ctx, uuid.Nil, branchID)
	if err != nil {
		return nil, fmt.Errorf("read branch extensions: %w", err)
	}
	resolved, err := catalog.Resolve(ctx, unionStrings(catalog.DefaultNames(), opted))
	if err != nil {
		return nil, fmt.Errorf("resolve extensions: %w", err)
	}
	var specs []deploy.Extension
	for _, x := range resolved {
		specs = append(specs, deploy.Extension{
			Name:    x.Name,
			Image:   x.Image,
			Preload: x.Preload,
			Library: x.Library,
			// Layout/ABI fields are empty for an amp-built extension and set only
			// by a third-party image (postgis, from CloudNativePG).
			SharePath:      x.Layout.SharePath,
			LibPath:        x.Layout.LibPath,
			LibraryPath:    x.Layout.LibraryPath,
			EnvPaths:       x.Layout.EnvPaths,
			ABIExtraExempt: x.ABIExtraExempt,
		})
	}
	return specs, nil
}

// extensionNames is the rendered mount set's names — the form recorded on the
// endpoint row for recreate/audit. Derived from the spec that was deployed, so
// the row can only claim what was actually rendered.
func extensionNames(exts []deploy.Extension) []string {
	if len(exts) == 0 {
		return nil
	}
	names := make([]string, len(exts))
	for i, e := range exts {
		names[i] = e.Name
	}
	return names
}

// unionStrings returns a ∪ b preserving order (a first, then b's new entries),
// de-duplicated.
func unionStrings(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	var out []string
	for _, s := range append(append([]string(nil), a...), b...) {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
