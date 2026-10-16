package extensions

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Store is the durable half of the catalog: extensions an operator registered
// through the API. The built-in table is compiled in; everything else lives
// here, so a third-party image reaches amp only because someone put it there.
type Store interface {
	ListExtensions(ctx context.Context) ([]Extension, error)
	GetExtension(ctx context.Context, name string) (Extension, bool, error)
	PutExtension(ctx context.Context, e Extension) error
	DeleteExtension(ctx context.Context, name string) error
	// ExtensionUsers returns the ids of branches whose opted-in set names this
	// extension. Deregistering one still in use would leave those branches
	// pointing at nothing, so the caller refuses instead.
	ExtensionUsers(ctx context.Context, name string) ([]string, error)
}

// ErrBuiltinName is returned when a registration would shadow a built-in entry.
// Shadowing is refused rather than resolved by precedence: an operator who
// registers "pgvector" means a different pgvector, and silently winning (or
// silently losing) against the built-in is the wrong answer either way.
var ErrBuiltinName = errors.New("name is a built-in extension")

// ErrInUse is returned when deregistering an extension some branch still mounts.
var ErrInUse = errors.New("extension is in use")

// ErrNotFound is returned for an unknown registered extension.
var ErrNotFound = errors.New("extension not found")

// ErrNameCollision is returned when a name is distinct from an existing one but
// sanitizes to the same Kubernetes object name, which would render two
// ImageVolumes with one name and make the pod spec invalid.
var ErrNameCollision = errors.New("name collides with an existing extension")

// ErrInvalid wraps every rejection of the registration itself, so a caller can
// answer 400 for those and keep 400 meaning "fix your body". A store failure or
// a missing store is NOT wrapped: reporting an outage as an invalid request
// sends the operator to edit a body that was fine.
var ErrInvalid = errors.New("invalid extension registration")

// Service resolves an extension name against the built-in table UNION the
// registered ones, and owns the register/deregister side.
//
// The built-in [Catalog] stays pure and I/O-free; this is the only thing that
// touches the database, so a caller that only needs the compiled-in set (engine
// bundle validation, for one) does not grow a context or an error return.
type Service struct {
	builtin  *Catalog
	store    Store
	registry string
}

// NewService pairs the built-in catalog with the registered-extension store. A
// nil store yields a service that serves only the built-ins and refuses every
// registration — what a deployment without the feature wants.
func NewService(builtin *Catalog, store Store) *Service {
	return &Service{builtin: builtin, store: store, registry: builtin.registry}
}

// Builtin exposes the compiled-in catalog for the callers that genuinely mean
// only it (engine bundles, which may only reference built-ins).
func (s *Service) Builtin() *Catalog { return s.builtin }

// DefaultNames is the always-mounted set. Registered extensions are never
// defaults: an operator registering an image is making it *available*, not
// mounting it on every endpoint in the deployment.
func (s *Service) DefaultNames() []string { return s.builtin.DefaultNames() }

// Has reports whether name resolves — the API's validation gate, so an unknown
// opt-in is a 400 rather than a mount that silently never appears.
func (s *Service) Has(ctx context.Context, name string) (bool, error) {
	if s.builtin.Has(name) {
		return true, nil
	}
	if s.store == nil {
		return false, nil
	}
	_, ok, err := s.store.GetExtension(ctx, name)
	return ok, err
}

// Resolve maps names to image references, built-ins first then registered ones,
// de-duplicated and preserving the order names are given. Unknown names are
// dropped rather than erroring, matching the built-in catalog: a branch that
// opted into an extension since deregistered must still deploy.
func (s *Service) Resolve(ctx context.Context, names []string) ([]Resolved, error) {
	out := s.builtin.Resolve(names)
	if s.store == nil {
		return out, nil
	}
	seen := make(map[string]struct{}, len(out))
	for _, r := range out {
		seen[r.Name] = struct{}{}
	}
	// One read for the whole set rather than a lookup per name: the set is small
	// and this runs on the endpoint-create path.
	registered, err := s.store.ListExtensions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list registered extensions: %w", err)
	}
	byName := make(map[string]Extension, len(registered))
	for _, e := range registered {
		byName[e.Name] = e
	}
	for _, n := range names {
		if _, dup := seen[n]; dup {
			continue
		}
		e, ok := byName[n]
		if !ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, resolve(e, s.registry))
	}
	return out, nil
}

