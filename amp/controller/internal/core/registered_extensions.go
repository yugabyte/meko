package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/yugabyte/amp/controller/internal/extensions"
)

// This file implements extensions.Store for both backends: the extensions an
// operator registered through the API (amp_registry.extensions, migration 0019),
// as distinct from the built-in table compiled into the binary.

// scanExtension reads one row into an extensions.Extension. The three JSON
// columns are decoded here so callers never see the wire shape.
func scanExtension(row pgx.Row) (extensions.Extension, error) {
	var (
		e                               extensions.Extension
		libraryPath, envPaths, abiExtra []byte
	)
	err := row.Scan(&e.Name, &e.ImageRef, &e.Version, &e.Description, &e.Library,
		&e.Preload, &e.Layout.SharePath, &e.Layout.LibPath,
		&libraryPath, &envPaths, &abiExtra)
	if err != nil {
		return extensions.Extension{}, err
	}
	if err := json.Unmarshal(libraryPath, &e.Layout.LibraryPath); err != nil {
		return extensions.Extension{}, fmt.Errorf("decode library_path for %q: %w", e.Name, err)
	}
	if err := json.Unmarshal(envPaths, &e.Layout.EnvPaths); err != nil {
		return extensions.Extension{}, fmt.Errorf("decode env_paths for %q: %w", e.Name, err)
	}
	if err := json.Unmarshal(abiExtra, &e.ABIExtraExempt); err != nil {
		return extensions.Extension{}, fmt.Errorf("decode abi_extra_exempt for %q: %w", e.Name, err)
	}
	return e, nil
}

const extensionSelect = `SELECT name, image, version, description, library, preload,
	        share_path, lib_path, library_path, env_paths, abi_extra_exempt
	   FROM amp_registry.extensions`

// ListExtensions returns every registered extension, name-ordered.
func (b *PgBackend) ListExtensions(ctx context.Context) ([]extensions.Extension, error) {
	rows, err := b.pool.Query(ctx, extensionSelect+` ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list extensions: %w", err)
	}
	defer rows.Close()
	var out []extensions.Extension
	for rows.Next() {
		e, err := scanExtension(rows)
		if err != nil {
			return nil, fmt.Errorf("scan extension: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetExtension reads one registered extension. A miss is (_, false, nil), not an
// error: "is this name registered?" is the caller's validation question.
func (b *PgBackend) GetExtension(ctx context.Context, name string) (extensions.Extension, bool, error) {
	e, err := scanExtension(b.pool.QueryRow(ctx, extensionSelect+` WHERE name = $1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return extensions.Extension{}, false, nil
	}
	if err != nil {
		return extensions.Extension{}, false, fmt.Errorf("get extension %q: %w", name, err)
	}
	return e, true, nil
}

// PutExtension creates or fully replaces a registration. Full replacement rather
// than a merge: the name is the identity and the body is the whole definition,
// so a field the caller omits means "not set" and not "keep what was there" —
// otherwise clearing a layout path would be impossible.
func (b *PgBackend) PutExtension(ctx context.Context, e extensions.Extension) error {
	libraryPath, err := json.Marshal(nonNilSlice(e.Layout.LibraryPath))
	if err != nil {
		return fmt.Errorf("encode library_path: %w", err)
	}
	envPaths, err := json.Marshal(nonNilMap(e.Layout.EnvPaths))
	if err != nil {
		return fmt.Errorf("encode env_paths: %w", err)
	}
	abiExtra, err := json.Marshal(nonNilSlice(e.ABIExtraExempt))
	if err != nil {
		return fmt.Errorf("encode abi_extra_exempt: %w", err)
	}
	if _, err := b.pool.Exec(ctx,
		`INSERT INTO amp_registry.extensions
		     (name, image, version, description, library, preload,
		      share_path, lib_path, library_path, env_paths, abi_extra_exempt)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10::jsonb, $11::jsonb)
		 ON CONFLICT (name) DO UPDATE SET
		     image = EXCLUDED.image,
		     version = EXCLUDED.version,
		     description = EXCLUDED.description,
		     library = EXCLUDED.library,
		     preload = EXCLUDED.preload,
		     share_path = EXCLUDED.share_path,
		     lib_path = EXCLUDED.lib_path,
		     library_path = EXCLUDED.library_path,
		     env_paths = EXCLUDED.env_paths,
		     abi_extra_exempt = EXCLUDED.abi_extra_exempt,
		     updated_at = now()`,
		e.Name, e.ImageRef, e.Version, e.Description, e.Library, e.Preload,
		e.Layout.SharePath, e.Layout.LibPath,
		string(libraryPath), string(envPaths), string(abiExtra)); err != nil {
		return fmt.Errorf("put extension %q: %w", e.Name, err)
	}
	return nil
}

// DeleteExtension removes a registration. Deleting one nothing references is the
// caller's precondition (see ExtensionUsers), so this is unconditional.
func (b *PgBackend) DeleteExtension(ctx context.Context, name string) error {
	if _, err := b.pool.Exec(ctx,
		`DELETE FROM amp_registry.extensions WHERE name = $1`, name); err != nil {
		return fmt.Errorf("delete extension %q: %w", name, err)
	}
	return nil
}

// ExtensionUsers returns the ids of live branches whose opted-in set names this
// extension — the gate on deregistering one. Reads the same jsonb array
// AddBranchExtensions writes.
func (b *PgBackend) ExtensionUsers(ctx context.Context, name string) ([]string, error) {
	payload, err := json.Marshal([]string{name})
	if err != nil {
		return nil, fmt.Errorf("encode extension name: %w", err)
	}
	rows, err := b.pool.Query(ctx,
		`SELECT branch_id::text FROM amp_registry.branches
		  WHERE dropped_at IS NULL AND extensions @> $1::jsonb
		  ORDER BY branch_id`, string(payload))
	if err != nil {
		return nil, fmt.Errorf("find extension users: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan extension user: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// nonNilSlice keeps a nil slice from marshalling to JSON null, which the NOT
// NULL jsonb columns reject.
func nonNilSlice(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func nonNilMap(v map[string]string) map[string]string {
	if v == nil {
		return map[string]string{}
	}
	return v
}
