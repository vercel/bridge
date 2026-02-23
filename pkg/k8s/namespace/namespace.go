// Package namespace manages bridge namespace lifecycle including creation,
// labeling, RBAC setup, and cleanup.
package namespace

import (
	"context"
	"fmt"

	"github.com/vercel/bridge/pkg/k8s/meta"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// rbacRoleName is the name of the Role created in each bridge namespace.
	rbacRoleName = "bridge-admin"
)

// CreateConfig holds configuration for creating a bridge namespace.
type CreateConfig struct {
	// Name is the namespace name (bridge-<device-id>).
	Name string
	// DeviceID is the KSUID of the device.
	DeviceID string
	// SourceDeployment is the name of the source deployment being cloned.
	SourceDeployment string
	// SourceNamespace is the namespace of the source deployment.
	SourceNamespace string
	// ServiceAccountName is the Administrator's ServiceAccount name for RBAC binding.
	ServiceAccountName string
	// ServiceAccountNamespace is the namespace where the Administrator's ServiceAccount lives.
	ServiceAccountNamespace string
}

// EnsureNamespace creates the bridge namespace with labels if it doesn't exist,
// and sets up RBAC for the Administrator's ServiceAccount.
func EnsureNamespace(ctx context.Context, client kubernetes.Interface, cfg CreateConfig) error {
	labels := map[string]string{
		meta.LabelManagedBy: meta.ManagedByAdministrator,
		meta.LabelDeviceID:  cfg.DeviceID,
	}
	if cfg.SourceDeployment != "" {
		labels[meta.LabelWorkloadSource] = cfg.SourceDeployment
	}
	if cfg.SourceNamespace != "" {
		labels[meta.LabelWorkloadSourceNamespace] = cfg.SourceNamespace
	}

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   cfg.Name,
			Labels: labels,
		},
	}

	existing, err := client.CoreV1().Namespaces().Get(ctx, cfg.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		// Namespace doesn't exist — create it with labels and move on to RBAC.
		if _, err := client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("failed to create namespace %q: %w", cfg.Name, err)
		}
	} else if err != nil {
		return fmt.Errorf("failed to get namespace %q: %w", cfg.Name, err)
	} else {
		// Namespace exists — update labels.
		if existing.Labels == nil {
			existing.Labels = make(map[string]string)
		}
		for k, v := range labels {
			existing.Labels[k] = v
		}
		if _, err := client.CoreV1().Namespaces().Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("failed to update namespace %q labels: %w", cfg.Name, err)
		}
	}

	// Set up RBAC: create Role + RoleBinding granting full access
	if err := ensureRole(ctx, client, cfg.Name); err != nil {
		return err
	}
	if err := ensureRoleBinding(ctx, client, cfg.Name, cfg.ServiceAccountName, cfg.ServiceAccountNamespace); err != nil {
		return err
	}

	return nil
}

// ListBridgeNamespaces returns all namespaces managed by the bridge administrator.
// Optionally filters by device ID.
func ListBridgeNamespaces(ctx context.Context, client kubernetes.Interface, deviceID string) ([]corev1.Namespace, error) {
	selector := meta.LabelManagedBy + "=" + meta.ManagedByAdministrator
	if deviceID != "" {
		selector += "," + meta.LabelDeviceID + "=" + deviceID
	}

	nsList, err := client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list bridge namespaces: %w", err)
	}

	return nsList.Items, nil
}

// DeleteNamespace removes a bridge namespace and all its resources.
func DeleteNamespace(ctx context.Context, client kubernetes.Interface, name string) error {
	err := client.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

func ensureRole(ctx context.Context, client kubernetes.Interface, namespace string) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rbacRoleName,
			Namespace: namespace,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"*"},
				Resources: []string{"*"},
				Verbs:     []string{"*"},
			},
		},
	}

	existing, err := client.RbacV1().Roles(namespace).Get(ctx, rbacRoleName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = client.RbacV1().Roles(namespace).Create(ctx, role, metav1.CreateOptions{})
		return err
	} else if err != nil {
		return err
	}

	// Update existing role rules
	existing.Rules = role.Rules
	_, err = client.RbacV1().Roles(namespace).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

func ensureRoleBinding(ctx context.Context, client kubernetes.Interface, namespace, saName, saNamespace string) error {
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rbacRoleName,
			Namespace: namespace,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      saName,
				Namespace: saNamespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     rbacRoleName,
		},
	}

	existing, err := client.RbacV1().RoleBindings(namespace).Get(ctx, rbacRoleName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = client.RbacV1().RoleBindings(namespace).Create(ctx, binding, metav1.CreateOptions{})
		return err
	} else if err != nil {
		return err
	}

	// Update existing binding
	existing.Subjects = binding.Subjects
	existing.RoleRef = binding.RoleRef
	_, err = client.RbacV1().RoleBindings(namespace).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}
