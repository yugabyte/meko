// Package store is the YCP fleet-registry persistence layer.
// Two implementations back the same interface: an in-memory store (dev fallback)
// and a Postgres-backed store (durable state).
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"meko/ycp/internal/api"
)

// Store is the fleet registry. Methods return errors so a real (Postgres) backend
// can surface failures; the in-memory impl always returns nil.
type Store interface {
	List() ([]*api.Environment, error)
	Get(id string) (*api.Environment, bool, error)
	// Create persists a new admin-registered environment (already carries its token).
	Create(e *api.Environment) error
	// Action applies a lifecycle action (suspend|resume|drain|deregister).
	Action(id, action string) (bool, error)
	// SetState forces an environment's state (used by bootstrap-progress).
	SetState(id string, state api.EnvState) (bool, error)
	// EnrollWithToken validates a single-use enrollment token, consumes it, mints an
	// env-scoped status token (returned in plaintext once), merges facts, and returns
	// the environment. ok=false means the token was invalid or already consumed.
	EnrollWithToken(tokenPlain, placement string, f api.ClusterFacts) (e *api.Environment, statusToken string, ok bool, err error)
	// ValidateStatusToken reports whether tokenPlain is the env's current status token.
	ValidateStatusToken(envID, tokenPlain string) (bool, error)
	// SeedEnrollment creates a PENDING environment carrying an enrollment token hash,
	// if it does not already exist (used to pre-register the demo agent's environment).
	SeedEnrollment(envID, placement, tokenPlain string) error
	// SetDesiredResources replaces the generic desired-state resource set for an
	// environment (#2). Resources are typed by Kind; the agent dispatches each to
	// a reconciler for that Kind.
	SetDesiredResources(envID string, res []api.DesiredResource) (bool, error)
	// DesiredResources returns the desired-state resources for an environment.
	DesiredResources(envID string) ([]api.DesiredResource, error)
	// ApplyStatus merges an agent status report (state, facts, components, datapacks).
	ApplyStatus(id string, r api.StatusReport) (bool, error)
	// Datapacks lists datapacks, optionally filtered to one environment.
	Datapacks(envFilter string) ([]map[string]any, error)
	// Revision is the current desired-state revision.
	Revision() (int, error)
	// SeedIfEmpty inserts the demo fleet when the registry is empty.
	SeedIfEmpty() error
	Close() error
}

// RandHex returns n random bytes hex-encoded.
func RandHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Slug normalizes a name into an environment id.
func Slug(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	return strings.NewReplacer(" ", "-", "_", "-", "/", "-").Replace(s)
}

// EnrollTokenTTL is how long a minted enrollment token stays valid. It is reusable
// within this window (so agent restarts can re-enroll); after it, a new token is minted.
const EnrollTokenTTL = 30 * time.Minute

// HashToken returns the hex SHA-256 of a token — only the hash is ever stored.
func HashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// NewToken mints a random token with the given prefix and returns (plaintext, hash).
func NewToken(prefix string) (plain, hash string) {
	plain = prefix + RandHex(16)
	return plain, HashToken(plain)
}

// seedEnvs is the demo fleet used when the registry starts empty.
func seedEnvs(now time.Time) []*api.Environment {
	base := []api.Environment{
		{ID: "acme-prod", Region: "us-east-1", Placement: "cloud", State: api.StateReady, Datapacks: 312, MaxDatapacks: 400, AgentVersion: "v1.4.2", Provider: "aws", K8sVersion: "1.30", NodeCount: 12},
		{ID: "globex-eu", Region: "eu-west-1", Placement: "cloud", State: api.StateReady, Datapacks: 204, MaxDatapacks: 400, AgentVersion: "v1.4.2", Provider: "aws", K8sVersion: "1.30", NodeCount: 8},
		{ID: "initech-onprem", Region: "on-prem / dc-1", Placement: "onprem", State: api.StateDegraded, Datapacks: 88, MaxDatapacks: 200, AgentVersion: "v1.4.0", Provider: "self-managed", K8sVersion: "1.29", NodeCount: 6},
		{ID: "stark-gov", Region: "on-prem / gov-1", Placement: "onprem", State: api.StateSuspended, Datapacks: 41, MaxDatapacks: 120, AgentVersion: "v1.3.9", Provider: "self-managed", K8sVersion: "1.28", NodeCount: 4},
	}
	out := make([]*api.Environment, 0, len(base))
	for i := range base {
		e := base[i]
		e.CreatedAt = now
		hb := now.Add(-time.Duration(8+i*3) * time.Second)
		e.LastHeartbeatAt = &hb
		e.MCPBaseURL = e.ID + ".mcp.customer-domain"
		e.CertFingerprint = "sha256:" + RandHex(6)
		out = append(out, &e)
	}
	return out
}

// mergeFacts applies enroll/status cluster facts onto an environment.
func mergeFacts(e *api.Environment, f api.ClusterFacts) {
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
