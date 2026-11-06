package memory

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/store"
)

// The export's format and version: a reader checks both before it trusts the rest. Version 2 is
// a zip; version 1 was one JSON file without embeddings or file contents.
const (
	ExportFormat  = "meko-datapack-export"
	ExportVersion = 2
)

// exportLeftOut is what an export doesn't carry, said in its datapack.json: the keyword indexes
// and entities, which whoever imports it rebuilds from the text.
var exportLeftOut = []string{"keyword indexes and entities (rebuilt from the text on import)"}

// The zip's entries. Each .jsonl holds one JSON object a line, so an importer can read a large one
// a record at a time; files and artifacts are stored under their SHA-256.
const (
	exportManifest    = "datapack.json"
	exportMemories    = "memories.jsonl"
	exportCollective  = "collective_memories.jsonl"
	exportConvs       = "conversations.jsonl"
	exportKnowledge   = "knowledge.jsonl"
	exportArtifacts   = "artifacts.jsonl"
	exportCallLog     = "call_log.jsonl"
	exportKnowledgeIn = "knowledge/"
	exportArtifactsIn = "artifacts/"
)

// exportPage is how many rows each read takes as the export walks a datapack.
const exportPage = 500

// ExportDatapack writes one datapack to w as a zip, an entry at a time: only the lists of files,
// artifacts and calls are held whole, and one file's bytes at a time. Its entries:
//   - datapack.json: the header (format, version, Meko's version, the embedding model and its
//     dimensions, what's left out, each entry's count) and the datapack.
//   - memories.jsonl: each memory with its state (learning or kept), history and embedding.
//   - collective_memories.jsonl: each with its knowledge-base embedding.
//   - conversations.jsonl: each with every turn, each turn with its search chunks and embeddings.
//   - knowledge.jsonl, and knowledge/<sha256>.<ext>: each file's metadata, its chunks with their
//     embeddings, and the file itself.
//   - artifacts.jsonl, and artifacts/<sha256>.<ext>: the same for artifacts.
//   - call_log.jsonl: the datapack's calls.
//
// datapack.json's placement is the project and folder it's filed in, by name with their
// descriptions, or null when it's Unfiled: a Meko Cloud sync files it there.
//
// Ids and dates are kept, and an embedding is null where the sweep hasn't embedded it yet. Every
// read goes through one snapshot of the database, so writes made while it runs neither drop nor
// repeat a row. Knowledge files' bytes are on disk, outside the snapshot: one deleted while it runs
// goes out without them, its path "". start is called once the reads that can fail before the
// first byte have worked, and gives the writer;
// an error before then means nothing was written, and one after leaves the zip cut short.
func (s *Service) ExportDatapack(ctx context.Context, datapack, version string, start func() io.Writer) error {
	return s.st.Snapshot(ctx, func(st *store.Store) error {
		return s.export(ctx, st, datapack, version, start)
	})
}

