// Package migrations embeds the SQL migration files for the
// controller-owned amp_voyager_control schema. It shares the controller's
// golang-migrate machinery (db.MigrateFS) but targets a separate database and
// schema. It must never create or alter ybvoyager_visualizer.* objects, which
// Voyager itself owns and migrates.
package migrations

import "embed"

// ControlSchema is the schema the Voyager migration control tables (migration,
// migration_run) live in — the controller-owned counterpart to the
// Voyager-owned ybvoyager_visualizer.* projection. It mirrors how the endpoint
// resource model is pinned to amp_registry.
const ControlSchema = "amp_voyager_control"

//go:embed *.sql
var FS embed.FS
