"""Environment-driven settings for the CLI agent."""

from __future__ import annotations

from dataclasses import dataclass
import os

from dotenv import load_dotenv


@dataclass(frozen=True)
class Settings:
    # OpenAI settings
    openai_api_key: str
    openai_model: str
    openai_embedding_model: str
    
    # YugabyteDB (common for vector store and system agent)
    yugabyte_host: str
    yugabyte_port: int
    yugabyte_user: str
    yugabyte_password: str
    mem0_db: str  # Database name for mem0 vector store
    system_agent_db: str  # Database name for system agent operations
    
    # Graph DB (PostgreSQL with Apache AGE)
    graph_host: str
    graph_port: int
    graph_db: str
    graph_user: str
    graph_password: str
    graph_name: str
    
    # Application settings
    default_user_id: str
    system_prompt: str


def _require_env(name: str) -> str:
    value = os.getenv(name, "").strip()
    if not value:
        raise RuntimeError(f"Missing required environment variable: {name}")
    return value


def _get_env(name: str, default: str = "") -> str:
    return os.getenv(name, default)


def get_settings() -> Settings:
    load_dotenv()

    return Settings(
        # OpenAI settings
        openai_api_key=_require_env("OPENAI_API_KEY"),
        openai_model=_get_env("OPENAI_MODEL", "gpt-4o"),
        openai_embedding_model=_get_env("OPENAI_EMBEDDING_MODEL", "text-embedding-3-small"),
        
        # YugabyteDB (common for vector store and system agent)
        yugabyte_host=_get_env("YUGABYTE_HOST", "localhost"),
        yugabyte_port=int(_get_env("YUGABYTE_PORT", "5433")),
        yugabyte_user=_get_env("YUGABYTE_USER", "yugabyte"),
        yugabyte_password=_require_env("YUGABYTE_PASSWORD"),
        mem0_db=_get_env("MEM0_DB", "mem0_db"),
        system_agent_db=_get_env("SYSTEM_AGENT_DB", "yugabyte"),
        
        # Graph DB (PostgreSQL with Apache AGE)
        graph_host=_get_env("GRAPH_HOST", "localhost"),
        graph_port=int(_get_env("GRAPH_PORT", "5432")),
        graph_db=_get_env("GRAPH_DB", "postgres"),
        graph_user=_get_env("GRAPH_USER", "postgres"),
        graph_password=_require_env("GRAPH_PASSWORD"),
        graph_name=_get_env("GRAPH_NAME", "yuga_ai_system_graph"),
        
        # Application settings
        default_user_id=_get_env("DEFAULT_USER_ID", "default"),
        system_prompt=_get_env(
            "SYSTEM_PROMPT",
            "You are a helpful assistant. Use any relevant stored memories when responding.",
        ),
    )
