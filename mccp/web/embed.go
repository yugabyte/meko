// Package web embeds the MCCP console single-page app.
package web

import "embed"

// FS holds the console assets served at "/".
//
//go:embed console.html
var FS embed.FS
