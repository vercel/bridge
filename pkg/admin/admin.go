// Package admin abstracts bridge administration operations. Implementations
// include a remote gRPC client (connecting to an in-cluster administrator)
// and a local implementation that performs operations directly via kubeconfig.
package admin

import "context"

// Admin is the interface for bridge administration operations.
type Admin interface {
	// CreateBridge provisions a new bridge in the cluster.
	CreateBridge(ctx context.Context, req CreateRequest) (*CreateResponse, error)

	// ListBridges returns existing bridges for a device.
	ListBridges(ctx context.Context, deviceID string) ([]*BridgeInfo, error)

	// Close releases any resources held by the admin (e.g. gRPC connections).
	Close() error
}

// CreateRequest holds parameters for creating a bridge.
type CreateRequest struct {
	DeviceID         string
	SourceDeployment string
	SourceNamespace  string
	Force            bool
}

// CreateResponse holds the result of a bridge creation.
type CreateResponse struct {
	Namespace      string
	PodName        string
	Port           int32
	DeploymentName string
	EnvVars        map[string]string
}

// BridgeInfo describes an existing bridge.
type BridgeInfo struct {
	DeviceID         string
	SourceDeployment string
	SourceNamespace  string
	Namespace        string
	CreatedAt        string
	Status           string
}
