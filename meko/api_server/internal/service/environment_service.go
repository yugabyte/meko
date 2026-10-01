package service

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// ErrValidation wraps input-validation failures (mapped to 400 by handlers).
var ErrValidation = errors.New("validation error")

// ErrKindMismatch is returned when an agent enrolls declaring a kind that does
// not match the kind the environment was registered with (mapped to 409). It
// stops an amp controller from attaching to a meko environment, or vice versa,
// which would otherwise reconcile the wrong desired state.
var ErrKindMismatch = errors.New("environment kind mismatch")

// ErrManagedRequiresOperator is returned when a scoped (account-bound) caller tries to
// register a MANAGED environment (mapped to 403). MANAGED is Yugabyte-operated shared
// infrastructure: the region APIs pin any account's storage pools onto the oldest READY
// MANAGED amp env in a region with no account check (pickManagedEnvTx), so a customer
// that could register one under its own API key would have its agent handed other
// tenants' desired state. Only an unscoped operator (admin secret, no account header)
// may register MANAGED, which keeps type='MANAGED' a marker a customer cannot set.
var ErrManagedRequiresOperator = errors.New("registering a MANAGED environment requires an operator (unscoped) caller")

// ErrAmbiguousID re-exports the repository's, so handlers map it to a status code
// without importing the storage layer. Raised when an unscoped caller names an id
// two accounts both hold (mapped to 409) -- send an account to disambiguate.
var ErrAmbiguousID = repository.ErrAmbiguousID

// ErrAPIKeyBound re-exports the repository's for the same reason. Raised when a
// registration names an api key another environment already holds (mapped to 409):
// one key drives one environment.
var ErrAPIKeyBound = repository.ErrAPIKeyBound

// ErrEnvironmentExists re-exports the repository's for the same reason. Raised when
// a registration's (account_id, id) is already taken (mapped to 409).
var ErrEnvironmentExists = repository.ErrEnvironmentExists

// AccountScope limits an operation to one account's environments. The zero value
// is unscoped and reaches every environment; --require-account-header stops such a
// call at the router, so it arrives here only where fleet-wide calls are allowed.
//
// It is a distinct type so that an account id cannot be passed where an
// environment id is wanted, and so the unscoped case is one documented rule
// rather than an empty string tested at each call site.
//
// Scoping is applied in SQL, against the (account_id, id) unique index, rather
// than by reading a row and comparing -- so an environment the scope does not own
// is not read at all.
type AccountScope string

// Environment kinds. An amp environment is driven by the amp controller (which
// reports a resource inventory and no meko-stack); a meko environment is driven
// by the datapack/stack agent. The kind is fixed at registration and is the only
// record of what an environment serves — the id derives from the name alone.
const (
	KindMeko = "meko"
	KindAmp  = "amp"
)

// normalizeKind lowercases and validates a declared kind, defaulting empty to
// meko so a pre-kind caller (an older console, the existing meko flow) keeps its
// behaviour. Returns ErrValidation for anything else.
func normalizeKind(kind string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "", KindMeko:
		return KindMeko, nil
	case KindAmp:
		return KindAmp, nil
	default:
		return "", fmt.Errorf("%w: unknown environment kind %q (want %q or %q)", ErrValidation, kind, KindMeko, KindAmp)
	}
}

// Environment types. BYOC is a customer-run data plane in their own cloud (what YCP
// exists for); MANAGED is one Yugabyte runs. Fixed at registration, like the kind.
const (
	TypeBYOC    = "BYOC"
	TypeManaged = "MANAGED"
)

// normalizeType uppercases and validates a declared type, defaulting empty to BYOC
// so a legacy row and a pre-type caller read as BYOC. Returns ErrValidation for
// anything else. Mirrors normalizeKind.
func normalizeType(t string) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(t)) {
	case "", TypeBYOC:
		return TypeBYOC, nil
	case TypeManaged:
		return TypeManaged, nil
	default:
		return "", fmt.Errorf("%w: unknown environment type %q (want %q or %q)", ErrValidation, t, TypeBYOC, TypeManaged)
	}
}

// The environment lifecycle states this control plane KNOWS. Not the set it
// accepts: an agent the customer upgrades on their own schedule can report a state
// added after this binary shipped, and per REVIEW.md ("a new value is safe only if
// the receiver tolerates unknown states") refusing it would strand that agent's
// whole fleet. Unknown is therefore stored and warned about, not rejected.
//
// A DELIBERATE COPY of EnvState in ycp/internal/api/types.go and the EnvState enum
// in design/ycp-openapi.yaml -- ycp and meko are separate Go modules that must not
// cross-import, and the spec is not Go at all. The copies drifting apart no longer
// breaks a fleet, but it does make the published contract untrue and the warning
// below misleading, so TestEnvStateListsAgreeAcrossTheRepo holds them together.
const (
	StatePending             = "PENDING"
	StateClusterProvisioning = "CLUSTER_PROVISIONING"
	StateEnrolling           = "ENROLLING"
	StateAttesting           = "ATTESTING"
	StateProvisioning        = "PROVISIONING"
	StateValidating          = "VALIDATING"
	StateReady               = "READY"
	StateDegraded            = "DEGRADED"
	StateSuspended           = "SUSPENDED"
	StateDraining            = "DRAINING"
	StateDeregistered        = "DEREGISTERED"
)

var envStates = map[string]struct{}{
	StatePending: {}, StateClusterProvisioning: {}, StateEnrolling: {},
	StateAttesting: {}, StateProvisioning: {}, StateValidating: {},
	StateReady: {}, StateDegraded: {}, StateSuspended: {},
	StateDraining: {}, StateDeregistered: {},
}

