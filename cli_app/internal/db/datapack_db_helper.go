package db

import (
	"database/sql"
	"fmt"
	"log/slog"
)

// registerDatapack inserts the account, datapack, knowledge-base, default
// agent, and agent-knowledge-base rows into the meko_system registry tables.
// It returns the datapack ID and default agent ID.
func registerDatapack(mainDB *sql.DB, datapackName, defaultAgentName, embedModel, inferModel, kbase string) (string, string, error) {
	// TODO: replace with real account from user authentication
	var accountID string
	err := mainDB.QueryRow(
		`INSERT INTO meko_system.account_registry (account_name)
		 VALUES ($1)
		 ON CONFLICT (account_name) DO UPDATE SET updated_at = NOW()
		 RETURNING id`,
		"default",
	).Scan(&accountID)
	if err != nil {
		slog.Error("failed to upsert account", "error", err)
		return "", "", fmt.Errorf("failed to upsert account: %w", err)
	}

	tx, err := mainDB.Begin()
	if err != nil {
		return "", "", fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	var datapackID string
	err = tx.QueryRow(
		`INSERT INTO meko_system.datapack_registry
			(account_id, datapack_name, llm_model, embedding_model)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id`,
		accountID, datapackName, inferModel, embedModel,
	).Scan(&datapackID)
	if err != nil {
		slog.Error("failed to insert datapack registry entry", "datapack", datapackName, "error", err)
		return "", "", fmt.Errorf("failed to insert datapack registry entry: %w", err)
	}

	if _, err := tx.Exec(
		`INSERT INTO meko_system.datapack_knowledge_base_registry
			(datapack_id, knowledge_base_url)
		 VALUES ($1, $2)`,
		datapackID, kbase,
	); err != nil {
		slog.Error("failed to insert knowledge base registry entry", "datapack_id", datapackID, "error", err)
		return "", "", fmt.Errorf("failed to insert knowledge base registry entry: %w", err)
	}

	if err := tx.Commit(); err != nil {
		slog.Error("failed to commit datapack transaction", "error", err)
		return "", "", fmt.Errorf("failed to commit datapack transaction: %w", err)
	}

	var defaultAgentID string
	err = mainDB.QueryRow(
		`INSERT INTO meko_system.agent_registry
			(datapack_id, agent_name, agent_metadata)
		 VALUES ($1, $2, $3)
		 RETURNING id`,
		datapackID, defaultAgentName,
		fmt.Sprintf(`{"llm_model":%q,"embedding_model":%q}`, inferModel, embedModel),
	).Scan(&defaultAgentID)
	if err != nil {
		slog.Error("failed to create default agent", "datapack_id", datapackID, "error", err)
		return "", "", fmt.Errorf("failed to create default agent: %w", err)
	}
	slog.Info("default agent created", "datapack_id", datapackID, "agent_id", defaultAgentID)

	if _, err := mainDB.Exec(
		`INSERT INTO meko_system.agent_knowledge_base (agent_id, knowledge_base_id)
		 SELECT $1, id
		 FROM meko_system.datapack_knowledge_base_registry
		 WHERE datapack_id = $2`,
		defaultAgentID, datapackID,
	); err != nil {
		slog.Error("failed to link default agent to knowledge bases", "agent_id", defaultAgentID, "error", err)
		return "", "", fmt.Errorf("failed to link default agent to knowledge bases: %w", err)
	}
	slog.Info("default agent linked to all datapack knowledge bases", "agent_id", defaultAgentID, "datapack_id", datapackID)

	return datapackID, defaultAgentID, nil
}

func startVectorIndexCreationForKnowledgeBase(dpDB *sql.DB, indexName, knowledgeBaseURL, embedModel string, embDims int) error {
	var sourceID string
	err := dpDB.QueryRow(
		`SELECT dist_rag.create_source($1)`,
		knowledgeBaseURL,
	).Scan(&sourceID)
	if err != nil {
		slog.Error("failed to create dist_rag source", "url", knowledgeBaseURL, "error", err)
		return fmt.Errorf("failed to create dist_rag source for %q: %w", knowledgeBaseURL, err)
	}
	slog.Info("dist_rag source created", "source_id", sourceID, "url", knowledgeBaseURL)

	embModelParams := fmt.Sprintf(`{"model": %q, "dimensions": %d}`, embedModel, embDims)
	chunkParams := `{"chunk_size": 1024, "chunk_overlap": 256}`

	// TODO: derive AI provider from embedModel when non-OpenAI models are supported
	var indexID string
	err = dpDB.QueryRow(
		`SELECT dist_rag.init_vector_index(
			r_index_name := $1,
			r_sources := ARRAY[$2]::UUID[],
			r_ai_provider := 'OPENAI',
			r_embedding_model_params := $3,
			r_chunk_params := $4
		) AS index_id`,
		indexName, sourceID, embModelParams, chunkParams,
	).Scan(&indexID)
	if err != nil {
		slog.Error("failed to init vector index", "index", indexName, "error", err)
		return fmt.Errorf("failed to init vector index %q: %w", indexName, err)
	}
	slog.Info("vector index initialized", "index_name", indexName, "index_id", indexID)

	if _, err := dpDB.Exec(
		`SELECT dist_rag.build_index(r_index_name := $1)`,
		indexName,
	); err != nil {
		slog.Error("failed to build vector index", "index", indexName, "error", err)
		return fmt.Errorf("failed to build vector index %q: %w", indexName, err)
	}
	slog.Info("vector index build started", "index_name", indexName)

	return nil
}


// createTablesForAgentMemory creates a dedicated database for the datapack and
// provisions the AGE graph extension and embedding tables inside it.
func createTablesForAgentMemory(mainDB *sql.DB, host string, port int, datapackID, agentName, embedModel, kbase string) (string, error) {
	safeID := sanitizeUUID(datapackID)
	dbName := safeID + "_datapack"
	slog.Info("creating datapack database", "datapack_id", datapackID, "database", dbName)

	// CREATE DATABASE cannot run inside a transaction.
	if _, err := mainDB.Exec(fmt.Sprintf(`CREATE DATABASE "%s"`, dbName)); err != nil {
		slog.Error("failed to create database for datapack", "database", dbName, "error", err)
		return "", fmt.Errorf("failed to create datapack %q: %w", dbName, err)
	}

	dpConnStr := fmt.Sprintf(
		"postgresql://%s:%d/%s?user=yugabyte&password=&sslmode=disable",
		host, port, dbName,
	)
	dpDB, err := sql.Open("postgres", dpConnStr)
	if err != nil {
		return "", fmt.Errorf("failed to open datapack database: %w", err)
	}
	defer dpDB.Close()

	if err := dpDB.Ping(); err != nil {
		return "", fmt.Errorf("failed to connect to datapack database %q: %w", dbName, err)
	}

	for _, ext := range []string{"vector", "age", "pg_dist_rag"} {
		if _, err := dpDB.Exec(fmt.Sprintf("CREATE EXTENSION IF NOT EXISTS %s", ext)); err != nil {
			slog.Error("failed to create extension", "extension", ext, "database", dbName, "error", err)
			return "", fmt.Errorf("failed to create extension %q in datapack database: %w", ext, err)
		}
	}
	slog.Debug("extensions created", "database", dbName)

	if _, err := dpDB.Exec(`SET search_path = ag_catalog, public`); err != nil {
		return "", fmt.Errorf("failed to set search_path for AGE: %w", err)
	}

	graphName := agentName + "_meko_graph"
	if _, err := dpDB.Exec(fmt.Sprintf(`SELECT * FROM ag_catalog.create_graph('%s')`, graphName)); err != nil {
		slog.Error("failed to create AGE graph", "database", dbName, "graph", graphName, "error", err)
		return "", fmt.Errorf("failed to create AGE graph %q: %w", graphName, err)
	}
	slog.Debug("AGE graph created", "database", dbName, "graph", graphName)

	embDims := EmbeddingDims(embedModel)

	embTable := agentName + "_meko_mem0_embeddings"
	createEmbeddingSQL := fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			graph_name TEXT NOT NULL,
			node_graphid BIGINT NOT NULL,
			node_name TEXT,
			embedding vector(%d) NOT NULL,
			user_id TEXT NOT NULL,
			agent_id TEXT,
			run_id TEXT,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW(),
			PRIMARY KEY (graph_name, node_graphid)
		)`, embTable, embDims)

	if _, err := dpDB.Exec(createEmbeddingSQL); err != nil {
		slog.Error("failed to create embedding table", "database", dbName, "table", embTable, "error", err)
		return "", fmt.Errorf("failed to create embedding table %q: %w", embTable, err)
	}

	embIndex := agentName + "_meko_mem0_embeddings_idx"
	createIndexSQL := fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS %s ON %s USING hnsw (embedding vector_cosine_ops)`,
		embIndex, embTable,
	)
	if _, err := dpDB.Exec(createIndexSQL); err != nil {
		slog.Error("failed to create HNSW index", "database", dbName, "index", embIndex, "error", err)
		return "", fmt.Errorf("failed to create HNSW index %q: %w", embIndex, err)
	}

	// userEmbTable := agentName + "_user_embeddings"
	// createUserEmbSQL := fmt.Sprintf(`
	// 	CREATE TABLE IF NOT EXISTS %s (
	// 		id UUID PRIMARY KEY,
	// 		vector vector(%d),
	// 		payload JSONB
	// 	)`, userEmbTable, embDims)

	// if _, err := dpDB.Exec(createUserEmbSQL); err != nil {
	// 	slog.Error("failed to create user embedding table", "database", dbName, "table", userEmbTable, "error", err)
	// 	return "", fmt.Errorf("failed to create user embedding table %q: %w", userEmbTable, err)
	// }

	kbIndexName := agentName + "_kb_index"
	if err := startVectorIndexCreationForKnowledgeBase(dpDB, kbIndexName, kbase, embedModel, embDims); err != nil {
		return "", err
	}

	slog.Info("agent memory provisioned", "datapack_id", datapackID, "agent_name", agentName, "database", dbName, "embedding_dims", embDims)
	return dbName, nil
}

// CreateDatapack registers a datapack in the registry tables, then creates a
// dedicated database and provisions the AGE graph + embedding tables for agent
// memory. host and port are needed to open a connection to the new database.
func CreateDatapack(mainDB *sql.DB, port int, host, datapackName, agentName, embedModel, inferModel, kbase string) (string, string, error) {
	slog.Info("creating datapack", "datapack", datapackName, "embed_model", embedModel, "inference_model", inferModel)

	datapackID, agentID, err := registerDatapack(mainDB, datapackName, agentName, embedModel, inferModel, kbase)
	if err != nil {
		return "", "", err
	}

	if _, err := createTablesForAgentMemory(mainDB, host, port, datapackID, agentName, embedModel, kbase); err != nil {
		return "", "", err
	}

	slog.Info("datapack created successfully", "datapack_id", datapackID, "agent_id", agentID)
	return datapackID, agentID, nil
}
