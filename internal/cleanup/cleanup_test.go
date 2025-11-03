package cleanup

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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

func newTestCluster(t *testing.T, server *httptest.Server) *castwarev1alpha1.Cluster {
	t.Helper()
	// Create a mock HTTP server

	return &castwarev1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "test-namespace",
		},
		Spec: castwarev1alpha1.ClusterSpec{
			Cluster: &castwarev1alpha1.ClusterMetadataSpec{
				ClusterID: uuid.NewString(),
			},
			API: castwarev1alpha1.APISpec{
				APIURL: server.URL,
			},
			APIKeySecret: "test-api-secret",
			HelmRepoURL:  "https://castai.github.io/helm-charts",
		},
	}
}

func newTestPod(t *testing.T) *corev1.Pod {
	t.Helper()
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "castware-operator-pod-" + uuid.NewString(),
			Namespace: "test-namespace",
			Labels: map[string]string{
				"app.kubernetes.io/instance": "castware-operator",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
}

//nolint:unparam
func createMockRelease(name, version, namespace string) *release.Release {
	return &release.Release{
		Name:      name,
		Namespace: namespace,
		Info: &release.Info{
			Status: release.StatusDeployed,
		},
		Chart: &chart.Chart{
			Metadata: &chart.Metadata{
				Name:    name,
				Version: version,
			},
		},
		Config: map[string]interface{}{},
	}
}