// normalizeState uppercases a reported state and rejects only an absent one.
//
// Absent is the whole of CLOUDGA-36984: it is what a dropped request body
// deserializes to, it is stored verbatim in a NOT NULL column so the DEFAULT never
// rescues it, and it renders as a blank chip on an environment whose heartbeat says
// it is healthy -- indistinguishable from a working agent. An unknown-but-present
// state is the opposite: legible in the console, and the expected shape of version
// skew, so it is accepted. stateIsKnown is how a caller says so in the log.
func normalizeState(state string) (string, error) {
	s := strings.ToUpper(strings.TrimSpace(state))
	if s == "" {
		return "", fmt.Errorf("%w: environment_state is required", ErrValidation)
	}
	return s, nil
}

func stateIsKnown(state string) bool {
	_, ok := envStates[state]
	return ok
}

// EnvironmentService owns the YCP fleet-registry logic: the api-key identity
// binding, status ingestion, and generic desired-state. Control-plane mode only.
type EnvironmentService struct {
	repo     *repository.EnvironmentRepo
	taskRepo *repository.EnvironmentTaskRepo
	extRepo  *repository.EnvironmentExtensionRepo

	// pruneMu guards lastPruned, which rate-limits the fleet-wide task-retention
	// delete. Every environment's agent heartbeats every 10s, so pruning on each
	// report that carried a task would run one fleet-wide DELETE per environment per
	// tick to delete nothing almost every time.
	pruneMu    sync.Mutex
	lastPruned time.Time

	// telemetry is what this control plane accepts from an enrolling amp agent.
	// Zero value checks nothing; SetTelemetryPolicy configures it.
	telemetry TelemetryPolicy
}

// pruneEvery is the floor between task-retention passes. Retention is 30 days, so
// the exact moment a row past the window goes is not interesting; not hammering the
// table is.
const pruneEvery = time.Hour

// NewEnvironmentService constructs the service.
func NewEnvironmentService(
	repo *repository.EnvironmentRepo,
	taskRepo *repository.EnvironmentTaskRepo,
) *EnvironmentService {
	return &EnvironmentService{repo: repo, taskRepo: taskRepo}
}

// ---- request DTOs ----

// ClusterFacts are the read-only facts the agent reports.
type ClusterFacts struct {
	Provider   string `json:"provider,omitempty"`
	Region     string `json:"region,omitempty"`
	K8sVersion string `json:"k8s_version,omitempty"`
	NodeCount  int    `json:"node_count,omitempty"`
}

// EnvComponent is one data-plane service's health.
type EnvComponent struct {
	Name    string `json:"name"`
	Healthy int    `json:"healthy"`
	Total   int    `json:"total"`
	Version string `json:"version,omitempty"`
	// Error is why a declared component is not running, when the agent could say.
	// A rejected spec (a private image with no tag, a missing Secret) otherwise shows
	// up only as 0/1 in the console and as a line in agent logs the operator who sent
	// the command usually cannot read.
	Error string `json:"error,omitempty"`
}

// ClusterSpec requests a new cluster (provision mode).
type ClusterSpec struct {
	Provider string `json:"provider"`
	Region   string `json:"region"`
}

// RegisterEnvironmentRequest is the register-environment body (YBM's or the
// console's).
type RegisterEnvironmentRequest struct {
	Name string `json:"name"`
	// Kind is "amp" (the amp controller) or "meko" (the datapack/stack agent);
	// empty defaults to "meko". It fixes what the environment runs. The returned id
	// derives from the name alone, so a name is unique per account across kinds.
	Kind string `json:"kind,omitempty"`
	// Type is "BYOC" (customer-run data plane) or "MANAGED"; empty defaults to
	// "BYOC". Fixed at registration, like Kind.
	Type        string       `json:"type,omitempty"`
	AccountID   string       `json:"account_id"`
	Placement   string       `json:"placement"`
	Region      string       `json:"region"`
	ClusterSpec *ClusterSpec `json:"cluster_spec,omitempty"`

	// APIKeyID is the id of the YBM API key the environment's agent will
	// authenticate with. Required: an environment with no key binding is one no
	// agent can ever attach to.
	APIKeyID string `json:"api_key_id"`
	// AgentBaseURL is the public base the agent should dial YBM's pass-through on
	// (e.g. "https://cloud.yugabyte.com/api/public/v1"). Used only to render the
	// install hint; empty falls back to the address this register call arrived on,
	// which is the direct-to-meko dev shape. Stored with the environment, so a later
	// read renders the same hint rather than a URL the agent cannot reach.
	AgentBaseURL string `json:"agent_base_url,omitempty"`

	// Desired is the desired state this environment starts with — the only way it
	// starts with anything, since registration seeds nothing. Omitted, the
	// environment runs nothing until an operator POSTs a desired set.
	Desired *[]models.DesiredResource `json:"desired,omitempty"`
}

// EnrollRequest is the agent's enrollment body. Identity comes from the api-key
// headers, not the body; Name rides along for logs only.
type EnrollRequest struct {
	Name      string `json:"name"`
	Placement string `json:"placement"`
	// Kind is the kind the agent believes it is (amp | meko). It is checked
	// against the environment's registered kind so an amp controller cannot
	// attach to a meko environment or vice versa; empty is treated as meko.
	Kind         string       `json:"kind,omitempty"`
	ClusterFacts ClusterFacts `json:"cluster_facts"`
	// Telemetry is the agent's account of where this deployment ships metrics and
	// logs. Checked against TelemetryPolicy before the enrolment is accepted.
	Telemetry *Telemetry `json:"telemetry,omitempty"`
}

