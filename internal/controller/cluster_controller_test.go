package controller

import (
	"castai-agent/pkg/services/providers/gke"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"testing"
	"time"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/castai"
	mock_castai "github.com/castai/castware-operator/internal/castai/mock"
	castaitest "github.com/castai/castware-operator/internal/castai/test"
	"github.com/castai/castware-operator/internal/config"
	"github.com/castai/castware-operator/internal/helm"
	mock_helm "github.com/castai/castware-operator/internal/helm/mock"
	"github.com/golang/mock/gomock"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("Cluster Controller", func() {
	Context("When reconciling a resource", func() {

		It("should successfully reconcile the resource", func() {

			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
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
		cluster := newTestCluster(t, clusterID, false)

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
		cluster := newTestCluster(t, clusterID, false)
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
		cluster := newTestCluster(t, clusterID, false)
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
		cluster := newTestCluster(t, clusterID, false)
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
		cluster := newTestCluster(t, clusterID, false)
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
		cluster := newTestCluster(t, clusterID, false)
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
		cluster := newTestCluster(t, clusterID, false)

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
		cluster := newTestCluster(t, clusterID, false)

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
		cluster := newTestCluster(t, clusterID, false)
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
		cluster := newTestCluster(t, clusterID, false)
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
		cluster := newTestCluster(t, clusterID, false)
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

func TestScanExistingComponent(t *testing.T) {
	t.Run("should return no error and not reconcile when the component CR exists", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		clusterID := uuid.NewString()

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

		reconcile, err := testOps.sut.scanExistingComponent(ctx, cluster, "test-component")
		r.NoError(err)
		r.False(reconcile)
	})

	t.Run("should create a new component CR when it does not exist but the helm chart is installed", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		clusterID := uuid.NewString()

		cluster := &castwarev1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ClusterSpec{
				Cluster: &castwarev1alpha1.ClusterMetadataSpec{
					ClusterID: clusterID,
				},
				MigrationMode: castwarev1alpha1.ClusterMigrationModeRead,
			},
		}
		testOps := newClusterTestOps(t, cluster)

		helmValues := map[string]interface{}{
			"image": map[string]interface{}{
				"repository": "castai/agent",
				"tag":        "v1.2.3",
			},
		}
		helmValuesJSON, err := json.Marshal(helmValues)
		r.NoError(err)

		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   cluster.Namespace,
			ReleaseName: "test-component",
		}).Return(&release.Release{
			Name: "test-component",
			Chart: &chart.Chart{
				Metadata: &chart.Metadata{
					Version: "1.2.3",
				},
			},
			Config: helmValues,
		}, nil)

		reconcile, err := testOps.sut.scanExistingComponent(ctx, cluster, "test-component")
		r.NoError(err)
		r.True(reconcile)

		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "test-component"}, actualComponent)
		r.NoError(err)
		r.Equal("test-component", actualComponent.Name)
		r.Equal(cluster.Namespace, actualComponent.Namespace)
		r.Equal(cluster.Name, actualComponent.Spec.Cluster)
		r.Equal("test-component", actualComponent.Spec.Component)
		r.True(actualComponent.Spec.Enabled)
		r.True(actualComponent.Spec.Readonly)
		r.Equal("1.2.3", actualComponent.Spec.Version)
		r.Equal(castwarev1alpha1.ComponentMigrationHelm, actualComponent.Spec.Migration)
		r.NotNil(actualComponent.Spec.Values)
		r.Equal(helmValuesJSON, actualComponent.Spec.Values.Raw)
	})
}

