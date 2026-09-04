package fsmount

import (
	"context"
	"errors"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestErrnoFromGRPC(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want syscall.Errno
	}{
		{"nil", nil, fs.OK},
		// An RPC aborted by FUSE_INTERRUPT surfaces from grpc-go as a status
		// with codes.Canceled. It must read as an interrupted call, not a
		// broken file: callers retry EINTR and die on EIO.
		{"canceled status", status.Error(codes.Canceled, "context canceled"), syscall.EINTR},
		{"bare context.Canceled", context.Canceled, syscall.EINTR},
		{"wrapped context.Canceled", errors.Join(errors.New("rpc"), context.Canceled), syscall.EINTR},
		{"not found", status.Error(codes.NotFound, "no such file"), syscall.ENOENT},
		{"permission denied", status.Error(codes.PermissionDenied, "denied"), syscall.EACCES},
		{"invalid argument", status.Error(codes.InvalidArgument, "bad"), syscall.EINVAL},
		{"unimplemented", status.Error(codes.Unimplemented, "nope"), syscall.ENOSYS},
		{"unavailable", status.Error(codes.Unavailable, "down"), syscall.EIO},
		{"deadline exceeded", status.Error(codes.DeadlineExceeded, "slow"), syscall.EIO},
		{"unknown status", status.Error(codes.Internal, "boom"), syscall.EIO},
		{"non-grpc error", errors.New("transport broke"), syscall.EIO},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, errnoFromGRPC(tt.err))
		})
	}
}
