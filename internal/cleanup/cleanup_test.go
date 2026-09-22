package cleanup

import (
	"context"
	"testing"
	"time"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/controller"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCleanup(t *testing.T) {

	t.Run("should delete operator CRs and CRDs", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Create test components with finalizers
		componentWithFinalizer := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "component-with-finalizer",
				Namespace: "test-namespace",
				Finalizers: []string{
					"castware.cast.ai/cleanup-helm",
				},
			},
			Spec: castwarev1alpha1.ComponentSpec{
				Component: "test-component-1",
				Cluster:   "test-cluster",
				Enabled:   true,
			},
		}

		// Component without finalizer
		componentWithoutFinalizer := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "component-without-finalizer",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ComponentSpec{
				Component: "test-component-2",
				Cluster:   "test-cluster",
				Enabled:   true,
			},
		}

		// Create test cluster
		cluster := &castwarev1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ClusterSpec{
				Cluster: &castwarev1alpha1.ClusterMetadataSpec{
					ClusterID: "test-cluster-id",
				},
			},
		}

		// Create test CRDs
		componentCRD := &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name: "components.castware.cast.ai",
			},
		}

		clusterCRD := &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name: "clusters.castware.cast.ai",
			},
		}

		ops := newTestOps(t, componentWithFinalizer, componentWithoutFinalizer, cluster, componentCRD, clusterCRD)

		// Run cleanup
		err := ops.sut.Run(ctx)
		r.NoError(err)

		// Verify all components are deleted
		componentList := &castwarev1alpha1.ComponentList{}
		err = ops.sut.List(ctx, componentList)
		r.NoError(err)
		r.Empty(componentList.Items, "all components should be deleted")

		// Verify all clusters are deleted
		clusterList := &castwarev1alpha1.ClusterList{}
		err = ops.sut.List(ctx, clusterList)
		r.NoError(err)
		r.Empty(clusterList.Items, "all clusters should be deleted")

		// Verify CRDs are deleted
		crdList := &apiextensionsv1.CustomResourceDefinitionList{}
		err = ops.sut.List(ctx, crdList)
		r.NoError(err)
		r.Empty(crdList.Items, "all CRDs should be deleted")
	})

	t.Run("umbrella CR keeps its finalizer and is marked delete candidate (CID-1052 label-gated handoff)", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Umbrella component CR with the helm cleanup finalizer.
		umbrella := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "castai-umbrella",
				Namespace: "test-namespace",
				Finalizers: []string{
					controller.ComponentFinalizer,
				},
			},
			Spec: castwarev1alpha1.ComponentSpec{
				Component: components.ComponentNameUmbrella,
				Cluster:   "test-cluster",
				Enabled:   true,
			},
		}

		// Non-umbrella component CR with the same finalizer.
		agent := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "castai-agent",
				Namespace: "test-namespace",
				Finalizers: []string{
					controller.ComponentFinalizer,
				},
			},
			Spec: castwarev1alpha1.ComponentSpec{
				Component: "castai-agent",
				Cluster:   "test-cluster",
				Enabled:   true,
			},
		}

		cluster := &castwarev1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ClusterSpec{
				Cluster: &castwarev1alpha1.ClusterMetadataSpec{
					ClusterID: "test-cluster-id",
				},
			},
		}

		// Seed the operator CRDs as well: Run always deletes them and errors
		// out if they are missing.
		componentCRD := &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name: "components.castware.cast.ai",
			},
		}
		clusterCRD := &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name: "clusters.castware.cast.ai",
			},
		}

		ops := newTestOps(t, umbrella, agent, cluster, componentCRD, clusterCRD)

		// Run cleanup
		err := ops.sut.Run(ctx)
		r.NoError(err)

		// The umbrella CR is left Terminating on purpose: the operator (absent
		// here) resolves the finalizer without uninstalling the release.
		gotUmbrella := &castwarev1alpha1.Component{}
		err = ops.sut.Get(ctx, client.ObjectKey{Namespace: umbrella.Namespace, Name: umbrella.Name}, gotUmbrella)
		r.NoError(err, "umbrella CR should still exist while it holds its finalizer")
		r.NotNil(gotUmbrella.DeletionTimestamp, "cleanup should have issued a Delete on the umbrella CR")
		r.NotZero(gotUmbrella.DeletionTimestamp.Time)
		r.Contains(gotUmbrella.Finalizers, controller.ComponentFinalizer, "finalizer must NOT be stripped from the umbrella CR (label-gated handoff)")
		r.Equal("true", gotUmbrella.Labels[controller.LabelDeleteCandidate], "umbrella CR should be marked as delete candidate")

		// The non-umbrella CR had its finalizer stripped, so the Delete
		// removed it entirely.
		gotAgent := &castwarev1alpha1.Component{}
		err = ops.sut.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: agent.Name}, gotAgent)
		r.True(apierrors.IsNotFound(err), "non-umbrella component CR should be fully deleted, got: %v", err)

		// Only the terminating umbrella CR remains.
		componentList := &castwarev1alpha1.ComponentList{}
		err = ops.sut.List(ctx, componentList)
		r.NoError(err)
		r.Len(componentList.Items, 1, "only the umbrella component CR should remain")

		// The cluster CR is deleted.
		gotCluster := &castwarev1alpha1.Cluster{}
		err = ops.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, gotCluster)
		r.True(apierrors.IsNotFound(err), "cluster CR should be deleted, got: %v", err)
	})

	t.Run("cleanup deletes only the operator CRDs; umbrella subcomponent CRDs survive (CID-1052)", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// The last three names are representative: these CRDs are owned by
		// external umbrella subcomponent charts (not this repo); the invariant
		// under test is that exactly the two operator CRDs are deleted.
		crdNames := []string{
			"components.castware.cast.ai",
			"clusters.castware.cast.ai",
			"migrations.live.cast.ai",
			"podmutations.pod-mutations.cast.ai",
			"recommendations.cast.ai",
			"gpurecommendations.cast.ai",
			"custommetricsexporterconfigs.cast.ai",
		}
		objs := make([]client.Object, 0, len(crdNames))
		for _, name := range crdNames {
			objs = append(objs, &apiextensionsv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name: name,
				},
			})
		}

		ops := newTestOps(t, objs...)

		// Run cleanup
		err := ops.sut.Run(ctx)
		r.NoError(err)

		operatorCRDs := []string{
			"components.castware.cast.ai",
			"clusters.castware.cast.ai",
		}
		for _, name := range operatorCRDs {
			crd := &apiextensionsv1.CustomResourceDefinition{}
			err := ops.sut.Get(ctx, client.ObjectKey{Name: name}, crd)
			r.True(apierrors.IsNotFound(err), "operator CRD %q should be deleted, got: %v", name, err)
		}

		umbrellaSubcomponentCRDs := []string{
			"migrations.live.cast.ai",
			"podmutations.pod-mutations.cast.ai",
			"recommendations.cast.ai",
			"gpurecommendations.cast.ai",
			"custommetricsexporterconfigs.cast.ai",
		}
		for _, name := range umbrellaSubcomponentCRDs {
			crd := &apiextensionsv1.CustomResourceDefinition{}
			err := ops.sut.Get(ctx, client.ObjectKey{Name: name}, crd)
			r.NoError(err, "umbrella subcomponent CRD %q must survive the operator cleanup", name)
		}
	})
}

type testOps struct {
	sut *Service
}

func newTestOps(t *testing.T, objs ...client.Object) *testOps {
	t.Helper()
	r := require.New(t)
	scheme := runtime.NewScheme()

	err := castwarev1alpha1.AddToScheme(scheme)
	r.NoError(err)

	err = corev1.AddToScheme(scheme)
	r.NoError(err)

	err = apiextensionsv1.AddToScheme(scheme)
	r.NoError(err)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(objs...).Build()

	opts := &testOps{
		sut: &Service{
			Client: c,
			log:    logrus.New(),
		},
	}

	return opts
}
