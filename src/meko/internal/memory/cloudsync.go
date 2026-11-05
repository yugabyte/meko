package memory

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/yugabyte/meko/src/meko/internal/store"
)

// A datapack syncing to Meko Cloud takes no writes: the sync exports it from one snapshot and then
// deletes it here, so a write made in between would be lost (internal/cloudsync).

// ErrDatapackSyncing is a write to a datapack that's syncing to Meko Cloud.
var ErrDatapackSyncing = errors.New("this datapack is syncing to Meko Cloud: try again once it's in Meko Cloud")

// BeginSync stops writes to a datapack, or returns ErrDatapackSyncing when a sync already holds it.
func (s *Service) BeginSync(id string) error {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	id = CanonicalID(id)
	if s.syncing[id] {
		return ErrDatapackSyncing
	}
	if s.syncing == nil {
		s.syncing = map[string]bool{}
	}
	s.syncing[id] = true
	return nil
}

// EndSync lets a datapack take writes again: its sync failed, or it's gone.
func (s *Service) EndSync(id string) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	delete(s.syncing, CanonicalID(id))
}

// FinishSync deletes a datapack Meko Cloud has taken, then lets its id take writes again: there's
// nothing left here to write to, so every call on it goes to Meko Cloud.
func (s *Service) FinishSync(ctx context.Context, id string) error {
	err := s.deleteDatapack(ctx, id)
	if err == nil || errors.Is(err, ErrNoDatapack) {
		s.EndSync(id)
		return nil
	}
	return err
}

// writable is ErrDatapackSyncing for a datapack syncing to Meko Cloud.
func (s *Service) writable(id string) error {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	if s.syncing[CanonicalID(id)] {
		return ErrDatapackSyncing
	}
	return nil
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
