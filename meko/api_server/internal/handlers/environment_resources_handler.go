package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/models"
	"meko/api_server/internal/service"
)

// maxPatchesPerCall bounds one region PATCH so a single call cannot hold an unbounded
// transaction open. It counts every patch across every region in the batch; a batch
// over this is a 400. The number is a policy, not a schema limit.
const maxPatchesPerCall = 100

// intents counts how many of the mutually-exclusive create/update/delete flags are set.
func intents(flags ...bool) int {
	n := 0
	for _, f := range flags {
		if f {
			n++
		}
	}
	return n
}

// ListResourcesByName returns the resources of a storage pool across every MANAGED
// environment it is mapped to, as `desired` (declared) and `observed` (agent-reported)
// lists per environment. No environment id is named: the storage pool is the first
// path segment of `name`, and the sticky (storage_pool, region) mappings say which
// environments answer.
//
// Filters: `name` (required — a resource's full hierarchical path, whose first
// segment is the storage pool), `kind`, and `subtree=true` (the named resource and
// its descendants). A storage pool nothing is mapped to returns an empty list, not a
// 404 — a LIST never assigns.
//
// No accountScope: this route is admin-secret-only, not on the account-scoped fleet
// group. The environments it reads are shared MANAGED amp infrastructure with no
// per-customer account_id; the storage pool in `name` is the tenant key. See the
// route wiring in router.go.
func (h *EnvironmentHandler) ListResourcesByName(c echo.Context) error {
	name := c.QueryParam("name")
	if name == "" {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", "name is required")
	}
	kind := c.QueryParam("kind")
	// A malformed subtree value is treated as false rather than a 400: it is a
	// read-only widening flag, and the default (exact match) is the safe reading.
	subtree, _ := strconv.ParseBool(c.QueryParam("subtree"))

	envs, err := h.svc.ListResourcesByName(name, kind, subtree)
	if err != nil {
		if errors.Is(err, service.ErrValidation) {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	return ok(c, http.StatusOK, map[string]any{"environments": envs})
}

// SetResourcesByRegion applies desired-state changes to a storage pool, addressed by
// region rather than by environment id. The body names the storage pool once and a
// batch of changes per region:
//
//	{ "storage_pool": "<pool>", "regions": [ { "region": "...", "resources": [ ... ] } ] }
//
// Each region resolves to one MANAGED environment via the sticky mapping (the first
// call for a (storage_pool, region) pins a READY environment, later calls reuse it).
// Every resource across every region is applied in ONE transaction: any invalid item
// fails the whole call with 400 and writes nothing. The response mirrors the LIST:
// the pool's desired and observed resources per environment touched.
//
// Strict body (unknown fields refused, same discipline as SetDesired) because a
// dropped key changes what a patch means. `spec` stays opaque — json.RawMessage — so
// a field only a newer agent knows still rides through.
//
// No accountScope, for the same reason as ListResourcesByName: admin-secret-only,
// addressing shared MANAGED amp environments keyed by storage pool, not by account.
func (h *EnvironmentHandler) SetResourcesByRegion(c echo.Context) error {
	var body struct {
		StoragePool string                   `json:"storage_pool"`
		Regions     []models.RegionResources `json:"regions"`
	}
	if !bindJSONStrict(c, &body) {
		return nil
	}
	if body.StoragePool == "" {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", "storage_pool is required")
	}
	if len(body.Regions) == 0 {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR",
			"regions is required and must name at least one region")
	}
	total := 0
	for ri, rr := range body.Regions {
		if rr.Region == "" {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR",
				"regions["+strconv.Itoa(ri)+"] requires a region")
		}
		if len(rr.Resources) == 0 {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR",
				"regions["+strconv.Itoa(ri)+"] must name at least one resource")
		}
		total += len(rr.Resources)
		for pi, p := range rr.Resources {
			if p.Kind == "" || p.Name == "" {
				return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR",
					"regions["+strconv.Itoa(ri)+"].resources["+strconv.Itoa(pi)+"] requires both kind and name")
			}
			// A delete that also names a spec is ambiguous — refuse it rather than
			// guess whether the caller meant to remove or to write.
			if p.Delete && len(p.Spec) > 0 && string(p.Spec) != "null" {
				return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR",
					"regions["+strconv.Itoa(ri)+"].resources["+strconv.Itoa(pi)+
						"] has delete:true and a spec; a delete must not carry a spec")
			}
			// create/update/delete are mutually exclusive intents; more than one is
			// ambiguous.
			if intents(p.Create, p.Update, p.Delete) > 1 {
				return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR",
					"regions["+strconv.Itoa(ri)+"].resources["+strconv.Itoa(pi)+
						"] sets more than one of create/update/delete; they are mutually exclusive")
			}
		}
	}
	if total > maxPatchesPerCall {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR",
			"a batch may carry at most "+strconv.Itoa(maxPatchesPerCall)+" patches across all regions")
	}

	envs, err := h.svc.SetResourcesByStoragePool(body.StoragePool, body.Regions)
	if err != nil {
		if errors.Is(err, service.ErrValidation) {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}
		// A region with no READY MANAGED environment cannot be served — a 404, the
		// same as naming an environment that does not exist.
		if errors.Is(err, service.ErrNoManagedEnv) {
			return apiError(c, http.StatusNotFound, "NOT_FOUND", err.Error())
		}
		// A create landing on an existing resource, or a cluster name already used
		// elsewhere in the pool, is a 409; an update naming an absent one is a 404.
		if errors.Is(err, service.ErrClusterNameInUse) {
			return apiError(c, http.StatusConflict, "CLUSTER_NAME_IN_USE", err.Error())
		}
		if errors.Is(err, service.ErrResourceExists) {
			return apiError(c, http.StatusConflict, "RESOURCE_EXISTS", err.Error())
		}
		if errors.Is(err, service.ErrResourceNotFound) {
			return apiError(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", err.Error())
		}
		// A lost concurrent-write race rolled the whole batch back, so the identical
		// request is safe to retry — a 409, not a 500.
		if errors.Is(err, service.ErrConcurrentPoolUpdate) {
			return apiError(c, http.StatusConflict, "CONCURRENT_UPDATE", err.Error())
		}
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	return ok(c, http.StatusOK, map[string]any{"environments": envs})
}
