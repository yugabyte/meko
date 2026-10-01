package ampbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"meko/ycp/internal/ampclient"
	"meko/ycp/internal/api"
)

// ExtensionCatalog reads the controller's extension catalog — builtins plus every
// image an operator registered — which is what createCluster and
// addBranchExtensions validate against. Any failure is an error, never an empty
// catalog: an empty one would tell the console this environment has no extensions.
func (b *Backend) ExtensionCatalog(ctx context.Context) ([]api.Extension, error) {
	resp, err := b.api.ListExtensions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list amp extensions: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read amp extension list: %w", err)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("list amp extensions: %w", httpErr(resp.StatusCode, body))
	}
	var list ampclient.ExtensionList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode amp extension list: %w", err)
	}
	out := make([]api.Extension, 0, len(list.Extensions))
	for _, e := range list.Extensions {
		x := api.Extension{Name: e.Name, Builtin: e.Builtin, Preload: e.Preload}
		if e.Version != nil {
			x.Version = *e.Version
		}
		if e.Description != nil {
			x.Description = *e.Description
		}
		out = append(out, x)
	}
	return out, nil
}
