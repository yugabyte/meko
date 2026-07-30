"""Agent service for managing agents and their databases."""

from __future__ import annotations

import json
import re
import uuid
from typing import Any, Dict, List, Optional

import psycopg2
from psycopg2 import sql
from psycopg2.extensions import ISOLATION_LEVEL_AUTOCOMMIT

try:
    from system_agent.config.settings import get_settings, Settings
    from system_agent.config.logging import get_logger
except ModuleNotFoundError:
    from config.settings import get_settings, Settings
    from config.logging import get_logger

logger = get_logger("agent_service")

# Connection cache to reuse connections
_connection_cache: Dict[str, Any] = {}


def sanitize_db_name(name: str) -> str:
    """
    Sanitize a name to be a valid database/graph name.
    
    - Converts to lowercase
    - Replaces spaces, dots, hyphens, and other invalid chars with underscores
    - Ensures it starts with a letter
    - Truncates to 63 characters (PostgreSQL limit)
    """
    sanitized = re.sub(r'[^a-zA-Z0-9_]', '_', name.lower())
    
    if sanitized and not sanitized[0].isalpha():
        sanitized = 'agent_' + sanitized
    
    sanitized = re.sub(r'_+', '_', sanitized)
    sanitized = sanitized.rstrip('_')
    
    return sanitized[:63]


def _get_yugabyte_connection(
    settings: Settings,
    dbname: Optional[str] = None,
    autocommit: bool = False,
    use_cache: bool = True
):
    """
    Get a YugabyteDB connection, optionally from cache.
    
    Args:
        settings: Application settings (passed to avoid repeated get_settings() calls)
        dbname: Database name (defaults to system_agent_db from settings)
        autocommit: Whether to enable autocommit mode (required for CREATE DATABASE)
        use_cache: Whether to use/store connection in cache
    
    Returns:
        psycopg2 connection object
    """
    if dbname is None:
        dbname = settings.system_agent_db
    
    cache_key = f"{dbname}:{autocommit}"
    
    # Check cache first
    if use_cache and cache_key in _connection_cache:
        conn = _connection_cache[cache_key]
        try:
            conn.cursor().execute("SELECT 1")
            return conn
        except Exception:
            del _connection_cache[cache_key]
    
    # Create new connection
    conn = psycopg2.connect(
        host=settings.yugabyte_host,
        port=settings.yugabyte_port,
        dbname=dbname,
        user=settings.yugabyte_user,
        password=settings.yugabyte_password,
    )
    
    if autocommit:
        conn.set_isolation_level(ISOLATION_LEVEL_AUTOCOMMIT)
    
    if use_cache:
        _connection_cache[cache_key] = conn
    
    return conn


def close_all_connections():
    """Close all cached connections."""
    global _connection_cache
    for conn in _connection_cache.values():
        try:
            conn.close()
        except Exception:
            pass
    _connection_cache = {}


# ============================================
# Modular Mem0 Config Builders
# ============================================

def _build_llm_config_openai(settings: Settings) -> Dict[str, Any]:
    """Build LLM config for OpenAI."""
    return {
        "provider": "openai",
        "config": {
            "model": settings.openai_model,
            "api_key": settings.openai_api_key,
        },
    }


def _build_llm_config_ollama() -> Dict[str, Any]:
    """Build LLM config for Ollama."""
    return {
        "provider": "ollama",
        "config": {
            "model": "llama3.2",
            "temperature": 0,
            "max_tokens": 2000,
            "ollama_base_url": "http://localhost:11434",
        },
    }


def _build_embedder_config_openai(settings: Settings) -> Dict[str, Any]:
    """Build embedder config for OpenAI."""
    return {
        "provider": "openai",
        "config": {
            "model": settings.openai_embedding_model,
            "api_key": settings.openai_api_key,
            "embedding_dims": 1536,
        },
    }


def _build_embedder_config_ollama() -> Dict[str, Any]:
    """Build embedder config for Ollama."""
    return {
        "provider": "ollama",
        "config": {
            "model": "nomic-embed-text:latest",
            "ollama_base_url": "http://localhost:11434",
            "embedding_dims": 768,
        },
    }


def _build_vector_store_config(settings: Settings, db_name: str) -> Dict[str, Any]:
    """Build vector store config (common for both providers)."""
    return {
        "provider": "pgvector",
        "config": {
            "host": settings.yugabyte_host,
            "port": settings.yugabyte_port,
            "dbname": db_name,
            "user": settings.yugabyte_user,
            "password": settings.yugabyte_password,
        },
    }


def _build_graph_store_config(settings: Settings, graph_name: str) -> Dict[str, Any]:
    """Build graph store config (common for both providers)."""
    return {
        "provider": "apache_age",
        "config": {
            "host": settings.graph_host,
            "port": settings.graph_port,
            "database": settings.graph_db,
            "user": settings.graph_user,
            "password": settings.graph_password,
            "graph_name": graph_name,
        },
        "threshold": 0.7,
    }


