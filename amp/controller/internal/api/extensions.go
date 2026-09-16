package api

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/extensions"
)

// The extension catalog API: what a branch may opt into, and (for an admin) the
// registration of images this controller does not build.
//
// Registration is deliberately synchronous and deploys nothing. It makes an
// image *available*; a branch opting in (addBranchExtensions) is what mounts it,
// and that already redeploys the branch's endpoints. Keeping the two apart means
// registering an extension can never disturb a running endpoint.

// requireAdmin resolves the acting user and checks the admin bit. There is no
// authentication yet, so the caller names itself with ?user_id= exactly as
// createStoragePool takes an owner_user_id; when real auth lands this is the one
// place that changes.
//
// It returns an *api* error response when the caller is not an admin, and a
// plain error only for a genuine lookup failure.
func (c *Controllers) requireAdmin(ctx context.Context, userID uuid.UUID) (Error, bool, error) {
	u, err := c.backend.GetUser(ctx, userID)
	if errors.Is(err, core.ErrNotFound) {
		return apiError("forbidden", "user_id does not name a known user"), false, nil
	}
	if err != nil {
		return Error{}, false, err
	}
	if !u.IsAdmin {
		return apiError("forbidden", "registering extensions requires an admin user"), false, nil
	}
	return Error{}, true, nil
}

// extensionToAPI renders a catalog entry.
func extensionToAPI(e extensions.Entry) Extension {
	out := Extension{
		Name:    e.Name,
		Image:   e.Image,
		Builtin: e.Builtin,
		Preload: e.Preload,
	}
	if e.Version != "" {
		out.Version = &e.Version
	}
	if e.Description != "" {
		out.Description = &e.Description
	}
	if e.Library != "" {
		out.Library = &e.Library
	}
	if len(e.ABIExtraExempt) > 0 {
		v := e.ABIExtraExempt
		out.AbiExtraExempt = &v
	}
	if l := layoutToAPI(e.Layout); l != nil {
		out.Layout = l
	}
	return out
}

// layoutToAPI renders a layout, or nil when the entry declares none (which is
// every built-in — the zero layout means "the tree deploy/extensions builds").
func layoutToAPI(l extensions.Layout) *ExtensionLayout {
	if l.SharePath == "" && l.LibPath == "" && len(l.LibraryPath) == 0 && len(l.EnvPaths) == 0 {
		return nil
	}
	out := &ExtensionLayout{}
	if l.SharePath != "" {
		out.SharePath = &l.SharePath
	}
	if l.LibPath != "" {
		out.LibPath = &l.LibPath
	}
	if len(l.LibraryPath) > 0 {
		v := l.LibraryPath
		out.LibraryPath = &v
	}
	if len(l.EnvPaths) > 0 {
		v := l.EnvPaths
		out.EnvPaths = &v
	}
	return out
}

// layoutFromAPI reads a layout off a request body.
func layoutFromAPI(l *ExtensionLayout) extensions.Layout {
	var out extensions.Layout
	if l == nil {
		return out
	}
	if l.SharePath != nil {
		out.SharePath = *l.SharePath
	}
	if l.LibPath != nil {
		out.LibPath = *l.LibPath
	}
	if l.LibraryPath != nil {
		out.LibraryPath = *l.LibraryPath
	}
	if l.EnvPaths != nil {
		out.EnvPaths = *l.EnvPaths
	}
	return out
}

// ListExtensions returns every extension a branch may enable.
func (c *Controllers) ListExtensions(ctx context.Context, _ ListExtensionsRequestObject) (ListExtensionsResponseObject, error) {
	if c.prov.Extensions == nil {
		return ListExtensions503JSONResponse{ServiceUnavailableJSONResponse{
			Body: apiError("service_unavailable", "externalized extensions are not supported on this controller"),
		}}, nil
	}
	entries, err := c.prov.Extensions.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Extension, 0, len(entries))
	for _, e := range entries {
		out = append(out, extensionToAPI(e))
	}
	n := len(out)
	return ListExtensions200JSONResponse{Extensions: out, Total: n, Count: n}, nil
}

