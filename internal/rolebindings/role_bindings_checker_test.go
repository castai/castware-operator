package rolebindings

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCheckExtendedPermissionsExist(t *testing.T) {
	t.Run("should return true when both RoleBinding and ClusterRoleBinding exist", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		roleBinding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-rolebinding",
				Namespace: "test-namespace",
				Labels: map[string]string{
					"castware.cast.ai/extended-permissions": "true",
				},
			},
		}

		clusterRoleBinding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-clusterrolebinding",
				Labels: map[string]string{
					"castware.cast.ai/extended-permissions": "true",
				},
			},
		}

		scheme := runtime.NewScheme()
		err := rbacv1.AddToScheme(scheme)
		r.NoError(err)

		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(roleBinding, clusterRoleBinding).Build()

		exists, err := CheckExtendedPermissionsExist(ctx, c, "test-namespace")
		r.NoError(err)
		r.True(exists)
	})

	t.Run("should return false when only RoleBinding exists", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		roleBinding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-rolebinding",
				Namespace: "test-namespace",
				Labels: map[string]string{
					"castware.cast.ai/extended-permissions": "true",
				},
			},
		}

		scheme := runtime.NewScheme()
		err := rbacv1.AddToScheme(scheme)
		r.NoError(err)

		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(roleBinding).Build()

		exists, err := CheckExtendedPermissionsExist(ctx, c, "test-namespace")
		r.NoError(err)
		r.False(exists)
	})

	t.Run("should return false when only ClusterRoleBinding exists", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		clusterRoleBinding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-clusterrolebinding",
				Labels: map[string]string{
					"castware.cast.ai/extended-permissions": "true",
				},
			},
		}

		scheme := runtime.NewScheme()
		err := rbacv1.AddToScheme(scheme)
		r.NoError(err)

		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterRoleBinding).Build()

		exists, err := CheckExtendedPermissionsExist(ctx, c, "test-namespace")
		r.NoError(err)
		r.False(exists)
	})

	t.Run("should return false when neither binding exists", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		scheme := runtime.NewScheme()
		err := rbacv1.AddToScheme(scheme)
		r.NoError(err)

		c := fake.NewClientBuilder().WithScheme(scheme).Build()

		exists, err := CheckExtendedPermissionsExist(ctx, c, "test-namespace")
		r.NoError(err)
		r.False(exists)
	})

	t.Run("should return false when bindings exist without extended permissions label", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		roleBinding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-rolebinding",
				Namespace: "test-namespace",
				Labels: map[string]string{
					"other-label": "other-value",
				},
			},
		}

		clusterRoleBinding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-clusterrolebinding",
				Labels: map[string]string{
					"other-label": "other-value",
				},
			},
		}

		scheme := runtime.NewScheme()
		err := rbacv1.AddToScheme(scheme)
		r.NoError(err)

		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(roleBinding, clusterRoleBinding).Build()

		exists, err := CheckExtendedPermissionsExist(ctx, c, "test-namespace")
		r.NoError(err)
		r.False(exists)
	})

	t.Run("should return false when RoleBinding is in different namespace", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		roleBinding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-rolebinding",
				Namespace: "other-namespace",
				Labels: map[string]string{
					"castware.cast.ai/extended-permissions": "true",
				},
			},
		}

		clusterRoleBinding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-clusterrolebinding",
				Labels: map[string]string{
					"castware.cast.ai/extended-permissions": "true",
				},
			},
		}

		scheme := runtime.NewScheme()
		err := rbacv1.AddToScheme(scheme)
		r.NoError(err)

		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(roleBinding, clusterRoleBinding).Build()

		exists, err := CheckExtendedPermissionsExist(ctx, c, "test-namespace")
		r.NoError(err)
		r.False(exists)
	})

	t.Run("should return true when multiple bindings exist with correct labels", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		roleBinding1 := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-rolebinding-1",
				Namespace: "test-namespace",
				Labels: map[string]string{
					"castware.cast.ai/extended-permissions": "true",
				},
			},
		}

		roleBinding2 := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-rolebinding-2",
				Namespace: "test-namespace",
				Labels: map[string]string{
					"castware.cast.ai/extended-permissions": "true",
				},
			},
		}

		clusterRoleBinding1 := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-clusterrolebinding-1",
				Labels: map[string]string{
					"castware.cast.ai/extended-permissions": "true",
				},
			},
		}

		clusterRoleBinding2 := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-clusterrolebinding-2",
				Labels: map[string]string{
					"castware.cast.ai/extended-permissions": "true",
				},
			},
		}

		scheme := runtime.NewScheme()
		err := rbacv1.AddToScheme(scheme)
		r.NoError(err)

		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(roleBinding1, roleBinding2, clusterRoleBinding1, clusterRoleBinding2).Build()

		exists, err := CheckExtendedPermissionsExist(ctx, c, "test-namespace")
		r.NoError(err)
		r.True(exists)
	})
}
