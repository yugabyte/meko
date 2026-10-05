package ampbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"meko/ycp/internal/ampclient"
	"meko/ycp/internal/api"
	"meko/ycp/reconcile"
)

// KindExtension is an extension registered in the controller's catalog
// (registerExtension): an image made AVAILABLE to clusters, not mounted anywhere.
// The name is the catalog name, so it is a root with no parent; the spec is the
// registerExtension body. Builtins are not resources — they ship with the
// controller release and can be neither registered nor removed.
//
// Observable rather than echoed: the controller's row carries every spec field
// and no generation, and its verbs are synchronous, so Update reads the row back
// and acts only when it differs.
const KindExtension = "extension"

// ycpAdminUserID is the controller user the agent registers extensions as.
// registerExtension and deregisterExtension are admin-only and name their caller
// in ?user_id=; the id is fixed so a restart reuses the same user rather than
// minting another. Materialized on first use, like a pool's owner.
var ycpAdminUserID = openapi_types.UUID{
	0x7c, 0xb0, 0x0a, 0xd0, 0x00, 0x00, 0x40, 0x00, 0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0x0e, 0x71,
}

// extensionList is one read of GET /extensions.
func (b *Backend) extensionList(ctx context.Context) ([]ampclient.Extension, error) {
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
	return list.Extensions, nil
}