// GetExtension returns one entry, built-in or registered.
func (c *Controllers) GetExtension(ctx context.Context, request GetExtensionRequestObject) (GetExtensionResponseObject, error) {
	if c.prov.Extensions == nil {
		return GetExtension503JSONResponse{ServiceUnavailableJSONResponse{
			Body: apiError("service_unavailable", "externalized extensions are not supported on this controller"),
		}}, nil
	}
	e, err := c.prov.Extensions.Get(ctx, request.ExtensionName)
	if errors.Is(err, extensions.ErrNotFound) {
		return GetExtension404JSONResponse{NotFoundJSONResponse(
			apiError("not_found", "no such extension"))}, nil
	}
	if err != nil {
		return nil, err
	}
	return GetExtension200JSONResponse(extensionToAPI(e)), nil
}

// RegisterExtension creates or fully replaces a registration.
func (c *Controllers) RegisterExtension(ctx context.Context, request RegisterExtensionRequestObject) (RegisterExtensionResponseObject, error) {
	if c.prov.Extensions == nil {
		return RegisterExtension503JSONResponse{ServiceUnavailableJSONResponse{
			Body: apiError("service_unavailable", "externalized extensions are not supported on this controller"),
		}}, nil
	}
	if errBody, ok, err := c.requireAdmin(ctx, request.Params.UserId); err != nil {
		return nil, err
	} else if !ok {
		return RegisterExtension403JSONResponse{ForbiddenJSONResponse(errBody)}, nil
	}

	e := extensions.Extension{
		Name:           request.ExtensionName,
		ImageRef:       request.Body.Image,
		Layout:         layoutFromAPI(request.Body.Layout),
		ABIExtraExempt: derefSlice(request.Body.AbiExtraExempt),
		Version:        derefString(request.Body.Version),
		Description:    derefString(request.Body.Description),
		Library:        derefString(request.Body.Library),
		Preload:        request.Body.Preload != nil && *request.Body.Preload,
	}
	stored, err := c.prov.Extensions.Register(ctx, e)
	switch {
	case errors.Is(err, extensions.ErrBuiltinName), errors.Is(err, extensions.ErrNameCollision):
		return RegisterExtension409JSONResponse{ConflictJSONResponse(
			apiError("conflict", err.Error()))}, nil
	case errors.Is(err, extensions.ErrInvalid):
		// Only a rejection of the body itself is a 400. A store failure or a
		// missing store falls through to 500: reporting an outage as an invalid
		// request sends the operator to fix a body that was fine.
		return RegisterExtension400JSONResponse{BadRequestJSONResponse(
			apiError("invalid_request", err.Error()))}, nil
	case err != nil:
		return nil, err
	}
	// Register returns what it stored, under the normalised name — not the one
	// this request was handed, which may differ.
	return RegisterExtension200JSONResponse(extensionToAPI(stored)), nil
}

// DeregisterExtension removes a registration.
func (c *Controllers) DeregisterExtension(ctx context.Context, request DeregisterExtensionRequestObject) (DeregisterExtensionResponseObject, error) {
	if c.prov.Extensions == nil {
		return DeregisterExtension503JSONResponse{ServiceUnavailableJSONResponse{
			Body: apiError("service_unavailable", "externalized extensions are not supported on this controller"),
		}}, nil
	}
	if errBody, ok, err := c.requireAdmin(ctx, request.Params.UserId); err != nil {
		return nil, err
	} else if !ok {
		return DeregisterExtension403JSONResponse{ForbiddenJSONResponse(errBody)}, nil
	}
	err := c.prov.Extensions.Deregister(ctx, request.ExtensionName)
	switch {
	case errors.Is(err, extensions.ErrNotFound):
		return DeregisterExtension404JSONResponse{NotFoundJSONResponse(
			apiError("not_found", "no such registered extension"))}, nil
	case errors.Is(err, extensions.ErrBuiltinName), errors.Is(err, extensions.ErrInUse):
		return DeregisterExtension409JSONResponse{ConflictJSONResponse(
			apiError("conflict", err.Error()))}, nil
	case err != nil:
		return nil, err
	}
	return DeregisterExtension204Response{}, nil
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefSlice(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}
