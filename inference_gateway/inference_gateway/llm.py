"""LLM provider factory.

Both providers return a `BaseChatModel`, so the rest of the code is
provider-agnostic. Adding a new provider (Anthropic direct, local Ollama,
etc.) means adding one branch here — no other file changes needed.
"""

from __future__ import annotations

from langchain_core.language_models import BaseChatModel

from .config import AppConfig


def build_llm(cfg: AppConfig) -> BaseChatModel:
    """Build the chat model implied by `cfg.llm_provider`.

    Raises `ValueError` for unknown providers so misconfiguration fails fast
    at startup instead of on the first chat request.

    `cfg.llm_max_tokens` is passed to the underlying provider so the gateway
    has an explicit ceiling on output length. Without it Bedrock / OpenAI
    fall back to their model-specific defaults (Claude Sonnet 4.5 ~8K
    tokens), which presents to the user as a mid-sentence cutoff when a
    long answer brushes the limit. Centralising the cap here means
    `chain.py`'s system-prompt budget hint stays in sync with the actual
    ceiling.
    """
    provider = cfg.llm_provider.lower()
    if provider == "openai":
        # Reads OPENAI_API_KEY from the environment.
        from langchain_openai import ChatOpenAI

        return ChatOpenAI(
            model=cfg.llm_model,
            temperature=0,
            max_tokens=cfg.llm_max_tokens,
        )

    if provider == "bedrock":
        # Reads from the standard AWS credential chain (env / shared creds /
        # instance role). `region_name` is explicit so the model id resolves
        # to the right region's invoke endpoint.
        from langchain_aws import ChatBedrockConverse

        return ChatBedrockConverse(
            model_id=cfg.llm_model,
            region_name=cfg.aws_region,
            temperature=0,
            max_tokens=cfg.llm_max_tokens,
        )

    raise ValueError(
        f"Unknown LLM provider: {cfg.llm_provider!r}. Supported: openai, bedrock."
    )
