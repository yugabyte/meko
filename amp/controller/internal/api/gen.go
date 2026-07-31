// Package api is the controller's REST API. The HTTP contract is defined in
// openapi_spec.yaml (amp-controller.md §3: API-first, versioned in the URL
// path); the models, the echo strict-server interface, and the Go client are
// generated from it into api.gen.go. The controllers (handlers.go) implement
// the generated StrictServerInterface, so the wire shape can never drift from
// the spec.
package api

import _ "embed"

//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen -config oapi-codegen.yaml openapi_spec.yaml

// specYAML is the OpenAPI document, embedded so the controller can serve it
// (GET /api/v1/openapi.yaml) and the explorer can render it.
//
//go:embed openapi_spec.yaml
var specYAML []byte
