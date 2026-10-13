package task

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/deploy"
	"github.com/yugabyte/amp/controller/internal/extensions"
	"github.com/yugabyte/amp/controller/internal/ingress"
	applog "github.com/yugabyte/amp/controller/internal/log"
	"github.com/yugabyte/amp/controller/internal/node"
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
	// PasswordVerifier is the password for the compute's default user, in
	// `pg_authid`'s form (see core.SCRAMPasswordVerifier). Empty means the
	// supervisor leaves the role's password alone — a redeploy, a read_only
	// endpoint and a pre-credentials endpoint's replay all render it empty.
	//
	// The JSON key changed with the algorithm, so a create task written by an
	// older controller and replayed by this one decodes it as empty. That is a
	// fresh endpoint whose cloud_admin has no password (loopback only) rather
	// than one carrying a wrong or default credential — see remote_auth's
	// "unset means leave it alone".
	//
	// The verifier is the one credential-shaped thing the control plane holds, and
	// it is held here — on the step, which no API route projects — rather than on
	// the task input, which GET /tasks returns as `params`. The plaintext is never
	// persisted anywhere.
	PasswordVerifier string `json:"password_verifier,omitempty"`
	// CPU and Memory are the pod's compute reservation (Kubernetes quantities,
	// e.g. "2" / "4Gi"), applied as request == limit for Guaranteed QoS. Carried
	// in the args so a reclaim/replay deploys the same size. Empty falls back to
	// the deployer/chart default (2 cores / 4Gi).
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	// Version pins the endpoint to a deployer version (today the endpoint image
	// tag). Carried in the args for the same reason as CPU/Memory — a replay must
	// deploy the same thing — and empty means the deployer's configured default,
	// which is what every create asks for until something pins one.
	Version string `json:"version,omitempty"`
	// Connectivity is the endpoint's client reachability (deploy.Connectivity*):
	// cluster_ip, nlb_internal or nlb_external. Carried in the args so a
	// reclaim/replay recreates the same exposure. Empty means cluster_ip.
	Connectivity string `json:"connectivity,omitempty"`
	// AllowedCIDRs restricts which sources may reach an nlb_* endpoint's load
	// balancer (loadBalancerSourceRanges). Empty = unrestricted.
	AllowedCIDRs []string `json:"allowed_cidrs,omitempty"`
	// Extended is per-endpoint compute configuration the control plane passes
	// through WITHOUT interpreting: extra Postgres settings, and for a locally
	// deployed compute the ports it binds. Stored under config.extended, nested
	// so a passthrough key can never collide with a product one.
	Extended json.RawMessage `json:"extended,omitempty"`
	// PgSettings are this endpoint's own extra PostgreSQL settings, merged over
	// its branch's at spec-build time (the endpoint's win).
	PgSettings map[string]string `json:"pg_settings,omitempty"`
	// PageserverSettings and SafekeeperSettings are this endpoint's overrides for
	// the two storage daemons in its pod. Unlike the compute spec they never travel
	// through the deployer — the supervisor reads them from the endpoint's row at
	// pod start — but they are carried here so a replay writes the same row.
	PageserverSettings map[string]any `json:"pageserver_settings,omitempty"`
	SafekeeperSettings map[string]any `json:"safekeeper_settings,omitempty"`
	// Tags are this endpoint's own key/value tags. The labels it deploys with are
	// these merged over its cluster's, read back from the record (see Execute).
	Tags map[string]string `json:"tags,omitempty"`
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
	ingress    IngressAllocator
	dnsZones   ingress.Zones
	yb         deploy.YBConn
	scURL      string
	extensions *extensions.Service
	remoteAuth RemoteAuthReader
}