// Entry is a catalog row as the API reports it: the extension plus where it came
// from, so a caller can tell what it may deregister.
type Entry struct {
	Extension
	// Builtin marks an entry compiled into this controller. A builtin entry
	// cannot be deregistered or overwritten.
	Builtin bool
	// Image is the resolved reference, so a caller sees what would actually be
	// mounted rather than having to re-derive it from the registry flag.
	Image string
}

// List returns every extension a branch may opt into, built-ins first, each
// group name-sorted.
func (s *Service) List(ctx context.Context) ([]Entry, error) {
	// Name-sorted, as both spec copies promise. builtin.order is INSERTION
	// order, which DefaultNames wants and this does not.
	names := append([]string(nil), s.builtin.order...)
	sort.Strings(names)
	var out []Entry
	for _, n := range names {
		e := s.builtin.byName[n]
		out = append(out, Entry{Extension: e, Builtin: true, Image: image(e, s.registry)})
	}
	if s.store == nil {
		return out, nil
	}
	registered, err := s.store.ListExtensions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list registered extensions: %w", err)
	}
	sort.Slice(registered, func(i, j int) bool { return registered[i].Name < registered[j].Name })
	for _, e := range registered {
		out = append(out, Entry{Extension: e, Image: image(e, s.registry)})
	}
	return out, nil
}

// Get returns one entry.
func (s *Service) Get(ctx context.Context, name string) (Entry, error) {
	if e, ok := s.builtin.byName[name]; ok {
		return Entry{Extension: e, Builtin: true, Image: image(e, s.registry)}, nil
	}
	if s.store == nil {
		return Entry{}, ErrNotFound
	}
	e, ok, err := s.store.GetExtension(ctx, name)
	if err != nil {
		return Entry{}, err
	}
	if !ok {
		return Entry{}, ErrNotFound
	}
	return Entry{Extension: e, Image: image(e, s.registry)}, nil
}

// Register validates and stores an extension, creating it or replacing it in
// full (the name is the identity), and returns the stored entry. Replacing one
// changes what endpoints mount at their NEXT deploy; endpoints already running
// keep the image they started with until they are recreated.
//
// Validation runs FIRST because it normalises: it is the trimmed name that must
// be tested against the built-in table and written to the store. Checking the
// raw name would let " pgvector" past the shadow check and then store it as
// "pgvector" — a row the built-in table hides from every read and that
// Deregister refuses to delete. Returning the entry is what lets the caller
// answer with the stored name rather than the one it was handed.
func (s *Service) Register(ctx context.Context, e Extension) (Entry, error) {
	if s.store == nil {
		return Entry{}, errors.New("extension registration is unavailable: no extension store is configured")
	}
	if err := Validate(&e); err != nil {
		return Entry{}, err
	}
	if s.builtin.Has(e.Name) {
		return Entry{}, fmt.Errorf("%q: %w", e.Name, ErrBuiltinName)
	}
	// An exact-name check is not enough: the chart sanitizes the name to an
	// RFC-1123 label for the volume, and "_" and "-" collapse to the same one
	// (pg_cron and pg-cron both render ext-pg-cron). Registering the dashed
	// spelling of a refused built-in is the obvious next thing an operator
	// tries, and a branch opting into both would then render two volumes with
	// one name — which the API server rejects, failing every deploy on that
	// branch long after this call returned 200.
	mine := volumeName(e.Name)
	for _, n := range s.builtin.order {
		if volumeName(n) == mine {
			return Entry{}, fmt.Errorf("%q collides with built-in %q (both mount as %q): %w",
				e.Name, n, mine, ErrBuiltinName)
		}
	}
	existing, err := s.store.ListExtensions(ctx)
	if err != nil {
		return Entry{}, fmt.Errorf("list registered extensions: %w", err)
	}
	for _, o := range existing {
		if o.Name != e.Name && volumeName(o.Name) == mine {
			return Entry{}, fmt.Errorf("%q collides with registered %q (both mount as %q): %w",
				e.Name, o.Name, mine, ErrNameCollision)
		}
	}
	if err := s.store.PutExtension(ctx, e); err != nil {
		return Entry{}, err
	}
	return Entry{Extension: e, Image: image(e, s.registry)}, nil
}

