package repository

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// An empty scope must not query anything: a recipient who can see none of the
// project's datapacks gets zeros, not the owner's numbers.
func TestProjectStatsRepo_EmptyScope_QueriesNothing(t *testing.T) {
	sys, sysMock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer sys.Close()
	data, dataMock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer data.Close()

	repo := NewProjectStatsRepo(data, sys)
	out, err := repo.Stats(nil, "datapacks", "knowledge_bases")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if out.Conversations != 0 || out.KBFiles != 0 || out.Artifacts != 0 {
		t.Fatalf("want zeros for an empty scope, got %+v", out)
	}
	if out.KBFilesByType == nil {
		t.Fatal("KBFilesByType must be an empty map, not nil: it marshals to {} on the wire")
	}
	// No expectations were set, so any query at all is a failure.
	if err := sysMock.ExpectationsWereMet(); err != nil {
		t.Errorf("system DB was queried for an empty scope: %v", err)
	}
	if err := dataMock.ExpectationsWereMet(); err != nil {
		t.Errorf("data DB was queried for an empty scope: %v", err)
	}
}

// knowledge decodes a GROUPING SETS result where no_type==1 marks the grand
// total row and the rest are the per-type breakdown. Confusing the two
// silently overwrites the totals with one type's count.
func TestProjectStatsRepo_Knowledge_DecodesGroupingSets(t *testing.T) {
	sys, sysMock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer sys.Close()
	data, dataMock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer data.Close()

	sysMock.ExpectQuery(`SELECT rag_source_id`).
		WillReturnRows(sqlmock.NewRows([]string{"rag_source_id"}).AddRow("src-1"))

	dataMock.ExpectQuery(`WITH latest_per_document`).
		WillReturnRows(
			sqlmock.NewRows([]string{"no_type", "document_type", "files", "chunks"}).
				AddRow(1, nil, 7, 42).             // grand total
				AddRow(0, "text/markdown", 4, 20). // per-type
				AddRow(0, "application/pdf", 3, 22),
		)

	repo := NewProjectStatsRepo(data, sys)
	out := ProjectStats{KBFilesByType: map[string]int64{}}
	if err := repo.knowledge([]string{"dp-1"}, "datapacks", "knowledge_bases", &out); err != nil {
		t.Fatalf("knowledge: %v", err)
	}

	if out.KBFiles != 7 {
		t.Errorf("KBFiles: want the grand-total row's 7, got %d", out.KBFiles)
	}
	if out.KBChunks != 42 {
		t.Errorf("KBChunks: want 42, got %d", out.KBChunks)
	}
	if got := out.KBFilesByType["text/markdown"]; got != 4 {
		t.Errorf("text/markdown: want 4, got %d", got)
	}
	if got := out.KBFilesByType["application/pdf"]; got != 3 {
		t.Errorf("application/pdf: want 3, got %d", got)
	}
	if _, ok := out.KBFilesByType[""]; ok {
		t.Error("the grand-total row must not land in the type breakdown")
	}
}

// A datapack with no RAG source has no knowledge to count, and dist_rag must
// not be queried with an empty source list.
func TestProjectStatsRepo_Knowledge_NoSources_SkipsDistRag(t *testing.T) {
	sys, sysMock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer sys.Close()
	data, dataMock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer data.Close()

	sysMock.ExpectQuery(`SELECT rag_source_id`).
		WillReturnRows(sqlmock.NewRows([]string{"rag_source_id"}))

	repo := NewProjectStatsRepo(data, sys)
	out := ProjectStats{KBFilesByType: map[string]int64{}}
	if err := repo.knowledge([]string{"dp-1"}, "datapacks", "knowledge_bases", &out); err != nil {
		t.Fatalf("knowledge: %v", err)
	}
	if out.KBFiles != 0 {
		t.Errorf("want 0 files, got %d", out.KBFiles)
	}
	if err := dataMock.ExpectationsWereMet(); err != nil {
		t.Errorf("dist_rag was queried with no sources: %v", err)
	}
}
