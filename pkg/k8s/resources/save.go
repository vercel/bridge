package resources

import (
	"context"
	"log/slog"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Save persists every resource in the bundle, dispatching to the appropriate
// upsert helper based on the concrete type.
func Save(ctx context.Context, client kubernetes.Interface, dynClient dynamic.Interface, b *Bundle) error {
	for _, r := range b.Resources {
		switch obj := r.Object.(type) {
		case *appsv1.Deployment:
			if err := upsertDeployment(ctx, client, obj); err != nil {
				return err
			}
		case *corev1.Service:
			if err := upsertService(ctx, client, obj); err != nil {
				return err
			}
		case *corev1.Secret:
			if err := upsertSecret(ctx, client, obj.Namespace, obj); err != nil {
				return err
			}
		case *corev1.ConfigMap:
			if err := upsertConfigMap(ctx, client, obj.Namespace, obj); err != nil {
				return err
			}
		case *corev1.ServiceAccount:
			if err := upsertServiceAccount(ctx, client, obj); err != nil {
				return err
			}
		case *unstructured.Unstructured:
			if dynClient != nil {
				upsertUnstructured(ctx, dynClient, obj)
			}
		}
	}
	return nil
}

func upsertDeployment(ctx context.Context, client kubernetes.Interface, deploy *appsv1.Deployment) error {
	ns := deploy.Namespace
	existing, err := client.AppsV1().Deployments(ns).Get(ctx, deploy.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = client.AppsV1().Deployments(ns).Create(ctx, deploy, metav1.CreateOptions{})
		return err
	} else if err != nil {
		return err
	}
	existing.Spec = deploy.Spec
	existing.Labels = deploy.Labels
	_, err = client.AppsV1().Deployments(ns).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

func upsertService(ctx context.Context, client kubernetes.Interface, svc *corev1.Service) error {
	existing, err := client.CoreV1().Services(svc.Namespace).Get(ctx, svc.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = client.CoreV1().Services(svc.Namespace).Create(ctx, svc, metav1.CreateOptions{})
		return err
	} else if err != nil {
		return err
	}
	existing.Spec.Ports = svc.Spec.Ports
	existing.Spec.Selector = svc.Spec.Selector
	existing.Labels = svc.Labels
	_, err = client.CoreV1().Services(svc.Namespace).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

func upsertSecret(ctx context.Context, client kubernetes.Interface, ns string, secret *corev1.Secret) error {
	existing, err := client.CoreV1().Secrets(ns).Get(ctx, secret.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = client.CoreV1().Secrets(ns).Create(ctx, secret, metav1.CreateOptions{})
		return err
	} else if err != nil {
		return err
	}
	secret.ResourceVersion = existing.ResourceVersion
	_, err = client.CoreV1().Secrets(ns).Update(ctx, secret, metav1.UpdateOptions{})
	return err
}

func upsertConfigMap(ctx context.Context, client kubernetes.Interface, ns string, cm *corev1.ConfigMap) error {
	existing, err := client.CoreV1().ConfigMaps(ns).Get(ctx, cm.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = client.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{})
		return err
	} else if err != nil {
		return err
	}
	cm.ResourceVersion = existing.ResourceVersion
	_, err = client.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

func upsertServiceAccount(ctx context.Context, client kubernetes.Interface, sa *corev1.ServiceAccount) error {
	ns := sa.Namespace
	existing, err := client.CoreV1().ServiceAccounts(ns).Get(ctx, sa.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = client.CoreV1().ServiceAccounts(ns).Create(ctx, sa, metav1.CreateOptions{})
		return err
	} else if err != nil {
		return err
	}
	sa.ResourceVersion = existing.ResourceVersion
	_, err = client.CoreV1().ServiceAccounts(ns).Update(ctx, sa, metav1.UpdateOptions{})
	return err
}

// upsertUnstructured creates or updates an unstructured resource via the dynamic
// client. Errors are logged as warnings rather than propagated, matching the
// previous behaviour for unknown resource types.
func upsertUnstructured(ctx context.Context, dynClient dynamic.Interface, u *unstructured.Unstructured) {
	gvr := gvkToGVR(u.GroupVersionKind())
	res := dynClient.Resource(gvr).Namespace(u.GetNamespace())
	existing, err := res.Get(ctx, u.GetName(), metav1.GetOptions{})
	if errors.IsNotFound(err) {
		if _, err := res.Create(ctx, u, metav1.CreateOptions{}); err != nil {
			slog.Warn("Failed to create unstructured resource", "gvk", u.GroupVersionKind(), "name", u.GetName(), "error", err)
		}
	} else if err == nil {
		u.SetResourceVersion(existing.GetResourceVersion())
		if _, err := res.Update(ctx, u, metav1.UpdateOptions{}); err != nil {
			slog.Warn("Failed to update unstructured resource", "gvk", u.GroupVersionKind(), "name", u.GetName(), "error", err)
		}
	} else {
		slog.Warn("Failed to check unstructured resource", "gvk", u.GroupVersionKind(), "name", u.GetName(), "error", err)
	}
}
