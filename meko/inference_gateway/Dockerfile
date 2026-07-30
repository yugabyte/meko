# syntax=docker/dockerfile:1.7

# ---- Builder: install the package + deps into a self-contained venv ----
FROM python:3.11-slim AS builder

ENV PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PIP_NO_CACHE_DIR=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

WORKDIR /build

# build-essential is a safety net for any sdist-only wheels among transitive deps
# (fastapi/uvicorn/langchain-* publish manylinux wheels, but pin-resolved deps may not).
RUN apt-get update \
 && apt-get install -y --no-install-recommends build-essential \
 && rm -rf /var/lib/apt/lists/*

RUN python -m venv /opt/venv
ENV PATH="/opt/venv/bin:$PATH"

# hatchling's build backend needs both pyproject.toml and the package source present
# at install time, so we copy them together.
COPY pyproject.toml ./
COPY inference_gateway ./inference_gateway

RUN pip install --upgrade pip \
 && pip install .


# ---- Runtime: small image with just the venv + a non-root user ----
FROM python:3.11-slim AS runtime

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PATH="/opt/venv/bin:$PATH"

# ca-certificates for outbound TLS (Bedrock / OpenAI / MCP).
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*

RUN groupadd --system app && useradd --system --gid app --home-dir /app app
WORKDIR /app

COPY --from=builder /opt/venv /opt/venv

USER app

# Matches DEFAULT_PORT in inference_gateway/config.py.
EXPOSE 8088

# Use the console script declared in pyproject.toml so CLI flags pass through:
#   docker run --rm -p 8088:8088 \
#     -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY -e AWS_REGION=us-east-1 \
#     inference-gateway \
#     --llm-provider bedrock --llm-model us.amazon.nova-2-lite-v1:0
ENTRYPOINT ["inference-gateway"]
CMD []
