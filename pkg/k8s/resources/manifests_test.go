package resources

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vercel/bridge/pkg/archive"
	"github.com/vercel/bridge/pkg/k8s/meta"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

const testDeviceID = "abc123def456"
const testProxyImage = "ghcr.io/vercel/bridge-cli:test"

const testSuffix = "-abc123"

func testDeployName(sourceName string) string {
	return sourceName + testSuffix
}

// testCreateFromManifests is a test helper that replicates the manifest-based
// pipeline: source → transforms → save.
func testCreateFromManifests(t *testing.T, client *k8stesting.Clientset, sourceManifests []byte, targetNS string) (string, *Bundle, error) {
	t.Helper()

	bundle, err := SourceFromManifests(sourceManifests)
	if err != nil {
		return "", nil, err
	}

	sourceName := FindDeploymentName(bundle)
	if sourceName == "" {
		return "", nil, fmt.Errorf("no deployment found in bundle")
	}

	sourceNS := FindNamespace(bundle)
	if sourceNS == "" {
		sourceNS = targetNS
	}

	tc := &TransformContext{
		Context:         context.Background(),
		DeviceID:        testDeviceID,
		SourceName:      sourceName,
		SourceNamespace: sourceNS,
	}

	transforms := []Transformer{
		PruneAllMetadata(),
		InjectProxyImage(testProxyImage),
		SuffixNames(testSuffix),
		InjectLabels(),
		TransformSelectors(),
		AppendBridgeService(targetNS),
		SetNamespace(targetNS),
		ClearClusterIPs(),
		RewriteRefs(),
	}

	if err := Transform(tc, bundle, transforms); err != nil {
		return "", nil, err
	}
	if err := Save(context.Background(), client, nil, bundle); err != nil {
		return "", nil, err
	}

	deployName := Lookup(NewResourceKey(sourceName, "apps", "Deployment"), bundle, tc.NameMap)
	return deployName, bundle, nil
}

func TestCreateFromManifests_SingleDeployment(t *testing.T) {
	manifests := packTestManifests(t, map[string]string{
		"deploy.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
spec:
  selector:
    matchLabels:
      app: my-app
  template:
    metadata:
      labels:
        app: my-app
    spec:
      containers:
      - name: app
        image: my-app:latest
        ports:
        - containerPort: 3000
`,
	})

	client := k8stesting.NewSimpleClientset()
	deployName, bundle, err := testCreateFromManifests(t, client, manifests, "default")
	require.NoError(t, err)

	expectedName := testDeployName("my-app")
	assert.Equal(t, expectedName, deployName)

	ap, _ := GetAppPorts(bundle, expectedName)
	assert.Contains(t, ap, int32(3000))

	gp, _ := GetGRPCPort(bundle, expectedName)
	assert.NotZero(t, gp)

	// Verify deployment was created with bridge labels and proxy image.
	deploy, err := client.AppsV1().Deployments("default").Get(context.Background(), expectedName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, meta.BridgeTypeProxy, deploy.Labels[meta.LabelBridgeType])
	assert.Equal(t, testProxyImage, deploy.Spec.Template.Spec.Containers[0].Image)

	// Verify bridge service was created.
	_, err = client.CoreV1().Services("default").Get(context.Background(), expectedName, metav1.GetOptions{})
	require.NoError(t, err)
}

func TestCreateFromManifests_MultipleDeployments(t *testing.T) {
	manifests := packTestManifests(t, map[string]string{
		"deploys.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  selector:
    matchLabels:
      app: api
  template:
    metadata:
      labels:
        app: api
    spec:
      containers:
      - name: api
        image: api:latest
        ports:
        - containerPort: 8080
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: worker
spec:
  selector:
    matchLabels:
      app: worker
  template:
    metadata:
      labels:
        app: worker
    spec:
      containers:
      - name: worker
        image: worker:latest
`,
	})

	client := k8stesting.NewSimpleClientset()
	deployName, bundle, err := testCreateFromManifests(t, client, manifests, "default")
	require.NoError(t, err)

	// FindDeploymentName returns the first deployment in the bundle.
	assert.NotEmpty(t, deployName)

	ap, _ := GetAppPorts(bundle, deployName)
	assert.Contains(t, ap, int32(8080))
}

