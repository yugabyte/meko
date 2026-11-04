package memory

import (
	"context"
	"errors"

	"github.com/yugabyte/meko/src/meko/internal/memoryengine"
	"github.com/yugabyte/meko/src/meko/internal/store"
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
// the memory engine, so memory search no longer finds it. One that isn't the local user's learning (kept
// ones included, as in Cloud) is not found. The writer is never held while the memory engine works: the
// copy commits first.
//
// If the memory engine fails to delete one, its copy is removed only while the memory is still there (a
// request cut off after the memory engine got it may still have deleted it, and then the copy is all that
// is left). With the memory engine down or starting, the call stops with that error, so the caller can
// retry; any other failure leaves that one a learning and reports it not found. On an error, the
// Promotion returned says what was done before it. A memory copied but not deleted (the undo
// failed too) is finished by promoting it again.
func (s *Service) PromoteMemories(ctx context.Context, datapack, agentID string, ids []string) (Promotion, error) {
	out := Promotion{Inserted: []string{}, Updated: []string{}, NotFound: []string{}}
	if err := s.needDatapack(ctx, datapack); err != nil {
		return out, err
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		_, err := s.st.CopyToPromoted(ctx, datapack, s.userID, id, agentOr(agentID))
		if errors.Is(err, store.ErrNotFound) {
			out.NotFound = append(out.NotFound, id)
			continue
		}
		if err != nil {
			return out, err
		}
		derr := s.memoryEngineErr(s.eng.Delete(ctx, datapack, id))
		if derr == nil || errors.Is(derr, ErrNotFound) {
			out.Inserted = append(out.Inserted, id)
			continue
		}
		bg := context.WithoutCancel(ctx)
		rec, gerr := s.st.GetVector(bg, store.Memories, datapack, id)
		switch {
		case gerr != nil: // can't tell whether the memory is gone: keep the copy, which loses nothing
			return out, errors.Join(derr, gerr)
		case rec == nil: // the memory engine deleted it after all
			out.Inserted = append(out.Inserted, id)
			continue
		}
		if rerr := s.st.DeletePromoted(bg, datapack, id); rerr != nil && s.log != nil {
			s.log.Error("couldn't undo a promotion", "memory_id", id, "error", rerr)
		}
		var notReady *memoryengine.NotReadyError
		if errors.Is(derr, memoryengine.ErrUnavailable) || errors.As(derr, &notReady) || ctx.Err() != nil {
			return out, derr
		}
		if s.log != nil {
			s.log.Warn("couldn't promote a memory: the memory engine didn't delete it", "memory_id", id, "error", derr)
		}
		out.NotFound = append(out.NotFound, id)
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