def _build_mem0_config_openai(settings: Settings, db_name: str, graph_name: str) -> Dict[str, Any]:
    """Build complete mem0 config for OpenAI provider."""
    return {
        "llm": _build_llm_config_openai(settings),
        "embedder": _build_embedder_config_openai(settings),
        "vector_store": _build_vector_store_config(settings, db_name),
        "graph_store": _build_graph_store_config(settings, graph_name),
    }


def _build_mem0_config_ollama(settings: Settings, db_name: str, graph_name: str) -> Dict[str, Any]:
    """Build complete mem0 config for Ollama provider."""
    return {
        "llm": _build_llm_config_ollama(),
        "embedder": _build_embedder_config_ollama(),
        "vector_store": _build_vector_store_config(settings, db_name),
        "graph_store": _build_graph_store_config(settings, graph_name),
    }


# ============================================
# Agent CRUD Operations
# ============================================

def ensure_agents_table() -> None:
    """Ensure the agents table exists in the system database."""
    settings = get_settings()  # Single call at entry point
    
    try:
        conn = _get_yugabyte_connection(settings)
        cur = conn.cursor()
        
        cur.execute("""
            CREATE TABLE IF NOT EXISTS agents (
                id UUID PRIMARY KEY,
                name VARCHAR NOT NULL,
                metadata JSONB DEFAULT '{}',
                db VARCHAR NOT NULL
            );
        """)
        
        conn.commit()
        cur.close()
        logger.info("Agents table ensured in system database")
    except Exception as e:
        logger.error(f"Failed to ensure agents table: {e}")
        raise


def create_agent(name: str, model_provider: str) -> Dict[str, Any]:
    """
    Create a new agent with its own database and graph.
    
    Args:
        name: The agent name
        model_provider: The LLM provider ('ollama' or 'openai')
    
    Returns:
        Dict containing agent info and mem0 config
    """
    settings = get_settings()  # Single call at entry point
    
    # Generate UUID and sanitize name for database
    agent_id = str(uuid.uuid4())
    db_name = sanitize_db_name(name) + "_db"
    graph_name = sanitize_db_name(name) + "_graph"
    
    logger.info(f"Creating agent: name={name}, db={db_name}, graph={graph_name}")
    
    try:
        # 1. Insert into agents table (using system_agent_db)
        conn = _get_yugabyte_connection(settings)
        cur = conn.cursor()
        
        cur.execute(
            """
            INSERT INTO agents (id, name, metadata, db)
            VALUES (%s, %s, %s, %s)
            """,
            (agent_id, name, json.dumps({"model_provider": model_provider}), db_name)
        )
        
        conn.commit()
        cur.close()
        logger.info(f"Agent record created: id={agent_id}")
        
        # 2. Create the new database (need autocommit for CREATE DATABASE)
        admin_conn = _get_yugabyte_connection(settings, autocommit=True, use_cache=False)
        admin_cur = admin_conn.cursor()
        
        # Check if database exists
        admin_cur.execute(
            "SELECT 1 FROM pg_database WHERE datname = %s",
            (db_name,)
        )
        
        if not admin_cur.fetchone():
            admin_cur.execute(
                sql.SQL("CREATE DATABASE {}").format(sql.Identifier(db_name))
            )
            logger.info(f"Database created: {db_name}")
        else:
            logger.info(f"Database already exists: {db_name}")
        
        admin_cur.close()
        admin_conn.close()
        
        # 3. Enable vector extension in the new database
        new_db_conn = _get_yugabyte_connection(settings, dbname=db_name, autocommit=True, use_cache=False)
        new_db_cur = new_db_conn.cursor()
        
        new_db_cur.execute("CREATE EXTENSION IF NOT EXISTS vector;")
        logger.info(f"Vector extension enabled in database: {db_name}")
        
        new_db_cur.close()
        new_db_conn.close()
        
        # 4. Build mem0 config
        if model_provider == "ollama":
            mem0_config = _build_mem0_config_ollama(settings, db_name, graph_name)
        else:
            mem0_config = _build_mem0_config_openai(settings, db_name, graph_name)
        
        return {
            "success": True,
            "agent_id": agent_id,
            "name": name,
            "db_name": db_name,
            "graph_name": graph_name,
            "model_provider": model_provider,
            "mem0_config": mem0_config,
        }
        
    except Exception as e:
        logger.error(f"Failed to create agent: {e}")
        return {
            "success": False,
            "error": str(e),
        }


def list_agents() -> List[Dict[str, Any]]:
    """List all agents from the database."""
    settings = get_settings()  # Single call at entry point
    
    try:
        conn = _get_yugabyte_connection(settings)
        cur = conn.cursor()
        
        cur.execute("SELECT id, name, metadata, db FROM agents ORDER BY name")
        rows = cur.fetchall()
        
        agents = []
        for row in rows:
            agents.append({
                "id": str(row[0]),
                "name": row[1],
                "metadata": row[2] if row[2] else {},
                "db": row[3],
            })
        
        cur.close()
        
        logger.info(f"Listed {len(agents)} agents")
        return agents
        
    except Exception as e:
        logger.error(f"Failed to list agents: {e}")
        return []
