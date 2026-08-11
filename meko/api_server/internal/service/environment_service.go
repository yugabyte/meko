package service

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

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

// enrollTokenTTL is how long a minted enrollment token stays valid. It is reusable
// within this window (kubeadm-style bootstrap token) so agent restarts can re-enroll.
const enrollTokenTTL = 30 * time.Minute

// EnvironmentService owns the YCP fleet-registry logic: enrollment-token
// handshake, status ingestion, and generic desired-state. Control-plane mode only.
type EnvironmentService struct {
	repo      *repository.EnvironmentRepo
	bootstrap BootstrapDefaults
}

// BootstrapDefaults is the stack a newly registered environment is told to run, so
// that an agent bootstraps Meko as soon as it enrolls rather than sitting idle until
// somebody remembers to send it a command.
//
// Registering an environment is already a statement of intent — nobody registers a
// data plane in order to run nothing in it — so the default desired state is written
// at registration. A caller can still override it per request, and an operator can
// replace it at any time through POST /environments/{id}/desired.
//
// What gets seeded is the whole Meko data plane, as one `meko-stack` resource: the
// MAGE-enabled YugabyteDB with the mage and pg_dist_rag extensions, Meko's own
// api-server over it, the MCP server, the RAG worker, the inference gateway and the
// web UI. The images come from the pinned release in dataplane_release.go, because the
// control plane is what knows which versions go together — see the comment there.
type BootstrapDefaults struct {
	// Enabled is false when --no-bootstrap-defaults is set: registration then seeds
	// nothing and every environment waits for an explicit command.
	Enabled bool
	// Release is the image set the seeded stack runs.
	Release DataPlaneRelease
	// ImagePullSecret and SecretRef are Secret NAMES in the agent's namespace: the
	// registry credentials, and the operator's own Secret holding OPENAI_API_KEY, the
	// AWS keys and the Cognito client secret. The control plane never learns either
	// value — desired state carries no secret material.
	ImagePullSecret string
	SecretRef       string
}

// mekoStackSpec mirrors the agent's kind "meko-stack" spec (ycp/cmd/agent/mekostack.go).
// Only the fields the control plane sets are here; the agent defaults the rest, so an
// older control plane and a newer agent still agree.
//
// Not shared as a package: ycp is a separate Go module, and the wire types between the
// two sides are duplicated rather than imported throughout (models.DesiredResource is
// the same story). The JSON tags are the contract.
type mekoStackSpec struct {
	Images          DataPlaneRelease `json:"images"`
	ImagePullSecret string           `json:"image_pull_secret,omitempty"`
	SecretRef       string           `json:"secret_ref,omitempty"`
}

// NewEnvironmentService constructs the service.
func NewEnvironmentService(repo *repository.EnvironmentRepo, bootstrap BootstrapDefaults) *EnvironmentService {
	return &EnvironmentService{repo: repo, bootstrap: bootstrap}
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

// RegisterEnvironmentRequest is the console's register-environment body.
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

	// Desired overrides the desired state this environment starts with. An explicit
	// empty array registers an environment that runs nothing until commanded; omitted,
	// the server's bootstrap defaults apply.
	Desired *[]models.DesiredResource `json:"desired,omitempty"`
}

// EnrollRequest is the agent's enrollment body.
type EnrollRequest struct {
	Name            string `json:"name"`
	EnrollmentToken string `json:"enrollment_token"`
	Placement       string `json:"placement"`
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
	Kind   string            `json:"kind"`
	ID     string            `json:"id"`
	Name   string            `json:"name,omitempty"`
	Parent string            `json:"parent,omitempty"`
	State  string            `json:"state,omitempty"`
	Attrs  map[string]string `json:"attrs,omitempty"`
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
}

// ---- token helpers ----

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

func newToken(prefix string) (plain, hash string) {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	plain = prefix + hex.EncodeToString(b)
	return plain, hashToken(plain)
}

func slug(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	return strings.NewReplacer(" ", "-", "_", "-", "/", "-").Replace(s)
}

// ---- operations ----

// List returns every environment.
func (s *EnvironmentService) List() ([]*models.Environment, error) { return s.repo.List() }

// Get returns one environment, or (nil, nil) if absent.
func (s *EnvironmentService) Get(id string) (*models.Environment, error) { return s.repo.Get(id) }