// ResourceStatus is one item of an amp environment's observed inventory: a
// storage pool, database (project), branch, or compute endpoint. The amp
// controller reports its whole inventory on each status heartbeat; the control
// plane stores it verbatim for the console. Kind-agnostic and flat — Parent
// links a resource to its owner (project→pool, branch→project, endpoint→branch)
// by id — so new resource kinds need no schema change.
type ResourceStatus struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Parent string `json:"parent,omitempty"`
	State  string `json:"state,omitempty"`
	// Error is why the resource is not converged, when the agent can say (a
	// controller rejection, a resource stuck non-Ready, or drift it cannot fix).
	// Stored and echoed for the console; empty when the resource is fine.
	Error string            `json:"error,omitempty"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

// StatusReport is the agent's heartbeat + phases.
type StatusReport struct {
	EnvironmentState string         `json:"environment_state"`
	AgentVersion     string         `json:"agent_version"`
	AppliedRevision  int            `json:"applied_revision"`
	ClusterFacts     ClusterFacts   `json:"cluster_facts"`
	Components       []EnvComponent `json:"components"`
	// Resources is the amp controller's observed inventory. A meko agent omits
	// it (nil); ApplyStatus leaves the stored inventory untouched when it is nil
	// and replaces it (including with an empty set) when it is present.
	Resources []ResourceStatus `json:"resources,omitempty"`
	// Tasks is a DELTA of the amp controller's task log: the tasks whose status has
	// moved since the agent's previous report. A meko agent omits it.
	//
	// Read it the OPPOSITE way to Resources above. Resources is a snapshot, so an
	// empty set means "this environment owns nothing" and replaces what was stored.
	// Tasks is a delta, so an empty set means "nothing changed this tick" -- the
	// common case -- and writes nothing. The control plane accumulates these deltas,
	// which is why it, and not the agent, holds the history.
	Tasks []TaskStatus `json:"tasks,omitempty"`
	// Telemetry re-attests on every tick what enroll attested once, so a deployment
	// that stops shipping telemetry after it enrolled is visible. nil from an agent
	// image that predates attestation — see TelemetryPolicy.check.
	Telemetry *Telemetry `json:"telemetry,omitempty"`
}

// TaskStatus is one amp controller task as the agent relays it. Kind-agnostic: the
// control plane stores and serves Kind and Result without interpreting either, so a
// controller that adds a task kind needs no change here.
type TaskStatus struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	ClusterID string `json:"cluster_id,omitempty"`
	TargetRef string `json:"target_ref,omitempty"`
	// Error is the controller's own failure reason, set when Status is failed.
	Error         string          `json:"error,omitempty"`
	Result        json.RawMessage `json:"result,omitempty"`
	YCPGeneration *int64          `json:"ycp_generation,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

func slug(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	return strings.NewReplacer(" ", "-", "_", "-", "/", "-").Replace(s)
}

// ---- operations ----

// List returns the scope's environments, or every one when unscoped.
func (s *EnvironmentService) List(scope AccountScope) ([]*models.Environment, error) {
	return s.repo.List(string(scope))
}

// Get returns one environment, or (nil, nil) if absent. An environment the scope
// does not own reads as absent, so a caller cannot tell another account's
// environment from one that was never registered.
func (s *EnvironmentService) Get(id string, scope AccountScope) (*models.Environment, error) {
	return s.repo.Get(string(scope), id)
}

// Register stores a new PENDING environment bound to the given API key. A name
// already claimed by this account comes back as ErrEnvironmentExists.
//
// A scoped registration is stamped with the scope's account and ignores the body's
// account_id: the scope comes from the account YBM authenticated, the body from
// whoever composed the request, and only one of those can be trusted to say whose
// environment this is. An unscoped registration still takes the body's account_id,
// which is how the console has always set it.
func (s *EnvironmentService) Register(
	req RegisterEnvironmentRequest, scope AccountScope,
) (e *models.Environment, err error) {
	accountID := req.AccountID
	if scope != "" {
		accountID = string(scope)
	}
	// account_id is a UUID column, so a malformed value would otherwise surface as a
	// database error on insert. Rejected here instead, and empty stays legal: an
	// unaccounted environment is stored under a sentinel.
	if accountID != "" {
		if _, perr := uuid.Parse(accountID); perr != nil {
			return nil, fmt.Errorf("%w: account_id %q is not a uuid", ErrValidation, accountID)
		}
	}
	// Required: the key binding is the environment's only agent credential, so an
	// environment registered without one could never be attached to. Stored in the
	// parsed (canonical) form, not as sent -- uuid.Parse also accepts uppercase,
	// {braces}, urn:uuid: and bare hex.
	apiKey, perr := uuid.Parse(strings.TrimSpace(req.APIKeyID))
	if perr != nil {
		return nil, fmt.Errorf("%w: api_key_id %q is not a uuid", ErrValidation, req.APIKeyID)
	}
	name := strings.TrimSpace(req.Name)
	if len(name) < 3 || len(name) > 60 {
		return nil, fmt.Errorf("%w: name must be 3-60 chars", ErrValidation)
	}
	kind, err := normalizeKind(req.Kind)
	if err != nil {
		return nil, err
	}
	typ, err := normalizeType(req.Type)
	if err != nil {
		return nil, err
	}
	// MANAGED is operator-only. A scoped caller is a customer acting under its own
	// account (and its own API key), and the region APIs pin other tenants' storage
	// pools onto any READY MANAGED amp env with no account check -- so a customer that
	// could register MANAGED would have its own agent handed other tenants' clusters,
	// branches and endpoints. Refuse it here so type='MANAGED' can only come from the
	// unscoped operator path (admin secret, no account header).
	if typ == TypeManaged && scope != "" {
		return nil, ErrManagedRequiresOperator
	}
	// Checked before the insert, not after: a declaration this environment's agent
	// cannot converge should fail the registration outright rather than leave a
	// registered environment whose desired state was quietly dropped.
	if req.Desired != nil {
		if err := checkKinds(kind, *req.Desired); err != nil {
			return nil, err
		}
		logFleetWithoutController(req.Name, *req.Desired)
	}
	placement := req.Placement
	if placement == "" {
		placement = "cloud"
	}
	state := StatePending
	if req.ClusterSpec != nil {
		state = StateClusterProvisioning
	}
	// The id is the slug of the name, whatever the kind. It deliberately encodes no
	// kind: the id is the environment's addressing key — unique per account, the
	// {id} path segment, reconcileAgent.envName, the namespace namespaceFor derives —
	// so an id
	// asserting one kind could never be reused by an environment that came to serve
	// more than one. The kind column is what disambiguates, and always was.
	//
	// Ids minted before this keep their "amp-" prefix and keep resolving: nothing
	// parses the prefix, and every lookup is exact-match on the stored id.
	//
	// The trade-off is deliberate. A name is now unique per account across kinds, so
	// one account cannot hold both an amp and a meko environment called "prod" — the
	// second registration is refused as a conflict instead of being silently
	// qualified into a different id than the caller asked for.
	id := slug(name)
	e = &models.Environment{
		ID: id, Kind: kind, Type: typ, AccountID: accountID, Placement: placement, Region: req.Region,
		State: state, MaxDatapacks: 400, CreatedAt: time.Now().UTC(),
		Components: json.RawMessage("[]"), Resources: json.RawMessage("[]"),
		APIKeyID: apiKey.String(), AgentBaseURL: strings.TrimSpace(req.AgentBaseURL),
	}
	// Insert stamps e.UUID, which the desired-state write below is keyed on. A name
	// another account already holds is not a conflict: (account_id, id) is what is
	// unique, so this registers that account's own environment.
	if err := s.repo.Insert(e); err != nil {
		return nil, err
	}
	// Desired state comes with the registration, so the agent has something to do the
	// moment it enrolls. Failing to write it must not fail the registration: the
	// environment and its key binding are already stored, and a desired-state write
	// can always be retried or replaced by the operator.
	if res := s.initialDesired(req); res != nil {
		if err := s.repo.ReplaceDesired(e.UUID, res); err != nil {
			return e, nil //nolint:nilerr // see above: the registration stands
		}
	}
	return e, nil
}

