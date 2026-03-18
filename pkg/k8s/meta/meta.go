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

	// LabelBridgeName is the user-facing bridge name used to find bridges via label selector.
	LabelBridgeName = "vercel.sh/bridge-name"

	// LabelBridgeDeployment identifies the specific bridge deployment a pod belongs to.
	LabelBridgeDeployment = "vercel.sh/bridge-deployment"

	// BridgeTypeProxy is the label value for bridge proxy resources.
	BridgeTypeProxy = "proxy"

	// ManagedByAdministrator is the value for the managed-by label.
	ManagedByAdministrator = "administrator"

	// ProxySelector is the label selector string for bridge proxy resources.
	ProxySelector = LabelBridgeType + "=" + BridgeTypeProxy

	// LabelDevcontainerConfigFile is the Docker label set by the devcontainer
	// CLI that records the absolute path to the devcontainer.json used to
	// create the container.
	LabelDevcontainerConfigFile = "devcontainer.config_file"

	// EnvInterceptorAddr is the environment variable that configures the
	// interceptor gRPC server listen address.
	EnvInterceptorAddr = "BRIDGE_INTERCEPTOR_ADDR"
)

// DeploymentSelector returns a label selector that matches pods for a specific bridge deployment.
func DeploymentSelector(deployName string) string {
	return LabelBridgeDeployment + "=" + deployName
}

// BridgeNameSelector returns a label selector that finds bridge resources by
// the user-facing bridge name and device ID.
func BridgeNameSelector(bridgeName, deviceID string) string {
	sel := LabelBridgeName + "=" + bridgeName
	if deviceID != "" {
		sel += "," + LabelDeviceID + "=" + deviceID
	}
	return sel
}

// DeviceSelector returns a label selector matching all bridge proxy resources
// belonging to a specific device.
func DeviceSelector(deviceID string) string {
	return LabelBridgeType + "=" + BridgeTypeProxy + "," + LabelDeviceID + "=" + deviceID
}