func TestCreateFromManifests_NoDeployment(t *testing.T) {
	manifests := packTestManifests(t, map[string]string{
		"cm.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: my-config
data:
  key: value
`,
	})

	client := k8stesting.NewSimpleClientset()
	_, _, err := testCreateFromManifests(t, client, manifests, "default")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no deployment found")
}

func TestCreateFromManifests_ResourcesGetPrefixedNames(t *testing.T) {
	manifests := packTestManifests(t, map[string]string{
		"deploy.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
spec:
  selector:
    matchLabels:
      app: my-app
  template:
    metadata:
      labels:
        app: my-app
    spec:
      containers:
      - name: app
        image: my-app:latest
        envFrom:
        - configMapRef:
            name: app-config
        - secretRef:
            name: app-secret
`,
		"config.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
data:
  DB_HOST: localhost
`,
		"secret.yaml": `apiVersion: v1
kind: Secret
metadata:
  name: app-secret
type: Opaque
data:
  password: cGFzc3dvcmQ=
`,
	})

	client := k8stesting.NewSimpleClientset()
	deployName, _, err := testCreateFromManifests(t, client, manifests, "test-ns")
	require.NoError(t, err)

	suffix := testSuffix

	// ConfigMap should be created with prefixed name and bridge labels.
	cm, err := client.CoreV1().ConfigMaps("test-ns").Get(context.Background(), "app-config"+suffix, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, meta.BridgeTypeProxy, cm.Labels[meta.LabelBridgeType])
	assert.Equal(t, deployName, cm.Labels[meta.LabelBridgeDeployment])

	// Original name should NOT exist.
	_, err = client.CoreV1().ConfigMaps("test-ns").Get(context.Background(), "app-config", metav1.GetOptions{})
	assert.True(t, err != nil, "original configmap name should not exist")

	// Secret should be created with prefixed name.
	secret, err := client.CoreV1().Secrets("test-ns").Get(context.Background(), "app-secret"+suffix, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, meta.BridgeTypeProxy, secret.Labels[meta.LabelBridgeType])
}

func TestCreateFromManifests_DeploymentRefsRewritten(t *testing.T) {
	manifests := packTestManifests(t, map[string]string{
		"deploy.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
spec:
  selector:
    matchLabels:
      app: my-app
  template:
    metadata:
      labels:
        app: my-app
    spec:
      serviceAccountName: my-sa
      containers:
      - name: app
        image: my-app:latest
        env:
        - name: DB_HOST
          valueFrom:
            configMapKeyRef:
              name: app-config
              key: DB_HOST
        - name: SECRET_KEY
          valueFrom:
            secretKeyRef:
              name: app-secret
              key: key
        envFrom:
        - configMapRef:
            name: app-config
        volumeMounts:
        - name: config-vol
          mountPath: /etc/config
      volumes:
      - name: config-vol
        configMap:
          name: app-config
`,
		"config.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
data:
  DB_HOST: localhost
`,
		"secret.yaml": `apiVersion: v1
kind: Secret
metadata:
  name: app-secret
type: Opaque
data:
  key: dmFsdWU=
`,
		"sa.yaml": `apiVersion: v1
kind: ServiceAccount
metadata:
  name: my-sa
`,
	})

	client := k8stesting.NewSimpleClientset()
	deployName, _, err := testCreateFromManifests(t, client, manifests, "default")
	require.NoError(t, err)

	suffix := testSuffix

	// Verify the Deployment's references were rewritten.
	deploy, err := client.AppsV1().Deployments("default").Get(context.Background(), deployName, metav1.GetOptions{})
	require.NoError(t, err)

	podSpec := deploy.Spec.Template.Spec

	// ServiceAccountName should be rewritten.
	assert.Equal(t, "my-sa"+suffix, podSpec.ServiceAccountName)

	// The proxy container is the first container (transformed).
	// Remaining containers/init containers keep env refs rewritten.
	// Check volumes — configMap name should be rewritten.
	for _, vol := range podSpec.Volumes {
		if vol.ConfigMap != nil {
			assert.Equal(t, "app-config"+suffix, vol.ConfigMap.Name,
				"volume configMap ref should be rewritten")
		}
	}

	// Verify the prefixed ServiceAccount was created.
	_, err = client.CoreV1().ServiceAccounts("default").Get(context.Background(), "my-sa"+suffix, metav1.GetOptions{})
	require.NoError(t, err)
}

func TestCreateFromManifests_Force(t *testing.T) {
	manifests := packTestManifests(t, map[string]string{
		"deploy.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
spec:
  selector:
    matchLabels:
      app: my-app
  template:
    metadata:
      labels:
        app: my-app
    spec:
      containers:
      - name: app
        image: my-app:latest
`,
	})

	expectedName := testDeployName("my-app")

	// Pre-create an existing deployment.
	client := k8stesting.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      expectedName,
			Namespace: "default",
			Labels: map[string]string{
				meta.LabelBridgeDeployment: expectedName,
			},
		},
	})

	// Force: tear down existing bridge before creating.
	ctx := context.Background()
	if _, err := client.AppsV1().Deployments("default").Get(ctx, expectedName, metav1.GetOptions{}); err == nil {
		_ = DeleteBridgeResources(ctx, client, "default", expectedName, testDeviceID)
	}

	deployName, _, err := testCreateFromManifests(t, client, manifests, "default")
	require.NoError(t, err)
	assert.Equal(t, expectedName, deployName)
}

