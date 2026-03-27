package vercel

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveTokenPrefersEnv(t *testing.T) {
	token, err := resolveToken(
		func(key string) string {
			if key == "VERCEL_TOKEN" {
				return "env-token"
			}
			return ""
		},
		func(string) ([]byte, error) {
			return nil, errors.New("should not be called")
		},
		t.TempDir(),
	)
	require.NoError(t, err)
	require.Equal(t, "env-token", token)
}

func TestResolveTokenReadsCLIAuthFile(t *testing.T) {
	homeDir := t.TempDir()
	authPath := filepath.Join(homeDir, "Library", "Application Support", "com.vercel.cli", "auth.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(authPath), 0o755))
	require.NoError(t, os.WriteFile(authPath, []byte(`{"token":"cli-token"}`), 0o644))

	token, err := resolveToken(
		func(string) string { return "" },
		os.ReadFile,
		homeDir,
	)
	require.NoError(t, err)
	require.Equal(t, "cli-token", token)
}

func TestResolveTokenReturnsErrTokenNotFound(t *testing.T) {
	token, err := resolveToken(
		func(string) string { return "" },
		func(string) ([]byte, error) { return nil, os.ErrNotExist },
		t.TempDir(),
	)
	require.ErrorIs(t, err, ErrTokenNotFound)
	require.Empty(t, token)
}
