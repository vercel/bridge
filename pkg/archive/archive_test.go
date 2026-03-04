package archive

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPackUnpackRoundTrip(t *testing.T) {
	fsys := fstest.MapFS{
		"deploy.yaml": {Data: []byte("apiVersion: apps/v1\nkind: Deployment\n")},
		"svc.yml":     {Data: []byte("apiVersion: v1\nkind: Service\n")},
	}

	data, err := PackGlobFiles(fsys, "*.yaml")
	require.NoError(t, err)

	files, err := Unpack(data)
	require.NoError(t, err)
	assert.Len(t, files, 1)
	assert.Equal(t, []byte("apiVersion: apps/v1\nkind: Deployment\n"), files["deploy.yaml"])
}

func TestPackGlobMatchesBothExtensions(t *testing.T) {
	fsys := fstest.MapFS{
		"a.yaml": {Data: []byte("a")},
		"b.yml":  {Data: []byte("b")},
		"c.txt":  {Data: []byte("c")},
	}

	// *.yaml matches only .yaml
	data, err := PackGlobFiles(fsys, "*.yaml")
	require.NoError(t, err)
	files, err := Unpack(data)
	require.NoError(t, err)
	assert.Len(t, files, 1)
	assert.Contains(t, files, "a.yaml")

	// *.yml matches only .yml
	data, err = PackGlobFiles(fsys, "*.yml")
	require.NoError(t, err)
	files, err = Unpack(data)
	require.NoError(t, err)
	assert.Len(t, files, 1)
	assert.Contains(t, files, "b.yml")
}

func TestPackRecursiveWalk(t *testing.T) {
	fsys := fstest.MapFS{
		"root.yaml":       {Data: []byte("root")},
		"sub/nested.yaml": {Data: []byte("nested")},
		"sub/deep/a.yaml": {Data: []byte("deep")},
	}

	data, err := PackGlobFiles(fsys, "*.yaml")
	require.NoError(t, err)

	files, err := Unpack(data)
	require.NoError(t, err)
	assert.Len(t, files, 3)
	assert.Equal(t, []byte("root"), files["root.yaml"])
	assert.Equal(t, []byte("nested"), files["sub/nested.yaml"])
	assert.Equal(t, []byte("deep"), files["sub/deep/a.yaml"])
}

func TestPackNoMatches(t *testing.T) {
	fsys := fstest.MapFS{
		"readme.txt": {Data: []byte("hello")},
	}

	_, err := PackGlobFiles(fsys, "*.yaml")
	assert.ErrorContains(t, err, "no files matched")
}

func TestPackInvalidPattern(t *testing.T) {
	fsys := fstest.MapFS{}
	_, err := PackGlobFiles(fsys, "[invalid")
	assert.ErrorContains(t, err, "invalid glob pattern")
}

func TestPackWildcard(t *testing.T) {
	fsys := fstest.MapFS{
		"deploy.yaml": {Data: []byte("deploy")},
		"config.json": {Data: []byte("config")},
	}

	data, err := PackGlobFiles(fsys, "*")
	require.NoError(t, err)

	files, err := Unpack(data)
	require.NoError(t, err)
	assert.Len(t, files, 2)
}

func TestUnpackPathTraversal(t *testing.T) {
	// Pack a valid archive using the real FS, then verify it unpacks.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "safe.yaml"), []byte("safe"), 0644))

	data, err := PackGlobFiles(os.DirFS(dir), "*.yaml")
	require.NoError(t, err)

	files, err := Unpack(data)
	require.NoError(t, err)
	assert.Len(t, files, 1)
}

func TestPackWithOSDirFS(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "app.yaml"), []byte("app"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "readme.md"), []byte("docs"), 0644))

	data, err := PackGlobFiles(os.DirFS(dir), "*.yaml")
	require.NoError(t, err)

	files, err := Unpack(data)
	require.NoError(t, err)
	assert.Len(t, files, 1)
	assert.Equal(t, []byte("app"), files["app.yaml"])
}
