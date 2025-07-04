package controller

import (
	providers "castai-agent/pkg/services/providers/types"
	mock_types "castai-agent/pkg/services/providers/types/mock"
	"context"
	"github.com/castai/castware-operator/api/v1alpha1"
	"github.com/golang/mock/gomock"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrlruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
	"time"
)

var _ = Describe("Cluster Controller", func() {
	Context("When reconciling a resource", func() {

		It("should successfully reconcile the resource", func() {

			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})
	})
})

func TestClusterController(t *testing.T) {
	t.Run("When cluster is not found stop reconciling with no error", func(t *testing.T) {
		r := require.New(t)
		testOps := newTestClusterTestOps(t, r)
		result, err := testOps.sut.Reconcile(context.Background(), ctrlruntime.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "test"}})
		r.NoError(err)
		r.Equal(ctrlruntime.Result{}, result)
	})
	t.Run("When cluster metadata is not specified should register a new cluster and set the cluster id in the crd", func(t *testing.T) {
		r := require.New(t)
		existing := &v1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: "test",
			},
			Spec: v1alpha1.ClusterSpec{
				Provider:     "eks",
				APIKeySecret: "test-secret",
			},
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-secret",
				Namespace: "test",
			},
			Data: map[string][]byte{
				"API_KEY": []byte("test-api-key"),
			},
		}
		testOps := newTestClusterTestOps(t, r, existing, secret)
		resp := &providers.ClusterRegistration{
			ClusterID:      uuid.New().String(),
			OrganizationID: uuid.New().String(),
		}
		testOps.mockProvider.EXPECT().RegisterCluster(gomock.Any(), gomock.Any()).Return(resp, nil)

		result, err := testOps.sut.Reconcile(context.Background(), ctrlruntime.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "test"}})
		r.NoError(err)

		actualCluster := &v1alpha1.Cluster{}
		r.NoError(testOps.sut.Get(ctx, types.NamespacedName{Namespace: existing.Namespace, Name: existing.Name}, actualCluster))

		r.Equal(resp.ClusterID, actualCluster.Spec.Cluster.ClusterID)
		r.Equal(ctrlruntime.Result{RequeueAfter: time.Second * 30}, result)
	})

}

type clusterTestOps struct {
	mockProvider *mock_types.MockProvider
	sut          *ClusterReconciler
}

func newTestClusterTestOps(t *testing.T, r *require.Assertions, objs ...client.Object) *clusterTestOps {
	t.Helper()
	scheme := runtime.NewScheme()
	r.NoError(v1alpha1.AddToScheme(scheme))
	r.NoError(corev1.AddToScheme(scheme))

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	mockProvider := mock_types.NewMockProvider(gomock.NewController(t))

	opts := &clusterTestOps{
		mockProvider: mockProvider,
		sut: &ClusterReconciler{
			Client: c,
			Scheme: c.Scheme(),
			Log:    logrus.New(),
			GetProvider: func(ctx context.Context, log logrus.FieldLogger, cluster *v1alpha1.Cluster) (providers.Provider, error) {
				return mockProvider, nil
			},
		},
	}

	return opts
}
