package store

import "context"

// SyncSize is what a datapack would add to a Meko Cloud account, for the sync's precheck: its
// memories (collective ones too), the turns Cloud would make searchable (none when the datapack is
// opted out of conversation search), and its knowledge files' bytes, all and the largest.
type SyncSize struct {
	Memories        int64
	SearchableTurns int64
	Files           int64
	FileBytes       int64
	LargestFile     int64
}

// SyncSize measures a datapack, or returns ErrNoDatapack.
func (s *Store) SyncSize(ctx context.Context, datapack string) (SyncSize, error) {
	if ok, err := s.DatapackExists(ctx, datapack); err != nil || !ok {
		if err == nil {
			err = ErrNoDatapack
		}
		return SyncSize{}, err
	}
	var out SyncSize
	err := s.r.QueryRowContext(ctx, "SELECT "+
		"(SELECT count(*) FROM memories WHERE datapack_id = ?1) + "+
		"(SELECT count(*) FROM promoted_memories p WHERE p.datapack_id = ?1 AND NOT EXISTS "+
		"(SELECT 1 FROM memories m WHERE m.id = p.id)), "+
		"(SELECT CASE WHEN d.conversation_search_opt_out = 1 THEN 0 ELSE "+
		"(SELECT count(*) FROM messages m JOIN conversations c ON c.rowid = m.conversation_rowid "+
		"WHERE c.datapack_id = ?1) END FROM datapacks d WHERE d.id = ?1), "+
		"(SELECT count(*) FROM kb_documents WHERE datapack_id = ?1), "+
		"(SELECT coalesce(sum(size_bytes), 0) FROM kb_documents WHERE datapack_id = ?1), "+
		"(SELECT coalesce(max(size_bytes), 0) FROM kb_documents WHERE datapack_id = ?1)", datapack).
		Scan(&out.Memories, &out.SearchableTurns, &out.Files, &out.FileBytes, &out.LargestFile)
	return out, err
}
