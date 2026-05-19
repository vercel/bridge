// Package fsmount implements the bridge filesystem service.
//
// The server side runs in the bridge proxy pod and exposes a read-only view of
// configured mount roots over gRPC. The client side (see client.go) mounts
// those roots inside the devcontainer using FUSE so files appear at their
// original absolute paths and reflect the live pod state.
package fsmount

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// maxReadChunk caps the bytes returned in a single ReadFile RPC. gRPC's
// default 4 MiB message limit gives plenty of headroom; keeping chunks small
// limits the worst-case wasted memory if the client cancels mid-read.
const maxReadChunk = 1 << 20 // 1 MiB

// Server implements bridgev1.BridgeFileSystemServiceServer. It only serves
// paths whose absolute form (after symlink resolution) is rooted at one of the
// configured mount roots.
type Server struct {
	bridgev1.UnimplementedBridgeFileSystemServiceServer

	roots []string // sorted, cleaned absolute paths
}

// NewServer creates a Server that serves only the given roots. Each root must
// be an absolute path. Paths outside any root are rejected with PermissionDenied.
func NewServer(roots []string) *Server {
	cleaned := make([]string, 0, len(roots))
	for _, r := range roots {
		if r == "" {
			continue
		}
		cleaned = append(cleaned, filepath.Clean(r))
	}
	return &Server{roots: cleaned}
}

// Roots returns the configured mount roots.
func (s *Server) Roots() []string {
	out := make([]string, len(s.roots))
	copy(out, s.roots)
	return out
}

// authorize returns the cleaned absolute path if it falls inside an allowed
// root, or a PermissionDenied error otherwise.
func (s *Server) authorize(path string) (string, error) {
	if path == "" {
		return "", status.Error(codes.InvalidArgument, "path is required")
	}
	if !filepath.IsAbs(path) {
		return "", status.Error(codes.InvalidArgument, "path must be absolute")
	}
	clean := filepath.Clean(path)
	for _, root := range s.roots {
		if clean == root || strings.HasPrefix(clean, root+string(filepath.Separator)) {
			return clean, nil
		}
	}
	return "", status.Errorf(codes.PermissionDenied, "path %q is not within any allowed mount root", path)
}

// Stat returns attributes for the path. It uses Lstat so symlinks report as
// symlinks rather than the target's attributes.
func (s *Server) Stat(_ context.Context, req *bridgev1.StatRequest) (*bridgev1.StatResponse, error) {
	path, err := s.authorize(req.GetPath())
	if err != nil {
		return nil, err
	}

	info, err := os.Lstat(path)
	if err != nil {
		return nil, mapFSError(err)
	}
	return &bridgev1.StatResponse{Attr: fileAttr(info)}, nil
}

// ReadDir lists entries in a directory. Returns FailedPrecondition if the path
// is not a directory.
func (s *Server) ReadDir(_ context.Context, req *bridgev1.ReadDirRequest) (*bridgev1.ReadDirResponse, error) {
	path, err := s.authorize(req.GetPath())
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, mapFSError(err)
	}

	out := make([]*bridgev1.DirEntry, 0, len(entries))
	for _, e := range entries {
		// We need the mode bits, so stat each entry. This is per-call,
		// not cached; FUSE clients should cache attrs on their side.
		info, err := e.Info()
		if err != nil {
			slog.Debug("ReadDir: failed to stat entry", "path", path, "name", e.Name(), "error", err)
			continue
		}
		out = append(out, &bridgev1.DirEntry{
			Name: e.Name(),
			Type: fileType(info.Mode()),
			Mode: uint32(info.Mode().Perm()),
		})
	}
	return &bridgev1.ReadDirResponse{Entries: out}, nil
}

// ReadFile reads a byte range of a regular file. The server caps each response
// at maxReadChunk; the client requests successive offsets to read more.
func (s *Server) ReadFile(_ context.Context, req *bridgev1.ReadFileRequest) (*bridgev1.ReadFileResponse, error) {
	path, err := s.authorize(req.GetPath())
	if err != nil {
		return nil, err
	}
	if req.GetOffset() < 0 {
		return nil, status.Error(codes.InvalidArgument, "offset must be non-negative")
	}
	size := req.GetSize()
	if size <= 0 || size > maxReadChunk {
		size = maxReadChunk
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, mapFSError(err)
	}
	defer f.Close()

	if _, err := f.Seek(req.GetOffset(), io.SeekStart); err != nil {
		return nil, mapFSError(err)
	}

	buf := make([]byte, size)
	n, err := io.ReadFull(f, buf)
	eof := false
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			eof = true
		} else {
			return nil, mapFSError(err)
		}
	}
	return &bridgev1.ReadFileResponse{Data: buf[:n], Eof: eof}, nil
}

// ReadLink returns the symlink target.
func (s *Server) ReadLink(_ context.Context, req *bridgev1.ReadLinkRequest) (*bridgev1.ReadLinkResponse, error) {
	path, err := s.authorize(req.GetPath())
	if err != nil {
		return nil, err
	}
	target, err := os.Readlink(path)
	if err != nil {
		return nil, mapFSError(err)
	}
	return &bridgev1.ReadLinkResponse{Target: target}, nil
}

// fileAttr converts os.FileInfo to a FileAttr proto.
func fileAttr(info os.FileInfo) *bridgev1.FileAttr {
	return &bridgev1.FileAttr{
		Mode:    uint32(info.Mode().Perm()),
		Size:    info.Size(),
		ModTime: info.ModTime().Unix(),
		Type:    fileType(info.Mode()),
	}
}

func fileType(mode os.FileMode) bridgev1.FileType {
	switch {
	case mode&os.ModeSymlink != 0:
		return bridgev1.FileType_FILE_TYPE_SYMLINK
	case mode.IsDir():
		return bridgev1.FileType_FILE_TYPE_DIR
	case mode.IsRegular():
		return bridgev1.FileType_FILE_TYPE_REGULAR
	default:
		return bridgev1.FileType_FILE_TYPE_UNSPECIFIED
	}
}

// mapFSError converts an os filesystem error into an appropriate gRPC status.
func mapFSError(err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, fs.ErrPermission):
		return status.Error(codes.PermissionDenied, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}