// Register mints a single-use enrollment token and stores a PENDING environment,
// returning the plaintext token once.
func (s *EnvironmentService) Register(req RegisterEnvironmentRequest) (*models.Environment, string, error) {
	name := strings.TrimSpace(req.Name)
	if len(name) < 3 || len(name) > 60 {
		return nil, "", fmt.Errorf("%w: name must be 3-60 chars", ErrValidation)
	}
	kind, err := normalizeKind(req.Kind)
	if err != nil {
		return nil, "", err
	}
	placement := req.Placement
	if placement == "" {
		placement = "cloud"
	}
	state := "PENDING"
	if req.ClusterSpec != nil {
		state = "CLUSTER_PROVISIONING"
	}
	plain, hash := newToken("ycp_enr_")
	exp := time.Now().UTC().Add(enrollTokenTTL)
	// A meko environment keeps a bare slug id — that is the id the existing meko
	// agent, e2e, and console already use, so prefixing it would silently break
	// them. An amp environment is prefixed "amp-", so an amp and a meko
	// environment of the same name never collide and the kind is legible from the
	// id. The kind column disambiguates regardless of the id shape.
	id := slug(name)
	if kind == KindAmp {
		id = KindAmp + "-" + id
	}
	e := &models.Environment{
		ID: id, Kind: kind, AccountID: req.AccountID, Placement: placement, Region: req.Region,
		State: state, MaxDatapacks: 400, CreatedAt: time.Now().UTC(),
		Components: json.RawMessage("[]"), Resources: json.RawMessage("[]"),
		EnrollTokenHash: hash, EnrollTokenExpiresAt: &exp,
	}
	if err := s.repo.Upsert(e); err != nil {
		return nil, "", err
	}
	// Desired state comes with the registration, so the agent has something to do the
	// moment it enrolls. Failing to write it must not fail the registration: the
	// enrollment token has already been minted and is returned exactly once, so
	// discarding it here would leave an environment nobody can attach an agent to.
	// A desired-state write can always be retried; a token cannot.
	if res := s.initialDesired(req, kind); res != nil {
		if err := s.repo.ReplaceDesired(e.ID, res); err != nil {
			return e, plain, nil //nolint:nilerr // see above: the token must survive
		}
	}
	return e, plain, nil
}

// initialDesired resolves what a newly registered environment should be running:
// whatever the caller asked for, else the server's bootstrap defaults, else nothing.
func (s *EnvironmentService) initialDesired(req RegisterEnvironmentRequest, kind string) []models.DesiredResource {
	if req.Desired != nil {
		return *req.Desired
	}
	// The meko-stack bootstrap default is meko's data plane; it is meaningless to
	// an amp environment, which is a running amp controller reporting the storage
	// pools / databases / branches / endpoints it already owns. An amp environment
	// therefore seeds nothing (the reconcile loop that would push amp desired state
	// is a later phase) unless the caller passed an explicit `desired`.
	if kind == KindAmp {
		return nil
	}
	if !s.bootstrap.Enabled {
		return nil
	}
	// One resource, because the stack is one thing. The agent orders the components
	// within it and knows which of them depends on which — that ordering is a property
	// of the data plane, not of a control-plane payload, and six resources here would
	// mean six specs that all have to keep agreeing about the same database, registry
	// and Secrets.
	spec, err := json.Marshal(mekoStackSpec{
		Images:          s.bootstrap.Release,
		ImagePullSecret: s.bootstrap.ImagePullSecret,
		SecretRef:       s.bootstrap.SecretRef,
	})
	if err != nil {
		return nil
	}
	// No datapack: a datapack is a customer's own artifact, created through the Meko
	// api-server this stack runs, once it is up. Seeding a placeholder would provision
	// one nobody asked for and then need explaining.
	return []models.DesiredResource{{Kind: "meko-stack", Name: "meko", Spec: spec}}
}

