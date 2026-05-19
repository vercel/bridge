//go:build linux

package fsmount_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/fsmount"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestFUSERoundTrip runs the FS gRPC server backed by a temp dir, mounts it
// via FUSE in a second temp dir, and verifies reads/listings reflect the
// backing files. Skips automatically if /dev/fuse is unavailable.
func TestFUSERoundTrip(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skipf("FUSE not available: %v", err)
	}

	// Backing directory served by the FS gRPC server.
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "hello.txt"), []byte("hello fuse"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(src, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "sub", "nested.txt"), []byte("nested"), 0o600))

	// Start an in-process gRPC server with the FS service.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	bridgev1.RegisterBridgeFileSystemServiceServer(srv, fsmount.NewServer([]string{src}))
	go srv.Serve(lis)
	defer srv.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()

	client := bridgev1.NewBridgeFileSystemServiceClient(conn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mountpoint := t.TempDir()
	fuseSrv, err := fsmount.Mount(ctx, mountpoint, src, client)
	if err != nil {
		t.Skipf("FUSE mount failed (typical in unprivileged CI): %v", err)
	}
	defer fuseSrv.Unmount()

	// Give the kernel a moment to wire up the mount.
	deadline := time.Now().Add(2 * time.Second)
	var data []byte
	for time.Now().Before(deadline) {
		data, err = os.ReadFile(filepath.Join(mountpoint, "hello.txt"))
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.NoError(t, err)
	assert.Equal(t, "hello fuse", string(data))

	// Directory listing reflects the backing dir.
	entries, err := os.ReadDir(mountpoint)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.ElementsMatch(t, []string{"hello.txt", "sub"}, names)

	// Nested file reads through the FUSE mount.
	nested, err := os.ReadFile(filepath.Join(mountpoint, "sub", "nested.txt"))
	require.NoError(t, err)
	assert.Equal(t, "nested", string(nested))
}
