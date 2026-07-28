// Package web embeds the YCP console single-page app.
package web

import "embed"

// FS holds the console assets served at "/".
//
//go:embed console.html
var FS embed.FS
