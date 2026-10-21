#!/usr/bin/env bash
# Regenerates both Swagger specs from the handler comments.
# docs/ is the public spec (no admin or internal routes).
# docs/private/ is the full spec, served only behind the admin secret.
set -euo pipefail
cd "$(dirname "$0")"

swag=(go run github.com/swaggo/swag/cmd/swag@v1.16.6)
common=(init -g main.go --parseDependency --parseInternal)

# The public spec keeps the default instance name so it stays docs.SwaggerInfo
# at docs/swagger.yaml. A second instance name would rename that variable.
"${swag[@]}" "${common[@]}" -o docs --tags '!admin,!internal'
"${swag[@]}" "${common[@]}" -o docs/private --instanceName private