// Deregister removes a registered extension. A built-in is refused, and so is
// one any branch still opts into — that branch's endpoints would otherwise lose
// the mount on their next deploy, taking any objects built on it with them.
func (s *Service) Deregister(ctx context.Context, name string) error {
	if s.builtin.Has(name) {
		return fmt.Errorf("%q: %w", name, ErrBuiltinName)
	}
	if s.store == nil {
		return ErrNotFound
	}
	if _, ok, err := s.store.GetExtension(ctx, name); err != nil {
		return err
	} else if !ok {
		return ErrNotFound
	}
	users, err := s.store.ExtensionUsers(ctx, name)
	if err != nil {
		return fmt.Errorf("check extension use: %w", err)
	}
	if len(users) > 0 {
		return fmt.Errorf("%q is opted into by %d branch(es) (%s): %w",
			name, len(users), strings.Join(truncate(users, 5), ", "), ErrInUse)
	}
	return s.store.DeleteExtension(ctx, name)
}

func truncate(v []string, n int) []string {
	if len(v) <= n {
		return v
	}
	return append(append([]string{}, v[:n]...), "...")
}

// --- validation -------------------------------------------------------------

// nameRe is what may become both a path segment under /extensions and (after the
// chart's own sanitising) a Kubernetes volume name. Deliberately narrow: this
// string is interpolated into a container mount path, so anything that could
// escape it is refused rather than escaped.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// envRe is the conventional shape of an environment variable name.
var envRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// moduleRe is a shared-library basename with no directory part and no suffix.
var moduleRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// Variables the endpoint's own wiring owns. A registration naming one of these
// does not "add a data dir": the chart renders envPaths as container `env:`
// entries AFTER `envFrom: configMapRef`, so the registration's value WINS over
// the ConfigMap, and the endpoint comes up wrong in a way that looks nothing
// like an extension problem. Setting REQUIRE_WRITE_TOKEN to a directory path,
// for instance, fails the supervisor's "true"|"1" match and brings the endpoint
// up UNFENCED.
//
// Matched by prefix as well as exact name, because the exact set drifts: the
// supervisor reads these (endpoint_supervisor/src/main.rs, fence.rs), and a new
// YB_* or WRITE_TOKEN_* added there would otherwise silently become
// overridable. A prefix rule costs a registration nothing — no extension needs
// a variable in any of these families.
var reservedEnvPrefixes = []string{
	"YB_",          // the backing YugabyteDB connection
	"WRITE_TOKEN_", // the write lease
	"SUPERVISOR_",  // supervisor behaviour
	"PAGESERVER_",  // internal service ports
	"SAFEKEEPER_",  //
	"COMPUTE_",     // compute wiring + the settings dict
	"ENDPOINT_",    // identity and resource shape
	"CONTROLLER_",  // control-plane coordination
	"CONTROL_PLANE_",
	"PG_", // PG_DISTRIB_DIR and friends
}

var reservedEnvNames = map[string]bool{
	// The loader path this feature itself extends, and the process basics.
	"LD_LIBRARY_PATH": true, "EXTENSION_LIBRARY_PATH": true,
	"PATH": true, "HOME": true,
	// Pod identity and wiring the supervisor reads by exact name.
	"BRANCH_ID": true, "BIN_DIR": true, "RUN_DIR": true,
	"REQUIRE_WRITE_TOKEN": true, "PRERENDERED_CONFIG": true,
	"PRIVILEGED_ROLE_NAME": true, "CUSTOMER_ROLE": true, "CGROUP_ROOT": true,
	"AUTOPREWARM": true, "OFFLOAD_LFC_INTERVAL_SECONDS": true,
	"SHARED_PRELOAD_LIBRARIES": true, "EXTRA_SHARED_PRELOAD_LIBRARIES": true,
	"BASE_SHARED_PRELOAD_LIBRARIES": true,
}