// ExtensionCatalog reads the controller's extension catalog — builtins plus every
// image an operator registered — which is what createCluster and
// addBranchExtensions validate against. Any failure is an error, never an empty
// catalog: an empty one would tell the console this environment has no extensions.
func (b *Backend) ExtensionCatalog(ctx context.Context) ([]api.Extension, error) {
	list, err := b.extensionList(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]api.Extension, 0, len(list))
	for _, e := range list {
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

// extensionCatalog is what List read of the catalog, kept for the apply verbs.
// read is false when the read failed, and then nothing else here is known.
type extensionCatalog struct {
	read       bool
	builtin    map[string]bool
	registered map[string]ampclient.Extension
}

func (c extensionCatalog) has(name string) bool {
	_, ok := c.registered[name]
	return ok || c.builtin[name]
}

// listExtensions reads the catalog into one Actual per REGISTERED extension.
//
// A failed read is not a short list: the extensions the last good read saw come
// back as DiscoveryError rows, so the engine neither re-registers them nor
// deregisters them for having gone missing.
func (b *Backend) listExtensions(ctx context.Context) ([]reconcile.Actual, extensionCatalog) {
	list, err := b.extensionList(ctx)
	if err != nil {
		b.mu.RLock()
		defer b.mu.RUnlock()
		out := make([]reconcile.Actual, 0, len(b.lastSeenExt))
		for _, name := range b.lastSeenExt {
			out = append(out, reconcile.Actual{
				Key:            reconcile.ResourceKey{Kind: KindExtension, Name: name},
				DiscoveryError: err,
			})
		}
		return out, extensionCatalog{}
	}
	cat := extensionCatalog{read: true, builtin: map[string]bool{}, registered: map[string]ampclient.Extension{}}
	var out []reconcile.Actual
	var seen []string
	for _, e := range list {
		if e.Builtin {
			cat.builtin[e.Name] = true
			continue
		}
		cat.registered[e.Name] = e
		seen = append(seen, e.Name)
		attrs := attrPut(attrPut(nil, "image", e.Image), "preload", fmt.Sprint(e.Preload))
		if e.Version != nil {
			attrs = attrPut(attrs, "version", *e.Version)
		}
		out = append(out, reconcile.Actual{
			Key:         reconcile.ResourceKey{Kind: KindExtension, Name: e.Name},
			ObservedGen: reconcile.GenerationNever,
			Ready:       true,
			State:       "ready",
			Attrs:       attrs,
		})
	}
	b.mu.Lock()
	b.lastSeenExt = seen
	b.mu.Unlock()
	return out, cat
}

func extensionSpecOf(r api.DesiredResource) (ampclient.RegisterExtensionRequest, error) {
	var spec ampclient.RegisterExtensionRequest
	if err := unmarshalSpec(r, &spec); err != nil {
		return spec, err
	}
	if strings.Contains(r.Name, sep) {
		return spec, reconcile.Rejected(fmt.Errorf("extension %q: a catalog name cannot contain %q", r.Name, sep))
	}
	if spec.Image == "" {
		return spec, reconcile.Rejected(fmt.Errorf("extension %q: spec.image is required", r.Name))
	}
	// The controller trims both and stores the trimmed value, so an untrimmed one
	// never matches what List reads back: it would be registered on every tick, and
	// an untrimmed name would also have its registration collected as undeclared.
	if strings.TrimSpace(r.Name) != r.Name || strings.TrimSpace(spec.Image) != spec.Image {
		return spec, reconcile.Rejected(fmt.Errorf("extension %q: name and image must not carry surrounding whitespace", r.Name))
	}
	return spec, nil
}

// registerExtension converges one declared extension: nothing when the catalog
// already holds exactly this registration, otherwise a full-replace PUT. It serves
// both Create and Update, since the controller's verb is an upsert.
func (b *Backend) registerExtension(ctx context.Context, r api.DesiredResource) error {
	spec, err := extensionSpecOf(r)
	if err != nil {
		return err
	}
	b.mu.RLock()
	cat := b.idx.ext
	b.mu.RUnlock()
	if cat.builtin[r.Name] {
		return reconcile.Rejected(fmt.Errorf(
			"extension %q is built into this controller; enable it on a cluster or branch instead of registering it", r.Name))
	}
	if have, ok := cat.registered[r.Name]; ok && sameRegistration(spec, have) {
		return nil
	}
	if err := b.ensureYCPAdmin(ctx); err != nil {
		return err
	}
	resp, err := b.api.RegisterExtension(ctx, r.Name,
		&ampclient.RegisterExtensionParams{UserId: ycpAdminUserID}, spec)
	if err != nil {
		return fmt.Errorf("register extension %q: %w", r.Name, err)
	}
	return extensionVerdict(resp, fmt.Sprintf("register extension %q", r.Name), http.StatusOK)
}

// deregisterExtension removes one extension from the catalog. The controller
// refuses (409) while any branch enables it. That is retried on backoff rather
// than reported rejected: the refusal clears without a spec change — the branches
// go, often in the same write as the extension — and a permanent mark on a delete
// never retires, since a delete is planned at generation 0 on every tick.
func (b *Backend) deregisterExtension(ctx context.Context, name string) error {
	b.mu.RLock()
	_, registered := b.idx.ext.registered[name]
	b.mu.RUnlock()
	if !registered {
		return nil // already gone
	}
	if err := b.ensureYCPAdmin(ctx); err != nil {
		return err
	}
	resp, err := b.api.DeregisterExtension(ctx, name, &ampclient.DeregisterExtensionParams{UserId: ycpAdminUserID})
	if err != nil {
		return fmt.Errorf("deregister extension %q: %w", name, err)
	}
	what := fmt.Sprintf("deregister extension %q", name)
	if resp.StatusCode == http.StatusConflict {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: still enabled on a branch: %w", what, httpErr(resp.StatusCode, body))
	}
	return extensionVerdict(resp, what, http.StatusNoContent, http.StatusNotFound)
}

// extensionVerdict classifies a register/deregister response. Not mutate: that
// reads 409 as "already in flight", right for an async create and wrong here — a
// 409 on register is a refusal (a builtin or colliding name), and reading it as
// success would hide it.
func extensionVerdict(resp *http.Response, what string, ok ...int) error {
	defer resp.Body.Close()
	if slices.Contains(ok, resp.StatusCode) {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	err := fmt.Errorf("%s: %w", what, httpErr(resp.StatusCode, body))
	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusConflict:
		return reconcile.Rejected(err)
	case http.StatusForbidden:
		return reconcile.Blocked(err)
	}
	return err
}

// ensureYCPAdmin creates the agent's admin user once per process. Idempotent on
// the explicit id, so repeating it after a restart is a no-op on the controller.
func (b *Backend) ensureYCPAdmin(ctx context.Context) error {
	b.mu.RLock()
	done := b.adminReady
	b.mu.RUnlock()
	if done {
		return nil
	}
	admin, id := true, ycpAdminUserID
	if err := b.mutate(ctx, func(c context.Context) (*http.Response, error) {
		return b.api.CreateUser(c, ampclient.CreateUserRequest{Id: &id, IsAdmin: &admin})
	}); err != nil {
		return fmt.Errorf("ensure ycp admin user %s: %w", ycpAdminUserID, err)
	}
	b.mu.Lock()
	b.adminReady = true
	b.mu.Unlock()
	return nil
}

// sameRegistration reports whether the catalog row already is this declaration.
// Both sides are normalised the same way because the controller omits every empty
// field on read, so an absent and an empty value are one registration.
func sameRegistration(spec ampclient.RegisterExtensionRequest, have ampclient.Extension) bool {
	return sameJSON(normalRegistration(spec), normalRegistration(ampclient.RegisterExtensionRequest{
		AbiExtraExempt: have.AbiExtraExempt,
		Description:    have.Description,
		Image:          have.Image,
		Layout:         have.Layout,
		Library:        have.Library,
		Preload:        &have.Preload,
		Version:        have.Version,
	}))
}

func normalRegistration(r ampclient.RegisterExtensionRequest) ampclient.RegisterExtensionRequest {
	str := func(p *string) *string {
		if p == nil || *p == "" {
			return nil
		}
		return p
	}
	strs := func(p *[]string) *[]string {
		if p == nil || len(*p) == 0 {
			return nil
		}
		return p
	}
	out := ampclient.RegisterExtensionRequest{
		AbiExtraExempt: strs(r.AbiExtraExempt),
		Description:    str(r.Description),
		Image:          r.Image,
		Library:        str(r.Library),
		Version:        str(r.Version),
	}
	if r.Preload != nil && *r.Preload {
		out.Preload = r.Preload
	}
	if l := r.Layout; l != nil {
		n := ampclient.ExtensionLayout{SharePath: str(l.SharePath), LibPath: str(l.LibPath), LibraryPath: strs(l.LibraryPath)}
		if l.EnvPaths != nil && len(*l.EnvPaths) > 0 {
			n.EnvPaths = l.EnvPaths
		}
		if n != (ampclient.ExtensionLayout{}) {
			out.Layout = &n
		}
	}
	return out
}

// notInCatalog refuses, retryably, to send a cluster or branch the controller
// would 400 for naming an extension it does not have. The usual cause is an
// extension declared in the same write and not registered yet: creates are
// dispatched concurrently, so nothing orders the registration first, and the
// controller's 400 would latch the cluster as rejected until its spec changed.
// Waiting a tick costs nothing when the registration is coming; when the name is
// a typo the resource keeps failing with this message, which names it.
func notInCatalog(cat extensionCatalog, names []string) error {
	if !cat.read {
		return nil // unknown; let the controller judge
	}
	var missing []string
	for _, n := range names {
		if !cat.has(n) {
			missing = append(missing, n)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("extensions %s are not in this controller's catalog yet; declare them as %s resources or fix the names",
		strings.Join(missing, ","), KindExtension)
}
