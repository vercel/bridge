package resources

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vercel/bridge/pkg/k8s/meta"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

// staticDiscovery stands in for the real discovery client. client-go's
// FakeDiscovery returns (nil, nil) from ServerPreferredNamespacedResources, so
// using it here would make every assertion below vacuous.
type staticDiscovery struct {
	lists []*metav1.APIResourceList
	err   error
}

func (s staticDiscovery) ServerPreferredNamespacedResources() ([]*metav1.APIResourceList, error) {
	return s.lists, s.err
}

const (
	cleanupDeploy = "api-devbox-39twy3"
	cleanupDevice = "39twy3bsmtyrjxgul8lul9cugfe"
	cleanupNS     = "default"
)

func cleanupSelector() string {
	return meta.LabelBridgeDeployment + "=" + cleanupDeploy + "," + meta.LabelDeviceID + "=" + cleanupDevice
}

func bridgeLabels(deploy string) map[string]string {
	return map[string]string{
		meta.LabelBridgeType:       meta.BridgeTypeProxy,
		meta.LabelBridgeDeployment: deploy,
		meta.LabelDeviceID:         cleanupDevice,
	}
}

var (
	hpaGVR = schema.GroupVersionResource{Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers"}
	pdbGVR = schema.GroupVersionResource{Group: "policy", Version: "v1", Resource: "poddisruptionbudgets"}
	podGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
)

func newUnstructured(apiVersion, kind, name string, labels map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(cleanupNS)
	u.SetName(name)
	u.SetLabels(labels)
	return u
}

func discoveryWithHPAandPDB() staticDiscovery {
	return staticDiscovery{lists: []*metav1.APIResourceList{
		{
			GroupVersion: "autoscaling/v2",
			APIResources: []metav1.APIResource{{
				Name: "horizontalpodautoscalers", Namespaced: true, Kind: "HorizontalPodAutoscaler",
				Verbs: metav1.Verbs{"list", "delete", "deletecollection"},
			}},
		},
		{
			GroupVersion: "policy/v1",
			APIResources: []metav1.APIResource{{
				Name: "poddisruptionbudgets", Namespaced: true, Kind: "PodDisruptionBudget",
				Verbs: metav1.Verbs{"list", "delete", "deletecollection"},
			}},
		},
	}}
}

var gvrToListKind = map[schema.GroupVersionResource]string{
	hpaGVR: "HorizontalPodAutoscalerList",
	pdbGVR: "PodDisruptionBudgetList",
	podGVR: "PodList",
}

func newDynClient(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	dc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind, objs...)
	addDeleteCollectionReactor(dc)
	return dc
}

// addDeleteCollectionReactor teaches the fake dynamic client to honour
// DeleteCollection. client-go's default ObjectReaction has a case for
// DeleteActionImpl but none for delete-collection, so the call is recorded and
// silently does nothing — every state assertion here would pass whether or not
// the production code deleted anything.
//
// The reactor works against the tracker rather than the client: Fake.Invokes
// holds a non-reentrant lock for the duration of the reactor chain, so calling
// dc.Resource(...) from inside a reactor deadlocks.
func addDeleteCollectionReactor(dc *dynamicfake.FakeDynamicClient) {
	dc.PrependReactor("delete-collection", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		dca, ok := action.(k8stesting.DeleteCollectionActionImpl)
		if !ok {
			return false, nil, nil
		}
		gvr := dca.GetResource()
		listKind, ok := gvrToListKind[gvr]
		if !ok {
			return false, nil, nil
		}
		sel := dca.GetListRestrictions().Labels
		if sel == nil {
			sel = labels.Everything()
		}

		// Tracker.List appends "List" to the kind it is given, so it wants the
		// item kind, not the list kind.
		itemKind := strings.TrimSuffix(listKind, "List")
		list, err := dc.Tracker().List(gvr, gvr.GroupVersion().WithKind(itemKind), dca.GetNamespace())
		if err != nil {
			return true, nil, err
		}
		items, err := apimeta.ExtractList(list)
		if err != nil {
			return true, nil, err
		}
		for _, item := range items {
			obj, ok := item.(*unstructured.Unstructured)
			if !ok || !sel.Matches(labels.Set(obj.GetLabels())) {
				continue
			}
			if err := dc.Tracker().Delete(gvr, obj.GetNamespace(), obj.GetName()); err != nil {
				return true, nil, err
			}
		}
		return true, nil, nil
	})
}

func remaining(t *testing.T, dc *dynamicfake.FakeDynamicClient, gvr schema.GroupVersionResource) []string {
	t.Helper()
	list, err := dc.Resource(gvr).Namespace(cleanupNS).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].GetName())
	}
	return names
}

// TestDeleteDynamicBridgeResources_RemovesUntypedKinds is the regression test
// for the leak: Save creates any kind via the dynamic client, but the typed
// delete path only knew five kinds, so HPAs and PDBs accumulated on every
// bridge. Observed in staging as 111 orphaned HPAs and 115 orphaned PDBs
// against 16 live bridge deployments.
func TestDeleteDynamicBridgeResources_RemovesUntypedKinds(t *testing.T) {
	dc := newDynClient(
		newUnstructured("autoscaling/v2", "HorizontalPodAutoscaler", cleanupDeploy, bridgeLabels(cleanupDeploy)),
		newUnstructured("policy/v1", "PodDisruptionBudget", cleanupDeploy, bridgeLabels(cleanupDeploy)),
	)

	deleteDynamicBridgeResources(context.Background(), discoveryWithHPAandPDB(), dc, cleanupNS, cleanupDeploy, cleanupSelector())

	assert.Empty(t, remaining(t, dc, hpaGVR), "HPA should be deleted")
	assert.Empty(t, remaining(t, dc, pdbGVR), "PDB should be deleted")
}

