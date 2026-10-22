package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/yugabyte/meko/backend/internal/store"
)

// More errors the datapack calls return.
var (
	ErrDuplicateName = errors.New("a datapack with that name already exists")
	ErrNoDefault     = errors.New("no default datapack")
)

// Datapack name limits, as Meko Cloud's.
const (
	minNameLen = 3
	maxNameLen = 60
)

// DefaultDatapack is the datapack a call that names none uses: the default one, or ErrNoDefault
// once the user has deleted it.
func (s *Service) DefaultDatapack(ctx context.Context) (string, error) {
	id, err := s.st.DefaultDatapackID(ctx)
	if err == nil && id == "" {
		err = ErrNoDefault
	}
	return id, err
}

// ResolveDatapack is the datapack a call works in: the one it names, which must exist, or the
// default when it names none.
func (s *Service) ResolveDatapack(ctx context.Context, id string) (string, error) {
	if id == "" {
		return s.DefaultDatapack(ctx)
	}
	return id, s.needDatapack(ctx, id)
}

// Datapacks is every datapack, oldest first.
func (s *Service) Datapacks(ctx context.Context) ([]store.Datapack, error) {
	return s.st.Datapacks(ctx)
}

// Datapack is one datapack, or ErrNoDatapack.
func (s *Service) Datapack(ctx context.Context, id string) (store.Datapack, error) {
	d, err := s.st.GetDatapack(ctx, id)
	if errors.Is(err, store.ErrNoDatapack) {
		return d, ErrNoDatapack
	}
	return d, err
}

func checkName(name string) error {
	n := utf8.RuneCountInString(name)
	switch {
	case n < minNameLen || n > maxNameLen:
		return fmt.Errorf("%w: a datapack name is %d to %d characters", ErrBadRequest, minNameLen, maxNameLen)
	case strings.ContainsAny(name, "<>"):
		return fmt.Errorf("%w: a datapack name can't contain HTML", ErrBadRequest)
	}
	return nil
}

// CreateDatapack makes a datapack, or returns ErrDuplicateName or ErrBadRequest.
func (s *Service) CreateDatapack(ctx context.Context, name, description string) (store.Datapack, error) {
	name = strings.TrimSpace(name)
	if err := checkName(name); err != nil {
		return store.Datapack{}, err
	}
	d, err := s.st.CreateDatapack(ctx, name, description)
	if errors.Is(err, store.ErrDuplicateName) {
		return d, ErrDuplicateName
	}
	return d, err
}

// UpdateDatapack changes what's given of a datapack's name, description and conversation-search
// opt-out (nil leaves it). The rename goes first: it's the part that can be refused (a taken
// name), and an opt-out written before a refusal would drop new turns from search unannounced.
func (s *Service) UpdateDatapack(ctx context.Context, id string, name, description *string, optOut *bool) (
	store.Datapack, error) {
	if name != nil {
		n := strings.TrimSpace(*name)
		if err := checkName(n); err != nil {
			return store.Datapack{}, err
		}
		name = &n
	}
	d, err := s.st.UpdateDatapack(ctx, id, name, description)
	switch {
	case errors.Is(err, store.ErrNoDatapack):
		return d, ErrNoDatapack
	case errors.Is(err, store.ErrDuplicateName):
		return d, ErrDuplicateName
	case err != nil:
		return d, err
	}
	if optOut != nil {
		if err = s.SetConversationSearchOptOut(ctx, id, *optOut); err != nil {
			return d, err
		}
		d.ConversationSearchOptOut = *optOut
	}
	return d, nil
}

// DeleteDatapack deletes a datapack and everything in it, its knowledge files' bytes too.
func (s *Service) DeleteDatapack(ctx context.Context, id string) error {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	err := s.st.DeleteDatapack(ctx, id)
	if errors.Is(err, store.ErrNoDatapack) {
		return ErrNoDatapack
	}
	if err != nil {
		return err
	}
	if err = s.files.RemoveDatapack(id); err != nil && s.log != nil {
		s.log.Warn("couldn't remove a deleted datapack's knowledge files; the next start sweeps them",
			"datapack_id", id, "error", err)
	}
	return nil
}

// DatapackCounts is what a datapack holds of the local user's, as datapack_describe reports it.
type DatapackCounts struct {
	Memories        int // learnings and kept memories
	Learnings       int // memories waiting for a keep-or-promote decision
	Promoted        int // collective memories
	Conversations   int
	KnowledgeChunks int  // the knowledge base's chunks: files' and promoted memories'
	KnowledgeBase   bool // the datapack has a knowledge base
}

// CountDatapack counts a datapack's memories, learnings, promoted memories, conversations and
// knowledge-base chunks.
func (s *Service) CountDatapack(ctx context.Context, id string) (DatapackCounts, error) {
	if err := s.needDatapack(ctx, id); err != nil {
		return DatapackCounts{}, err
	}
	c, err := s.st.CountDatapack(ctx, id, s.userID)
	if err != nil {
		return DatapackCounts{}, err
	}
	chunks, hasBase, err := s.st.KnowledgeCounts(ctx, id)
	if err != nil {
		return DatapackCounts{}, err
	}
	return DatapackCounts{Memories: c.Memories, Learnings: c.Learnings, Promoted: c.Promoted,
		Conversations: c.Conversations, KnowledgeChunks: chunks, KnowledgeBase: hasBase}, nil
}
