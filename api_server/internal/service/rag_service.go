package service

import (
	"database/sql"
	"log/slog"
	"strings"
	"time"

	"meko/api_server/internal/repository"
)

// dist_rag.vector_indexes splits a fully-qualified index into two columns
// (schema_name, index_name); the SQL functions take them as separate
// parameters too. We keep a dotted form available for log lines and for
// the JSON response body via FullIndexName().
const (
	DatapackIndexSchema = "datapacks"
	DatapackIndexName   = "knowledge_bases"
)

// RagService wraps dist_rag operations on a per-datapack database.
type RagService struct {
	repo       *repository.RagRepo
	schemaName string
	indexName  string
}

func NewRagService(db *sql.DB, schemaName, indexName string) *RagService {
	return &RagService{
		repo:       repository.NewRagRepo(db, schemaName, indexName),
		schemaName: schemaName,
		indexName:  indexName,
	}
}

func (s *RagService) SchemaName() string { return s.schemaName }
func (s *RagService) IndexName() string  { return s.indexName }

// FullIndexName returns the dotted "schema.index" form used in log lines
// and in the API response body for /knowledge-bases/create.
func (s *RagService) FullIndexName() string {
	return s.schemaName + "." + s.indexName
}

// EnsureVectorIndex creates the vector index if it is missing. Idempotent:
// a no-op (no error) when the index already exists.
func (s *RagService) EnsureVectorIndex() error {
	exists, err := s.repo.IndexExists()
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	slog.Info("vector index does not exist, creating lazily",
		"index_name", s.FullIndexName())
	indexID, err := s.repo.InitVectorIndex()
	if err != nil {
		return err
	}
	slog.Info("vector index created",
		"index_name", s.FullIndexName(), "index_id", indexID)
	return nil
}

func (s *RagService) LookupIndexID() (string, error) {
	return s.repo.LookupIndexID()
}

func (s *RagService) LookupSourceByURI(sourceURI string) (string, error) {
	return s.repo.LookupSourceByURI(sourceURI)
}

func (s *RagService) SourceExists(sourceID string) (bool, error) {
	return s.repo.SourceExists(sourceID)
}

func (s *RagService) CreateSource(sourceURI, tenantID string) (string, error) {
	sourceID, err := s.repo.CreateSource(sourceURI, tenantID)
	if err != nil {
		return "", err
	}
	slog.Info("dist_rag source created",
		"source_id", sourceID, "source_uri", sourceURI, "tenant_id", tenantID)
	return sourceID, nil
}

func (s *RagService) AddSourceToIndex(sourceID string) error {
	indexID, err := s.repo.LookupIndexID()
	if err != nil {
		return err
	}

	chunkParams := `{"chunk_size": 1024, "chunk_overlap": 256}`

	if err := s.repo.AddSourceToIndex(indexID, sourceID, chunkParams); err != nil {
		return err
	}
	slog.Info("source added to index",
		"index_name", s.FullIndexName(), "index_id", indexID, "source_id", sourceID)
	return nil
}

func (s *RagService) GetExistingDocURIs(sourceID string) (map[string]bool, error) {
	return s.repo.GetExistingDocURIs(sourceID)
}

func (s *RagService) InsertDocument(sourceID, documentName, documentURI, checksum, documentType string) error {
	return s.repo.InsertDocument(sourceID, documentName, documentURI, checksum, documentType)
}

func (s *RagService) BuildIndex() error {
	if err := s.repo.BuildIndex(); err != nil {
		return err
	}
	slog.Info("index build triggered", "index_name", s.FullIndexName())
	return nil
}

// WaitAndBuild polls until the source is ready, then triggers build_index.
// Runs synchronously — callers should invoke via goroutine if needed.
func (s *RagService) WaitAndBuild(sourceID string) {
	const pollInterval = 2 * time.Second
	const maxWait = 5 * time.Minute
	deadline := time.Now().Add(maxWait)

	for {
		status, err := s.repo.GetSourceStatus(sourceID)
		if err != nil {
			slog.Error("WaitAndBuild: check source status failed",
				"source_id", sourceID, "error", err)
			return
		}
		switch status {
		case "COMPLETED":
			slog.Info("WaitAndBuild: source ready, triggering build_index",
				"source_id", sourceID)
			if err := s.BuildIndex(); err != nil {
				slog.Error("WaitAndBuild: build_index failed",
					"source_id", sourceID, "error", err)
			}
			return
		case "FAILED":
			slog.Error("WaitAndBuild: source failed", "source_id", sourceID)
			return
		case "QUEUED", "DISCOVERING", "PROCESSING":
			slog.Info("WaitAndBuild: source not ready yet",
				"source_id", sourceID, "status", status)
		default:
			slog.Warn("WaitAndBuild: unexpected source status",
				"source_id", sourceID, "status", status)
		}
		if time.Now().After(deadline) {
			slog.Error("WaitAndBuild: timed out",
				"source_id", sourceID, "status", status)
			return
		}
		time.Sleep(pollInterval)
	}
}

// WaitAndBuildAsync opens its own connection to the datapack DB so it can
// run as a background goroutine that outlives the HTTP request.
func WaitAndBuildAsync(datapackConnStr, schemaName, indexName, sourceID string) {
	if !strings.Contains(datapackConnStr, "sslmode=") {
		if strings.Contains(datapackConnStr, "?") {
			datapackConnStr += "&sslmode=disable"
		} else {
			datapackConnStr += "?sslmode=disable"
		}
	}
	db, err := sql.Open("postgres", datapackConnStr)
	if err != nil {
		slog.Error("WaitAndBuildAsync: failed to open datapack DB",
			"source_id", sourceID, "error", err)
		return
	}
	defer db.Close()

	svc := NewRagService(db, schemaName, indexName)
	svc.WaitAndBuild(sourceID)
}

func (s *RagService) GetSourceStatuses(sourceIDs []string) ([]repository.SourceStatus, error) {
	return s.repo.GetSourceStatuses(sourceIDs)
}

func (s *RagService) GetPipelineStatus(sourceURIs []string) (*repository.PipelineStatus, error) {
	return s.repo.GetPipelineStatus(sourceURIs)
}