// TestDeleteDynamicBridgeResources_LeavesOtherBridges guards the blast radius:
// the sweep discovers kinds dynamically, so it must still be pinned to this
// bridge's labels and leave concurrent bridges alone.
func TestDeleteDynamicBridgeResources_LeavesOtherBridges(t *testing.T) {
	const otherDeploy = "api-teams-3axkir"
	otherLabels := map[string]string{
		meta.LabelBridgeType:       meta.BridgeTypeProxy,
		meta.LabelBridgeDeployment: otherDeploy,
		meta.LabelDeviceID:         "3axkirsinjb0mbdk35ncywuw65f",
	}

	dc := newDynClient(
		newUnstructured("autoscaling/v2", "HorizontalPodAutoscaler", cleanupDeploy, bridgeLabels(cleanupDeploy)),
		newUnstructured("autoscaling/v2", "HorizontalPodAutoscaler", otherDeploy, otherLabels),
		// An unlabelled HPA belonging to the real workload, not to any bridge.
		newUnstructured("autoscaling/v2", "HorizontalPodAutoscaler", "api-devbox", nil),
	)

	deleteDynamicBridgeResources(context.Background(), discoveryWithHPAandPDB(), dc, cleanupNS, cleanupDeploy, cleanupSelector())

	assert.ElementsMatch(t, []string{otherDeploy, "api-devbox"}, remaining(t, dc, hpaGVR),
		"only this bridge's HPA should be deleted")
}

// TestDeleteDynamicBridgeResources_FallsBackToPerItemDelete covers kinds whose
// discovery entry omits deletecollection: they must still be cleaned up one at
// a time rather than skipped.
func TestDeleteDynamicBridgeResources_FallsBackToPerItemDelete(t *testing.T) {
	disco := staticDiscovery{lists: []*metav1.APIResourceList{{
		GroupVersion: "autoscaling/v2",
		APIResources: []metav1.APIResource{{
			Name: "horizontalpodautoscalers", Namespaced: true, Kind: "HorizontalPodAutoscaler",
			Verbs: metav1.Verbs{"list", "delete"}, // no deletecollection
		}},
	}}}
	dc := newDynClient(
		newUnstructured("autoscaling/v2", "HorizontalPodAutoscaler", cleanupDeploy, bridgeLabels(cleanupDeploy)),
		newUnstructured("autoscaling/v2", "HorizontalPodAutoscaler", "api-teams-3axkir", nil),
	)

	deleteDynamicBridgeResources(context.Background(), disco, dc, cleanupNS, cleanupDeploy, cleanupSelector())

	assert.Equal(t, []string{"api-teams-3axkir"}, remaining(t, dc, hpaGVR),
		"labelled HPA deleted item-by-item, unlabelled one untouched")
}

// TestDeleteDynamicBridgeResources_SkipsGarbageCollectedKinds verifies the
// sweep does not churn on resources the API server's GC already removes. Pods
// inherit the bridge labels from the deployment's pod template, so they match
// the selector and would otherwise be deleted individually.
func TestDeleteDynamicBridgeResources_SkipsGarbageCollectedKinds(t *testing.T) {
	disco := staticDiscovery{lists: []*metav1.APIResourceList{{
		GroupVersion: "v1",
		APIResources: []metav1.APIResource{{
			Name: "pods", Namespaced: true, Kind: "Pod",
			Verbs: metav1.Verbs{"list", "delete", "deletecollection"},
		}},
	}}}
	dc := newDynClient(newUnstructured("v1", "Pod", cleanupDeploy+"-abc123", bridgeLabels(cleanupDeploy)))

	deleteDynamicBridgeResources(context.Background(), disco, dc, cleanupNS, cleanupDeploy, cleanupSelector())

	assert.Len(t, remaining(t, dc, podGVR), 1, "pods are garbage collected with the deployment, not swept")
}

// TestDeleteDynamicBridgeResources_ToleratesDiscoveryFailure ensures cleanup
// degrades rather than panicking: this runs on error paths where the typed
// deletes above have already succeeded.
func TestDeleteDynamicBridgeResources_ToleratesDiscoveryFailure(t *testing.T) {
	dc := newDynClient(newUnstructured("autoscaling/v2", "HorizontalPodAutoscaler", cleanupDeploy, bridgeLabels(cleanupDeploy)))

	assert.NotPanics(t, func() {
		deleteDynamicBridgeResources(context.Background(),
			staticDiscovery{lists: nil, err: assert.AnError}, dc, cleanupNS, cleanupDeploy, cleanupSelector())
	})
	assert.Len(t, remaining(t, dc, hpaGVR), 1, "nothing deleted when discovery yields no kinds")
}

// TestDeleteDynamicBridgeResources_NilDynClient covers callers without a
// dynamic client, which must still be able to run the typed cleanup.
func TestDeleteDynamicBridgeResources_NilDynClient(t *testing.T) {
	assert.NotPanics(t, func() {
		deleteDynamicBridgeResources(context.Background(), discoveryWithHPAandPDB(), nil, cleanupNS, cleanupDeploy, cleanupSelector())
	})
}
