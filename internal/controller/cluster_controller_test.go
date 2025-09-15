package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/castai"
	mock_castai "github.com/castai/castware-operator/internal/castai/mock"
	"github.com/castai/castware-operator/internal/helm"
	mock_helm "github.com/castai/castware-operator/internal/helm/mock"
	"github.com/golang/mock/gomock"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/release"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
		t.Parallel()
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
				Version:   "0.1",
			},
		}
		action2 := &castai.Action{
			Id:         uuid.NewString(),
			CreateTime: &now,
			ActionUpgrade: &castai.ActionUpgrade{
				Component: "test-component",
				Version:   "0.2",
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
		t.Parallel()
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
		mockClient.EXPECT().AckAction(gomock.Any(), clusterID, action.Id, errUnknownAction).Return(nil)

		_, err := testOps.sut.pollActions(ctx, mockClient, cluster)
		r.NoError(err)
	})

	t.Run("should poll and do nothing when there are no actions", func(t *testing.T) {
		t.Parallel()
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
		t.Parallel()
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
		t.Parallel()
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

	t.Run("should change component CR version action is upgrade", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)
		ctx := context.Background()
		clusterID := uuid.NewString()
		now := time.Now()
		action := &castai.Action{
			Id:         uuid.NewString(),
			CreateTime: &now,
			ActionUpgrade: &castai.ActionUpgrade{
				Component: "test-component",
				Version:   "0.2",
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
			Spec: castwarev1alpha1.ComponentSpec{Cluster: cluster.Name, Version: "0.1"},
		}

		testOps := newClusterTestOps(t, cluster, component)

		mockClient.EXPECT().PollActions(gomock.Any(), clusterID).Return(&castai.PollActionsResponse{
			Actions: []*castai.Action{action},
		}, nil)
		mockClient.EXPECT().AckAction(gomock.Any(), clusterID, action.Id, nil).Return(nil)

		_, err := testOps.sut.pollActions(ctx, mockClient, cluster)
		r.NoError(err)

		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "test-component"}, actualComponent)
		r.NoError(err)

		r.Equal("0.2", actualComponent.Spec.Version)
	})

	t.Run("should override values and change version when action is upgrade", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)
		ctx := context.Background()
		clusterID := uuid.NewString()
		now := time.Now()
		action1 := &castai.Action{
			Id:            uuid.NewString(),
			CreateTime:    &now,
			ActionUpgrade: &castai.ActionUpgrade{Component: "test-component-1", Version: "0.2"},
		}
		action2 := &castai.Action{
			Id:         uuid.NewString(),
			CreateTime: &now,
			ActionUpgrade: &castai.ActionUpgrade{
				Component:       "test-component-2",
				Version:         "0.2",
				ValuesOverrides: map[string]string{"value2.test": "value2-value", "value3": "value3-value", "value1": "value1-changed"},
			},
		}
		action3 := &castai.Action{
			Id:         uuid.NewString(),
			CreateTime: &now,
			ActionUpgrade: &castai.ActionUpgrade{
				Component:       "test-component-3",
				Version:         "0.2",
				ValuesOverrides: map[string]string{"value2.test": "value2-value", "value3": "value3-value"},
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
		component1 := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-component-1",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ComponentSpec{
				Cluster: cluster.Name,
				Version: "0.1",
				Values:  &v1.JSON{Raw: []byte(`{"value1": "value1-value"}`)},
			},
		}
		component2 := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-component-2",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ComponentSpec{
				Cluster: cluster.Name,
				Version: "0.1",
				Values:  &v1.JSON{Raw: []byte(`{"value1": "value1-value"}`)},
			},
		}
		component3 := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-component-3",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ComponentSpec{Cluster: cluster.Name, Version: "0.1"},
		}

		testOps := newClusterTestOps(t, cluster, component1, component2, component3)

		mockClient.EXPECT().PollActions(gomock.Any(), clusterID).Return(&castai.PollActionsResponse{
			Actions: []*castai.Action{action1, action2, action3},
		}, nil)
		mockClient.EXPECT().AckAction(gomock.Any(), clusterID, action1.Id, nil).Return(nil)
		mockClient.EXPECT().AckAction(gomock.Any(), clusterID, action2.Id, nil).Return(nil)
		mockClient.EXPECT().AckAction(gomock.Any(), clusterID, action3.Id, nil).Return(nil)

		_, err := testOps.sut.pollActions(ctx, mockClient, cluster)
		r.NoError(err)

		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: component1.Name}, actualComponent)
		r.NoError(err)
		r.Equal("0.2", actualComponent.Spec.Version)
		actualValues := map[string]interface{}{}
		r.NoError(json.Unmarshal(actualComponent.Spec.Values.Raw, &actualValues))
		r.Len(actualValues, 1)
		r.Equal("value1-value", actualValues["value1"])

		actualComponent = &castwarev1alpha1.Component{}
		err = testOps.sut.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: component2.Name}, actualComponent)
		r.NoError(err)
		r.Equal("0.2", actualComponent.Spec.Version)
		actualValues = map[string]interface{}{}
		r.NoError(json.Unmarshal(actualComponent.Spec.Values.Raw, &actualValues))
		r.Len(actualValues, 3)
		r.Equal("value1-changed", actualValues["value1"])
		r.Equal("value2-value", (actualValues["value2"]).(map[string]interface{})["test"])
		r.Equal("value3-value", actualValues["value3"])

		actualComponent = &castwarev1alpha1.Component{}
		err = testOps.sut.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: component3.Name}, actualComponent)
		r.NoError(err)
		r.Equal("0.2", actualComponent.Spec.Version)
		actualValues = map[string]interface{}{}
		r.NoError(json.Unmarshal(actualComponent.Spec.Values.Raw, &actualValues))
		r.Len(actualValues, 2)
		r.Equal("value2-value", (actualValues["value2"]).(map[string]interface{})["test"])
		r.Equal("value3-value", actualValues["value3"])
	})

	t.Run("should delete component CR version action is uninstall", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)
		ctx := context.Background()
		clusterID := uuid.NewString()
		now := time.Now()
		action := &castai.Action{
			Id:         uuid.NewString(),
			CreateTime: &now,
			ActionUninstall: &castai.ActionUninstall{
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
			Spec: castwarev1alpha1.ComponentSpec{Cluster: cluster.Name, Version: "0.1"},
		}

		testOps := newClusterTestOps(t, cluster, component)

		mockClient.EXPECT().PollActions(gomock.Any(), clusterID).Return(&castai.PollActionsResponse{
			Actions: []*castai.Action{action},
		}, nil)
		mockClient.EXPECT().AckAction(gomock.Any(), clusterID, action.Id, nil).Return(nil)

		_, err := testOps.sut.pollActions(ctx, mockClient, cluster)
		r.NoError(err)

		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "test-component"}, actualComponent)
		r.Error(err)
		r.True(apierrors.IsNotFound(err))
	})

	t.Run("should set rollback status in component CR version action is rollback", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)
		ctx := context.Background()
		clusterID := uuid.NewString()
		now := time.Now()
		action := &castai.Action{
			Id:         uuid.NewString(),
			CreateTime: &now,
			ActionRollback: &castai.ActionRollback{
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
			Spec: castwarev1alpha1.ComponentSpec{Cluster: cluster.Name, Version: "0.1"},
		}

		testOps := newClusterTestOps(t, cluster, component)

		mockClient.EXPECT().PollActions(gomock.Any(), clusterID).Return(&castai.PollActionsResponse{
			Actions: []*castai.Action{action},
		}, nil)
		mockClient.EXPECT().AckAction(gomock.Any(), clusterID, action.Id, nil).Return(nil)
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   component.Namespace,
			ReleaseName: component.Spec.Component,
		}).Return(&release.Release{
			Name:    "test-component",
			Version: 2,
		}, nil)

		_, err := testOps.sut.pollActions(ctx, mockClient, cluster)
		r.NoError(err)

		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "test-component"}, actualComponent)
		r.NoError(err)
		r.True(actualComponent.Status.Rollback)
	})

	t.Run("should ack with error when action is upgrade and the component is not installed", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)
		ctx := context.Background()
		clusterID := uuid.NewString()
		now := time.Now()
		action := &castai.Action{
			Id:         uuid.NewString(),
			CreateTime: &now,
			ActionUpgrade: &castai.ActionUpgrade{
				Component: "test-component",
				Version:   "0.1",
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
		mockClient.EXPECT().AckAction(gomock.Any(), clusterID, action.Id, errors.New("component not found")).Return(nil)

		_, err := testOps.sut.pollActions(ctx, mockClient, cluster)
		r.NoError(err)
	})

	t.Run("should ack with error when action is uninstall and the component is not installed", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)
		ctx := context.Background()
		clusterID := uuid.NewString()
		now := time.Now()
		action := &castai.Action{
			Id:         uuid.NewString(),
			CreateTime: &now,
			ActionUninstall: &castai.ActionUninstall{
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
		mockClient.EXPECT().AckAction(gomock.Any(), clusterID, action.Id, errComponentNotFount).Return(nil)

		_, err := testOps.sut.pollActions(ctx, mockClient, cluster)
		r.NoError(err)
	})

	t.Run("should ack with error when action is upgrade and the component already up to date", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)
		ctx := context.Background()
		clusterID := uuid.NewString()
		now := time.Now()
		action := &castai.Action{
			Id:         uuid.NewString(),
			CreateTime: &now,
			ActionUpgrade: &castai.ActionUpgrade{
				Component: "test-component",
				Version:   "0.1",
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
			Spec: castwarev1alpha1.ComponentSpec{Cluster: cluster.Name, Version: "0.1"},
		}

		testOps := newClusterTestOps(t, cluster, component)

		mockClient.EXPECT().PollActions(gomock.Any(), clusterID).Return(&castai.PollActionsResponse{
			Actions: []*castai.Action{action},
		}, nil)
		mockClient.EXPECT().AckAction(gomock.Any(), clusterID, action.Id, errors.New("component already up to date")).Return(nil)

		_, err := testOps.sut.pollActions(ctx, mockClient, cluster)
		r.NoError(err)
	})

	t.Run("should ack with error when action action is rollback and the component was never upgraded", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)
		ctx := context.Background()
		clusterID := uuid.NewString()
		now := time.Now()
		action := &castai.Action{
			Id:         uuid.NewString(),
			CreateTime: &now,
			ActionRollback: &castai.ActionRollback{
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
			Spec: castwarev1alpha1.ComponentSpec{Cluster: cluster.Name, Version: "0.1"},
		}

		testOps := newClusterTestOps(t, cluster, component)

		mockClient.EXPECT().PollActions(gomock.Any(), clusterID).Return(&castai.PollActionsResponse{
			Actions: []*castai.Action{action},
		}, nil)
		mockClient.EXPECT().AckAction(gomock.Any(), clusterID, action.Id, ErrNothingToRollback).Return(nil)
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   component.Namespace,
			ReleaseName: component.Spec.Component,
		}).Return(&release.Release{
			Name:    "test-component",
			Version: 1,
		}, nil)

		_, err := testOps.sut.pollActions(ctx, mockClient, cluster)
		r.NoError(err)

		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "test-component"}, actualComponent)
		r.NoError(err)
		r.False(actualComponent.Status.Rollback)
	})

	t.Run("should ack with no error when action is install, the component exists and upsert is enabled", func(t *testing.T) {
		t.Parallel()
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
				Upsert:    true,
				Version:   "0.2",
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
		mockClient.EXPECT().AckAction(gomock.Any(), clusterID, action.Id, nil).Return(nil)

		_, err := testOps.sut.pollActions(ctx, mockClient, cluster)
		r.NoError(err)

		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "test-component"}, actualComponent)
		r.NoError(err)

		r.Equal("0.2", actualComponent.Spec.Version)
	})
}

type clusterTestOps struct {
	sut      *ClusterReconciler
	mockHelm *mock_helm.MockClient
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

	ctrl := gomock.NewController(t)
	mockHelm := mock_helm.NewMockClient(ctrl)

	opts := &clusterTestOps{
		mockHelm: mockHelm,
		sut: &ClusterReconciler{
			Client:     c,
			Scheme:     c.Scheme(),
			Log:        logrus.New(),
			HelmClient: mockHelm,
		},
	}

	return opts
}