// EnrollWithToken validates and consumes an enrollment token, mints a status token,
// merges facts, and returns the environment. ok=false ⇒ invalid/consumed token.
//
// declaredKind is the kind the enrolling agent believes it is; empty is treated
// as meko. It must match the kind the environment was registered with, so an amp
// controller cannot attach to a meko environment (or vice versa) and start
// reporting or reconciling the wrong thing — a mismatch returns ErrKindMismatch.
func (s *EnvironmentService) EnrollWithToken(tokenPlain, placement, declaredKind string, f ClusterFacts) (*models.Environment, string, bool, error) {
	if tokenPlain == "" {
		return nil, "", false, fmt.Errorf("%w: enrollment_token required", ErrValidation)
	}
	kind, err := normalizeKind(declaredKind)
	if err != nil {
		return nil, "", false, err
	}
	id, found, err := s.repo.EnrollLookup(hashToken(tokenPlain))
	if err != nil || !found {
		return nil, "", false, err
	}
	e, err := s.repo.Get(id)
	if err != nil || e == nil {
		return nil, "", false, err
	}
	envKind := e.Kind
	if envKind == "" {
		envKind = KindMeko
	}
	if kind != envKind {
		return nil, "", false, fmt.Errorf("%w: environment %q is kind %q, agent declared %q", ErrKindMismatch, e.ID, envKind, kind)
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
	statusPlain, statusHash := newToken("ycp_sts_")
	e.StatusTokenHash = statusHash
	// The enrollment token is deliberately NOT burned here, even though the agent
	// now persists the status token and so does not need to enroll again in normal
	// operation.
	//
	// Burning it makes every failure mode permanent instead of TTL-bounded:
	// EnrollLookup requires a non-empty hash, so once cleared the agent's
	// re-enroll-on-401 path can never succeed, and an agent that failed to persist
	// its state — or whose status token was rotated or invalidated — is locked out
	// with no recovery short of re-registering the environment, which resets its
	// state and datapack counts. Leaving it reusable for the rest of its 30-minute
	// window keeps that window as a genuine recovery budget.
	//
	// Single-use is still the right end state, but it needs a re-issue path first
	// (an endpoint that mints a fresh enrollment token for a known environment, or
	// status-token rotation authenticated by the current token). Until then the
	// TTL is the only bound, which is what it was designed to be.
	if err := s.repo.Upsert(e); err != nil {
		return nil, "", false, err
	}
	return e, statusPlain, true, nil
}

// ValidateStatusToken reports whether tokenPlain is envID's current status token.
func (s *EnvironmentService) ValidateStatusToken(envID, tokenPlain string) (bool, error) {
	e, err := s.repo.Get(envID)
	if err != nil || e == nil || e.StatusTokenHash == "" {
		return false, err
	}
	return e.StatusTokenHash == hashToken(tokenPlain), nil
}

// ApplyStatus merges an agent status report. ok=false ⇒ unknown environment.
func (s *EnvironmentService) ApplyStatus(envID string, r StatusReport) (bool, error) {
	e, err := s.repo.Get(envID)
	if err != nil || e == nil {
		return false, err
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
	if err := s.repo.Upsert(e); err != nil {
		return false, err
	}
	return true, nil
}

// SetState moves an environment to an agent-reported bootstrap phase. Distinct
// from Action, which applies operator intent from a fixed set.
func (s *EnvironmentService) SetState(envID, state string) (bool, error) {
	e, err := s.repo.Get(envID)
	if err != nil || e == nil {
		return false, err
	}
	e.State = state
	return true, s.repo.Upsert(e)
}

// Entitlement is the plan the control plane reports. Static until a billing source
// is wired; kept here so the handler has no policy in it.
func (s *EnvironmentService) Entitlement() models.Entitlement {
	return models.Entitlement{Plan: "enterprise", Source: "ybm-aeon"}
}

// EnrolledStack is the meko-stack spec an enrolling agent is handed: everything it
// needs to deploy the Meko data plane, in the enroll response.
//
// It is read out of the environment's stored resources rather than recomposed from the
// bootstrap defaults, so that the stack an agent is given is the one this environment
// was actually registered with — including a `desired` override passed at registration,
// and any replacement an operator has POSTed since. One source of truth, whichever way
// the agent asks for it.
//
// Returns nil when the environment has no stack: --no-bootstrap-defaults, or a
// registration that declared individual resources instead. The agent then runs whatever
// desired state declares, which is the behaviour that predates this.
func (s *EnvironmentService) EnrolledStack(envID string) (json.RawMessage, error) {
	res, err := s.repo.ListDesired(envID)
	if err != nil {
		return nil, err
	}
	for _, r := range res {
		if r.Kind == "meko-stack" {
			return r.Spec, nil
		}
	}
	return nil, nil
}

// SetDesired replaces the desired resource set. ok=false ⇒ unknown environment.
func (s *EnvironmentService) SetDesired(envID string, res []models.DesiredResource) (bool, error) {
	e, err := s.repo.Get(envID)
	if err != nil || e == nil {
		return false, err
	}
	return true, s.repo.ReplaceDesired(envID, res)
}

// Desired returns the environment's desired resources.
func (s *EnvironmentService) Desired(envID string) ([]models.DesiredResource, error) {
	return s.repo.ListDesired(envID)
}

// Action applies a lifecycle action (suspend|resume|drain|deregister).
func (s *EnvironmentService) Action(envID, action string) (bool, error) {
	e, err := s.repo.Get(envID)
	if err != nil || e == nil {
		return false, err
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
		return false, fmt.Errorf("%w: unknown action %q", ErrValidation, action)
	}
	return true, s.repo.Upsert(e)
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
