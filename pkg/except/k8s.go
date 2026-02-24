package except

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// GRPCFromK8s converts a Kubernetes API error to a gRPC status error.
func GRPCFromK8s(err error, msg string) error {
	switch {
	case apierrors.IsNotFound(err):
		return status.Errorf(codes.NotFound, "%s: %v", msg, err)
	case apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err):
		return status.Errorf(codes.AlreadyExists, "%s: %v", msg, err)
	case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err):
		return status.Errorf(codes.PermissionDenied, "%s: %v", msg, err)
	case apierrors.IsInvalid(err) || apierrors.IsBadRequest(err):
		return status.Errorf(codes.InvalidArgument, "%s: %v", msg, err)
	case apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err):
		return status.Errorf(codes.DeadlineExceeded, "%s: %v", msg, err)
	default:
		return status.Errorf(codes.Internal, "%s: %v", msg, err)
	}
}
