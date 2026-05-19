package fsmount

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// readChunkSize is the maximum number of bytes the client requests from the
// server per ReadFile RPC. Must not exceed the server's maxReadChunk; smaller
// values trade higher round-trip count for less peak memory.
const readChunkSize = 1 << 20 // 1 MiB

// cacheTTL is the duration that attribute and lookup results are valid in the
// kernel cache. Short enough that updates in the pod become visible quickly,
// long enough to avoid an RPC per stat from tools like `ls -la`.
const cacheTTL = 5 * time.Second

// Mount mounts the FUSE filesystem at mountpoint, fronting the given remote
// root path on the bridge server. The mountpoint directory must exist and be
// empty. Mount blocks until the returned server is unmounted; callers should
// run it in a goroutine and call Unmount on shutdown.
//
// remoteRoot is the absolute path on the pod (one of the server's allowed
// roots) that this mount exposes at mountpoint locally.
func Mount(ctx context.Context, mountpoint, remoteRoot string, client bridgev1.BridgeFileSystemServiceClient) (*fuse.Server, error) {
	if err := os.MkdirAll(mountpoint, 0755); err != nil {
		return nil, fmt.Errorf("create mountpoint %q: %w", mountpoint, err)
	}

	root := &node{client: client, remotePath: remoteRoot}

	entryTimeout := cacheTTL
	attrTimeout := cacheTTL
	opts := &fs.Options{
		EntryTimeout: &entryTimeout,
		AttrTimeout:  &attrTimeout,
		MountOptions: fuse.MountOptions{
			FsName:     "bridge",
			Name:       "bridge",
			AllowOther: true,
			// The bridge devcontainer is shared with the developer's local
			// user; AllowOther + default permissions let all users in the
			// container read the mount.
			DisableXAttrs: true,
		},
	}

	server, err := fs.Mount(mountpoint, root, opts)
	if err != nil {
		return nil, fmt.Errorf("fuse mount %q: %w", mountpoint, err)
	}

	slog.Info("FUSE mount established", "mountpoint", mountpoint, "remote_root", remoteRoot)

	// Best-effort: unmount when ctx is cancelled.
	go func() {
		<-ctx.Done()
		if err := server.Unmount(); err != nil {
			slog.Debug("FUSE unmount", "mountpoint", mountpoint, "error", err)
		}
	}()

	return server, nil
}

// node is a FUSE node backed by a path on the remote bridge server. Each node
// stores its absolute remote path; child paths are joined lazily on Lookup.
type node struct {
	fs.Inode

	client     bridgev1.BridgeFileSystemServiceClient
	remotePath string
}

// Compile-time interface assertions.
var (
	_ fs.NodeGetattrer  = (*node)(nil)
	_ fs.NodeLookuper   = (*node)(nil)
	_ fs.NodeReaddirer  = (*node)(nil)
	_ fs.NodeOpener     = (*node)(nil)
	_ fs.NodeReader     = (*node)(nil)
	_ fs.NodeReadlinker = (*node)(nil)
	_ fs.NodeSetattrer  = (*node)(nil)
)

// Getattr fetches attributes for the current node.
func (n *node) Getattr(ctx context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	resp, err := n.client.Stat(ctx, &bridgev1.StatRequest{Path: n.remotePath})
	if err != nil {
		return errnoFromGRPC(err)
	}
	applyAttr(&out.Attr, resp.GetAttr())
	return fs.OK
}

// Setattr is a no-op because the mount is read-only. We accept the call (so
// tools like `cp -p` don't fail when restoring metadata they can't actually
// change) but report current attributes back unchanged.
func (n *node) Setattr(ctx context.Context, _ fs.FileHandle, _ *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	return n.Getattr(ctx, nil, out)
}

// Lookup resolves a child name within this directory.
func (n *node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	childPath := path.Join(n.remotePath, name)
	resp, err := n.client.Stat(ctx, &bridgev1.StatRequest{Path: childPath})
	if err != nil {
		return nil, errnoFromGRPC(err)
	}
	attr := resp.GetAttr()

	child := n.NewInode(ctx,
		&node{client: n.client, remotePath: childPath},
		fs.StableAttr{Mode: fuseModeBits(attr)},
	)
	applyAttr(&out.Attr, attr)
	return child, fs.OK
}