func TestReconcileCluster(t *testing.T) {
	r := require.New(t)

	r.NoError(os.Setenv("GKE_CLUSTER_NAME", "castware-operator-test"))
	r.NoError(os.Setenv("GKE_LOCATION", "local"))
	r.NoError(os.Setenv("GKE_PROJECT_ID", "local-testenv"))
	r.NoError(os.Setenv("GKE_REGION", "local1"))
	apiServer := newTestApiServer(t)

	t.Cleanup(func() {
		r.NoError(os.Unsetenv("GKE_CLUSTER_NAME"))
		r.NoError(os.Unsetenv("GKE_LOCATION"))
		r.NoError(os.Unsetenv("GKE_PROJECT_ID"))
		r.NoError(os.Unsetenv("GKE_REGION"))
		apiServer.Close()
	})

	t.Run("should register a new cluster", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		// clusterID := uuid.NewString()

		cluster := &castwarev1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ClusterSpec{
				MigrationMode: castwarev1alpha1.ClusterMigrationModeWrite,
				Provider:      gke.Name,
				APIKeySecret:  "api-key-secret",
				API: castwarev1alpha1.APISpec{
					APIURL: apiServer.URL,
				},
			},
		}
		apiKeySecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "api-key-secret",
				Namespace: "test-namespace",
			},
			Data: map[string][]byte{"API_KEY": []byte("api-key")},
		}

		testOps := newClusterTestOps(t, cluster, apiKeySecret)
		req := reconcile.Request{NamespacedName: client.ObjectKey{Name: cluster.Name, Namespace: cluster.Namespace}}

		_, err := testOps.sut.Reconcile(ctx, req)
		r.NoError(err)

		actualCluster := &castwarev1alpha1.Cluster{}
		err = testOps.sut.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, actualCluster)
		r.NoError(err)
		r.NotEmpty(actualCluster.Spec.Cluster.ClusterID)
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   "test-namespace",
			ReleaseName: "castware-operator",
		}).Return(&release.Release{
			Name: "castware-operator",
			Chart: &chart.Chart{
				Metadata: &chart.Metadata{
					Version: "1.2.3",
				},
			},
		}, nil)

		_, err = testOps.sut.Reconcile(ctx, req)
		r.NoError(err)
		err = testOps.sut.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, actualCluster)
		r.NoError(err)

		availableCondition := meta.FindStatusCondition(actualCluster.Status.Conditions, typeAvailableCluster)
		r.NotNil(availableCondition)
		r.Equal(metav1.ConditionTrue, availableCondition.Status)
	})
}

type clusterTestOps struct {
	sut             *ClusterReconciler
	mockHelm        *mock_helm.MockClient
	mockChartLoader *mock_helm.MockChartLoader
}

func newClusterTestOps(t *testing.T, objs ...client.Object) *clusterTestOps {
	t.Helper()
	r := require.New(t)
	scheme := runtime.NewScheme()

	err := castwarev1alpha1.AddToScheme(scheme)
	r.NoError(err)

	err = corev1.AddToScheme(scheme)
	r.NoError(err)

	err = batchv1.AddToScheme(scheme)
	r.NoError(err)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(objs...).Build()

	ctrl := gomock.NewController(t)
	mockChartLoader := mock_helm.NewMockChartLoader(ctrl)
	mockHelm := mock_helm.NewMockClient(ctrl)

	// Create a fake Clientset for pod log reading operations
	fakeClientset := kubernetesfake.NewClientset()

	opts := &clusterTestOps{
		mockHelm:        mockHelm,
		mockChartLoader: mockChartLoader,
		sut: &ClusterReconciler{
			Client: c,
			Scheme: c.Scheme(),
			Log:    logrus.New(),
			Config: &config.Config{
				RequestTimeout:  time.Second,
				HelmReleaseName: "castware-operator",
				PodNamespace:    "test-namespace",
			},
			HelmClient:  mockHelm,
			ChartLoader: mockChartLoader,
			Clientset:   fakeClientset,
			RestConfig:  nil,
		},
	}

	return opts
}

func newTestCluster(t *testing.T, clusterID string, available bool) *castwarev1alpha1.Cluster {
	t.Helper()
	testCluster := &castwarev1alpha1.Cluster{
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
	if available {
		testCluster.Status.Conditions = []metav1.Condition{
			{
				Type:    typeAvailableCluster,
				Status:  metav1.ConditionTrue,
				Reason:  "ClusterIdAvailable",
				Message: "Cluster reconciled",
			},
		}
	}

	return testCluster
}

func newTestApiServer(t *testing.T) *httptest.Server {
	t.Helper()

	dummyUser, _ := json.Marshal(castaitest.CreateUserObject())

	actionResultUrlRegex := regexp.MustCompile("/cluster-management/v1/clusters/(.*?)/components:recordActionResult")

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/me":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(dummyUser)
			return
		case "/v1/kubernetes/external-clusters":
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fmt.Sprintf(`{"id": "%s", "organizationId": "%s"}`, uuid.NewString(), uuid.NewString())))

			w.WriteHeader(http.StatusOK)
			return
		default:
			if actionResultUrlRegex.MatchString(r.URL.Path) {
				if r.Method != http.MethodPost {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}

				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
				w.WriteHeader(http.StatusOK)
				return
			}
			Fail(fmt.Sprintf("Unexpected request path: %s", r.URL.Path))
		}
	}))
	return apiServer
}
