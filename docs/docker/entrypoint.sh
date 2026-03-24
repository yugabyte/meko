#!/usr/bin/env bash
set -euo pipefail

DOCS_DIR="/workspace/docs"

# --- setup-fork mode ---
if [ "${1:-}" = "setup-fork" ]; then
    echo "==> Setting up fork..."

    if ! gh auth status &>/dev/null; then
        echo "ERROR: GitHub CLI is not authenticated."
        echo ""
        echo "Run the following inside the container:"
        echo "  gh auth login"
        echo ""
        echo "Then re-run: ./docs-dev.sh setup-fork"
        exit 1
    fi

    GITHUB_USER=$(gh api user --jq '.login')
    if [[ ! "${GITHUB_USER}" =~ ^[a-zA-Z0-9_-]+$ ]]; then
        echo "ERROR: Unexpected GitHub username format: ${GITHUB_USER}"
        exit 1
    fi
    echo "    GitHub user: ${GITHUB_USER}"

    if ! gh api "repos/${GITHUB_USER}/meko" &>/dev/null; then
        echo "    Fork not found. Creating fork..."
        cd /workspace
        gh repo fork --remote=false
    else
        echo "    Fork already exists."
    fi

    cd /workspace
    git remote set-url origin "https://github.com/${GITHUB_USER}/meko.git" 2>/dev/null \
        || git remote add origin "https://github.com/${GITHUB_USER}/meko.git"
    git remote set-url upstream "https://github.com/yugabyte/meko.git" 2>/dev/null \
        || git remote add upstream "https://github.com/yugabyte/meko.git"

    echo "    Remotes configured:"
    git remote -v
    echo "==> Fork setup complete."
    exit 0
fi

# --- Normal startup ---
cd "${DOCS_DIR}"

# If HUGO_POLL is set, create a wrapper that injects --poll into "hugo server" calls.
# This enables file-change polling for bind mounts that don't support inotify (e.g. Colima).
if [ -n "${HUGO_POLL:-}" ]; then
    # Validate HUGO_POLL value (must be a duration like 500ms, 1s, etc.)
    if [[ ! "${HUGO_POLL}" =~ ^[0-9]+(ms|s)$ ]]; then
        echo "ERROR: Invalid HUGO_POLL value '${HUGO_POLL}'. Expected format: 700ms, 1s, etc."
        exit 1
    fi
    # Move real binary out of the way, then create wrapper at the original path
    mv /usr/local/bin/hugo /usr/local/bin/hugo-real
    POLL_VAL="${HUGO_POLL}"
    cat > /usr/local/bin/hugo <<WRAPPER
#!/usr/bin/env bash
if [[ "\$*" == *"server"* ]]; then
    exec /usr/local/bin/hugo-real "\$@" --poll "${POLL_VAL}"
else
    exec /usr/local/bin/hugo-real "\$@"
fi
WRAPPER
    chmod +x /usr/local/bin/hugo
    echo "==> File polling enabled (${HUGO_POLL})"
fi

# Install npm dependencies if needed
if [ ! -d "node_modules" ] || [ "package.json" -nt "node_modules/.package-lock.json" ]; then
    echo "==> Installing npm dependencies..."
    npm install
fi

# Ensure Hugo modules (Docsy theme) are cached
echo "==> Fetching Hugo modules..."
hugo mod get

echo "==> Starting docs server..."
exec "$@"
