// Package embeds provides embedded static assets for the bridge CLI.
package embeds

import _ "embed"

// ServerFacadeSchema is the dereferenced JSON Schema for ServerFacade.
//
//go:embed jsonschema/ServerFacade.schema.json
var ServerFacadeSchema []byte

// ProfileSchema is the dereferenced JSON Schema for Profile.
//
//go:embed jsonschema/Profile.schema.json
var ProfileSchema []byte
