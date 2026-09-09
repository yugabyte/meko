package service

import (
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
// by the datapack/stack agent. The kind is fixed at registration and baked into
// the id so both can coexist in one fleet.
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

// EnvironmentService owns the YCP fleet-registry logic: the api-key identity
// binding, status ingestion, and generic desired-state. Control-plane mode only.
type EnvironmentService struct {
	repo     *repository.EnvironmentRepo
	taskRepo *repository.EnvironmentTaskRepo

	// pruneMu guards lastPruned, which rate-limits the fleet-wide task-retention
	// delete. Every environment's agent heartbeats every 10s, so pruning on each
	// report that carried a task would run one fleet-wide DELETE per environment per
	// tick to delete nothing almost every time.
	pruneMu    sync.Mutex
	lastPruned time.Time
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
	// empty defaults to "meko". It fixes what the environment runs and is baked
	// into the returned id.
	Kind        string       `json:"kind,omitempty"`
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
	// Checked before the insert, not after: a declaration this environment's agent
	// cannot converge should fail the registration outright rather than leave a
	// registered environment whose desired state was quietly dropped.
	if req.Desired != nil {
		if err := checkKinds(kind, *req.Desired); err != nil {
			return nil, err
		}
	}
	placement := req.Placement
	if placement == "" {
		placement = "cloud"
	}
	state := "PENDING"
	if req.ClusterSpec != nil {
		state = "CLUSTER_PROVISIONING"
	}
	// A meko environment keeps a bare slug id — that is the id the existing meko
	// agent, e2e, and console already use, so prefixing it would silently break
	// them. An amp environment is prefixed "amp-", so an amp and a meko
	// environment of the same name never collide and the kind is legible from the
	// id. The kind column disambiguates regardless of the id shape.
	id := slug(name)
	if kind == KindAmp {
		id = KindAmp + "-" + id
	}
	e = &models.Environment{
		ID: id, Kind: kind, AccountID: accountID, Placement: placement, Region: req.Region,
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
func (s *EnvironmentService) EnrollByKey(apiKeyID, placement, declaredKind string, f ClusterFacts) (*models.Environment, error) {
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
	e.Live = true
	if placement != "" {
		e.Placement = placement
	}
	mergeFacts(e, f)
	e.State = "ATTESTING"
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
	e, err := s.repo.GetByUUID(envUUID)
	if err != nil || e == nil {
		return StatusResult{}, err
	}
	now := time.Now().UTC()
	e.State = r.EnvironmentState
	e.AgentVersion = r.AgentVersion
	e.LastHeartbeatAt = &now
	mergeFacts(e, r.ClusterFacts)
	if comps, err := json.Marshal(r.Components); err == nil {
		e.Components = comps
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
	e, err := s.repo.GetByUUID(envUUID)
	if err != nil || e == nil {
		return false, err
	}
	e.State = state
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
	KindAmp:  {"storage-pool", "cluster", "branch", "endpoint"},
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
	return true, s.repo.ReplaceDesired(e.UUID, res)
}

// Desired returns the environment's desired resources, by key -- callers have
// already resolved the environment, whether by scope or by api key id.
func (s *EnvironmentService) Desired(envUUID string) ([]models.DesiredResource, error) {
	return s.repo.ListDesired(envUUID)
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
		e.State = "SUSPENDED"
	case "resume":
		e.State = "READY"
	case "drain":
		e.State = "DRAINING"
	case "deregister":
		e.State = "DEREGISTERED"
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