// initialDesired is what a newly registered environment starts out declaring: what
// the caller asked for, or nothing.
//
// There is no server-side default any more, for either kind. A data plane's
// workloads are installed by its chart — yb-amp-controller for amp, the meko chart
// for meko — and the agent converges only what lives inside them, so a seeded
// meko-stack had no consumer once cmd/agent retired and could only ever report as
// undeliverable.
func (s *EnvironmentService) initialDesired(req RegisterEnvironmentRequest) []models.DesiredResource {
	if req.Desired != nil {
		return *req.Desired
	}
	return nil
}

// EnrollByKey is the agent's key-authenticated first check-in: it resolves the
// environment bound to the key, merges facts, and moves it to ATTESTING. (nil,
// nil) means no environment is bound to the key -- the handler answers 401.
//
// declaredKind is the kind the enrolling agent believes it is; empty is treated
// as meko. It must match the kind the environment was registered with, so an amp
// controller cannot attach to a meko environment (or vice versa) and start
// reporting or reconciling the wrong thing — a mismatch returns ErrKindMismatch.
func (s *EnvironmentService) EnrollByKey(apiKeyID, placement, declaredKind string, f ClusterFacts, t *Telemetry) (*models.Environment, error) {
	kind, err := normalizeKind(declaredKind)
	if err != nil {
		return nil, err
	}
	e, err := s.repo.GetByAPIKeyID(apiKeyID)
	if err != nil || e == nil {
		return nil, err
	}
	envKind := e.Kind
	if envKind == "" {
		envKind = KindMeko
	}
	if kind != envKind {
		return nil, fmt.Errorf("%w: environment %q is kind %q, agent declared %q", ErrKindMismatch, e.ID, envKind, kind)
	}
	degraded := s.telemetry.check(kind, t)
	e.Live = true
	if placement != "" {
		e.Placement = placement
	}
	mergeFacts(e, f)
	e.State = StateAttesting
	if degraded != "" {
		// Degraded from the moment it checks in, not from its first heartbeat: an
		// operator reading the console straight after registering should see the
		// problem, not an ATTESTING row that looks fine for a tick.
		e.State = StateDegraded
		// Accepted, but said where an operator looks rather than only in a log: an
		// environment reporting nowhere is one the console can show no metrics for,
		// and that has to be visible from the moment it enrolls. Folded into what is
		// already stored rather than replacing it — enrolment carries no component
		// report, so overwriting would erase the last good one.
		if comps, cerr := json.Marshal(withTelemetryComponent(storedComponents(e.Components), degraded)); cerr == nil {
			e.Components = comps
		}
	}
	// The MCP base URL and cert fingerprint are meko-specific: the meko agent fronts
	// a per-environment MCP endpoint under mekodata.ai and presents a TLS cert. The
	// amp controller reports neither, so synthesizing a mekodata.ai hostname and a
	// random fingerprint for it is misleading noise in the console — populate them
	// only for meko, and leave them empty for amp.
	if kind == KindMeko {
		e.MCPBaseURL = e.ID + ".mcp.mekodata.ai"
		e.CertFingerprint = "sha256:" + randHex(6)
	}
	if err := s.repo.Update(e); err != nil {
		return nil, err
	}
	return e, nil
}

// ResolveAgent returns the environment bound to the API key an agent call
// authenticated with, or nil when the key is bound to nothing -- a revoked key
// and a never-registered one are deliberately the same answer.
func (s *EnvironmentService) ResolveAgent(apiKeyID string) (*models.Environment, error) {
	if apiKeyID == "" {
		return nil, nil
	}
	return s.repo.GetByAPIKeyID(apiKeyID)
}

// StatusResult is what ApplyStatus managed to persist.
//
// TasksRecorded is reported separately from the error because the two halves of a
// report have different durability requirements. The environment's state and
// inventory are written first and unconditionally -- they are what its health rides
// on, and a task-table problem must not stop the fleet view updating. But the task
// delta is delivered ONCE: if the agent is told the report succeeded it retires that
// delta, and a transition dropped here is then gone for good, leaving a row stuck
// non-terminal that PruneTerminal deliberately spares. So the agent has to be told
// which half landed, rather than inferring it from a status code that covers both.
type StatusResult struct {
	// Found is false when the environment is unknown.
	Found bool
	// TasksRecorded is false only when the report carried a task delta that could not
	// be stored. True when it carried none, so a caller with nothing to retire is not
	// told to hold it.
	TasksRecorded bool
}

