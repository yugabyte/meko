-- Point the knowledge_bases and conversation_history_search indexes at OpenAI.
-- api_server's migrations seed both as Bedrock Titan, like hosted dev/prod, but this
-- stack cannot call Bedrock: the rag workers' AWS pair is MinIO's and AWS_ENDPOINT_URL
-- sends every AWS call to MinIO, and the only embedding key start.sh requires is
-- OPENAI_API_KEY. Left as seeded, every document would fail at embed time and the MCP
-- server could not embed a query.
--
-- Idempotent: only a row still on AWS_BEDROCK is touched, so a rerun, and a database that
-- was seeded OpenAI before the seed changed, are left alone. The embedder is the one the
-- seed used before: text-embedding-3-large reduced to the 1536 the vector columns hold.

UPDATE dist_rag.vector_indexes
SET
  ai_provider = 'OPENAI'::dist_rag.ai_provider_enum,
  embedding_model_params = '{"model": "text-embedding-3-large", "dimensions": 1536}'::jsonb
WHERE schema_name = 'datapacks'
  AND index_name IN ('knowledge_bases', 'conversation_history_search')
  AND ai_provider = 'AWS_BEDROCK'::dist_rag.ai_provider_enum;
