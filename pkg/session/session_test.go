package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
)

// withTempHome redirects HOME to a temp dir for the duration of the test so
// session files don't leak into the developer's real ~/.bridge.
func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	return dir
}

func TestSave_WritesFreshSession(t *testing.T) {
	home := withTempHome(t)

	require.NoError(t, Save("my-api", "/abs/path/devcontainer.json"))

	path := filepath.Join(home, ".bridge", "sessions", "my-api.json")
	_, err := os.Stat(path)
	require.NoError(t, err)

	s, err := Load("my-api")
	require.NoError(t, err)
	assert.Equal(t, "my-api", s.GetName())
	assert.Equal(t, "/abs/path/devcontainer.json", s.GetDevcontainerConfigPath())
	// Dev fields default to zero for a freshly created session.
	assert.Equal(t, int32(0), s.GetDevPid())
	assert.Empty(t, s.GetDevCommand())
}

// TestWrite_PreservesAllFields ensures the new dev_pid / dev_started_at /
// dev_command fields survive a write/read cycle, so `bridge dev` can stash
// the running command's PID and read it back on the next invocation.
func TestWrite_PreservesAllFields(t *testing.T) {
	withTempHome(t)

	now := timestamppb.Now()
	in := &bridgev1.Session{
		Name:                   "my-api",
		TimeCreated:            now,
		DevcontainerConfigPath: "/abs/devcontainer.json",
		DevPid:                 12345,
		DevStartedAt:           now,
		DevCommand:             "pnpm dev",
	}
	require.NoError(t, Write(in))

	out, err := Load("my-api")
	require.NoError(t, err)
	assert.Equal(t, in.GetName(), out.GetName())
	assert.Equal(t, in.GetDevcontainerConfigPath(), out.GetDevcontainerConfigPath())
	assert.Equal(t, in.GetDevPid(), out.GetDevPid())
	assert.Equal(t, in.GetDevCommand(), out.GetDevCommand())
	require.NotNil(t, out.GetDevStartedAt())
	assert.Equal(t, in.GetDevStartedAt().AsTime().Unix(), out.GetDevStartedAt().AsTime().Unix())
}

// TestSave_OverwritesPreviousState confirms that `bridge create` (which calls
// Save) wipes prior dev state — a freshly recreated bridge shouldn't carry
// stale PIDs from a previous incarnation.
func TestSave_OverwritesPreviousState(t *testing.T) {
	withTempHome(t)

	now := timestamppb.Now()
	require.NoError(t, Write(&bridgev1.Session{
		Name:                   "my-api",
		TimeCreated:            now,
		DevcontainerConfigPath: "/old.json",
		DevPid:                 9999,
		DevCommand:             "old cmd",
	}))

	require.NoError(t, Save("my-api", "/new.json"))

	out, err := Load("my-api")
	require.NoError(t, err)
	assert.Equal(t, "/new.json", out.GetDevcontainerConfigPath())
	assert.Equal(t, int32(0), out.GetDevPid(), "Save must reset dev_pid")
	assert.Empty(t, out.GetDevCommand(), "Save must reset dev_command")
}

func TestDelete_Idempotent(t *testing.T) {
	withTempHome(t)

	// Delete non-existent — must not error.
	require.NoError(t, Delete("never-existed"))

	require.NoError(t, Save("my-api", "/x.json"))
	require.NoError(t, Delete("my-api"))

	_, err := Load("my-api")
	require.Error(t, err)
}
