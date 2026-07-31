# syntax=docker/dockerfile:1

FROM ghcr.io/astral-sh/uv:python3.11-bookworm-slim

WORKDIR /app

ENV PYTHONUNBUFFERED=1 \
    UV_COMPILE_BYTECODE=1 \
    UV_LINK_MODE=copy

# Install deps first so source-only changes reuse this layer.
COPY pyproject.toml uv.lock ./
RUN --mount=type=cache,target=/root/.cache/uv \
    uv sync --frozen --no-install-project --no-dev

COPY README.md ./
COPY src/ src/

RUN --mount=type=cache,target=/root/.cache/uv \
    uv sync --frozen --no-dev

ENV PATH="/app/.venv/bin:${PATH}" \
    AMP_MCP_TRANSPORT=http \
    AMP_MCP_HOST=0.0.0.0 \
    AMP_MCP_PORT=8000

EXPOSE 8000

RUN useradd --create-home --shell /usr/sbin/nologin app \
    && chown -R app:app /app
USER app

ENTRYPOINT ["uv", "run", "--frozen", "src/server.py"]
