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
#     --build-arg PATCH=pgvector-pg18.patch --build-arg TRUSTED=true \
#     -f deploy/extensions/extension.Dockerfile .

ARG PG_BASE=amp-pg18-build:local

FROM ${PG_BASE} AS build
WORKDIR /home/nonroot/build

# The shared patch set (reused verbatim from the legacy compute image).
COPY --chown=nonroot compute/image/patches /patches

# NAME     - display/package name (informational; drives the image tag).
# VERSION  - upstream version being built.
# URL      - source tarball URL, verified against its pin in
#            downloads_checksum.txt (scripts/safe_curl.sh).
# PATCH    - filename under compute/image/patches/ to apply (-p1), or empty.
# PREP     - shell snippet run inside the extracted source dir before the patch
#            (e.g. fetch an extra per-version SQL file). Empty = skip. It
#            fetches with `sh "$SAFE_CURL" fetch <url> <dest>`, so what it
#            downloads is pinned in the same manifest.
# TRUSTED  - "true" appends `trusted = true` to every shipped *.control that
#            lacks it, so the extension can be created by a non-superuser
#            (matches the legacy image's behaviour for common extensions).
ARG NAME
ARG VERSION
ARG URL
ARG PATCH=""
ARG PREP=""
ARG TRUSTED=""
# Extensions whose top-level `make` builds more than we ship set this to the one
# target that builds AND installs what we want, run in place of the default
# build-then-install pair. documentdb needs it: its bare `make` also descends
# into internal/pg_documentdb_distributed, which wants Citus.
ARG MAKE_TARGETS=""

# stage/ and out/ live under the writable build dir: the build-tools base runs as
# the unprivileged `nonroot` user, which can't create directories at the
# filesystem root (/stage, /out). The final scratch stage copies from out/.
# The optional gh_token secret authenticates the source fetch for extensions
# whose tarball lives in a private repo (e.g. hexabridge on a private GitHub);
# build.sh passes it as a BuildKit secret so the token never lands in a layer.
# required=false keeps public extensions (pgvector, pg_cron) building with no
# secret mounted. The checksum is still verified, so a bad/missing token fails
# closed (a 404 fails the fetch, and anything else fails the checksum) rather
# than shipping garbage.
# mode=0444: this stage runs as the base image's `nonroot` user, but a secret
# mount defaults to root-owned 0400 — unreadable here, which silently drops the
# auth header and 404s the private fetch. World-readable is safe: the mount is an
# ephemeral tmpfs scoped to this RUN, never written to a layer.
# The checksum manifest and its checker are bind-mounted, not copied, so the
# build stage carries neither.
RUN --mount=type=secret,id=gh_token,required=false,mode=0444 \
    --mount=type=bind,source=downloads_checksum.txt,target=/safe_curl/downloads_checksum.txt \
    --mount=type=bind,source=scripts/safe_curl.sh,target=/safe_curl/scripts/safe_curl.sh \
    set -eux; \
    SAFE_CURL=/safe_curl/scripts/safe_curl.sh; export SAFE_CURL; \
    stage=/home/nonroot/build/stage; \
    out=/home/nonroot/build/out; \
    if [ -s /run/secrets/gh_token ]; then \
        # safe_curl.sh fetches anonymously, so an authenticated fetch is its
        # own curl, verified after the fact.
        curl -fsSL -H "Authorization: Bearer $(cat /run/secrets/gh_token)" "$URL" -o src.tgz; \
        sh "$SAFE_CURL" verify "$URL" src.tgz; \
    else \
        sh "$SAFE_CURL" fetch "$URL" src.tgz; \
    fi; \
    mkdir src; \
    tar -xzf src.tgz --strip-components=1 -C src; \
    cd src; \
    if [ -n "$PREP" ]; then sh -eu -c "$PREP"; fi; \
    if [ -n "$PATCH" ]; then patch --batch --fuzz=0 -p1 < "/patches/${PATCH}"; fi; \
    if [ -n "$MAKE_TARGETS" ]; then \
        # Serial on purpose. MAKE_TARGETS names a target that BUILDS AND
        # INSTALLS in one invocation (documentdb's install-documentdb recurses
        # into its three modules with `install`), so unlike the else-branch below
        # there is no separate serial install phase to be safe in. With -j, make
        # inside a module can run the install of a .so while its own link is
        # still in flight: ninstall.sh copies a partially written file, and the
        # postmaster then dies at preload with
        #   could not load library ".../pg_documentdb_extended_rum_core.so":
        #   file too short
        # intermittently — green on one run, FATAL on the next. The missing
        # dependency is upstream's; -j is what turns it into a corrupt image.
        make $MAKE_TARGETS USE_PGXS=1 OPTFLAGS="" PG_CONFIG="$PG_CONFIG" DESTDIR="$stage"; \
    else \
        make -j "$(nproc)" USE_PGXS=1 OPTFLAGS="" PG_CONFIG="$PG_CONFIG"; \
        make install USE_PGXS=1 OPTFLAGS="" PG_CONFIG="$PG_CONFIG" DESTDIR="$stage"; \
    fi; \
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
