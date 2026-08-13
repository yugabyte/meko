# syntax=docker/dockerfile:1
# Generic yb-amp extension image (the common path for pure-C / PGXS extensions).
#
# Fetches an extension's source tarball, optionally runs a PREP hook and applies
# a patch from compute/image/patches/, builds it against the ABI-anchor PG18
# base (pg18-build.Dockerfile), and repackages ONLY the install artifacts into a
# minimal `FROM scratch` tree:
#
#   /share/postgresql/extension/   <- *.control + *.sql   (extension_control_path)
#   /lib/postgresql/               <- *.so                 (dynamic_library_path)
#
# That tree is what the endpoint mounts as a Kubernetes ImageVolume at
# /extensions/<name>; the two GUCs point PostgreSQL at the two subdirs.
#
# Complex extensions (Rust/pgrx, heavy system deps) that don't fit the
# fetch->patch->make->make install shape get their own Dockerfile instead; this
# one stays deliberately small. See deploy/extensions/README.md.
#
# Build (from the repo root) — driven by build.sh, but standalone:
#   docker build -t amp-ext-pgvector:0.8.2 \
#     --build-arg PG_BASE=amp-pg18-build:local \
#     --build-arg NAME=pgvector --build-arg VERSION=0.8.2 \
#     --build-arg URL=https://github.com/pgvector/pgvector/archive/refs/tags/v0.8.2.tar.gz \
#     --build-arg SHA256=<sha> --build-arg PATCH=pgvector-pg18.patch --build-arg TRUSTED=true \
#     -f deploy/extensions/extension.Dockerfile .

ARG PG_BASE=amp-pg18-build:local

FROM ${PG_BASE} AS build
WORKDIR /home/nonroot/build

# The shared patch set (reused verbatim from the legacy compute image).
COPY --chown=nonroot compute/image/patches /patches

# NAME     - display/package name (informational; drives the image tag).
# VERSION  - upstream version being built.
# URL      - source tarball URL (fetched + checksum-verified).
# SHA256   - expected sha256 of the tarball.
# PATCH    - filename under compute/image/patches/ to apply (-p1), or empty.
# PREP     - shell snippet run inside the extracted source dir before the patch
#            (e.g. fetch an extra per-version SQL file). Empty = skip.
# TRUSTED  - "true" appends `trusted = true` to every shipped *.control that
#            lacks it, so the extension can be created by a non-superuser
#            (matches the legacy image's behaviour for common extensions).
ARG NAME
ARG VERSION
ARG URL
ARG SHA256
ARG PATCH=""
ARG PREP=""
ARG TRUSTED=""

# stage/ and out/ live under the writable build dir: the build-tools base runs as
# the unprivileged `nonroot` user, which can't create directories at the
# filesystem root (/stage, /out). The final scratch stage copies from out/.
# The optional gh_token secret authenticates the source fetch for extensions
# whose tarball lives in a private repo (e.g. hexabridge on a private GitHub);
# build.sh passes it as a BuildKit secret so the token never lands in a layer.
# required=false keeps public extensions (pgvector, pg_cron) building with no
# secret mounted. The checksum is still verified, so a bad/missing token fails
# closed (404 body -> sha mismatch) rather than shipping garbage.
# mode=0444: this stage runs as the base image's `nonroot` user, but a secret
# mount defaults to root-owned 0400 — unreadable here, which silently drops the
# auth header and 404s the private fetch. World-readable is safe: the mount is an
# ephemeral tmpfs scoped to this RUN, never written to a layer.
RUN --mount=type=secret,id=gh_token,required=false,mode=0444 \
    set -eux; \
    stage=/home/nonroot/build/stage; \
    out=/home/nonroot/build/out; \
    auth=""; \
    if [ -s /run/secrets/gh_token ]; then \
        auth="Authorization: Bearer $(cat /run/secrets/gh_token)"; \
    fi; \
    curl -fsSL ${auth:+-H "$auth"} "$URL" -o src.tgz; \
    echo "${SHA256}  src.tgz" | sha256sum -c -; \
    mkdir src; \
    tar -xzf src.tgz --strip-components=1 -C src; \
    cd src; \
    if [ -n "$PREP" ]; then sh -eu -c "$PREP"; fi; \
    if [ -n "$PATCH" ]; then patch --batch --fuzz=0 -p1 < "/patches/${PATCH}"; fi; \
    make -j "$(nproc)" USE_PGXS=1 OPTFLAGS="" PG_CONFIG="$PG_CONFIG"; \
    make install USE_PGXS=1 OPTFLAGS="" PG_CONFIG="$PG_CONFIG" DESTDIR="$stage"; \
    SHAREDIR="$($PG_CONFIG --sharedir)"; \
    PKGLIBDIR="$($PG_CONFIG --pkglibdir)"; \
    mkdir -p "$out/share/postgresql/extension" "$out/lib/postgresql"; \
    cp -a "${stage}${SHAREDIR}/extension/." "$out/share/postgresql/extension/"; \
    if [ -d "${stage}${PKGLIBDIR}" ]; then cp -a "${stage}${PKGLIBDIR}/." "$out/lib/postgresql/"; fi; \
    if [ "$TRUSTED" = "true" ]; then \
        for c in "$out"/share/postgresql/extension/*.control; do \
            grep -q '^trusted' "$c" || echo 'trusted = true' >> "$c"; \
        done; \
    fi; \
    ls -R "$out"

# Minimal artifact-only image: nothing but the extension tree, so the
# ImageVolume mount is tiny and has no attack surface.
FROM scratch
COPY --from=build /home/nonroot/build/out/ /
