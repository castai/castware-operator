package controller

import (
	"context"
	"io"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	components "github.com/castai/castware-operator/internal/component"
)

func TestRequiresExtendedPermissions(t *testing.T) {
	t.Parallel()

	t.Run("umbrella with tags.readonly is minimal-permission", func(t *testing.T) {
		t.Parallel()
		component := &castwarev1alpha1.Component{
			Spec: castwarev1alpha1.ComponentSpec{
				Component: components.ComponentNameUmbrella,
				Values:    &v1.JSON{Raw: []byte(`{"tags":{"readonly":true}}`)},
			},
		}
		require.False(t, requiresExtendedPermissions(component))
	})

	t.Run("umbrella with tags.full is extended-permission", func(t *testing.T) {
		t.Parallel()
		component := &castwarev1alpha1.Component{
			Spec: castwarev1alpha1.ComponentSpec{
				Component: components.ComponentNameUmbrella,
				Values:    &v1.JSON{Raw: []byte(`{"tags":{"full":true}}`)},
			},
		}
		require.True(t, requiresExtendedPermissions(component))
	})

	t.Run("umbrella with nil values is extended-permission (safe default)", func(t *testing.T) {
		t.Parallel()
		component := &castwarev1alpha1.Component{
			Spec: castwarev1alpha1.ComponentSpec{
				Component: components.ComponentNameUmbrella,
				Values:    nil,
			},
		}
		require.True(t, requiresExtendedPermissions(component))
	})

	t.Run("umbrella with malformed values is extended-permission (safe default)", func(t *testing.T) {
		t.Parallel()
		component := &castwarev1alpha1.Component{
			Spec: castwarev1alpha1.ComponentSpec{
				Component: components.ComponentNameUmbrella,
				// Invalid JSON: UnmarshalJSON returns an error, and the umbrella
				// fallback must be extended-permissions-required.
				Values: &v1.JSON{Raw: []byte(`{not-json`)},
			},
		}
		require.True(t, requiresExtendedPermissions(component))
	})

	t.Run("cluster-controller is always extended-permission", func(t *testing.T) {
		t.Parallel()
		component := &castwarev1alpha1.Component{
			Spec: castwarev1alpha1.ComponentSpec{
				Component: components.ComponentNameClusterController,
				Values:    nil,
			},
		}
		require.True(t, requiresExtendedPermissions(component))
	})

	t.Run("agent is never extended-permission", func(t *testing.T) {
		t.Parallel()
		component := &castwarev1alpha1.Component{
			Spec: castwarev1alpha1.ComponentSpec{
				Component: components.ComponentNameAgent,
				Values:    nil,
			},
		}
		require.False(t, requiresExtendedPermissions(component))
	})
}

func TestRequiresExtendedPermissionsByName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := "test-ns"
	log := logrus.New()
	log.SetOutput(io.Discard)

	buildClient := func(objs ...client.Object) client.Client {
		scheme := runtime.NewScheme()
		require.NoError(t, castwarev1alpha1.AddToScheme(scheme))
		return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	}

	t.Run("umbrella CR with tags.readonly is minimal-permission", func(t *testing.T) {
		t.Parallel()
		umbrella := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{Name: components.ComponentNameUmbrella, Namespace: ns},
			Spec: castwarev1alpha1.ComponentSpec{
				Component: components.ComponentNameUmbrella,
				Values:    &v1.JSON{Raw: []byte(`{"tags":{"readonly":true}}`)},
			},
		}
		c := buildClient(umbrella)
		require.False(t, requiresExtendedPermissionsByName(ctx, c, log, ns, components.ComponentNameUmbrella))
	})

	t.Run("umbrella CR with malformed values is extended-permission (safe default)", func(t *testing.T) {
		t.Parallel()
		umbrella := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{Name: components.ComponentNameUmbrella, Namespace: ns},
			Spec: castwarev1alpha1.ComponentSpec{
				Component: components.ComponentNameUmbrella,
				Values:    &v1.JSON{Raw: []byte(`{not-json`)},
			},
		}
		c := buildClient(umbrella)
		require.True(t, requiresExtendedPermissionsByName(ctx, c, log, ns, components.ComponentNameUmbrella))
	})

	t.Run("missing umbrella CR is extended-permission (safe default)", func(t *testing.T) {
		t.Parallel()
		c := buildClient() // no CR exists
		require.True(t, requiresExtendedPermissionsByName(ctx, c, log, ns, components.ComponentNameUmbrella))
	})

	t.Run("missing cluster-controller CR delegates to name-only (extended)", func(t *testing.T) {
		t.Parallel()
		c := buildClient()
		require.True(t, requiresExtendedPermissionsByName(ctx, c, log, ns, components.ComponentNameClusterController))
	})

	t.Run("missing agent CR delegates to name-only (minimal)", func(t *testing.T) {
		t.Parallel()
		c := buildClient()
		require.False(t, requiresExtendedPermissionsByName(ctx, c, log, ns, components.ComponentNameAgent))
	})
}