// ApplyStatus merges an agent status report. Found=false ⇒ unknown environment.
func (s *EnvironmentService) ApplyStatus(envUUID string, r StatusReport) (StatusResult, error) {
	// Before the read: a report with no state costs no query, and the heartbeat must
	// NOT be refreshed for it -- a report that did not happen buying the environment
	// another staleness window is the whole of CLOUDGA-36984.
	state, err := normalizeState(r.EnvironmentState)
	if err != nil {
		slog.Warn("reject agent status report", "env_uuid", envUUID,
			"reported_state", r.EnvironmentState, "error", err)
		return StatusResult{}, err
	}
	// Stored either way -- see envStates on why an unknown state is not an error --
	// but an operator seeing an unfamiliar state in the console should be able to
	// find out here whether this binary is simply older than the agent.
	if !stateIsKnown(state) {
		slog.Warn("agent reported a state this control plane does not know",
			"env_uuid", envUUID, "reported_state", state)
	}
	e, err := s.repo.GetByUUID(envUUID)
	if err != nil || e == nil {
		return StatusResult{}, err
	}
	now := time.Now().UTC()
	e.State = state
	// Guarded because a report can name a state and still omit this: normalizeState
	// above refuses only a report with NO state, and nullStr turns the empty string
	// into a NULL, erasing the version last reported. The merged fields below
	// (mergeFacts, Components, Resources) all hold to the same rule.
	if r.AgentVersion != "" {
		e.AgentVersion = r.AgentVersion
	}
	e.LastHeartbeatAt = &now
	mergeFacts(e, r.ClusterFacts)
	// Re-checked every tick, not only at enroll: a later `helm upgrade` that turns
	// telemetry off or repoints it would otherwise show up as nothing worse than a
	// gradually emptier dashboard. A refusal cannot un-enroll an environment that is
	// already running, so here it degrades rather than erroring.
	// Configuration first, then whether it is actually working. A deployment can
	// pass every configuration check and still be shipping nothing, which is the
	// failure the install-time preflight can only catch once.
	degraded := s.telemetry.check(e.Kind, r.Telemetry)
	if degraded == "" {
		degraded = s.telemetry.checkRuntime(e.Kind, r.Telemetry)
	}
	if degraded != "" {
		e.State = StateDegraded
	}
	// nil means the agent could not collect (no RBAC, a transient API error) — not
	// that the environment has no workloads, so the stored list is left alone. Marshal
	// would write `null`, which is not empty, so the repo's len==0 default never
	// rescues it and one blip would erase the last good report. Same guard as
	// Resources below, for the same reason.
	//
	// A telemetry problem still has to be recorded, and an amp agent reports no
	// components at all — so it is folded into whatever list survives that rule
	// rather than written over it.
	//
	// The recovery case is why this cannot be `degraded != ""`. For amp r.Components
	// is ALWAYS nil (the agent's Role cannot list pods, so k8scomponents latches and
	// reports none), so once the problem clears, both a `degraded != ""` and an
	// `r.Components != nil` test are false and the stale telemetry row is written
	// back unchanged, for ever — the state recovers above and the row does not.
	// hadTelemetryRow is what notices there is something to clean up.
	stored := storedComponents(e.Components)
	if r.Components != nil || degraded != "" || hasTelemetryComponent(stored) {
		base := r.Components
		if base == nil {
			base = stored
		}
		if comps, err := json.Marshal(withTelemetryComponent(base, degraded)); err == nil {
			e.Components = comps
		}
	}
	// The amp controller reports its inventory here; a meko agent omits it (nil),
	// in which case the stored inventory is left as it was. A present-but-empty
	// set is a real report (the environment currently owns nothing) and replaces
	// what was stored.
	if r.Resources != nil {
		if res, err := json.Marshal(r.Resources); err == nil {
			e.Resources = res
		}
	}
	if err := s.repo.Update(e); err != nil {
		return StatusResult{}, err
	}
	res := StatusResult{Found: true, TasksRecorded: true}

	// Tasks are a delta, so unlike Resources above an empty set is not a report of
	// "no tasks" -- it is the quiet tick, which is most of them. Only a non-empty
	// delta writes.
	//
	// Recorded after the heartbeat rather than in the same transaction: state and
	// inventory are what the environment's health rides on, and a task write that
	// failed must not cost us those. What it DOES cost is this report's claim on the
	// delta -- see StatusResult.
	if len(r.Tasks) > 0 {
		if err := s.recordTasks(envUUID, r.Tasks); err != nil {
			slog.Warn("record amp tasks for environment (the agent is told to hold the delta)",
				"env_uuid", envUUID, "tasks", len(r.Tasks), "error", err)
			res.TasksRecorded = false
		}
	}
	return res, nil
}

// recordTasks upserts a relayed task delta and prunes what has aged out.
func (s *EnvironmentService) recordTasks(envUUID string, tasks []TaskStatus) error {
	if s.taskRepo == nil {
		return nil
	}
	rows := make([]models.EnvironmentTask, 0, len(tasks))
	for _, t := range tasks {
		rows = append(rows, models.EnvironmentTask{
			TaskID:        t.ID,
			Kind:          t.Kind,
			Status:        t.Status,
			ClusterID:     t.ClusterID,
			TargetRef:     t.TargetRef,
			Error:         t.Error,
			Result:        t.Result,
			YCPGeneration: t.YCPGeneration,
			CreatedAt:     t.CreatedAt,
			UpdatedAt:     t.UpdatedAt,
		})
	}
	if err := s.taskRepo.UpsertTasks(envUUID, rows); err != nil {
		return err
	}
	// Pruning on the one path that grows the table means there is no separate loop to
	// run, own or forget -- but rate-limited, since that path is every environment's
	// 10s heartbeat.
	if s.claimPrune() {
		if n, err := s.taskRepo.PruneTerminal(repository.TaskRetention); err != nil {
			slog.Warn("prune aged amp tasks", "error", err)
		} else if n > 0 {
			slog.Info("pruned aged amp tasks", "rows", n, "retention", repository.TaskRetention)
		}
	}
	return nil
}

