package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/castai"
	mock_castai "github.com/castai/castware-operator/internal/castai/mock"
	"github.com/golang/mock/gomock"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = Describe("Cluster Controller", func() {
	Context("When reconciling a resource", func() {

		It("should successfully reconcile the resource", func() {

			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})

		It("", func() {

		})
	})
})

func TestPollActions(t *testing.T) {

	t.Run("should poll and ack a valid action with no error", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)
		ctx := context.Background()
		clusterID := uuid.NewString()
		now := time.Now()
		action1 := &castai.Action{
			Id:         uuid.NewString(),
			CreateTime: &now,
			ActionInstall: &castai.ActionInstall{
				Component: "test-component",
			},
		}
		action2 := &castai.Action{
			Id:         uuid.NewString(),
			CreateTime: &now,
			ActionUpgrade: &castai.ActionUpgrade{
				Component: "test-component",
			},
		}

		testOps := newClusterTestOps(t)
		cluster := &castwarev1alpha1.Cluster{
			Spec: castwarev1alpha1.ClusterSpec{
				Cluster: &castwarev1alpha1.ClusterMetadataSpec{
					ClusterID: clusterID,
				},
			},
		}

		mockClient.EXPECT().PollActions(gomock.Any(), clusterID).Return(&castai.PollActionsResponse{
			Actions: []*castai.Action{action1, action2},
		}, nil)
		mockClient.EXPECT().AckAction(gomock.Any(), clusterID, action1.Id, nil).Return(nil)
		mockClient.EXPECT().AckAction(gomock.Any(), clusterID, action2.Id, nil).Return(nil)

		_, err := testOps.sut.pollActions(ctx, mockClient, cluster)
		r.NoError(err)
	})

	t.Run("should poll and ack an invalid action with error", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)
		ctx := context.Background()
		clusterID := uuid.NewString()
		now := time.Now()
		action := &castai.Action{
			Id:         uuid.NewString(),
			CreateTime: &now,
		}

		testOps := newClusterTestOps(t)
		cluster := &castwarev1alpha1.Cluster{
			Spec: castwarev1alpha1.ClusterSpec{
				Cluster: &castwarev1alpha1.ClusterMetadataSpec{
					ClusterID: clusterID,
				},
			},
		}

		mockClient.EXPECT().PollActions(gomock.Any(), clusterID).Return(&castai.PollActionsResponse{
			Actions: []*castai.Action{action},
		}, nil)
		mockClient.EXPECT().AckAction(gomock.Any(), clusterID, action.Id, unknownActionError).Return(nil)

		_, err := testOps.sut.pollActions(ctx, mockClient, cluster)
		r.NoError(err)
	})

	t.Run("should poll and do nothing when there are no actions", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)
		ctx := context.Background()
		clusterID := uuid.NewString()
		testOps := newClusterTestOps(t)
		cluster := &castwarev1alpha1.Cluster{
			Spec: castwarev1alpha1.ClusterSpec{
				Cluster: &castwarev1alpha1.ClusterMetadataSpec{
					ClusterID: clusterID,
				},
			},
		}

		mockClient.EXPECT().PollActions(gomock.Any(), clusterID).Return(&castai.PollActionsResponse{
			Actions: []*castai.Action{},
		}, nil)

		_, err := testOps.sut.pollActions(ctx, mockClient, cluster)
		r.NoError(err)
	})

	t.Run("should create a new component CR when action is install", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)
		ctx := context.Background()
		clusterID := uuid.NewString()
		now := time.Now()
		action := &castai.Action{
			Id:         uuid.NewString(),
			CreateTime: &now,
			ActionInstall: &castai.ActionInstall{
				Component: "test-component",
			},
		}
		cluster := &castwarev1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ClusterSpec{
				Cluster: &castwarev1alpha1.ClusterMetadataSpec{
					ClusterID: clusterID,
				},
			},
		}

		testOps := newClusterTestOps(t, cluster)

		mockClient.EXPECT().PollActions(gomock.Any(), clusterID).Return(&castai.PollActionsResponse{
			Actions: []*castai.Action{action},
		}, nil)
		mockClient.EXPECT().AckAction(gomock.Any(), clusterID, action.Id, nil).Return(nil)

		_, err := testOps.sut.pollActions(ctx, mockClient, cluster)
		r.NoError(err)

		actualCR := &castwarev1alpha1.Component{}
		r.NoError(testOps.sut.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "test-component"}, actualCR))
		r.Equal(cluster.Namespace, actualCR.ObjectMeta.Namespace)
		r.Equal(cluster.Name, actualCR.Spec.Cluster)
		r.Equal("test-component", actualCR.Name)
	})

	t.Run("should ack with error when action is install and the component is already installed", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)
		ctx := context.Background()
		clusterID := uuid.NewString()
		now := time.Now()
		action := &castai.Action{
			Id:         uuid.NewString(),
			CreateTime: &now,
			ActionInstall: &castai.ActionInstall{
				Component: "test-component",
			},
		}
		cluster := &castwarev1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ClusterSpec{
				Cluster: &castwarev1alpha1.ClusterMetadataSpec{
					ClusterID: clusterID,
				},
			},
		}
		component := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-component",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ComponentSpec{Cluster: cluster.Name},
		}

		testOps := newClusterTestOps(t, cluster, component)

		mockClient.EXPECT().PollActions(gomock.Any(), clusterID).Return(&castai.PollActionsResponse{
			Actions: []*castai.Action{action},
		}, nil)
		mockClient.EXPECT().AckAction(gomock.Any(), clusterID, action.Id, errors.New("component already exists")).Return(nil)

		_, err := testOps.sut.pollActions(ctx, mockClient, cluster)
		r.NoError(err)
	})
}

type clusterTestOps struct {
	sut *ClusterReconciler
}

func newClusterTestOps(t *testing.T, objs ...client.Object) *clusterTestOps {
	t.Helper()
	r := require.New(t)
	scheme := runtime.NewScheme()

	err := castwarev1alpha1.AddToScheme(scheme)
	r.NoError(err)

	err = corev1.AddToScheme(scheme)
	r.NoError(err)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(objs...).Build()

	opts := &clusterTestOps{
		sut: &ClusterReconciler{
			Client: c,
			Scheme: c.Scheme(),
			Log:    logrus.New(),
		},
	}

	return opts
}
