-- 0019: extensions an operator registers, as opposed to the ones compiled in.
--
-- The externalized-extension catalog has until now been a Go table
-- (controller/internal/extensions/catalog.go): the set of images this repo
-- builds, versioned in lockstep with deploy/extensions/extensions.yaml. That is
-- the right home for what we build and cannot be the home for what we do not —
-- pinning a third-party image reference in the product means every new extension
-- is a controller release, and it puts someone else's registry in our source.
--
-- This table is the other half: a name, the image to mount, and the metadata the
-- endpoint needs to make that image usable. A branch opts in by name exactly as
-- it does for a built-in, so nothing downstream of the catalog knows the
-- difference.
--
-- `name` is the primary key rather than a surrogate id because the name IS the
-- identity everywhere else: it is the mount path (/extensions/<name>), the
-- opt-in token stored on amp_registry.branches.extensions, and the
-- shared_preload_libraries entry. A surrogate id would have to be resolved back
-- to the name at every use. Registration is therefore an upsert on the name, and
-- a name that collides with a built-in is refused in Go (extensions.Register) —
-- the built-in set is not in the database to constrain against.
--
-- The layout columns describe where the image keeps its pieces, each relative to
-- the mount root. They are NOT NULL with '' / '[]' / '{}' defaults so "not
-- declared" is one value rather than two: an empty share_path means the layout
-- deploy/extensions produces, which is what the chart already defaults to.
--
-- abi_extra_exempt waives the server's FMGR_ABI_EXTRA check for the named
-- modules, which is what makes an image built against stock PostgreSQL loadable.
-- Getting it wrong crashes the postmaster, so the API that writes this column is
-- admin-only; the column itself carries no privilege.

CREATE TABLE IF NOT EXISTS amp_registry.extensions (
    name              text        PRIMARY KEY,
    image             text        NOT NULL,
    version           text        NOT NULL DEFAULT '',
    description       text        NOT NULL DEFAULT '',
    -- Shared-library basename when it differs from `name` (the
    -- shared_preload_libraries entry for a preload extension). '' means `name`.
    library           text        NOT NULL DEFAULT '',
    preload           boolean     NOT NULL DEFAULT false,
    -- Relative to /extensions/<name>. '' takes the deploy/extensions default
    -- (share/postgresql, lib/postgresql).
    share_path        text        NOT NULL DEFAULT '',
    lib_path          text        NOT NULL DEFAULT '',
    -- Dirs of system shared libraries the image bundles, appended to
    -- LD_LIBRARY_PATH: a JSON array of relative paths.
    library_path      jsonb       NOT NULL DEFAULT '[]'::jsonb,
    -- Runtime data dirs by environment variable name: a JSON object of
    -- {ENV_NAME: relative path}.
    env_paths         jsonb       NOT NULL DEFAULT '{}'::jsonb,
    -- Modules exempt from the FMGR_ABI_EXTRA match: a JSON array of bare module
    -- names.
    abi_extra_exempt  jsonb       NOT NULL DEFAULT '[]'::jsonb,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
