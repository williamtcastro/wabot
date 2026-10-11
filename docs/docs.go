// Package docs embeds the OpenAPI spec served at /openapi.yaml.
package docs

import _ "embed"

//go:embed openapi.yaml
var OpenAPI []byte
