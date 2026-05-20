package devcontainer

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseDevcontainer is a tiny test helper that drives Config's Unmarshal path
// so the test focuses on DevCommand semantics, not JSON wiring.
func parseDevcontainer(t *testing.T, body string) *Config {
	t.Helper()
	var c Config
	require.NoError(t, json.Unmarshal([]byte(body), &c))
	return &c
}

func TestDevCommand_StringForm(t *testing.T) {
	c := parseDevcontainer(t, `{"image":"x","devCommand":"pnpm dev"}`)
	got, err := c.DevCommand()
	require.NoError(t, err)
	assert.Equal(t, "pnpm dev", got)
}

func TestDevCommand_ArrayForm(t *testing.T) {
	c := parseDevcontainer(t, `{"image":"x","devCommand":["go","run","./cmd/server"]}`)
	got, err := c.DevCommand()
	require.NoError(t, err)
	assert.Equal(t, "go run ./cmd/server", got)
}

func TestDevCommand_TrimsWhitespace(t *testing.T) {
	c := parseDevcontainer(t, `{"image":"x","devCommand":"  pnpm dev  "}`)
	got, err := c.DevCommand()
	require.NoError(t, err)
	assert.Equal(t, "pnpm dev", got)
}

func TestDevCommand_Missing(t *testing.T) {
	c := parseDevcontainer(t, `{"image":"x"}`)
	got, err := c.DevCommand()
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestDevCommand_InvalidType(t *testing.T) {
	c := parseDevcontainer(t, `{"image":"x","devCommand":42}`)
	_, err := c.DevCommand()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "devCommand must be a string or array")
}

// TestDevCommand_SurvivesRoundTrip ensures we don't lose devCommand when the
// Config is re-serialized — important since bridge create rewrites the file.
func TestDevCommand_SurvivesRoundTrip(t *testing.T) {
	original := `{"image":"x","devCommand":"pnpm dev","customField":true}`
	c := parseDevcontainer(t, original)

	out, err := json.Marshal(c)
	require.NoError(t, err)

	var round Config
	require.NoError(t, json.Unmarshal(out, &round))

	got, err := round.DevCommand()
	require.NoError(t, err)
	assert.Equal(t, "pnpm dev", got)
}
