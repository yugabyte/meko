package task

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/deploy"
	"github.com/yugabyte/amp/controller/internal/extensions"
	applog "github.com/yugabyte/amp/controller/internal/log"
)

// StepCreateEndpoint is the task_step.kind for the create_endpoint step.
const StepCreateEndpoint = "create_endpoint"

// CreateEndpointArgs are the inputs to the create_endpoint step — the shared step
// that create-cluster, branch, and scale-up all use (§8). It records the
// endpoint, deploys the compute, records the address, and clears the branch
// recipe.
type CreateEndpointArgs struct {
	EndpointID uuid.UUID `json:"endpoint_id"`
	ClusterID  uuid.UUID `json:"cluster_id"`
	BranchID   uuid.UUID `json:"branch_id"`
	Role       string    `json:"role"`
	PgVersion  int       `json:"pg_version"`
	// Name is the caller's handle for the endpoint, unique among its branch's
	// endpoints. Empty means unnamed.
	Name string `json:"name,omitempty"`
	// CPU and Memory are the pod's compute reservation (Kubernetes quantities,
	// e.g. "2" / "4Gi"), applied as request == limit for Guaranteed QoS. Carried
	// in the args so a reclaim/replay deploys the same size. Empty falls back to
	// the deployer/chart default (2 cores / 4Gi).
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	// Extensions are externalized extensions to enable on this endpoint (catalog
	// names). The step upserts them onto the branch's durable set before
	// resolving mounts, so the opt-in survives a recreate and forks inherit it.
	Extensions []string `json:"extensions,omitempty"`
	// Extended is per-endpoint compute configuration the control plane passes
	// through WITHOUT interpreting: extra Postgres settings, and for a locally
	// deployed compute the ports it binds. Stored under config.extended, nested
	// so a passthrough key can never collide with a product one.
	Extended json.RawMessage `json:"extended,omitempty"`
	// PgSettings are this endpoint's own extra PostgreSQL settings, merged over
	// its branch's at spec-build time (the endpoint's win).
	PgSettings map[string]string `json:"pg_settings,omitempty"`
}

// createEndpointStep records the endpoint, provisions the compute, and records
// the resulting address. All three effects are idempotent, so the step
// converges when replayed: the record insert is id-keyed ON CONFLICT DO NOTHING,
// the deploy is keyed by the endpoint's stable name (deployer idempotency), and
// the address write is an absolute UPDATE. Its deploy collaborators are bound at
// construction (deployer is required); only the embedded args are serialized.
type createEndpointStep struct {
	CreateEndpointArgs
	state      StateStore
	deployer   deploy.Deployer
	yb         deploy.YBConn
	scURL      string
	extensions *extensions.Catalog
}

// NewCreateEndpointStep binds create_endpoint args + its deps into an executable step.
func NewCreateEndpointStep(args CreateEndpointArgs, deps Deps) Step {
	return &createEndpointStep{
		CreateEndpointArgs: args,
		state:              deps.State,
		deployer:           deps.Deployer,
		yb:                 deps.EndpointYB,
		scURL:              deps.ControllerURL,
		extensions:         deps.Extensions,
	}
}

func buildCreateEndpointStep(raw json.RawMessage, deps Deps) (Step, error) {
	var args CreateEndpointArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("decode create_endpoint args: %w", err)
	}
	return NewCreateEndpointStep(args, deps), nil
}

func (*createEndpointStep) Kind() string { return StepCreateEndpoint }