// claimPrune reports whether this caller should run the retention pass, and marks it
// claimed if so. Per process, not per fleet: two api-server replicas may each run one,
// which is harmless -- the delete is idempotent and the second finds nothing.
func (s *EnvironmentService) claimPrune() bool {
	s.pruneMu.Lock()
	defer s.pruneMu.Unlock()
	now := time.Now()
	// The zero value claims immediately, so a freshly started control plane clears
	// whatever aged out while it was down rather than waiting an hour first.
	if !s.lastPruned.IsZero() && now.Sub(s.lastPruned) < pruneEvery {
		return false
	}
	s.lastPruned = now
	return true
}

// Tasks returns one page of an environment's relayed amp task history. The caller
// resolves the environment first, so this takes the row's uuid and never an id.
func (s *EnvironmentService) Tasks(envUUID string, opts repository.EnvironmentTaskListOpts) (repository.EnvironmentTaskListResult, error) {
	if s.taskRepo == nil {
		return repository.EnvironmentTaskListResult{Items: []models.EnvironmentTask{}}, nil
	}
	return s.taskRepo.ListTasks(envUUID, opts)
}

// Task returns one relayed task of an environment, or nil when there is none.
func (s *EnvironmentService) Task(envUUID, taskID string) (*models.EnvironmentTask, error) {
	if s.taskRepo == nil {
		return nil, nil
	}
	return s.taskRepo.GetTask(envUUID, taskID)
}

// SetState moves an environment to an agent-reported bootstrap phase. Distinct
// from Action, which applies operator intent from a fixed set.
func (s *EnvironmentService) SetState(envUUID, state string) (bool, error) {
	// BootstrapProgress.Phase is typed EnvState on the wire and required by the
	// spec, so a phase is a state and gets the same check: present or rejected,
	// unknown tolerated.
	phase, err := normalizeState(state)
	if err != nil {
		return false, err
	}
	e, err := s.repo.GetByUUID(envUUID)
	if err != nil || e == nil {
		return false, err
	}
	e.State = phase
	return true, s.repo.Update(e)
}

// Entitlement is the plan the control plane reports. Static until a billing source
// is wired; kept here so the handler has no policy in it.
func (s *EnvironmentService) Entitlement() models.Entitlement {
	return models.Entitlement{Plan: "enterprise", Source: "ybm-aeon"}
}

// resourceKinds is the set of resource kinds each environment kind's agent knows how
// to converge: mekobackend's and ampbackend's, duplicated here because ycp is a
// separate Go module the api_server must not import (models.DesiredResource is the
// same story). **A kind added to a backend has to be added here too** — until it is,
// declaring one is refused, which is the failure direction to prefer over silently
// storing a resource nothing converges.
var resourceKinds = map[string][]string{
	KindMeko: {"agent", "datapack"},
	// "controller" and "fleet-version" are the odd ones out: they move software
	// rather than resources — the version the amp controller release runs, and the
	// version its endpoints are rolled onto.
	KindAmp: {"storage-pool", "cluster", "branch", "endpoint", "controller", "fleet-version"},
}

// logFleetWithoutController reports a full desired set that declares a
// fleet-version but no controller. Not refused, because nothing real declares one:
// the fleet can only reach a version the controller deploys, so the agent would
// wait on a controller nobody manages, and this log is the only trace of why.
func logFleetWithoutController(envID string, res []models.DesiredResource) {
	fleet, ctl := false, false
	for _, r := range res {
		fleet = fleet || r.Kind == "fleet-version"
		ctl = ctl || r.Kind == "controller"
	}
	if fleet && !ctl {
		slog.Error("desired state declares a fleet-version without a controller; the agent will wait on it indefinitely",
			"env", envID)
	}
}

// checkKinds refuses a declared set naming a kind this environment's agent does not
// serve. Without it a typo is stored, shipped to the agent, and comes back as
// permanent drift a tick later — a slow, indirect way to learn about a misspelling
// the control plane could have caught at declare time.
//
// Only the kind is checked. Name shape is the backend's rule (mekobackend requires
// "<agent>/<datapack>", ampbackend its own paths) and duplicating it here would give
// two places to drift, with the control plane's copy able to refuse a declaration the
// agent would have accepted.
func checkKinds(envKind string, res []models.DesiredResource) error {
	known, ok := resourceKinds[envKind]
	if !ok {
		return nil // an environment kind with no list yet: let the backend judge
	}
	for i, r := range res {
		if slices.Contains(known, r.Kind) {
			continue
		}
		return fmt.Errorf("%w: resources[%d] has kind %q, which a %s environment does not serve (want one of %s)",
			ErrValidation, i, r.Kind, envKind, strings.Join(known, ", "))
	}
	return nil
}

// SetDesired replaces the desired resource set. ok=false ⇒ unknown environment,
// or one the scope does not own.
func (s *EnvironmentService) SetDesired(envID string, scope AccountScope, res []models.DesiredResource) (bool, error) {
	e, err := s.Get(envID, scope)
	if err != nil || e == nil {
		return false, err
	}
	if err := checkKinds(e.Kind, res); err != nil {
		return true, err
	}
	logFleetWithoutController(e.UUID, res)
	return true, s.repo.ReplaceDesired(e.UUID, res)
}

// Desired returns the environment's desired resources, by key -- callers have
// already resolved the environment, whether by scope or by api key id.
func (s *EnvironmentService) Desired(envUUID string) ([]models.DesiredResource, error) {
	return s.repo.ListDesired(envUUID)
}

