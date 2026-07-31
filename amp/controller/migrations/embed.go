// Package migrations embeds the controller's SQL migration files so they ship
// inside the binary and run on startup.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
