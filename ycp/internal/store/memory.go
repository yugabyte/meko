package store

import (
	"sort"
	"sync"
	"time"

	"meko/ycp/internal/api"
)

// Memory is the in-memory Store (dev fallback when no database is configured).
type Memory struct {
	mu        sync.RWMutex
	envs      map[string]*api.Environment
	datapacks map[string][]api.DatapackPhase
	desired   map[string][]api.DesiredResource
	revision  int
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{envs: map[string]*api.Environment{}, datapacks: map[string][]api.DatapackPhase{}, desired: map[string][]api.DesiredResource{}, revision: 7}
}

func (m *Memory) SeedIfEmpty() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.envs) > 0 {
		return nil
	}
	for _, e := range seedEnvs(time.Now().UTC()) {
		m.envs[e.ID] = e
	}
	return nil
}

func (m *Memory) List() ([]*api.Environment, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*api.Environment, 0, len(m.envs))
	for _, e := range m.envs {
		cp := *e
		out = append(out, &cp)
	}
	sortEnvs(out)
	return out, nil
}

func (m *Memory) Get(id string) (*api.Environment, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.envs[id]
	if !ok {
		return nil, false, nil
	}
	cp := *e
	return &cp, true, nil
}

func (m *Memory) Create(e *api.Environment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.envs[e.ID] = e
	return nil
}

func (m *Memory) Action(id, action string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.envs[id]
	if !ok {
		return false, nil
	}
	applyAction(e, action)
	return true, nil
}

func (m *Memory) SetState(id string, state api.EnvState) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.envs[id]
	if !ok {
		return false, nil
	}
	e.State = state
	return true, nil
}

func (m *Memory) EnrollWithToken(tokenPlain, placement string, f api.ClusterFacts) (*api.Environment, string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := HashToken(tokenPlain)
	now := time.Now()
	var e *api.Environment
	for _, cand := range m.envs {
		if cand.EnrollTokenHash == h && cand.EnrollExpiresAt != nil && now.Before(*cand.EnrollExpiresAt) {
			e = cand
			break
		}
	}
	if e == nil {
		return nil, "", false, nil
	}
	applyEnroll(e, placement, f)
	statusPlain, statusHash := NewToken("mko_sts_")
	e.StatusTokenHash = statusHash
	cp := *e
	return &cp, statusPlain, true, nil
}

func (m *Memory) ValidateStatusToken(envID, tokenPlain string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.envs[envID]
	if !ok || e.StatusTokenHash == "" {
		return false, nil
	}
	return e.StatusTokenHash == HashToken(tokenPlain), nil
}

func (m *Memory) SeedEnrollment(envID, placement, tokenPlain string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	exp := time.Now().Add(EnrollTokenTTL)
	if e, ok := m.envs[envID]; ok {
		// (re)arm the seeded env's token with a fresh TTL so an agent restart can re-enroll.
		e.EnrollTokenHash = HashToken(tokenPlain)
		e.EnrollExpiresAt = &exp
		return nil
	}
	m.envs[envID] = &api.Environment{
		ID: envID, Placement: placement, State: api.StatePending, MaxDatapacks: 400,
		CreatedAt: time.Now().UTC(), EnrollTokenHash: HashToken(tokenPlain), EnrollExpiresAt: &exp,
	}
	return nil
}

func (m *Memory) SetDesiredResources(envID string, res []api.DesiredResource) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.envs[envID]; !ok {
		return false, nil
	}
	m.desired[envID] = res
	return true, nil
}

func (m *Memory) DesiredResources(envID string) ([]api.DesiredResource, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]api.DesiredResource(nil), m.desired[envID]...), nil
}

func (m *Memory) ApplyStatus(id string, r api.StatusReport) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.envs[id]
	if !ok {
		return false, nil
	}
	applyStatus(e, r)
	m.datapacks[id] = r.DatapackPhases
	return true, nil
}

func (m *Memory) Datapacks(envFilter string) ([]map[string]any, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []map[string]any{}
	for envID, dps := range m.datapacks {
		if envFilter != "" && envFilter != envID {
			continue
		}
		for _, d := range dps {
			out = append(out, map[string]any{"id": d.ID, "name": d.Name, "environment_id": envID, "phase": d.Phase, "db": d.DB, "endpoint": d.Endpoint, "error": d.Error})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
	return out, nil
}

func (m *Memory) Revision() (int, error) { return m.revision, nil }
func (m *Memory) Close() error           { return nil }

// ---- shared merge helpers (used by both impls) ----

func sortEnvs(out []*api.Environment) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].Live != out[j].Live {
			return out[i].Live
		}
		return out[i].ID < out[j].ID
	})
}

func applyAction(e *api.Environment, action string) {
	switch action {
	case "suspend":
		e.State = api.StateSuspended
	case "resume":
		e.State = api.StateReady
	case "drain":
		e.State = api.StateDraining
	case "deregister":
		e.State = api.StateDeregistered
	}
}

func applyEnroll(e *api.Environment, placement string, f api.ClusterFacts) {
	e.Live = true
	if placement != "" {
		e.Placement = placement
	}
	mergeFacts(e, f)
	e.State = api.StateAttesting
	e.MCPBaseURL = e.ID + ".mcp.mekodata.ai"
	e.CertFingerprint = "sha256:" + RandHex(6)
}

func applyStatus(e *api.Environment, r api.StatusReport) {
	now := time.Now().UTC()
	e.State = r.EnvironmentState
	e.AgentVersion = r.AgentVersion
	e.LastHeartbeatAt = &now
	e.Components = r.Components
	mergeFacts(e, r.ClusterFacts)
	e.Datapacks = len(r.DatapackPhases)
}