func (s *createEndpointStep) Execute(ctx context.Context) StepResult {
	if s.deployer == nil {
		return Fail(fmt.Errorf("create_endpoint step requires a deployer, but none is configured"))
	}
	logger := applog.FromContext(ctx).With(applog.KeyEndpointID, s.EndpointID.String())

	// Persist any requested opt-in extensions onto the branch's durable set
	// first, so they survive a recreate and forks inherit them. Idempotent
	// (absolute union); a no-op when none were requested. Only meaningful with a
	// catalog (the handler rejects opt-ins otherwise), but harmless without one.
	if len(s.Extensions) > 0 {
		if err := s.state.AddBranchExtensions(ctx, s.ClusterID, s.BranchID, s.Extensions); err != nil {
			return Fail(fmt.Errorf("add branch extensions: %w", err))
		}
	}

	// Resolve the endpoint's externalized extensions: the branch's opted-in set
	// (inherited across forks, plus anything just added above) unioned with the
	// catalog defaults, mapped to pinned images. Nil catalog => none (Docker
	// driver / feature off). Done before the record so the resolved set is
	// persisted in the recreate config.
	extSpecs, extNames, err := s.resolveExtensions(ctx)
	if err != nil {
		return Fail(err)
	}

	// Record the endpoint row first: it claims the single-writer slot (a second
	// writer is rejected here) and gives the deploy a stable id, before any
	// compute is spent. config is the spec stored for (re)creation.
	config, err := endpointConfig(s.CreateEndpointArgs, extNames)
	if err != nil {
		return Fail(err)
	}
	settings, err := json.Marshal(orEmptyMap(s.PgSettings))
	if err != nil {
		return Fail(fmt.Errorf("encode endpoint pg_settings: %w", err))
	}
	if err := s.state.EnsureEndpointRecord(ctx, s.EndpointID, s.ClusterID, s.BranchID, s.Role, s.Name, config, settings); err != nil {
		return Fail(fmt.Errorf("ensure endpoint record: %w", err))
	}

	// The deploy spec speaks the storage layer's flat branch id: a branch's id is
	// the storage branch id verbatim, rendered via hex32 (no enclosing tenant).
	spec := deploy.EndpointSpec{
		Name:          EndpointDeployName(s.EndpointID),
		EndpointID:    s.EndpointID.String(),
		BranchID:      hex32(s.BranchID),
		PgVersion:     s.PgVersion,
		YB:            s.yb,
		ControllerURL: s.scURL,
		CPU:           s.CPU,
		Memory:        s.Memory,
		Extensions:    extSpecs,
		// No pg_settings here: the endpoint's supervisor reads the branch's and
		// the endpoint's straight from amp_registry and merges them itself, so a
		// change is a registry write rather than a redeploy. EndpointID above is
		// what it needs to find them.
	}

	deployed, err := s.deployer.CreateEndpoint(ctx, spec)
	if err != nil {
		return Fail(fmt.Errorf("deploy endpoint: %w", err))
	}
	if err := s.state.SetEndpointAddress(ctx, s.EndpointID, deployed.Address, deployed.TailnetAddress); err != nil {
		return Fail(fmt.Errorf("record endpoint address: %w", err))
	}

	logger.Info("endpoint created and deployed", "address", deployed.Address, "tailnet_address", deployed.TailnetAddress)
	return Ok(mustResult(map[string]string{"endpoint_id": s.EndpointID.String(), "address": deployed.Address, "tailnet_address": deployed.TailnetAddress}))
}

// resolveExtensions computes the endpoint's externalized extension mounts: the
// branch's opted-in set (durable, inherited across forks) unioned with the
// catalog defaults, resolved to pinned image references. It returns the deploy
// specs (for the EndpointSpec) and the resolved names (for the recreate config).
// With no catalog it returns (nil, nil) — the endpoint ships only image-baked
// extensions. Unknown names are dropped by the catalog, so a stale branch entry
// can't wedge the deploy.
func (s *createEndpointStep) resolveExtensions(ctx context.Context) ([]deploy.Extension, []string, error) {
	if s.extensions == nil {
		return nil, nil, nil
	}
	opted, err := s.state.BranchExtensions(ctx, s.ClusterID, s.BranchID)
	if err != nil {
		return nil, nil, fmt.Errorf("read branch extensions: %w", err)
	}
	names := unionStrings(s.extensions.DefaultNames(), opted)
	resolved := s.extensions.Resolve(names)
	if len(resolved) == 0 {
		return nil, nil, nil
	}
	specs := make([]deploy.Extension, len(resolved))
	got := make([]string, len(resolved))
	for i, r := range resolved {
		specs[i] = deploy.Extension{Name: r.Name, Image: r.Image, Preload: r.Preload}
		got[i] = r.Name
	}
	return specs, got, nil
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

// endpointConfig is the spec stored on the endpoint row for (re)creation. The
// endpoint's own id is the row id, so it is not part of the recreate spec. This
// is the JSON persisted on the endpoint row for (re)creation. extensions is the
// resolved externalized-extension set (names), recorded so a recreate/audit sees
// what the endpoint mounted; omitted when empty.

// orEmptyMap normalizes a nil map to an empty one so it encodes as `{}` rather
// than `null` — the column is NOT NULL and readers expect an object.
func orEmptyMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func endpointConfig(args CreateEndpointArgs, extensions []string) ([]byte, error) {
	cfg := map[string]any{
		"cluster_id": args.ClusterID,
		"branch_id":  args.BranchID,
		"role":       args.Role,
		"pg_version": args.PgVersion,
		"cpu":        args.CPU,
		"memory":     args.Memory,
	}
	if len(extensions) > 0 {
		cfg["extensions"] = extensions
	}
	if len(args.Extended) > 0 {
		cfg["extended"] = args.Extended
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("encode endpoint config: %w", err)
	}
	return b, nil
}
