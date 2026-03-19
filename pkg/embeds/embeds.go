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

// CommandResultSchema is the dereferenced JSON Schema for CommandResult.
//
//go:embed jsonschema/CommandResult.schema.json
var CommandResultSchema []byte

// CreateCommandResponseSchema is the dereferenced JSON Schema for CreateCommandResponse.
//
//go:embed jsonschema/CreateCommandResponse.schema.json
var CreateCommandResponseSchema []byte

// GetCommandResponseSchema is the dereferenced JSON Schema for GetCommandResponse.
//
//go:embed jsonschema/GetCommandResponse.schema.json
var GetCommandResponseSchema []byte

// RemoveCommandResponseSchema is the dereferenced JSON Schema for RemoveCommandResponse.
//
//go:embed jsonschema/RemoveCommandResponse.schema.json
var RemoveCommandResponseSchema []byte