// listResourcesForEnv filters an already-resolved environment's resources into two
// lists: the declared desired set (what an operator asked for) and the agent-reported
// observed inventory (what the data plane last reported). A caller diffs the two to
// see what has yet to converge.
//
// Both lists are filtered by the same rule: keep an entry when its kind matches (or
// kind is unset) AND its full hierarchical path equals name (or name is unset), or —
// with subtree — the path is name or a descendant of it. The two lists spell the
// path differently, which resourcePath reconciles: a desired row's Name is already
// the full path, while an observed row's Name is only the leaf and the path lives in
// its ID as "<kind>/<path>".
func (s *EnvironmentService) listResourcesForEnv(
	e *models.Environment, kind, name string, subtree bool,
) (desired []models.DesiredResource, observed []ResourceStatus, err error) {
	all, err := s.repo.ListDesired(e.UUID)
	if err != nil {
		return nil, nil, err
	}
	desired = []models.DesiredResource{}
	for _, d := range all {
		if matchResource(kind, name, subtree, d.Kind, d.Name) {
			desired = append(desired, d)
		}
	}

	observed = []ResourceStatus{}
	if len(bytes.TrimSpace(e.Resources)) > 0 {
		var obs []ResourceStatus
		if uerr := json.Unmarshal(e.Resources, &obs); uerr != nil {
			return desired, nil, fmt.Errorf("decode observed resources: %w", uerr)
		}
		for _, o := range obs {
			if matchResource(kind, name, subtree, o.Kind, resourcePath(o.Kind, o.ID)) {
				observed = append(observed, o)
			}
		}
	}
	return desired, observed, nil
}

// resourcePath returns a resource's full hierarchical path. A desired row's Name is
// already the path; an observed ResourceStatus carries the leaf in Name and the path
// in ID as "<kind>/<path>".
func resourcePath(kind, id string) string {
	return strings.TrimPrefix(id, kind+"/")
}

// matchResource applies the kind/name/subtree filter to one resource identified by
// its kind and full path.
func matchResource(filterKind, filterName string, subtree bool, kind, path string) bool {
	if filterKind != "" && kind != filterKind {
		return false
	}
	if filterName == "" {
		return true
	}
	if path == filterName {
		return true
	}
	return subtree && strings.HasPrefix(path, filterName+"/")
}

// validateResourcePatches checks a batch before any write: every spec that is not a
// delete must be a JSON object (the shallow-merge target), and no kind may be one the
// environment's agent does not serve. It returns ErrValidation on the first problem,
// so a caller runs it before opening the transaction and the batch stays
// all-or-nothing.
func validateResourcePatches(envKind string, patches []models.ResourcePatch) error {
	check := make([]models.DesiredResource, len(patches))
	for i, p := range patches {
		check[i] = models.DesiredResource{Kind: p.Kind, Name: p.Name}
		if !p.Delete && len(bytes.TrimSpace(p.Spec)) > 0 {
			var probe map[string]json.RawMessage
			if json.Unmarshal(p.Spec, &probe) != nil {
				return fmt.Errorf("%w: patches[%d] spec must be a JSON object", ErrValidation, i)
			}
		}
	}
	return checkKinds(envKind, check)
}

// createOnlyDefault returns a copy of regions in which every non-delete patch carrying
// no explicit intent flag is marked Create. On a shared MANAGED pool a write is
// create-only by default, so a flagless PATCH cannot silently overwrite an existing
// resource; the caller opts into a merge with update:true. The copy is deep enough that
// the caller's backing arrays are untouched. The repository's own synthesized
// storage-pool prepend runs after this and stays a plain upsert, so first touch never
// trips the create guard.
func createOnlyDefault(regions []models.RegionResources) []models.RegionResources {
	out := make([]models.RegionResources, len(regions))
	for ri, rr := range regions {
		res := make([]models.ResourcePatch, len(rr.Resources))
		copy(res, rr.Resources)
		for pi := range res {
			p := &res[pi]
			if !p.Delete && !p.Create && !p.Update {
				p.Create = true
			}
		}
		out[ri] = models.RegionResources{Region: rr.Region, Resources: res}
	}
	return out
}

// ErrNoManagedEnv re-exports the repository's, so handlers map it to a 404 without
// importing the storage layer. Raised when a region names no READY MANAGED
// environment for a storage pool to pin to.
var ErrNoManagedEnv = repository.ErrNoEligibleEnvironment

// ErrResourceExists, ErrResourceNotFound and ErrClusterNameInUse re-export the
// repository's create/update guard sentinels so handlers map them to 409/404 without
// importing the storage layer.
var (
	ErrResourceExists   = repository.ErrResourceExists
	ErrResourceNotFound = repository.ErrResourceNotFound
	ErrClusterNameInUse = repository.ErrClusterNameInUse
)

// ErrConcurrentPoolUpdate re-exports the repository's retryable-deadlock sentinel, so
// handlers map a lost concurrent-write race to a retryable status without importing the
// storage layer.
var ErrConcurrentPoolUpdate = repository.ErrConcurrentPoolUpdate

// EnvironmentResources is one environment's slice of a region-addressed response:
// which environment answered (its operator-facing id and region) and the storage
// pool's desired and observed resources there. Both SetResourcesByStoragePool and
// ListResourcesByName return a slice of these, so a PATCH reads back like a LIST.
type EnvironmentResources struct {
	EnvironmentID string                   `json:"environment_id"`
	Region        string                   `json:"region"`
	Desired       []models.DesiredResource `json:"desired"`
	Observed      []ResourceStatus         `json:"observed"`
}