// NewCreateEndpointStep binds create_endpoint args + its deps into an executable step.
func NewCreateEndpointStep(args CreateEndpointArgs, deps Deps) Step {
	return &createEndpointStep{
		CreateEndpointArgs: args,
		ingress:            deps.Ingress,
		dnsZones:           deps.DNSZones,
		state:              deps.State,
		deployer:           deps.Deployer,
		yb:                 deps.EndpointYB,
		scURL:              deps.ControllerURL,
		extensions:         deps.Extensions,
		remoteAuth:         deps.RemoteAuth,
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

	// Ahead of the record write: the resolved set is persisted in the recreate
	// config below.
	exts, err := resolveBranchExtensions(ctx, s.extensions, s.state, s.BranchID)
	if err != nil {
		return Fail(err)
	}
	// Rendered from the args: this step's desired state is the row it is about to
	// write, so it assembles the record rather than reading one back.
	spec := deploy.EndpointSpecFrom(core.EndpointDeploy{
		ID:           s.EndpointID,
		BranchID:     s.BranchID,
		PgVersion:    s.PgVersion,
		CPU:          s.CPU,
		Memory:       s.Memory,
		Version:      s.Version,
		Connectivity: s.Connectivity,
		AllowedCIDRs: s.AllowedCIDRs,
	}, s.yb, s.scURL, exts)
	// The create is the only deploy that carries a verifier; EndpointSpecFrom
	// cannot supply one because nothing stores it.
	spec.PasswordVerifier = s.PasswordVerifier

	// Record the endpoint row first: it claims the single-writer slot (a second
	// writer is rejected here) and gives the deploy a stable id, before any
	// compute is spent. config is the spec stored for (re)creation.
	config, err := endpointConfig(s.CreateEndpointArgs, extensionNames(spec.Extensions), s.dnsName())
	if err != nil {
		return Fail(err)
	}
	settings, err := json.Marshal(orEmptyMap(s.PgSettings))
	if err != nil {
		return Fail(fmt.Errorf("encode endpoint pg_settings: %w", err))
	}
	pageserverSettings, err := json.Marshal(orEmptyAnyMap(s.PageserverSettings))
	if err != nil {
		return Fail(fmt.Errorf("encode endpoint pageserver_settings: %w", err))
	}
	safekeeperSettings, err := json.Marshal(orEmptyAnyMap(s.SafekeeperSettings))
	if err != nil {
		return Fail(fmt.Errorf("encode endpoint safekeeper_settings: %w", err))
	}
	tags, err := json.Marshal(orEmptyMap(s.Tags))
	if err != nil {
		return Fail(fmt.Errorf("encode endpoint tags: %w", err))
	}
	if err := s.state.EnsureEndpointRecord(ctx, s.EndpointID, s.ClusterID, s.BranchID, s.Role, s.Name,
		config, settings, pageserverSettings, safekeeperSettings, tags); err != nil {
		return Fail(fmt.Errorf("ensure endpoint record: %w", err))
	}
	// The labels are the one thing read back rather than assembled from the args:
	// they include the cluster's tags and its pool, and the record's projection is
	// where those are joined in, so the create renders exactly what every
	// redeploy will.
	rec, err := s.state.EndpointDeploy(ctx, s.EndpointID)
	if err != nil {
		return Fail(fmt.Errorf("read endpoint labels: %w", err))
	}
	spec.Labels = rec.Labels
	spec.Identity = deploy.IdentityLabels(rec)
	spec.IdentityAnnotations = deploy.IdentityAnnotations(rec)
	// Read back rather than minted again: on a replay the record already exists
	// and the insert above wrote nothing, so the stored name is the one clients
	// may already hold, whatever the zones say now.
	spec.DNSName = rec.DNSName

	if deploy.IsIngress(s.Connectivity) {
		slot, err := s.ingressSlot(ctx)
		if err != nil {
			return Fail(err)
		}
		spec.Ingress = slot
		logger.Info("ingress slot", "shard", slot.Shard, "port", slot.Port, "dns_name", slot.DNSName)
	}

	deployed, err := s.deployer.CreateEndpoint(ctx, spec)
	if err != nil {
		return Fail(fmt.Errorf("deploy endpoint: %w", err))
	}
	if err := s.state.SetEndpointAddress(ctx, s.EndpointID, deployed.Address, deployed.TailnetAddress); err != nil {
		return Fail(fmt.Errorf("record endpoint address: %w", err))
	}
	// Record what the deployer actually ran, not what the args asked for: an empty
	// Version means "the deployer's default", and the deployer is the only thing
	// that knows what that resolved to. An empty report is no information at all
	// (LocalDeployer places nothing) and must not overwrite a version already on
	// the row with "unknown".
	if deployed.Version != "" {
		if err := s.state.SetEndpointVersion(ctx, s.EndpointID, deployed.Version); err != nil {
			return Fail(fmt.Errorf("record endpoint version: %w", err))
		}
		// The first endpoint on an empty fleet sets the fleet version: its record
		// may name whatever the last controller to start over the empty fleet
		// deployed. Logged rather than failed — the endpoint is up, and a stale
		// record only under-reports until the next roll.
		if err := core.AdvanceFleetVersion(ctx, s.state, deployed.Version); err != nil {
			logger.Warn("record fleet version after endpoint create", "error", err)
		}
	}

	// Before success, so a replay that finds the floor set skips the wait.
	outcome, err := s.recordRootFloor(ctx, logger, spec.Name)
	if err != nil {
		return Fail(fmt.Errorf("record root fork floor: %w", err))
	}

	logger.Info("endpoint created and deployed", "address", deployed.Address, "tailnet_address", deployed.TailnetAddress, "version", deployed.Version)
	result := map[string]string{"endpoint_id": s.EndpointID.String(), "address": deployed.Address, "tailnet_address": deployed.TailnetAddress}
	// Read by core.BackfillRootForkLSN, which must not take an outcome that
	// leaves the password in doubt for one the catalog has.
	if outcome != "" {
		result["compute_password"] = outcome
	}
	return Ok(mustResult(result))
}

// The step result's compute_password values, which core.BackfillRootForkLSN
// reads: applied, applied_no_lsn, already_set, unset and failed are the
// supervisor's answer; unknown is no answer, and unsupported an endpoint image
// older than GET /v1/remote-auth, which applies the password but cannot say so.
const (
	computePasswordApplied      = "applied"
	computePasswordAppliedNoLSN = "applied_no_lsn"
	computePasswordAlreadySet   = "already_set"
	computePasswordUnset        = "unset"
	computePasswordFailed       = "failed"
	computePasswordUnknown      = "unknown"
	computePasswordUnsupported  = "unsupported"
)

// unknownForkFloor is the floor of a root whose catalog has a password set at
// an LSN nothing recorded: every fork point stays allowed.
const unknownForkFloor = "0/0"

// remoteAuthPoll paces the supervisor polls and remoteAuthBudget bounds them.
// The supervisor decides within about a minute of the compute answering
// queries, and CreateEndpoint has already waited for the pod, so the budget is
// slack for a slow first boot rather than for the deploy. Variables so tests
// need not wait them out.
var (
	remoteAuthPoll   = 2 * time.Second
	remoteAuthBudget = 3 * time.Minute
)

// recordRootFloor records a root branch's floor (core.Branch.ForkLSN): the LSN
// this endpoint's supervisor reports after committing the create's password. A
// fork below it would start from a catalog with no password, which is why the
// API refuses one.
//
// applied records the LSN. already_set, unsupported and applied_no_lsn mean
// the catalog has a password but nothing says from where, so they record 0/0,
// as BackfillRootForkLSN would: the root stops requiring a password and every
// fork point stays allowed. Recording "now" instead would refuse forks into
// history that has the password.
//
// A floor that cannot be recorded is logged, never a failed create. The one
// error returned is a shutdown that cut the attempt short, so the step is
// interrupted and the controller that resumes it records the floor.
//
// The outcome returned goes in the step's result, and is empty when the step
// did not ask. Any failure to find out reports unknown, so a password that may
// not have landed never reads as one that did.
func (s *createEndpointStep) recordRootFloor(ctx context.Context, logger *slog.Logger, name string) (string, error) {
	// Without a controller URL neither deployer opens the supervisor's API, so
	// asking would wait out the budget against a closed port and record unknown.
	if s.PasswordVerifier == "" || s.remoteAuth == nil || s.scURL == "" {
		return "", nil
	}
	base := s.deployer.EndpointSupervisorURL(name)
	if base == "" {
		return "", nil
	}
	logger = logger.With(applog.KeyBranchID, s.BranchID.String())
	pending, err := s.state.RootForkLSNPending(ctx, s.BranchID)
	if err != nil {
		logger.Warn("read root fork floor; recording none", "error", err)
		return computePasswordUnknown, ctx.Err()
	}
	if !pending {
		return "", nil
	}
	status, err := s.awaitRemoteAuth(ctx, base)
	if ctx.Err() != nil {
		logger.Warn("interrupted waiting for the compute-password outcome; recording no fork floor", "error", ctx.Err())
		return computePasswordUnknown, ctx.Err()
	}
	var floor, outcome string
	switch status.State {
	case node.RemoteAuthApplied:
		if _, perr := core.ParseLSN(status.LSN); perr == nil {
			floor, outcome = status.LSN, computePasswordApplied
		} else {
			logger.Warn("compute password applied but no usable lsn reported; recording fork floor 0/0", "lsn", status.LSN)
			floor, outcome = unknownForkFloor, computePasswordAppliedNoLSN
		}
	case node.RemoteAuthAlreadySet:
		logger.Info("branch catalog already had a password; recording fork floor 0/0")
		floor, outcome = unknownForkFloor, computePasswordAlreadySet
	case node.RemoteAuthUnsupported:
		logger.Info("endpoint image cannot report the compute-password outcome; recording fork floor 0/0")
		floor, outcome = unknownForkFloor, computePasswordUnsupported
	case node.RemoteAuthUnset:
		logger.Warn("endpoint deployed without its password; recording no fork floor")
		return computePasswordUnset, nil
	case node.RemoteAuthFailed:
		logger.Warn("supervisor failed to set the compute password; recording no fork floor")
		return computePasswordFailed, nil
	default:
		// Still pending at the budget, never answered, or a state this
		// controller does not know.
		logger.Warn("compute-password outcome unknown; recording no fork floor",
			"state", string(status.State), "budget", remoteAuthBudget, "error", err)
		return computePasswordUnknown, nil
	}
	if err := s.state.SetRootForkLSN(ctx, s.BranchID, floor); err != nil {
		logger.Warn("record root fork floor", "fork_lsn", floor, "error", err)
		return outcome, ctx.Err()
	}
	logger.Info("recorded root fork floor", "fork_lsn", floor)
	return outcome, nil
}

// awaitRemoteAuth polls the supervisor at base until it has decided, the
// budget passes or ctx ends. An error from one poll is retried: a fresh
// endpoint's Service can refuse for a few seconds after the pod is Ready
// (meko#724). Returns the last answer, pending if none, and the last poll's error.
func (s *createEndpointStep) awaitRemoteAuth(ctx context.Context, base string) (node.RemoteAuthStatus, error) {
	deadline := time.Now().Add(remoteAuthBudget)
	for {
		status, err := s.remoteAuth.RemoteAuth(ctx, base)
		if err == nil && status.State != node.RemoteAuthPending {
			return status, nil
		}
		if err != nil {
			status = node.RemoteAuthStatus{State: node.RemoteAuthPending}
		}
		if !time.Now().Before(deadline) {
			return status, err
		}
		select {
		case <-ctx.Done():
			return status, ctx.Err()
		case <-time.After(remoteAuthPoll):
		}
	}
}

// ingressSlot resolves this endpoint's place on a shard.
//
// The allocation row is the record of where an endpoint answers, so nothing is
// carried in the step's args that could drift from it. A replay therefore
// redeploys the SAME address, which matters because issuing a second port would
// leave the first held until its quarantine expired and change an address a
// client may already hold.
//
// That replay-safety lives in Allocate, which returns the endpoint's existing
// live allocation rather than a new one — a step is the unit of re-run, so an
// allocator that answered a replay with an error would make this step fail
// permanently the second time it ran. There is deliberately no Get-then-Allocate
// guard here: a rule the caller has to remember is one the next caller forgets.
func (s *createEndpointStep) ingressSlot(ctx context.Context) (deploy.IngressSlot, error) {
	if s.ingress == nil {
		return deploy.IngressSlot{}, fmt.Errorf(
			"connectivity %s needs a shared ingress, but none is configured on this controller", s.Connectivity)
	}
	family := ingress.FamilyInternal
	if s.Connectivity == deploy.ConnectivityIngressExternal {
		family = ingress.FamilyExternal
	}
	alloc, err := s.ingress.Allocate(ctx, s.EndpointID, family)
	if err != nil {
		return deploy.IngressSlot{}, err
	}
	return slotFrom(alloc), nil
}

func slotFrom(a ingress.Allocation) deploy.IngressSlot {
	slot := deploy.IngressSlot{
		Shard:      a.Shard,
		Port:       a.Port,
		DNSName:    a.DNSName,
		ShardHost:  a.ShardHost,
		RecordType: a.RecordType,
	}
	// Deliberately NOT blanked here for an unpublished name. Doing that encoded
	// the decision in this one caller, and every redeploy assembles the spec from
	// the allocation row instead — so the bare name reached the deployer anyway
	// and overwrote a working address. deploy.PublishableDNSName decides, from
	// the stored name, on every path.
	return slot
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

// orEmptyAnyMap is orEmptyMap for the daemon-settings maps, whose values are
// mixed-type scalars. Both exist so a nil map marshals as `{}` rather than a JSON
// null, which the NOT NULL columns would reject.
func orEmptyAnyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// dnsName is the name to publish for an nlb_* endpoint, or "" for any other
// mode or where its family has no zone.
func (s *createEndpointStep) dnsName() string {
	if !deploy.IsDedicatedLB(s.Connectivity) {
		return ""
	}
	family := ingress.FamilyInternal
	if s.Connectivity == deploy.ConnectivityNLBExternal {
		family = ingress.FamilyExternal
	}
	return deploy.DedicatedLBDNSName(s.EndpointID, s.dnsZones.For(family))
}

func endpointConfig(args CreateEndpointArgs, extensions []string, dnsName string) ([]byte, error) {
	cfg := map[string]any{
		"cluster_id": args.ClusterID,
		"branch_id":  args.BranchID,
		"role":       args.Role,
		"pg_version": args.PgVersion,
		"cpu":        args.CPU,
		"memory":     args.Memory,
	}
	// Stored so GetEndpoint can report the exposure the endpoint was created
	// with, and so a controller-driven redeploy recreates the same one.
	if args.Connectivity != "" {
		cfg["connectivity"] = args.Connectivity
	}
	if len(args.AllowedCIDRs) > 0 {
		cfg["allowed_cidrs"] = args.AllowedCIDRs
	}
	if len(extensions) > 0 {
		cfg["extensions"] = extensions
	}
	if dnsName != "" {
		cfg["dns_name"] = dnsName
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
