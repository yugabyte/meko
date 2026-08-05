// Package migrations embeds the SQL migration files for the
// controller-owned amp_voyager_control schema. It shares the controller's
// golang-migrate machinery (db.MigrateFS) but targets a separate database and
// schema. It must never create or alter ybvoyager_visualizer.* objects, which
// Voyager itself owns and migrates.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
