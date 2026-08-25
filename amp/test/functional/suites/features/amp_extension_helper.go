package features

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
)

// unknownExtensionRE pulls the name out of the controller's `unknown extension
// "x"` 400 (controller/internal/api/handlers.go).
var unknownExtensionRE = regexp.MustCompile(`unknown extension "([^"]+)"`)

// unknownExtensionName returns the extension a create was rejected for as
// unknown, or "" for any other error. Only a 400 counts: an unknown name is a
// request the controller understood and refused, never a transport failure.
func unknownExtensionName(err error) string {
	var apiErr *controller.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		return ""
	}
	if m := unknownExtensionRE.FindStringSubmatch(apiErr.Message); m != nil {
		return m[1]
	}
	return ""
}

// optInExtensionEndpoint is the shared prologue of every per-extension DEPTH
// test: opt the branch into one catalog extension, create an endpoint on it,
// probe that the ImageVolume actually mounted, and CREATE EXTENSION. It skips
// (ok = false, nil error) on the three conditions that are the environment's
// rather than the product's — no extension catalog at all, a catalog that does
// not know this extension yet (the image under test predates it), and a cluster
// whose kubelet cannot mount ImageVolumes. pg_extensions_matrix is where those
// same conditions HARD-FAIL under functional.extensions.require_catalog; a
// depth test skipping visibly does not weaken that contract.
func optInExtensionEndpoint(ctx context.Context, cl ampCtl, proj ampCluster, connect endpointConnector,
	skip func(string), catalogName, sqlName string) (sqlConn, bool, error) {
	if _, err := cl.AddBranchExtensionsAndWait(ctx, proj.poolID, proj.projID, proj.branchID, controller.AddBranchExtensionsRequest{
		Extensions: []string{catalogName},
	}); err != nil {
		if extensionsUnavailable(err) {
			skip("externalized extensions are not available on this control plane (no extension catalog): " + err.Error())
			return nil, false, nil
		}
		if unknownExtensionName(err) == catalogName {
			skip("this controller's catalog does not know " + catalogName + " — the image under test predates it; pg_extensions_matrix owns the hard contract once it ships")
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("opt branch into %s: %w", catalogName, err)
	}
	ep, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return nil, false, fmt.Errorf("create endpoint after opting the cluster into %s: %w", catalogName, err)
	}
	c, err := connect(ctx, ep)
	if err != nil {
		return nil, false, fmt.Errorf("connect endpoint: %w", err)
	}

	// Same ground truth the pgvector test probes: an incapable kubelet brings
	// the pod up READY with no mount and no API error.
	available, err := c.QueryInt(ctx, "SELECT count(*) FROM pg_available_extensions WHERE name = $1", sqlName)
	if err != nil {
		c.Close()
		return nil, false, fmt.Errorf("probe pg_available_extensions: %w", err)
	}
	if available == 0 {
		c.Close()
		skip("endpoint came up without the " + catalogName + " ImageVolume mounted (pg_available_extensions has no '" + sqlName + "'): the cluster likely lacks ImageVolume support")
		return nil, false, nil
	}
	if _, err := c.Exec(ctx, "CREATE EXTENSION "+quoteExtIdent(sqlName)); err != nil {
		c.Close()
		return nil, false, fmt.Errorf("CREATE EXTENSION %s on an endpoint that opted into %s: %w", sqlName, catalogName, err)
	}
	return c, true, nil
}

// assertPreloaded proves a preload extension's library actually reached
// shared_preload_libraries — the failure "created but never loaded" that
// CREATE EXTENSION alone cannot catch for an extension whose _PG_init registers
// workers or shared memory.
func assertPreloaded(ctx context.Context, c sqlConn, library string) error {
	libs, err := c.QueryString(ctx, "SHOW shared_preload_libraries")
	if err != nil {
		return fmt.Errorf("SHOW shared_preload_libraries: %w", err)
	}
	if !containsToken(libs, library) {
		return fmt.Errorf("shared_preload_libraries is %q, missing %q — the endpoint chart must append a preload extension's library when the cluster opts in, or its background workers and shared memory never exist", libs, library)
	}
	return nil
}

// containsToken reports whether the comma-separated GUC value carries exactly
// this library name (substring matching would let "pg_cron_extra" pass for
// "pg_cron").
func containsToken(csv, want string) bool {
	start := 0
	for i := 0; i <= len(csv); i++ {
		if i == len(csv) || csv[i] == ',' {
			token := csv[start:i]
			for len(token) > 0 && (token[0] == ' ' || token[0] == '"') {
				token = token[1:]
			}
			for len(token) > 0 && (token[len(token)-1] == ' ' || token[len(token)-1] == '"') {
				token = token[:len(token)-1]
			}
			if token == want {
				return true
			}
			start = i + 1
		}
	}
	return false
}
