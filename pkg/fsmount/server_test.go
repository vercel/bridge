package fsmount

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestServer_AuthorizeAllowsRootAndChildren(t *testing.T) {
	dir := t.TempDir()
	s := NewServer([]string{dir})

	for _, p := range []string{
		dir,
		filepath.Join(dir, "file"),
		filepath.Join(dir, "sub", "deep", "file"),
	} {
		_, err := s.authorize(p)
		assert.NoError(t, err, "path %q should be allowed", p)
	}
}

func TestServer_AuthorizeRejectsOutsidePaths(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	s := NewServer([]string{root})

	for _, tc := range []struct {
		name string
		path string
		code codes.Code
	}{
		{"empty", "", codes.InvalidArgument},
		{"relative", "foo/bar", codes.InvalidArgument},
		{"different-root", filepath.Join(other, "f"), codes.PermissionDenied},
		{"sibling-prefix", root + "-sibling", codes.PermissionDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.authorize(tc.path)
			require.Error(t, err)
			assert.Equal(t, tc.code, status.Code(err))
		})
	}
}

func TestServer_StatReadDirReadFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello world"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))

	s := NewServer([]string{dir})
	ctx := context.Background()

	// Stat the directory.
	statResp, err := s.Stat(ctx, &bridgev1.StatRequest{Path: dir})
	require.NoError(t, err)
	assert.Equal(t, bridgev1.FileType_FILE_TYPE_DIR, statResp.GetAttr().GetType())

	// Stat a regular file.
	statResp, err = s.Stat(ctx, &bridgev1.StatRequest{Path: filepath.Join(dir, "hello.txt")})
	require.NoError(t, err)
	assert.Equal(t, bridgev1.FileType_FILE_TYPE_REGULAR, statResp.GetAttr().GetType())
	assert.Equal(t, int64(11), statResp.GetAttr().GetSize())

	// ReadDir lists both entries.
	rd, err := s.ReadDir(ctx, &bridgev1.ReadDirRequest{Path: dir})
	require.NoError(t, err)
	names := make([]string, 0, len(rd.GetEntries()))
	for _, e := range rd.GetEntries() {
		names = append(names, e.GetName())
	}
	assert.ElementsMatch(t, []string{"hello.txt", "sub"}, names)

	// ReadFile returns the full payload when size is zero.
	rf, err := s.ReadFile(ctx, &bridgev1.ReadFileRequest{Path: filepath.Join(dir, "hello.txt")})
	require.NoError(t, err)
	assert.Equal(t, "hello world", string(rf.GetData()))
	assert.True(t, rf.GetEof())

	// ReadFile with offset returns a slice.
	rf, err = s.ReadFile(ctx, &bridgev1.ReadFileRequest{Path: filepath.Join(dir, "hello.txt"), Offset: 6, Size: 5})
	require.NoError(t, err)
	assert.Equal(t, "world", string(rf.GetData()))
}

func TestServer_StatRejectsOutsidePath(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	s := NewServer([]string{root})

	_, err := s.Stat(context.Background(), &bridgev1.StatRequest{Path: filepath.Join(other, "x")})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
}

func TestServer_StatMapsNotFound(t *testing.T) {
	dir := t.TempDir()
	s := NewServer([]string{dir})

	_, err := s.Stat(context.Background(), &bridgev1.StatRequest{Path: filepath.Join(dir, "missing")})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestServer_ReadLinkReturnsTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(target, []byte("data"), 0o600))
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(target, link))

	s := NewServer([]string{dir})
	resp, err := s.ReadLink(context.Background(), &bridgev1.ReadLinkRequest{Path: link})
	require.NoError(t, err)
	assert.Equal(t, target, resp.GetTarget())
}
