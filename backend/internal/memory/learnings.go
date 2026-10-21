package memory

import (
	"context"
	"errors"

	"github.com/yugabyte/meko/backend/internal/store"
)

// ErrPromoted is a memory that was promoted to the datapack's collective memory: it's no longer a
// memory to update or delete.
var ErrPromoted = errors.New("the memory was promoted")

// KeepMemories keeps the given learnings as memories (they leave the learnings list) and says how
// many it kept. Ids that aren't the local user's learnings are skipped.
func (s *Service) KeepMemories(ctx context.Context, datapack string, ids []string) (int, error) {
	if err := s.needDatapack(ctx, datapack); err != nil {
		return 0, err
	}
	return s.st.KeepMemories(ctx, datapack, s.userID, ids)
}

// Promotion is what promoting memories did, as Meko Cloud reports it. Promotion only inserts, so
// Updated is always empty.
type Promotion struct {
	Inserted []string `json:"inserted_ids"`
	Updated  []string `json:"updated_ids"`
	NotFound []string `json:"not_found_ids"`
}

// PromoteMemories moves the given learnings into the datapack's collective memory, as Meko Cloud
// does: each is copied with its id, text and provenance, then deleted from the memories through
// the engine, so memory search no longer finds it. One that isn't the local user's learning (kept
// ones included, as in Cloud) is not found. If the engine can't delete one, its copy is removed
// and it's reported not found: it stays a learning. The writer is never held while the engine
// works: the copy commits first.
func (s *Service) PromoteMemories(ctx context.Context, datapack, agentID string, ids []string) (Promotion, error) {
	out := Promotion{Inserted: []string{}, Updated: []string{}, NotFound: []string{}}
	if err := s.needDatapack(ctx, datapack); err != nil {
		return out, err
	}
	for _, id := range ids {
		_, err := s.st.CopyToPromoted(ctx, datapack, s.userID, id, agentOr(agentID))
		if errors.Is(err, store.ErrNotFound) {
			out.NotFound = append(out.NotFound, id)
			continue
		}
		if err != nil {
			return out, err
		}
		if derr := s.engineErr(s.eng.Delete(ctx, datapack, id)); derr != nil && !errors.Is(derr, ErrNotFound) {
			if rerr := s.st.DeletePromoted(context.WithoutCancel(ctx), datapack, id); rerr != nil && s.log != nil {
				s.log.Error("couldn't undo a promotion", "memory_id", id, "error", rerr)
			}
			if s.log != nil {
				s.log.Warn("couldn't promote a memory: the engine didn't delete it", "memory_id", id, "error", derr)
			}
			out.NotFound = append(out.NotFound, id)
			continue
		}
		out.Inserted = append(out.Inserted, id)
	}
	return out, nil
}

// PromotedMemories is a page of the datapack's promoted memories, most recently promoted first, and
// how many there are.
func (s *Service) PromotedMemories(ctx context.Context, datapack string, page, pageSize int) (
	[]store.Promoted, int, error) {
	if err := s.needDatapack(ctx, datapack); err != nil {
		return nil, 0, err
	}
	return s.st.PromotedMemories(ctx, datapack, pageSize, (max(page, 1)-1)*pageSize)
}

// DeletePromoted deletes a promoted memory for good, or returns ErrNotFound.
func (s *Service) DeletePromoted(ctx context.Context, datapack, id string) error {
	err := s.st.DeletePromoted(ctx, datapack, id)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// promoted returns ErrPromoted if id was promoted in the datapack.
func (s *Service) promoted(ctx context.Context, datapack, id string) error {
	p, err := s.st.IsPromoted(ctx, datapack, id)
	if err == nil && p {
		return ErrPromoted
	}
	return err
}

// IsPromoted reports whether id was promoted in the datapack.
func (s *Service) IsPromoted(ctx context.Context, datapack, id string) (bool, error) {
	return s.st.IsPromoted(ctx, datapack, id)
}
