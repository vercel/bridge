// Package embeds provides embedded static assets for the bridge CLI.
package embeds

import _ "embed"

// ReactorSchema is the dereferenced JSON Schema for Reactor.
//
//go:embed jsonschema/Reactor.schema.json
var ReactorSchema []byte
