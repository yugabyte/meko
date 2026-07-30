"""Mem0 memory helpers for the CLI agent."""

from __future__ import annotations

from typing import Any, Dict, Iterable, List, Optional

from mem0 import Memory

try:
    from system_agent.config.settings import get_settings
    from system_agent.config.logging import get_logger
except ModuleNotFoundError:  # Allow running from within the cli/ directory.
    from system_agent.config.settings import get_settings
    from system_agent.config.logging import get_logger

# Get logger for memory operations
logger = get_logger("memory")


def build_mem0_memory() -> Memory:
    """Build a mem0 Memory instance with pgvector and Apache AGE."""
    settings = get_settings()
    config = {
        "llm": {
            "provider": "openai",
            "config": {"model": settings.openai_model, "api_key": settings.openai_api_key},
        },
        "embedder": {
            "provider": "openai",
            "config": {
                "model": settings.openai_embedding_model,
                "api_key": settings.openai_api_key,
                "embedding_dims": 1536,  # text-embedding-3-small produces 1536-dim vectors
            },
        },
        "vector_store": {
            "provider": "pgvector",
            "config": {
                "host": settings.yugabyte_host,
                "port": settings.yugabyte_port,
                "dbname": settings.mem0_db,
                "user": settings.yugabyte_user,
                "password": settings.yugabyte_password,
            },
        },
        "graph_store": {
            "provider": "apache_age",
            "config": {
                "host": settings.graph_host,
                "port": settings.graph_port,
                "database": settings.graph_db,
                "user": settings.graph_user,
                "password": settings.graph_password,
                "graph_name": settings.graph_name,
            },
            "threshold": 0.7  # Similarity threshold for node matching (0.0 to 1.0)
        },
    }
    logger.info("Building mem0 Memory instance with pgvector + Apache AGE")
    return Memory.from_config(config)


def add_memory(
    memory: Memory,
    text: str,
    user_id: str,
    metadata: Optional[Dict[str, Any]] = None,
) -> None:
    """Add a memory to the store."""
    if not text.strip():
        logger.debug("Skipping empty memory text")
        return
    try:
        result = memory.add(text, user_id=user_id, metadata=metadata or {})
        logger.info(
            "MEMORY ADDED | user_id=%s | role=%s | text=%s",
            user_id,
            metadata.get("role", "unknown") if metadata else "unknown",
            text,
        )
        if result:
            logger.debug("Add result: %s", result)
    except Exception as e:
        logger.error("Failed to add memory | user_id=%s | error=%s", user_id, str(e))


def search_memories(memory: Memory, query: str, user_id: str, limit: int = 5) -> List[Dict[str, Any]]:
    """
    Search for relevant memories.
    
    Returns:
        List of memory dicts, each containing 'memory' key with the text content.
    """
    try:
        raw_results = memory.search(query, user_id=user_id, limit=limit)
        logger.debug("Raw search results: %s", raw_results)
        
        # Handle the dict format: {'results': [...], 'relations': [...]}
        if isinstance(raw_results, dict):
            memory_list = raw_results.get("results", [])
            relations = raw_results.get("relations", [])
            logger.info(
                "MEMORY SEARCH | user_id=%s | query=%s | memories_count=%d | relations_count=%d",
                user_id,
                query[:50] + "..." if len(query) > 50 else query,
                len(memory_list),
                len(relations),
            )
        else:
            # Fallback if format is different (plain list)
            memory_list = raw_results if raw_results else []
            logger.info(
                "MEMORY SEARCH | user_id=%s | query=%s | results_count=%d",
                user_id,
                query[:50] + "..." if len(query) > 50 else query,
                len(memory_list),
            )
        
        # Log each retrieved memory
        if memory_list:
            for i, item in enumerate(memory_list):
                memory_text = _extract_memory_text(item)
                score = item.get("score", "N/A") if isinstance(item, dict) else "N/A"
                logger.info("  RETRIEVED [%d] (score=%.3f) | %s", i + 1, score if isinstance(score, float) else 0, memory_text)
        
        return memory_list
    except Exception as e:
        logger.error("Memory search failed | user_id=%s | error=%s", user_id, str(e))
        return []


def get_all_memories(memory: Memory, user_id: str) -> List[Dict[str, Any]]:
    """Get all memories for a user."""
    try:
        raw_results = memory.get_all(user_id=user_id)
        
        # Handle dict format if applicable
        if isinstance(raw_results, dict):
            memory_list = raw_results.get("results", raw_results.get("memories", []))
        else:
            memory_list = raw_results if raw_results else []
        
        logger.info("MEMORY GET_ALL | user_id=%s | total_count=%d", user_id, len(memory_list))
        return memory_list
    except Exception as e:
        logger.error("Get all memories failed | user_id=%s | error=%s", user_id, str(e))
        return []


def clear_memories(memory: Memory, user_id: str) -> bool:
    """Clear all memories for a user."""
    try:
        if hasattr(memory, "delete_all"):
            memory.delete_all(user_id=user_id)
            logger.info("MEMORY CLEARED | user_id=%s | method=delete_all", user_id)
            return True
        if hasattr(memory, "clear"):
            memory.clear(user_id=user_id)
            logger.info("MEMORY CLEARED | user_id=%s | method=clear", user_id)
            return True
    except Exception as e:
        logger.error("Failed to clear memories | user_id=%s | error=%s", user_id, str(e))
        return False
    return False


def _extract_memory_text(item: Any) -> str:
    """Extract text content from a memory item."""
    if isinstance(item, dict):
        # mem0 uses 'memory' key for the text content
        if "memory" in item and item["memory"]:
            return str(item["memory"])
        # Fallback to other possible keys
        for key in ("text", "content"):
            if key in item and item[key]:
                return str(item[key])
        return str(item)
    return str(item)


def format_memory_items(items: Iterable[Any], include_score: bool = True) -> str:
    """
    Format memory items for LLM context injection.
    
    Args:
        items: List of memory items from mem0
        include_score: Whether to include relevance score (default True)
    
    Returns:
        Formatted string with memories and optional scores
    """
    formatted = []
    for item in items:
        memory_text = _extract_memory_text(item)
        if memory_text.strip():
            if include_score and isinstance(item, dict) and "score" in item:
                score = item["score"]
                # Format score as percentage for readability
                score_pct = score * 100 if isinstance(score, float) else 0
                formatted.append(f"{memory_text} (relevance: {score_pct:.1f}%)")
            else:
                formatted.append(memory_text)
    
    if not formatted:
        return ""
    
    return "\n".join(f"- {entry}" for entry in formatted)