func TestCreateFromManifests_MultiDocumentYAML(t *testing.T) {
	manifests := packTestManifests(t, map[string]string{
		"all.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: settings
data:
  env: production
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: webapp
spec:
  selector:
    matchLabels:
      app: webapp
  template:
    metadata:
      labels:
        app: webapp
    spec:
      containers:
      - name: web
        image: webapp:latest
        ports:
        - containerPort: 8080
`,
	})

	client := k8stesting.NewSimpleClientset()
	deployName, _, err := testCreateFromManifests(t, client, manifests, "default")
	require.NoError(t, err)

	expectedName := testDeployName("webapp")
	assert.Equal(t, expectedName, deployName)

	// ConfigMap should be created with prefixed name.
	suffix := testSuffix
	_, err = client.CoreV1().ConfigMaps("default").Get(context.Background(), "settings"+suffix, metav1.GetOptions{})
	require.NoError(t, err)
}

func TestCreateFromManifests_CreateReturnsError(t *testing.T) {
	manifests := packTestManifests(t, map[string]string{
		"deploy.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
spec:
  selector:
    matchLabels:
      app: my-app
  template:
    metadata:
      labels:
        app: my-app
    spec:
      containers:
      - name: app
        image: my-app:latest
`,
		"config.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
data:
  DB_HOST: localhost
`,
	})

	client := k8stesting.NewSimpleClientset()

	// Make Deployment creation fail.
	client.PrependReactor("create", "deployments", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("simulated deployment creation failure")
	})

	bundle, err := SourceFromManifests(manifests)
	require.NoError(t, err)

	sourceName := FindDeploymentName(bundle)

	tc := &TransformContext{
		Context:         context.Background(),
		DeviceID:        testDeviceID,
		SourceName:      sourceName,
		SourceNamespace: "default",
	}

	transforms := []Transformer{
		PruneAllMetadata(),
		InjectProxyImage(testProxyImage),
		SuffixNames(testSuffix),
		InjectLabels(),
		TransformSelectors(),
		AppendBridgeService("default"),
		SetNamespace("default"),
		ClearClusterIPs(),
		RewriteRefs(),
	}

	err = Transform(tc, bundle, transforms)
	require.NoError(t, err)

	deployName := Lookup(NewResourceKey(sourceName, "apps", "Deployment"), bundle, tc.NameMap)

	err = Save(context.Background(), client, nil, bundle)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "simulated deployment creation failure")

	// Caller is responsible for cleanup; verify it works.
	_ = DeleteBridgeResources(context.Background(), client, "default", deployName, testDeviceID)

	var cleanedUp bool
	for _, action := range client.Actions() {
		if action.GetVerb() == "delete-collection" && action.GetResource().Resource == "configmaps" {
			dc := action.(clienttesting.DeleteCollectionAction)
			expectedSel := meta.LabelBridgeDeployment + "=" + deployName + "," + meta.LabelDeviceID + "=" + testDeviceID
			if dc.GetListRestrictions().Labels.String() == expectedSel {
				cleanedUp = true
				break
			}
		}
	}
	assert.True(t, cleanedUp, "DeleteBridgeResources should clean up configmaps")
}

func TestCreateFromManifests_DoesNotOverrideExistingResources(t *testing.T) {
	manifests := packTestManifests(t, map[string]string{
		"deploy.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
spec:
  selector:
    matchLabels:
      app: my-app
  template:
    metadata:
      labels:
        app: my-app
    spec:
      containers:
      - name: app
        image: my-app:latest
        envFrom:
        - configMapRef:
            name: app-config
`,
		"config.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
data:
  DB_HOST: bridge-value
`,
	})

	// Pre-create a ConfigMap with the ORIGINAL (unprefixed) name, simulating
	// a resource that already exists in the cluster.
	existingCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-config",
			Namespace: "default",
		},
		Data: map[string]string{"DB_HOST": "original-value"},
	}
	client := k8stesting.NewSimpleClientset(existingCM)

	_, _, err := testCreateFromManifests(t, client, manifests, "default")
	require.NoError(t, err)

	// The original ConfigMap should be untouched.
	origCM, err := client.CoreV1().ConfigMaps("default").Get(context.Background(), "app-config", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "original-value", origCM.Data["DB_HOST"])

	// The bridge ConfigMap should exist with the prefixed name.
	suffix := testSuffix
	bridgeCM, err := client.CoreV1().ConfigMaps("default").Get(context.Background(), "app-config"+suffix, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "bridge-value", bridgeCM.Data["DB_HOST"])
}

// packTestManifests creates a tar.gz archive from a map of filename to YAML content.
func packTestManifests(t *testing.T, files map[string]string) []byte {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0644))
	}
	data, err := archive.PackGlobFiles(os.DirFS(dir), "*.yaml")
	require.NoError(t, err)
	return data
}
