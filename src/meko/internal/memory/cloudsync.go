package memory

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/yugabyte/meko/src/meko/internal/store"
)

// A datapack syncing to Meko Cloud takes no writes: the sync exports it from one snapshot and then
// deletes it here, so a write made in between would be lost (internal/cloudsync). Every write holds
// its datapack's gate for its whole length (writing), and BeginSync takes the gate exclusively, so a
// sync starts only once the writes already under way have finished, and every later one is refused.

// ErrDatapackSyncing is a write to a datapack that's syncing to Meko Cloud.
var ErrDatapackSyncing = errors.New("this datapack is syncing to Meko Cloud: try again once it's in Meko Cloud")

// gate is a datapack's write gate.
func (s *Service) gate(id string) *sync.RWMutex {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	if s.gates == nil {
		s.gates = map[string]*sync.RWMutex{}
	}
	g, ok := s.gates[id]
	if !ok {
		g = &sync.RWMutex{}
		s.gates[id] = g
	}
	return g
}

// Syncing says whether a datapack is syncing to Meko Cloud.
func (s *Service) Syncing(id string) bool {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	return s.syncing[CanonicalID(id)]
}

// writing holds a datapack's gate for a write, or returns ErrDatapackSyncing. done releases it. A
// write never calls another guarded write while it holds the gate: a sync waiting for the gate would
// leave the nested one waiting too.
func (s *Service) writing(id string) (done func(), err error) {
	g := s.gate(CanonicalID(id))
	g.RLock()
	if s.Syncing(id) {
		g.RUnlock()
		return nil, ErrDatapackSyncing
	}
	return g.RUnlock, nil
}

// BeginSync stops writes to a datapack, once the writes already under way have finished, or returns
// ErrDatapackSyncing when a sync already holds it.
func (s *Service) BeginSync(id string) error {
	id = CanonicalID(id)
	g := s.gate(id)
	g.Lock()
	defer g.Unlock()
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	if s.syncing[id] {
		return ErrDatapackSyncing
	}
	if s.syncing == nil {
		s.syncing = map[string]bool{}
	}
	s.syncing[id] = true
	return nil
}

// HoldSyncs stops writes to every datapack whose sync a stop interrupted, before anything else can
// write (the memory extractor among them); the sync's Resume takes it from there.
func (s *Service) HoldSyncs(ctx context.Context) error {
	jobs, err := s.SyncJobs(ctx)
	if err != nil {
		return err
	}
	for id := range jobs {
		if err = s.BeginSync(id); err != nil && !errors.Is(err, ErrDatapackSyncing) {
			return err
		}
	}
	return nil
}

// EndSync lets a datapack take writes again: its sync failed, or it's gone.
func (s *Service) EndSync(id string) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	delete(s.syncing, CanonicalID(id))
}

// FinishSync deletes a datapack Meko Cloud has taken, then lets its id take writes again: there's
// nothing left here to write to, so every call on it goes to Meko Cloud. Its Context Gaps questions
// stay, under its id, as for any Meko Cloud datapack.
func (s *Service) FinishSync(ctx context.Context, id string) error {
	err := s.deleteDatapack(ctx, id, true)
	if err == nil || errors.Is(err, ErrNoDatapack) {
		s.EndSync(id)
		return nil
	}
	return err
}

// SyncSize measures one of this Meko's datapacks for a sync's precheck.
func (s *Service) SyncSize(ctx context.Context, id string) (store.SyncSize, error) {
	out, err := s.st.SyncSize(ctx, CanonicalID(id))
	if errors.Is(err, store.ErrNoDatapack) {
		return out, ErrNoDatapack
	}
	return out, err
}

// keySyncJobs holds the syncs to Meko Cloud whose import Meko Cloud has, as {datapack id: import
// id}, so a sync a stop interrupted is resumed rather than left with the datapack on both sides.
const keySyncJobs = "cloud_sync.jobs"

// SyncJobs is the syncs to Meko Cloud whose import Meko Cloud has: {datapack id: import id}.
func (s *Service) SyncJobs(ctx context.Context) (map[string]string, error) {
	v, ok, err := s.st.Setting(ctx, keySyncJobs)
	out := map[string]string{}
	if err != nil || !ok || v == "" {
		return out, err
	}
	err = json.Unmarshal([]byte(v), &out)
	return out, err
}

// SetSyncJob saves a datapack's import id, or forgets it when importID is "".
func (s *Service) SetSyncJob(ctx context.Context, datapack, importID string) error {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	jobs, err := s.SyncJobs(ctx)
	if err != nil {
		return err
	}
	if importID == "" {
		delete(jobs, CanonicalID(datapack))
	} else {
		jobs[CanonicalID(datapack)] = importID
	}
	if len(jobs) == 0 {
		return s.st.DeleteSetting(ctx, keySyncJobs)
	}
	b, err := json.Marshal(jobs)
	if err != nil {
		return err
	}
	return s.st.SetSetting(ctx, keySyncJobs, string(b))
}

// keyCloudSync is the settings key of "Cloud Sync": "1" on, anything else off. Off by default, so a
// datapack can be synced to Meko Cloud only once the owner turns it on in Settings.
const keyCloudSync = "cloud_sync.enabled"

// CloudSyncEnabled says whether syncing datapacks to Meko Cloud is turned on.
func (s *Service) CloudSyncEnabled(ctx context.Context) bool {
	v, _, err := s.st.Setting(ctx, keyCloudSync)
	return err == nil && v == "1"
}

// SetCloudSyncEnabled turns syncing datapacks to Meko Cloud on or off. Turning it off doesn't stop a
// sync already under way.
func (s *Service) SetCloudSyncEnabled(ctx context.Context, on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	return s.st.SetSetting(ctx, keyCloudSync, v)
}