func (s *Service) export(ctx context.Context, st *store.Store, datapack, version string,
	start func() io.Writer) error {
	d, err := st.GetDatapack(ctx, datapack)
	if err != nil {
		return err
	}
	model, _, err := st.Embedder(ctx)
	if err != nil {
		return err
	}
	learnings, err := idSet(func(offset int) ([]string, error) {
		recs, _, lerr := st.ListMemoryPage(ctx, store.MemoryPage{DatapackID: datapack, UserID: s.userID,
			Scope: store.ScopeLearnings, Limit: exportPage, Offset: offset})
		return recordIDs(recs), lerr
	})
	if err != nil {
		return err
	}
	// A promotion that stopped partway leaves the memory in both tables: it's exported once, as a
	// collective memory.
	promoted, err := idSet(func(offset int) ([]string, error) {
		ps, _, perr := st.PromotedMemories(ctx, datapack, exportPage, offset)
		out := make([]string, len(ps))
		for i, p := range ps {
			out[i] = p.ID
		}
		return out, perr
	})
	if err != nil {
		return err
	}
	// The lists the later entries walk, read now so a failure gets an error reply.
	var docs []store.Document
	if err = pages(func(offset int) (int, error) {
		ds, _, derr := st.ListDocuments(ctx, store.DocumentPage{DatapackID: datapack, Limit: exportPage,
			Offset: offset})
		docs = append(docs, ds...)
		return len(ds), derr
	}); err != nil {
		return err
	}
	var arts []store.Artifact
	if err = pages(func(offset int) (int, error) {
		as, _, aerr := st.ListArtifacts(ctx, datapack, "", exportPage, offset)
		arts = append(arts, as...)
		return len(as), aerr
	}); err != nil {
		return err
	}
	calls, err := st.Calls(ctx, datapack, time.Time{})
	if err != nil {
		return err
	}
	placement, err := exportPlacement(ctx, st, datapack)
	if err != nil {
		return err
	}

	z := zip.NewWriter(start())
	counts := map[string]int{}
	entry := func(name string) (io.Writer, error) {
		return z.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Now()})
	}
	jsonl := func(name string, fill func(emit func(any) error) error) error {
		f, ferr := entry(name)
		if ferr != nil {
			return ferr
		}
		enc := json.NewEncoder(f)
		return fill(func(v any) error {
			counts[name]++
			return enc.Encode(v)
		})
	}
	file := func(name string, r io.Reader) error {
		f, ferr := entry(name)
		if ferr == nil {
			_, ferr = io.Copy(f, r)
		}
		return ferr
	}

	if err = jsonl(exportMemories, func(emit func(any) error) error {
		return pages(func(offset int) (int, error) {
			recs, _, perr := st.ListMemoryPage(ctx, store.MemoryPage{DatapackID: datapack, UserID: s.userID,
				Limit: exportPage, Offset: offset})
			if perr != nil {
				return 0, perr
			}
			for _, r := range recs {
				if promoted[r.ID] {
					continue
				}
				m := fromRecord(datapack, r)
				h, herr := st.History(ctx, datapack, m.ID)
				if herr != nil {
					return 0, herr
				}
				v, verr := st.MemoryEmbedding(ctx, datapack, m.ID)
				if verr != nil {
					return 0, verr
				}
				state := "kept"
				if learnings[m.ID] {
					state = "learning"
				}
				if err = emit(map[string]any{"id": m.ID, "text": m.Memory, "hash": m.Hash, "state": state,
					"agent_id": m.AgentID, "conversation_id": m.RunID, "role": m.Role, "actor_id": m.ActorID,
					"attributed_to": m.AttributedTo, "expiration_date": m.ExpirationDate, "metadata": m.Metadata,
					"created_at": m.CreatedAt, "updated_at": m.UpdatedAt, "history": h,
					"embedding": v}); err != nil {
					return 0, err
				}
			}
			return len(recs), nil
		})
	}); err != nil {
		return err
	}
	if err = jsonl(exportCollective, func(emit func(any) error) error {
		return pages(func(offset int) (int, error) {
			ps, _, perr := st.PromotedMemories(ctx, datapack, exportPage, offset)
			for _, p := range ps {
				if perr != nil {
					break
				}
				v, verr := st.PromotedEmbedding(ctx, datapack, p.ID)
				if verr != nil {
					return 0, verr
				}
				perr = emit(map[string]any{"id": p.ID, "text": p.Text, "hash": p.Hash, "agent_id": p.AgentID,
					"conversation_id": p.ConversationID, "role": p.Role, "actor_id": p.ActorID, "source": p.Source,
					"connector_id": p.ConnectorID, "metadata": p.Payload, "created_at": p.CreatedAt,
					"updated_at": p.UpdatedAt, "promoted_at": p.PromotedAt, "promoted_by": p.PromotedBy,
					"embedding": v})
			}
			return len(ps), perr
		})
	}); err != nil {
		return err
	}
	if err = jsonl(exportConvs, func(emit func(any) error) error {
		return pages(func(offset int) (int, error) {
			cs, _, perr := st.ListConversations(ctx, datapack, "", exportPage, offset)
			for _, c := range cs {
				if perr != nil {
					break
				}
				chunks, cerr := st.MessageChunks(ctx, datapack, c.ID)
				if cerr != nil {
					return 0, cerr
				}
				turns := []map[string]any{}
				if terr := pages(func(mo int) (int, error) {
					ms, merr := st.Messages(ctx, datapack, c.ID, exportPage, mo)
					for _, m := range ms {
						turnChunks := chunks[m.ID]
						if turnChunks == nil {
							turnChunks = []store.ExportChunk{}
						}
						turns = append(turns, map[string]any{"id": m.ID, "input": m.Input, "output": m.Output,
							"reasoning": m.Reasoning, "plan": m.Plan, "metadata": m.Metadata, "created_at": m.CreatedAt,
							"chunks": turnChunks})
					}
					return len(ms), merr
				}); terr != nil {
					return 0, terr
				}
				perr = emit(map[string]any{"id": c.ID, "title": c.Title, "agent_id": c.AgentID, "session_id": c.SessionID,
					"run_id": c.RunID, "metadata": c.Metadata, "created_at": c.CreatedAt, "updated_at": c.UpdatedAt,
					"turns": turns})
			}
			return len(cs), perr
		})
	}); err != nil {
		return err
	}

	// Knowledge files' bytes first, each opened as its path is decided: they live on disk, outside
	// the snapshot, and one deleted since is left out (its record's path is then "").
	stored := map[string]string{} // a document's id to where its bytes are in the zip
	for _, d := range docs {
		f, oerr := s.files.Open(datapack, d.ContentHash)
		if errors.Is(oerr, os.ErrNotExist) {
			continue
		}
		if oerr != nil {
			return oerr
		}
		path := exportKnowledgeIn + d.ContentHash + safeExt(d.Filename)
		err = file(path, f)
		_ = f.Close()
		if err != nil {
			return err
		}
		stored[d.ID] = path
	}
	if err = jsonl(exportKnowledge, func(emit func(any) error) error {
		for _, d := range docs {
			chunks, cerr := st.DocumentChunks(ctx, d.Rowid)
			if cerr != nil {
				return cerr
			}
			if chunks == nil {
				chunks = []store.ExportChunk{}
			}
			if eerr := emit(map[string]any{"id": d.ID, "filename": d.Filename, "type": d.DocumentType,
				"sha256": d.ContentHash, "size_bytes": d.SizeBytes, "status": d.Status, "error": d.Error,
				"uploaded_at": d.UploadedAt, "completed_at": d.CompletedAt, "path": stored[d.ID],
				"chunks": chunks}); eerr != nil {
				return eerr
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err = jsonl(exportArtifacts, func(emit func(any) error) error {
		for _, a := range arts {
			if eerr := emit(map[string]any{"id": a.ID, "filename": a.Filename, "content_type": a.ContentType,
				"sha256": a.ContentHash, "size_bytes": a.SizeBytes, "agent_id": a.AgentID, "created_at": a.CreatedAt,
				"path": exportArtifactsIn + a.ContentHash + safeExt(a.Filename)}); eerr != nil {
				return eerr
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, a := range arts {
		b, aerr := st.ArtifactBytes(ctx, datapack, a.ContentHash)
		if aerr != nil {
			return aerr
		}
		if err = file(exportArtifactsIn+a.ContentHash+safeExt(a.Filename), bytes.NewReader(b)); err != nil {
			return err
		}
	}

	if err = jsonl(exportCallLog, func(emit func(any) error) error {
		for _, c := range calls {
			if eerr := emit(map[string]any{"id": c.ID, "at": c.At, "tool": c.Tool, "agent_id": c.AgentID,
				"conversation_id": c.ConversationID, "credential": c.Credential, "ok": c.OK, "error_code": c.ErrorCode,
				"ms": c.Ms, "memory_n": c.MemoryN, "kb_n": c.KBN, "conversation_n": c.ConversationN,
				"score_high": c.ScoreHigh, "score_mid": c.ScoreMid, "score_low": c.ScoreLow,
				"tokens_sent": c.TokensSent, "tokens_saved": c.TokensSaved, "tokens_if_raw": c.TokensIfRaw,
				"input_tokens":  c.InputTokens,
				"output_tokens": c.OutputTokens, "steps": c.Steps}); eerr != nil {
				return eerr
			}
		}
		return nil
	}); err != nil {
		return err
	}

	// The header last, so it can count what the other entries hold.
	if err = jsonl(exportManifest, func(emit func(any) error) error {
		return emit(map[string]any{"format": ExportFormat, "version": ExportVersion,
			"exported_at": time.Now().UTC().Format(time.RFC3339), "meko_version": version,
			"embedding": map[string]any{"model": model.Model, "dims": model.Dims}, "left_out": exportLeftOut,
			"counts": map[string]int{"memories": counts[exportMemories], "collective_memories": counts[exportCollective],
				"conversations": counts[exportConvs], "knowledge_files": counts[exportKnowledge],
				"artifacts": counts[exportArtifacts], "calls": counts[exportCallLog]},
			"datapack": d, "placement": placement})
	}); err != nil {
		return err
	}
	return z.Close()
}

// unsafeExt is what a file's extension loses in its zip path.
var unsafeExt = regexp.MustCompile(`[^a-z0-9]`)

// safeExt is a filename's extension for its stored copy's path: lowercase letters and digits only,
// at most 10, with its dot, or "" when it has none.
func safeExt(name string) string {
	ext := unsafeExt.ReplaceAllString(strings.ToLower(strings.TrimPrefix(filepath.Ext(name), ".")), "")
	if ext == "" {
		return ""
	}
	return "." + ext[:min(len(ext), 10)]
}

// idSet collects what read returns, page by page, as a set.
func idSet(read func(offset int) ([]string, error)) (map[string]bool, error) {
	out := map[string]bool{}
	err := pages(func(offset int) (int, error) {
		page, err := read(offset)
		for _, id := range page {
			out[id] = true
		}
		return len(page), err
	})
	return out, err
}

func recordIDs(recs []store.Record) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.ID
	}
	return out
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

// exportPlacement is where a datapack is filed, for datapack.json: nil when it's Unfiled.
func exportPlacement(ctx context.Context, st *store.Store, datapack string) (map[string]string, error) {
	ps, err := st.Placements(ctx, []string{datapack})
	if err != nil {
		return nil, err
	}
	pl, ok := ps[datapack]
	if !ok {
		return nil, nil
	}
	p, err := st.Project(ctx, pl.ProjectID)
	if err != nil {
		return nil, err
	}
	folders, err := st.Folders(ctx, pl.ProjectID)
	if err != nil {
		return nil, err
	}
	out := map[string]string{"project_name": p.Name, "project_description": p.Description,
		"folder_name": pl.FolderName, "folder_description": ""}
	for _, f := range folders {
		if f.ID == pl.FolderID {
			out["folder_description"] = f.Description
		}
	}
	return out, nil
}
