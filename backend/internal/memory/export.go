package memory

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/yugabyte/meko/backend/internal/store"
)

// The export's format and version: a reader checks both before it trusts the rest.
const (
	ExportFormat  = "meko-datapack-export"
	ExportVersion = 1
)

// exportLeftOut is what an export doesn't carry, said in the file itself: vectors, indexes and
// entities are the local model's and rebuilt by whoever imports it, and v1 lists files and
// artifacts without their bytes.
var exportLeftOut = []string{
	"embeddings (vectors, rebuilt on import)",
	"keyword indexes and entities (rebuilt on import)",
	"knowledge file contents and their chunks (listed by SHA-256)",
	"artifact contents (listed by SHA-256)",
	"the call log",
}

// exportPage is how many rows each read takes as the export walks a datapack.
const exportPage = 500

// ExportDatapack writes one datapack as JSON to w, a section at a time, so a large datapack is
// never all in memory: its memories (with their history and whether each is a learning or kept),
// collective memories, conversations with every turn, and its knowledge files and artifacts as
// lists. Ids and dates are kept. version is Meko's, for the header.
func (s *Service) ExportDatapack(ctx context.Context, datapack, version string, w io.Writer) error {
	d, err := s.Datapack(ctx, datapack)
	if err != nil {
		return err
	}
	model, _, err := s.Embedder(ctx)
	if err != nil {
		return err
	}
	learnings, err := s.learningIDs(ctx, datapack)
	if err != nil {
		return err
	}
	e := &exportWriter{w: w}
	e.raw(`{`)
	e.field("format", ExportFormat)
	e.field("version", ExportVersion)
	e.field("exported_at", time.Now().UTC().Format(time.RFC3339))
	e.field("meko_version", version)
	e.field("embedding_model", model.Model)
	e.field("left_out", exportLeftOut)
	e.field("datapack", d)

	e.list("memories", func(emit func(any)) error {
		return pages(func(offset int) (int, error) {
			recs, _, err := s.st.ListMemoryPage(ctx, store.MemoryPage{DatapackID: datapack, UserID: s.userID,
				Limit: exportPage, Offset: offset})
			for _, r := range recs {
				m := fromRecord(datapack, r)
				h, herr := s.st.History(ctx, datapack, m.ID)
				if herr != nil {
					return 0, herr
				}
				state := "kept"
				if learnings[m.ID] {
					state = "learning"
				}
				emit(map[string]any{"id": m.ID, "text": m.Memory, "state": state, "agent_id": m.AgentID,
					"conversation_id": m.RunID, "role": m.Role, "actor_id": m.ActorID, "metadata": m.Metadata,
					"created_at": m.CreatedAt, "updated_at": m.UpdatedAt, "history": h})
			}
			return len(recs), err
		})
	})
	e.list("collective_memories", func(emit func(any)) error {
		return pages(func(offset int) (int, error) {
			ps, _, err := s.st.PromotedMemories(ctx, datapack, exportPage, offset)
			for _, p := range ps {
				emit(map[string]any{"id": p.ID, "text": p.Text, "agent_id": p.AgentID,
					"conversation_id": p.ConversationID, "role": p.Role, "actor_id": p.ActorID, "metadata": p.Payload,
					"created_at": p.CreatedAt, "updated_at": p.UpdatedAt, "promoted_at": p.PromotedAt})
			}
			return len(ps), err
		})
	})
	e.list("conversations", func(emit func(any)) error {
		return pages(func(offset int) (int, error) {
			cs, _, err := s.st.ListConversations(ctx, datapack, "", exportPage, offset)
			for _, c := range cs {
				var turns []map[string]any
				if terr := pages(func(mo int) (int, error) {
					ms, merr := s.st.Messages(ctx, datapack, c.ID, exportPage, mo)
					for _, m := range ms {
						turns = append(turns, map[string]any{"id": m.ID, "input": m.Input, "output": m.Output,
							"reasoning": m.Reasoning, "plan": m.Plan, "metadata": m.Metadata, "created_at": m.CreatedAt})
					}
					return len(ms), merr
				}); terr != nil {
					return 0, terr
				}
				if turns == nil {
					turns = []map[string]any{}
				}
				emit(map[string]any{"id": c.ID, "title": c.Title, "agent_id": c.AgentID, "session_id": c.SessionID,
					"metadata": c.Metadata, "created_at": c.CreatedAt, "updated_at": c.UpdatedAt, "turns": turns})
			}
			return len(cs), err
		})
	})
	e.list("knowledge_files", func(emit func(any)) error {
		return pages(func(offset int) (int, error) {
			ds, _, err := s.st.ListDocuments(ctx, store.DocumentPage{DatapackID: datapack, Limit: exportPage,
				Offset: offset})
			for _, d := range ds {
				emit(map[string]any{"id": d.ID, "filename": d.Filename, "type": d.DocumentType, "sha256": d.ContentHash,
					"size_bytes": d.SizeBytes, "status": d.Status, "chunks": d.ChunksProcessed, "uploaded_at": d.UploadedAt})
			}
			return len(ds), err
		})
	})
	e.list("artifacts", func(emit func(any)) error {
		return pages(func(offset int) (int, error) {
			as, _, err := s.st.ListArtifacts(ctx, datapack, "", exportPage, offset)
			for _, a := range as {
				emit(map[string]any{"id": a.ID, "filename": a.Filename, "content_type": a.ContentType,
					"sha256": a.ContentHash, "size_bytes": a.SizeBytes, "agent_id": a.AgentID, "created_at": a.CreatedAt})
			}
			return len(as), err
		})
	})
	e.raw("}\n")
	return e.err
}

// learningIDs is the datapack's learnings (memories neither kept nor promoted), by id.
func (s *Service) learningIDs(ctx context.Context, datapack string) (map[string]bool, error) {
	out := map[string]bool{}
	err := pages(func(offset int) (int, error) {
		recs, _, err := s.st.ListMemoryPage(ctx, store.MemoryPage{DatapackID: datapack, UserID: s.userID,
			Scope: store.ScopeLearnings, Limit: exportPage, Offset: offset})
		for _, r := range recs {
			out[r.ID] = true
		}
		return len(recs), err
	})
	return out, err
}

// pages calls read with offsets 0, exportPage, ... until it reads a short page or fails.
func pages(read func(offset int) (int, error)) error {
	for offset := 0; ; offset += exportPage {
		n, err := read(offset)
		if err != nil || n < exportPage {
			return err
		}
	}
}

// exportWriter writes one JSON object by hand, a field or a list item at a time, and keeps the
// first error, after which it writes nothing.
type exportWriter struct {
	w     io.Writer
	err   error
	comma bool // a field has been written, so the next needs a comma
}

func (e *exportWriter) raw(s string) {
	if e.err == nil {
		_, e.err = io.WriteString(e.w, s)
	}
}

func (e *exportWriter) value(v any) {
	if e.err != nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		e.err = err
		return
	}
	_, e.err = e.w.Write(b)
}

func (e *exportWriter) key(name string) {
	if e.comma {
		e.raw(",")
	}
	e.comma = true
	e.value(name)
	e.raw(":")
}

func (e *exportWriter) field(name string, v any) {
	e.key(name)
	e.value(v)
}

// list writes name as an array of what fill emits; an error from fill stops the export.
func (e *exportWriter) list(name string, fill func(emit func(any)) error) {
	e.key(name)
	e.raw("[")
	first := true
	err := fill(func(v any) {
		if !first {
			e.raw(",")
		}
		first = false
		e.value(v)
	})
	if err != nil && e.err == nil {
		e.err = err
	}
	e.raw("]")
}