// volumeName mirrors the chart's yb-amp-endpoint.extVolumeName helper: runs of
// anything outside [a-z0-9] collapse to a single "-", which is what makes two
// distinct catalog names able to claim one Kubernetes volume. Kept in step with
// templates/_helpers.tpl.
func volumeName(name string) string {
	return strings.Trim(nonLabelRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

var nonLabelRe = regexp.MustCompile(`[^a-z0-9]+`)

// reservedEnv reports whether the endpoint owns this variable.
func reservedEnv(name string) bool {
	if reservedEnvNames[name] {
		return true
	}
	for _, p := range reservedEnvPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Validate checks a registration and normalises it in place. It is exported so
// the API can reject a bad body with 400 before any task is planned, and so the
// rules live next to the type rather than in a handler.
func Validate(e *Extension) error {
	e.Name = strings.TrimSpace(e.Name)
	if !nameRe.MatchString(e.Name) {
		return fmt.Errorf("%w: name %q must match %s: it becomes the /extensions/<name> mount path and the volume name", ErrInvalid, e.Name, nameRe)
	}
	e.ImageRef = strings.TrimSpace(e.ImageRef)
	if err := validateImage(e.ImageRef); err != nil {
		return err
	}
	if e.Library != "" && !moduleRe.MatchString(e.Library) {
		return fmt.Errorf("%w: library %q must be a bare shared-library name with no directory or suffix", ErrInvalid, e.Library)
	}
	for _, p := range []struct{ field, val string }{
		{"layout.share_path", e.Layout.SharePath},
		{"layout.lib_path", e.Layout.LibPath},
	} {
		if p.val == "" {
			continue
		}
		if err := validateRelPath(p.field, p.val); err != nil {
			return err
		}
	}
	for _, d := range e.Layout.LibraryPath {
		if err := validateRelPath("layout.library_path", d); err != nil {
			return err
		}
	}
	for k, v := range e.Layout.EnvPaths {
		if !envRe.MatchString(k) {
			return fmt.Errorf("%w: layout.env_paths: %q is not a valid environment variable name", ErrInvalid, k)
		}
		if reservedEnv(k) {
			return fmt.Errorf("%w: layout.env_paths: %q is set by the endpoint itself and cannot be overridden", ErrInvalid, k)
		}
		if err := validateRelPath("layout.env_paths["+k+"]", v); err != nil {
			return err
		}
	}
	for _, m := range e.ABIExtraExempt {
		if strings.HasSuffix(m, ".so") {
			return fmt.Errorf("%w: abi_extra_exempt: %q must drop the .so suffix — the server matches the bare module name", ErrInvalid, m)
		}
		if !moduleRe.MatchString(m) {
			return fmt.Errorf("%w: abi_extra_exempt: %q is not a bare module name", ErrInvalid, m)
		}
	}
	return nil
}

// validateRelPath refuses anything that could point outside the extension's own
// mount. These strings are joined onto /extensions/<name> and handed to the
// loader and to PostgreSQL's path GUCs, so an absolute path or a ".." segment
// would read from the endpoint image instead of the volume.
func validateRelPath(field, p string) error {
	switch {
	case p == "":
		return fmt.Errorf("%w: %s must not be empty", ErrInvalid, field)
	case strings.HasPrefix(p, "/"):
		return fmt.Errorf("%w: %s %q must be relative to the /extensions/<name> mount root", ErrInvalid, field, p)
	case strings.Contains(p, `\`):
		return fmt.Errorf("%w: %s %q must not contain a backslash", ErrInvalid, field, p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return fmt.Errorf("%w: %s %q must not contain empty or relative path segments", ErrInvalid, field, p)
		}
	}
	return nil
}

// validateImage requires a reference pinned to a tag or digest. An unpinned
// reference resolves to :latest, and the endpoint mounts ImageVolumes with
// pullPolicy IfNotPresent — so a node that already cached :latest keeps serving
// the old bits forever while a fresh node pulls the new ones, and two endpoints
// on one branch silently disagree about what the extension is.
func validateImage(ref string) error {
	if ref == "" {
		return fmt.Errorf("%w: image is required: a registered extension is defined by the image it mounts", ErrInvalid)
	}
	if strings.ContainsAny(ref, " \t\n") {
		return fmt.Errorf("%w: image %q must not contain whitespace", ErrInvalid, ref)
	}
	if i := strings.Index(ref, "@"); i >= 0 {
		if !strings.HasPrefix(ref[i+1:], "sha256:") || len(ref[i+1:]) != len("sha256:")+64 {
			return fmt.Errorf("%w: image %q has a malformed digest", ErrInvalid, ref)
		}
		return nil
	}
	// A tag is the part after the last ':' that is not inside a host:port — i.e.
	// it must come after the last '/'.
	tag := ""
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		tag = ref[i+1:]
	}
	if tag == "" {
		return fmt.Errorf("%w: image %q must be pinned to a tag or @sha256 digest", ErrInvalid, ref)
	}
	if tag == "latest" {
		return fmt.Errorf("%w: image %q: :latest is refused — ImageVolumes pull IfNotPresent, so a cached node would never see a new push", ErrInvalid, ref)
	}
	return nil
}
