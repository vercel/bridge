package meta

const (
	// LabelBridgeType identifies bridge resource types (e.g., "proxy").
	LabelBridgeType = "vercel.sh/bridge-type"

	// LabelManagedBy identifies resources managed by the bridge administrator.
	LabelManagedBy = "vercel.sh/bridge-managed-by"

	// LabelDeviceID stores the device KSUID on the namespace.
	LabelDeviceID = "vercel.sh/bridge-device-id"

	// LabelWorkloadSource stores the name of the source deployment that was cloned.
	LabelWorkloadSource = "vercel.sh/bridge-workload-source"

	// LabelWorkloadSourceNamespace stores the namespace of the source deployment.
	LabelWorkloadSourceNamespace = "vercel.sh/bridge-workload-source-ns"

	// LabelBridgeDeployment identifies the specific bridge deployment a pod belongs to.
	LabelBridgeDeployment = "vercel.sh/bridge-deployment"

	// BridgeTypeProxy is the label value for bridge proxy resources.
	BridgeTypeProxy = "proxy"

	// ManagedByAdministrator is the value for the managed-by label.
	ManagedByAdministrator = "administrator"

	// ProxySelector is the label selector string for bridge proxy resources.
	ProxySelector = LabelBridgeType + "=" + BridgeTypeProxy

	// EnvInterceptorAddr is the environment variable that configures the
	// interceptor gRPC server listen address.
	EnvInterceptorAddr = "BRIDGE_INTERCEPTOR_ADDR"
)

// DeploymentSelector returns a label selector that matches pods for a specific bridge deployment.
func DeploymentSelector(deployName string) string {
	return LabelBridgeDeployment + "=" + deployName
}

// DeviceSelector returns a label selector matching all bridge proxy resources
// belonging to a specific device.
func DeviceSelector(deviceID string) string {
	return LabelBridgeType + "=" + BridgeTypeProxy + "," + LabelDeviceID + "=" + deviceID
}
