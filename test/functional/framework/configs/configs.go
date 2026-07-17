// Package configs holds the single embedded file that backs every framework
// default (the "canonical defaults" pattern): default.yaml, at the module
// root under test/framework/configs/ for discoverability. This package does
// nothing but embed and expose the bytes — core.LoadConfig parses them as
// layer zero of every config load, so it stays a leaf: no dependency on core
// or any other framework package, no import cycle.
package configs

import _ "embed"

// DefaultYAML is the raw contents of default.yaml, embedded at build time.
//
//go:embed default.yaml
var DefaultYAML []byte
