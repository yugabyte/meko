"""LLM provider factory.

Both providers return a `BaseChatModel`, so the rest of the code is
provider-agnostic. Adding a new provider (Anthropic direct, local Ollama,
etc.) means adding one branch here — no other file changes needed.
"""

from __future__ import annotations

from langchain_core.language_models import BaseChatModel

from .config import AppConfig


def build_llm(
    cfg: AppConfig,
    *,
    model: str | None = None,
    max_tokens: int | None = None,
) -> BaseChatModel:
    """Build the chat model implied by `cfg.llm_provider`.

    `model` / `max_tokens` override the config defaults — used to build a
    separate, smaller summarization client without a second provider branch.

    Raises `ValueError` for unknown providers so misconfiguration fails fast
    at startup instead of on the first chat request.
    """
    provider = cfg.llm_provider.lower()
    model_id = model or cfg.llm_model
    mt = max_tokens or cfg.llm_max_tokens
    if provider == "openai":
        # Reads OPENAI_API_KEY from the environment.
        from langchain_openai import ChatOpenAI

        return ChatOpenAI(model=model_id, temperature=0, max_tokens=mt)

    if provider == "bedrock":
        # Reads from the standard AWS credential chain (env / shared creds /
        # instance role). `region_name` is explicit so the model id resolves
        # to the right region's invoke endpoint.
        from langchain_aws import ChatBedrockConverse

        return ChatBedrockConverse(
            model_id=model_id,
            region_name=cfg.aws_region,
            temperature=0,
            max_tokens=mt,
        )

    raise ValueError(
        f"Unknown LLM provider: {cfg.llm_provider!r}. Supported: openai, bedrock."
    )


def build_summary_llm(cfg: AppConfig) -> BaseChatModel:
    """Small/cheap client for summarizing conversation-history chunks on a
    cache hit. Uses `cfg.conv_summary_model` (falls back to the main model)
    with a small output budget since summaries are short."""
    return build_llm(cfg, model=cfg.conv_summary_model or cfg.llm_model, max_tokens=1024)
