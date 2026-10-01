package resources

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vercel/bridge/pkg/k8s/meta"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// TestInjectCA_SecretPerBridge verifies that two bridges from one device get
// their own CA secrets, each labeled for, and mounted by, its own bridge. A
// shared secret let one bridge's create replace another's CA and its remove
// delete it.
func TestInjectCA_SecretPerBridge(t *testing.T) {
	const suffix = "-dev123"
	type bridged struct {
		secretName, secretDeployLabel, mountedSecret string
	}
	bridge := func(bridgeName string) bridged {
		bundle, err := SourceFromManifests(packTestManifests(t, map[string]string{"manifests.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
spec:
  selector:
    matchLabels:
      app: app
  template:
    metadata:
      labels:
        app: app
    spec:
      containers:
        - name: app
          image: app:latest
`}))
		require.NoError(t, err)

		tc := &TransformContext{Context: context.Background(), DeviceID: "dev123", BridgeName: bridgeName, SourceName: "app", SourceNamespace: "default"}
		require.NoError(t, Transform(tc, bundle, []Transformer{
			InjectCA("default"),
			Rename(func(name string) string {
				if name == "app" {
					return "bdg-" + bridgeName + suffix
				}
				return "bdg-" + name + suffix
			}),
			InjectLabels(),
			RewriteRefs(),
		}))

		var out bridged
		for _, r := range bundle.Resources {
			switch obj := r.Object.(type) {
			case *corev1.Secret:
				out.secretName = obj.Name
				out.secretDeployLabel = obj.Labels[meta.LabelBridgeDeployment]
			case *appsv1.Deployment:
				for _, v := range obj.Spec.Template.Spec.Volumes {
					if v.Name == caSecretVolumeName {
						out.mountedSecret = v.Secret.SecretName
					}
				}
			}
		}
		return out
	}

	a, b := bridge("alpha"), bridge("beta")

	assert.Equal(t, "bdg-bridge-ca-alpha"+suffix, a.secretName)
	assert.Equal(t, "bdg-bridge-ca-beta"+suffix, b.secretName)
	assert.Equal(t, "bdg-alpha"+suffix, a.secretDeployLabel, "removed with its own bridge")
	assert.Equal(t, "bdg-beta"+suffix, b.secretDeployLabel, "removed with its own bridge")
	assert.Equal(t, a.secretName, a.mountedSecret)
	assert.Equal(t, b.secretName, b.mountedSecret)
}