// Readdir streams the directory entries.
func (n *node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	resp, err := n.client.ReadDir(ctx, &bridgev1.ReadDirRequest{Path: n.remotePath})
	if err != nil {
		return nil, errnoFromGRPC(err)
	}
	entries := make([]fuse.DirEntry, 0, len(resp.GetEntries()))
	for _, e := range resp.GetEntries() {
		entries = append(entries, fuse.DirEntry{
			Name: e.GetName(),
			Mode: fuseModeBits(&bridgev1.FileAttr{Type: e.GetType(), Mode: e.GetMode()}),
		})
	}
	return fs.NewListDirStream(entries), fs.OK
}

// Open accepts read-only opens. Writes are rejected with EROFS.
func (n *node) Open(_ context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if int32(flags)&(syscall.O_WRONLY|syscall.O_RDWR) != 0 {
		return nil, 0, syscall.EROFS
	}
	// We're stateless: no per-handle state needed. fuseFlags=FOPEN_KEEP_CACHE
	// lets the kernel cache reads across opens, which is safe for the
	// read-only view (TTL invalidates on mtime change).
	return nil, fuse.FOPEN_KEEP_CACHE, fs.OK
}

// Read fetches a byte range. The kernel chunks reads at the configured max
// read size; for any single Read call we satisfy `dest` with up to
// readChunkSize per round trip, looping if needed.
func (n *node) Read(ctx context.Context, _ fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	want := int64(len(dest))
	if want <= 0 {
		return fuse.ReadResultData(nil), fs.OK
	}
	if want > readChunkSize {
		want = readChunkSize
	}

	resp, err := n.client.ReadFile(ctx, &bridgev1.ReadFileRequest{
		Path:   n.remotePath,
		Offset: off,
		Size:   want,
	})
	if err != nil {
		return nil, errnoFromGRPC(err)
	}
	return fuse.ReadResultData(resp.GetData()), fs.OK
}

// Readlink resolves a symlink target.
func (n *node) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	resp, err := n.client.ReadLink(ctx, &bridgev1.ReadLinkRequest{Path: n.remotePath})
	if err != nil {
		return nil, errnoFromGRPC(err)
	}
	return []byte(resp.GetTarget()), fs.OK
}

// applyAttr fills a fuse.Attr from a proto FileAttr.
func applyAttr(out *fuse.Attr, attr *bridgev1.FileAttr) {
	if attr == nil {
		return
	}
	out.Mode = fuseModeBits(attr)
	out.Size = uint64(attr.GetSize())
	out.Mtime = uint64(attr.GetModTime())
	out.Atime = out.Mtime
	out.Ctime = out.Mtime
}

// fuseModeBits composes a kernel mode value (type bits | permission bits) from
// a proto FileAttr.
func fuseModeBits(attr *bridgev1.FileAttr) uint32 {
	if attr == nil {
		return 0
	}
	perm := attr.GetMode() & 07777
	switch attr.GetType() {
	case bridgev1.FileType_FILE_TYPE_DIR:
		return perm | fuse.S_IFDIR
	case bridgev1.FileType_FILE_TYPE_SYMLINK:
		return perm | fuse.S_IFLNK
	case bridgev1.FileType_FILE_TYPE_REGULAR:
		return perm | fuse.S_IFREG
	default:
		return perm
	}
}

// errnoFromGRPC maps a gRPC error to the closest syscall errno.
func errnoFromGRPC(err error) syscall.Errno {
	if err == nil {
		return fs.OK
	}
	st, ok := status.FromError(err)
	if !ok {
		// Common case: context cancellation or transport-level error.
		if errors.Is(err, context.Canceled) {
			return syscall.EINTR
		}
		return syscall.EIO
	}
	switch st.Code() {
	case codes.NotFound:
		return syscall.ENOENT
	case codes.PermissionDenied:
		return syscall.EACCES
	case codes.InvalidArgument:
		return syscall.EINVAL
	case codes.Unimplemented:
		return syscall.ENOSYS
	case codes.Unavailable, codes.DeadlineExceeded:
		return syscall.EIO
	default:
		return syscall.EIO
	}
}