// SetResourcesByStoragePool applies desired-state changes to a storage pool addressed
// by region, never by environment id. Each region resolves to one MANAGED environment
// through the sticky mapping (first call pins a READY one, later calls reuse it), and
// every patch across every region is applied in ONE transaction — all-or-nothing.
//
// The whole call is validated before the transaction opens: the storage pool must be a
// UUID (an aeon account id), kinds against the amp set (region APIs serve MANAGED amp
// resources), and every name confined to the <storage_pool> subtree. The storage-pool
// row itself is auto-created on first touch with an owner derived from the pool name,
// so a first child patch has a parent. The response mirrors the LIST: the pool's
// resources per environment touched.
func (s *EnvironmentService) SetResourcesByStoragePool(
	storagePool string, regions []models.RegionResources,
) ([]EnvironmentResources, error) {
	storagePool = strings.TrimSpace(storagePool)
	if storagePool == "" {
		return nil, fmt.Errorf("%w: storage_pool is required", ErrValidation)
	}
	// A MANAGED storage pool is addressed by an aeon account id, which is a UUID; the
	// synthesized pool's owner is derived from it (deriveOwner). Reject a non-UUID here
	// rather than mint a UUIDv5 owner from an arbitrary string. PATCH-only: the LIST
	// side stays lenient so an operator can probe any name.
	if _, err := uuid.Parse(storagePool); err != nil {
		return nil, fmt.Errorf("%w: storage_pool %q must be a UUID", ErrValidation, storagePool)
	}
	if len(regions) == 0 {
		return nil, fmt.Errorf("%w: at least one region is required", ErrValidation)
	}
	for ri, rr := range regions {
		if strings.TrimSpace(rr.Region) == "" {
			return nil, fmt.Errorf("%w: regions[%d].region is required", ErrValidation, ri)
		}
		if len(rr.Resources) == 0 {
			return nil, fmt.Errorf("%w: regions[%d] has no resources", ErrValidation, ri)
		}
		if err := validateResourcePatches(KindAmp, rr.Resources); err != nil {
			return nil, fmt.Errorf("regions[%d]: %w", ri, err)
		}
		for pi, p := range rr.Resources {
			if !underStoragePool(storagePool, p.Name) {
				return nil, fmt.Errorf("%w: regions[%d].resources[%d] name %q is not under storage pool %q",
					ErrValidation, ri, pi, p.Name, storagePool)
			}
		}
	}
	poolSpec, err := deriveStoragePoolSpec(storagePool)
	if err != nil {
		return nil, err
	}
	assignments, err := s.repo.ApplyStoragePoolPatches(storagePool, createOnlyDefault(regions), poolSpec)
	if err != nil {
		return nil, err
	}
	// Read back the whole pool subtree per environment so a PATCH answers like a LIST.
	return s.resourcesForAssignments(assignments, "", storagePool, true)
}

// ListResourcesByName returns a storage pool's resources across every MANAGED
// environment it is mapped to. The storage pool is the first path segment of name, so
// the endpoint stays generic across kinds. It never assigns: a pool nothing is mapped
// to returns an empty slice.
func (s *EnvironmentService) ListResourcesByName(
	name, kind string, subtree bool,
) ([]EnvironmentResources, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrValidation)
	}
	assignments, err := s.repo.ListStoragePoolEnvs(storagePoolFromName(name))
	if err != nil {
		return nil, err
	}
	return s.resourcesForAssignments(assignments, kind, name, subtree)
}

// resourcesForAssignments reads each resolved environment back and builds its
// desired+observed lists, filtered by the same kind/name/subtree rule the by-env list
// used. Reads run after the PATCH's transaction has committed, so the response is a
// fresh snapshot rather than the in-transaction state.
func (s *EnvironmentService) resourcesForAssignments(
	assignments []repository.StoragePoolAssignment, kind, name string, subtree bool,
) ([]EnvironmentResources, error) {
	out := []EnvironmentResources{}
	for _, a := range assignments {
		e, err := s.repo.GetByUUID(a.EnvUUID)
		if err != nil {
			return nil, err
		}
		if e == nil {
			continue // raced with a delete; skip rather than fail the whole response
		}
		desired, observed, err := s.listResourcesForEnv(e, kind, name, subtree)
		if err != nil {
			return nil, err
		}
		out = append(out, EnvironmentResources{
			EnvironmentID: e.ID,
			Region:        a.Region,
			Desired:       desired,
			Observed:      observed,
		})
	}
	return out, nil
}

// storagePoolFromName is the first path segment of a resource name — the storage pool
// a full path (pool/cluster/branch) belongs to.
func storagePoolFromName(name string) string {
	return strings.SplitN(strings.TrimSpace(name), "/", 2)[0]
}

// underStoragePool reports whether a resource name is the storage pool itself or a
// descendant of it, the scope every region PATCH name must satisfy.
func underStoragePool(pool, name string) bool {
	name = strings.TrimSpace(name)
	return name == pool || strings.HasPrefix(name, pool+"/")
}

// deriveStoragePoolSpec is the auto-created storage-pool spec: a pool needs an owner,
// but users are not YCP desired state, so the owner is derived from the pool name.
func deriveStoragePoolSpec(pool string) ([]byte, error) {
	return json.Marshal(map[string]string{"owner_user_id": deriveOwner(pool).String()})
}

// deriveOwner turns a storage pool name into a stable owner user id: the name parsed
// as a UUID when it already is one, otherwise a deterministic UUIDv5 of the name.
// Deterministic either way, so re-touching the same pool derives the same owner.
func deriveOwner(pool string) uuid.UUID {
	if id, err := uuid.Parse(pool); err == nil {
		return id
	}
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(pool))
}

// Action applies a lifecycle action (suspend|resume|drain|deregister) and returns
// the environment it acted on. nil ⇒ unknown environment, or one the scope does
// not own. The environment comes back rather than a bool because deregistration's
// response carries the api_key_id binding, so YBM can revoke the key.
func (s *EnvironmentService) Action(envID string, scope AccountScope, action string) (*models.Environment, error) {
	e, err := s.Get(envID, scope)
	if err != nil || e == nil {
		return nil, err
	}
	switch action {
	case "suspend":
		e.State = StateSuspended
	case "resume":
		e.State = StateReady
	case "drain":
		e.State = StateDraining
	case "deregister":
		e.State = StateDeregistered
	default:
		return nil, fmt.Errorf("%w: unknown action %q", ErrValidation, action)
	}
	return e, s.repo.Update(e)
}

func mergeFacts(e *models.Environment, f ClusterFacts) {
	if f.Region != "" {
		e.Region = f.Region
	}
	if f.Provider != "" {
		e.Provider = f.Provider
	}
	if f.K8sVersion != "" {
		e.K8sVersion = f.K8sVersion
	}
	if f.NodeCount > 0 {
		e.NodeCount = f.NodeCount
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
