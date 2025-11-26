package rolebindings

import (
	"context"
	"fmt"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	extendedPermissionsLabel = "castware.cast.ai/extended-permissions"
	extendedPermissionsValue = "true"
)

// CheckExtendedPermissionsExist checks if RoleBinding and ClusterRoleBinding with
// 'castware.cast.ai/extended-permissions: "true"' label exist in the given namespace.
// Returns (roleBindingExists && clusterRoleBindingExists, error).
func CheckExtendedPermissionsExist(ctx context.Context, ctrlClient client.Client, namespace string) (bool, error) {

	// Check for RoleBindings with the extended-permissions label
	roleBindingList := &rbacv1.RoleBindingList{}
	if err := ctrlClient.List(ctx, roleBindingList,
		client.InNamespace(namespace),
		client.MatchingLabels{extendedPermissionsLabel: extendedPermissionsValue},
	); err != nil {
		return false, fmt.Errorf("failed to list RoleBindings: %w", err)
	}

	// Check for ClusterRoleBindings with the extended-permissions label
	clusterRoleBindingList := &rbacv1.ClusterRoleBindingList{}
	if err := ctrlClient.List(ctx, clusterRoleBindingList,
		client.MatchingLabels{extendedPermissionsLabel: extendedPermissionsValue},
	); err != nil {
		return false, fmt.Errorf("failed to list ClusterRoleBindings: %w", err)
	}

	roleBindingExists := len(roleBindingList.Items) > 0
	clusterRoleBindingExists := len(clusterRoleBindingList.Items) > 0

	return roleBindingExists && clusterRoleBindingExists, nil
}
