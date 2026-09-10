package controller

import (
	"castai-agent/pkg/services/providers/gke"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/castai"
	mock_castai "github.com/castai/castware-operator/internal/castai/mock"
	castaitest "github.com/castai/castware-operator/internal/castai/test"
	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/config"
	"github.com/castai/castware-operator/internal/helm"
	mock_helm "github.com/castai/castware-operator/internal/helm/mock"
)

var _ = Describe("Cluster Controller", func() {
	Context("When reconciling a resource", func() {
		It("should successfully reconcile the resource", func() {
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})
	})
})

const helmReleaseNameSpotHandler = "castai-spot-handler"

// testAPIKeySecretName is the shared API key secret name used across controller
// tests. Declared as a const so goconst doesn't flag the repeated literal.
const testAPIKeySecretName = "api-key-secret"

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
				Component:   "test-component",
				Version:     "0.2",
				ReleaseName: "test-release",
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
		r.NoError(testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "test-component"}, actualCR))
		r.Equal(cluster.Namespace, actualCR.Namespace)
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

	t.Run("should set ReleaseName from action when installing component", func(t *testing.T) {
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
				Component:   "test-component",
				Version:     "1.0.0",
				ReleaseName: "custom-release-name",
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

		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "test-component"}, actualComponent)
		r.NoError(err)
		r.Equal("custom-release-name", actualComponent.Spec.ReleaseName, "ReleaseName should be set from action")
		r.Equal("1.0.0", actualComponent.Spec.Version)
	})

	t.Run("should default ReleaseName to component name when not provided in install action", func(t *testing.T) {
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
				Version:   "1.0.0",
				// ReleaseName not specified
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

		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "test-component"}, actualComponent)
		r.NoError(err)
		r.Equal("test-component", actualComponent.Spec.ReleaseName, "ReleaseName should default to component name")
		r.Equal("1.0.0", actualComponent.Spec.Version)
	})

	t.Run("should set ReleaseName when installing with upsert", func(t *testing.T) {
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
				Component:   "test-component",
				Upsert:      true,
				Version:     "0.2",
				ReleaseName: "upsert-release-name",
			},
		}
		cluster := newTestCluster(t, clusterID, false)
		component := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-component",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ComponentSpec{
				Cluster:     cluster.Name,
				Component:   "test-component",
				Version:     "0.1",
				ReleaseName: "old-release-name",
			},
		}

		testOps := newClusterTestOps(t, cluster, component)

		mockClient.EXPECT().PollActions(gomock.Any(), clusterID).Return(&castai.PollActionsResponse{
			Actions: []*castai.Action{action},
		}, nil)
		mockClient.EXPECT().AckAction(gomock.Any(), clusterID, action.Id, nil).Return(nil)

		_, err := testOps.sut.pollActions(ctx, mockClient, cluster)
		r.NoError(err)

		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "test-component"}, actualComponent)
		r.NoError(err)

		// Upsert triggers upgrade path, which should update the ReleaseName
		r.Equal("old-release-name", actualComponent.Spec.ReleaseName, "ReleaseName should be preserved from existing component during upsert upgrade")
		r.Equal("0.2", actualComponent.Spec.Version)
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
				Component:   "test-component",
				Version:     "0.2",
				ReleaseName: "test-release",
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
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "test-component"}, actualComponent)
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
			Id:         uuid.NewString(),
			CreateTime: &now,
			ActionUpgrade: &castai.ActionUpgrade{
				Component:   "test-component-1",
				Version:     "0.2",
				ReleaseName: "test-release-1",
			},
		}
		action2 := &castai.Action{
			Id:         uuid.NewString(),
			CreateTime: &now,
			ActionUpgrade: &castai.ActionUpgrade{
				Component:       "test-component-2",
				Version:         "0.2",
				ValuesOverrides: map[string]string{"value2.test": "value2-value", "value3": "value3-value", "value1": "value1-changed"},
				ReleaseName:     "test-release-2",
			},
		}
		action3 := &castai.Action{
			Id:         uuid.NewString(),
			CreateTime: &now,
			ActionUpgrade: &castai.ActionUpgrade{
				Component:       "test-component-3",
				Version:         "0.2",
				ValuesOverrides: map[string]string{"value2.test": "value2-value", "value3": "value3-value"},
				ReleaseName:     "test-release-3",
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
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: component1.Name}, actualComponent)
		r.NoError(err)
		r.Equal("0.2", actualComponent.Spec.Version)
		actualValues := map[string]interface{}{}
		r.NoError(json.Unmarshal(actualComponent.Spec.Values.Raw, &actualValues))
		r.Len(actualValues, 1)
		r.Equal("value1-value", actualValues["value1"])

		actualComponent = &castwarev1alpha1.Component{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: component2.Name}, actualComponent)
		r.NoError(err)
		r.Equal("0.2", actualComponent.Spec.Version)
		actualValues = map[string]interface{}{}
		r.NoError(json.Unmarshal(actualComponent.Spec.Values.Raw, &actualValues))
		r.Len(actualValues, 3)
		r.Equal("value1-changed", actualValues["value1"])
		r.Equal("value2-value", (actualValues["value2"]).(map[string]interface{})["test"])
		r.Equal("value3-value", actualValues["value3"])

		actualComponent = &castwarev1alpha1.Component{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: component3.Name}, actualComponent)
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
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "test-component"}, actualComponent)
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
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "test-component"}, actualComponent)
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
				Component:   "test-component",
				Version:     "0.1",
				ReleaseName: "test-release",
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
				Component:   "test-component",
				Version:     "0.1",
				ReleaseName: "test-release",
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

	t.Run("should ack with error when action is upgrade and missing release name", func(t *testing.T) {
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
				Component:   "test-component",
				Version:     "0.2",
				ReleaseName: "", // Missing release name
			},
		}
		cluster := newTestCluster(t, clusterID, false)

		testOps := newClusterTestOps(t, cluster)

		mockClient.EXPECT().PollActions(gomock.Any(), clusterID).Return(&castai.PollActionsResponse{
			Actions: []*castai.Action{action},
		}, nil)
		mockClient.EXPECT().AckAction(gomock.Any(), clusterID, action.Id, errors.New("release name is required for component upgrade")).Return(nil)

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
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "test-component"}, actualComponent)
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
			Spec: castwarev1alpha1.ComponentSpec{
				Cluster:   cluster.Name,
				Component: "test-component",
			},
		}

		testOps := newClusterTestOps(t, cluster, component)

		mockClient.EXPECT().PollActions(gomock.Any(), clusterID).Return(&castai.PollActionsResponse{
			Actions: []*castai.Action{action},
		}, nil)
		mockClient.EXPECT().AckAction(gomock.Any(), clusterID, action.Id, nil).Return(nil)

		_, err := testOps.sut.pollActions(ctx, mockClient, cluster)
		r.NoError(err)

		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "test-component"}, actualComponent)
		r.NoError(err)

		r.Equal("0.2", actualComponent.Spec.Version)
	})
}

func TestScanExistingComponent(t *testing.T) {
	t.Run("should return no error and not reconcile when the component CR exists", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)
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

		reconcile, err := testOps.sut.scanExistingComponent(ctx, mockClient, cluster, "release-name", "test-component")
		r.NoError(err)
		r.False(reconcile)
	})

	t.Run("should create a new component CR when it does not exist but the helm chart is installed", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)
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
			ReleaseName: "release-name",
		}).Return(&release.Release{
			Name: "release-name",
			Chart: &chart.Chart{
				Metadata: &chart.Metadata{
					Version: "1.2.3",
				},
			},
			Config: helmValues,
		}, nil)

		reconcile, err := testOps.sut.scanExistingComponent(ctx, mockClient, cluster, "release-name", "test-component")
		r.NoError(err)
		r.True(reconcile)

		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "test-component"}, actualComponent)
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
				APIKeySecret:  testAPIKeySecretName,
				API: castwarev1alpha1.APISpec{
					APIURL: apiServer.URL,
				},
			},
		}
		apiKeySecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testAPIKeySecretName,
				Namespace: "test-namespace",
			},
			Data: map[string][]byte{"API_KEY": []byte("api-key")},
		}

		testOps := newClusterTestOps(t, cluster, apiKeySecret)
		req := reconcile.Request{NamespacedName: client.ObjectKey{Name: cluster.Name, Namespace: cluster.Namespace}}

		_, err := testOps.sut.Reconcile(ctx, req)
		r.NoError(err)

		actualCluster := &castwarev1alpha1.Cluster{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, actualCluster)
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
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, actualCluster)
		r.NoError(err)

		availableCondition := meta.FindStatusCondition(actualCluster.Status.Conditions, typeAvailableCluster)
		r.NotNil(availableCondition)
		r.Equal(metav1.ConditionTrue, availableCondition.Status)
	})

	t.Run("should re-register cluster with operator install method when already registered but LastRegistrationVersion is empty", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()

		existingClusterID := uuid.NewString()
		cluster := &castwarev1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ClusterSpec{
				MigrationMode: castwarev1alpha1.ClusterMigrationModeWrite,
				Provider:      gke.Name,
				APIKeySecret:  testAPIKeySecretName,
				API: castwarev1alpha1.APISpec{
					APIURL: apiServer.URL,
				},
				Cluster: &castwarev1alpha1.ClusterMetadataSpec{
					ClusterID: existingClusterID,
				},
			},
		}
		apiKeySecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testAPIKeySecretName,
				Namespace: "test-namespace",
			},
			Data: map[string][]byte{"API_KEY": []byte("api-key")},
		}

		testOps := newClusterTestOps(t, cluster, apiKeySecret)
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

		req := reconcile.Request{NamespacedName: client.ObjectKey{Name: cluster.Name, Namespace: cluster.Namespace}}

		_, err := testOps.sut.Reconcile(ctx, req)
		r.NoError(err)

		actualCluster := &castwarev1alpha1.Cluster{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, actualCluster)
		r.NoError(err)

		r.Equal(existingClusterID, actualCluster.Spec.Cluster.ClusterID)
		r.NotEmpty(actualCluster.Status.LastRegistrationVersion)
		r.Equal("0.0.1-test", actualCluster.Status.LastRegistrationVersion)

		availableCondition := meta.FindStatusCondition(actualCluster.Status.Conditions, typeAvailableCluster)
		r.NotNil(availableCondition)
		r.Equal(metav1.ConditionTrue, availableCondition.Status)
	})
}

func TestSyncTerraformComponents(t *testing.T) {
	t.Run("should return false when cluster terraform flag is not set", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)

		cluster := newTestCluster(t, uuid.NewString(), true)
		cluster.Spec.Terraform = false

		testOps := newClusterTestOps(t, cluster)

		reconcile, err := testOps.sut.syncTerraformComponents(ctx, mockClient, cluster)
		r.NoError(err)
		r.False(reconcile)
	})

	t.Run("should return false and requeue when component CR does not exist yet", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)

		cluster := newTestCluster(t, uuid.NewString(), true)
		cluster.Spec.Terraform = true
		cluster.Spec.MigrationMode = castwarev1alpha1.ClusterMigrationModeWrite

		testOps := newClusterTestOps(t, cluster)

		reconcile, err := testOps.sut.syncTerraformComponents(ctx, mockClient, cluster)
		r.NoError(err)
		r.False(reconcile)
	})

	t.Run("should return false when component CR exists but migration is not terraform", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)

		cluster := newTestCluster(t, uuid.NewString(), true)
		cluster.Spec.Terraform = true

		component := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "castai-agent",
				Namespace: cluster.Namespace,
			},
			Spec: castwarev1alpha1.ComponentSpec{
				Component: "castai-agent",
				Cluster:   cluster.Name,
				Migration: castwarev1alpha1.ComponentMigrationHelm,
			},
		}

		testOps := newClusterTestOps(t, cluster, component)

		reconcile, err := testOps.sut.syncTerraformComponents(ctx, mockClient, cluster)
		r.NoError(err)
		r.False(reconcile)
	})

	t.Run("should return false when component requires extended permissions that are not enabled", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)

		cluster := newTestCluster(t, uuid.NewString(), true)
		cluster.Spec.Terraform = true
		cluster.Spec.MigrationMode = castwarev1alpha1.ClusterMigrationModeWrite

		component := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cluster-controller",
				Namespace: cluster.Namespace,
			},
			Spec: castwarev1alpha1.ComponentSpec{
				Component: "cluster-controller",
				Cluster:   cluster.Name,
				Migration: castwarev1alpha1.ComponentMigrationTerraform,
				Version:   "",
			},
		}

		testOps := newClusterTestOps(t, cluster, component)

		reconcile, err := testOps.sut.syncTerraformComponents(ctx, mockClient, cluster)
		r.NoError(err)
		r.False(reconcile)
	})

	t.Run("should clear migration flag when component has version set in write mode", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)

		cluster := newTestCluster(t, uuid.NewString(), true)
		cluster.Spec.Terraform = true
		cluster.Spec.MigrationMode = castwarev1alpha1.ClusterMigrationModeWrite

		component := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "castai-agent",
				Namespace: cluster.Namespace,
			},
			Spec: castwarev1alpha1.ComponentSpec{
				Component: "castai-agent",
				Cluster:   cluster.Name,
				Migration: castwarev1alpha1.ComponentMigrationTerraform,
				Version:   "1.5.0",
			},
		}

		testOps := newClusterTestOps(t, cluster, component)

		reconcile, err := testOps.sut.syncTerraformComponents(ctx, mockClient, cluster)
		r.NoError(err)
		r.True(reconcile)

		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "castai-agent"}, actualComponent)
		r.NoError(err)
		r.Equal("", actualComponent.Spec.Migration)
		r.Equal("1.5.0", actualComponent.Spec.Version)
	})

	t.Run("should clear migration flag and leave version empty in autoupgrade mode", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)

		cluster := newTestCluster(t, uuid.NewString(), true)
		cluster.Spec.Terraform = true
		cluster.Spec.MigrationMode = castwarev1alpha1.ClusterMigrationModeAutoupgrade

		component := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "castai-agent",
				Namespace: cluster.Namespace,
			},
			Spec: castwarev1alpha1.ComponentSpec{
				Component: "castai-agent",
				Cluster:   cluster.Name,
				Migration: castwarev1alpha1.ComponentMigrationTerraform,
				Version:   "",
			},
		}

		testOps := newClusterTestOps(t, cluster, component)

		reconcile, err := testOps.sut.syncTerraformComponents(ctx, mockClient, cluster)
		r.NoError(err)
		r.True(reconcile)

		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "castai-agent"}, actualComponent)
		r.NoError(err)
		r.Equal("", actualComponent.Spec.Migration)
		r.Equal("", actualComponent.Spec.Version)
	})

	t.Run("should detect and set version from existing helm release in write mode", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)

		cluster := newTestCluster(t, uuid.NewString(), true)
		cluster.Spec.Terraform = true
		cluster.Spec.MigrationMode = castwarev1alpha1.ClusterMigrationModeWrite

		component := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "castai-agent",
				Namespace: cluster.Namespace,
			},
			Spec: castwarev1alpha1.ComponentSpec{
				Component:   "castai-agent",
				Cluster:     cluster.Name,
				Migration:   castwarev1alpha1.ComponentMigrationTerraform,
				Version:     "",
				ReleaseName: "castai-agent",
			},
		}

		testOps := newClusterTestOps(t, cluster, component)

		helmValues := map[string]interface{}{
			"replicaCount": 2,
		}

		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   cluster.Namespace,
			ReleaseName: "castai-agent",
		}).Return(&release.Release{
			Name: "castai-agent",
			Chart: &chart.Chart{
				Metadata: &chart.Metadata{
					Version: "1.2.3",
				},
			},
			Config: helmValues,
		}, nil)

		reconcile, err := testOps.sut.syncTerraformComponents(ctx, mockClient, cluster)
		r.NoError(err)
		r.True(reconcile)

		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "castai-agent"}, actualComponent)
		r.NoError(err)
		r.Equal("", actualComponent.Spec.Migration)
		r.Equal("1.2.3", actualComponent.Spec.Version)
		r.NotNil(actualComponent.Spec.Values)

		var actualValues map[string]interface{}
		err = json.Unmarshal(actualComponent.Spec.Values.Raw, &actualValues)
		r.NoError(err)
		r.Equal(float64(2), actualValues["replicaCount"])
	})

	t.Run("should leave version empty when no existing installation found in write mode", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)

		cluster := newTestCluster(t, uuid.NewString(), true)
		cluster.Spec.Terraform = true
		cluster.Spec.MigrationMode = castwarev1alpha1.ClusterMigrationModeWrite

		component := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "castai-agent",
				Namespace: cluster.Namespace,
			},
			Spec: castwarev1alpha1.ComponentSpec{
				Component:   "castai-agent",
				Cluster:     cluster.Name,
				Migration:   castwarev1alpha1.ComponentMigrationTerraform,
				Version:     "",
				ReleaseName: "castai-agent",
			},
		}

		testOps := newClusterTestOps(t, cluster, component)

		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   cluster.Namespace,
			ReleaseName: "castai-agent",
		}).Return(nil, driver.ErrReleaseNotFound)

		mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameAgent).Return(&castai.Component{HelmChart: components.ComponentNameAgent, ReleaseName: "castai-agent"}, nil)

		reconcile, err := testOps.sut.syncTerraformComponents(ctx, mockClient, cluster)
		r.NoError(err)
		r.True(reconcile)

		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: components.ComponentNameAgent}, actualComponent)
		r.NoError(err)
		r.Equal("", actualComponent.Spec.Migration)
		r.Equal("", actualComponent.Spec.Version)
	})
}

func TestScanExistingComponentsWithTerraform(t *testing.T) {
	t.Run("should not create component CR when cluster terraform flag is true", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)

		cluster := newTestCluster(t, uuid.NewString(), true)
		cluster.Spec.Terraform = true

		testOps := newClusterTestOps(t, cluster)

		testOps.mockHelm.EXPECT().GetRelease(gomock.Any()).Times(0)

		reconcile, err := testOps.sut.scanExistingComponents(ctx, mockClient, cluster)
		r.NoError(err)
		r.False(reconcile)

		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "castai-agent"}, actualComponent)
		r.Error(err)
		r.True(apierrors.IsNotFound(err))
	})
}

func TestScanExistingComponentSpotHandler(t *testing.T) {
	t.Run("should create a new spot-handler component CR when helm chart is installed", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		clusterID := uuid.NewString()
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)

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

		// spot-handler is an umbrella sub-component: the scan gate probes the
		// umbrella release (absent here) before allowing the spot-handler CR.
		expectGateUmbrellaNotInstalled(mockClient, testOps.mockHelm, cluster.Namespace)

		helmValues := map[string]interface{}{
			"image": map[string]interface{}{
				"repository": "castai/spot-handler",
				"tag":        "v2.0.0",
			},
		}
		helmValuesJSON, err := json.Marshal(helmValues)
		r.NoError(err)

		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   cluster.Namespace,
			ReleaseName: "castai-spot-handler",
		}).Return(&release.Release{
			Name: "spot-handler",
			Chart: &chart.Chart{
				Metadata: &chart.Metadata{
					Version: "2.0.0",
				},
			},
			Config: helmValues,
		}, nil)

		reconcile, err := testOps.sut.scanExistingComponent(ctx, mockClient, cluster, "castai-spot-handler", "spot-handler")
		r.NoError(err)
		r.True(reconcile)

		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "spot-handler"}, actualComponent)
		r.NoError(err)
		r.Equal("spot-handler", actualComponent.Name)
		r.Equal(cluster.Namespace, actualComponent.Namespace)
		r.Equal(cluster.Name, actualComponent.Spec.Cluster)
		r.Equal("spot-handler", actualComponent.Spec.Component)
		r.True(actualComponent.Spec.Enabled)
		r.True(actualComponent.Spec.Readonly)
		r.Equal("2.0.0", actualComponent.Spec.Version)
		r.Equal(castwarev1alpha1.ComponentMigrationHelm, actualComponent.Spec.Migration)
		r.NotNil(actualComponent.Spec.Values)
		r.Equal(helmValuesJSON, actualComponent.Spec.Values.Raw)
	})

	t.Run("should create spot-handler component CR from DaemonSet when helm release not found", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		clusterID := uuid.NewString()
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)

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

		daemonSet := &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "castai-spot-handler",
				Namespace: "test-namespace",
				Labels: map[string]string{
					"app.kubernetes.io/name": "castai-spot-handler",
					"helm.sh/chart":          "castai-spot-handler-2.5.0",
				},
			},
		}

		testOps := newClusterTestOps(t, cluster, daemonSet)

		// spot-handler is an umbrella sub-component: the scan gate probes the
		// umbrella release (absent here) before allowing the spot-handler CR.
		expectGateUmbrellaNotInstalled(mockClient, testOps.mockHelm, cluster.Namespace)

		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   cluster.Namespace,
			ReleaseName: "castai-spot-handler",
		}).Return(nil, driver.ErrReleaseNotFound)

		mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameSpotHandler).
			Return(&castai.Component{HelmChart: helmReleaseNameSpotHandler}, nil)

		reconcile, err := testOps.sut.scanExistingComponent(ctx, mockClient, cluster, "castai-spot-handler", "spot-handler")
		r.NoError(err)
		r.True(reconcile)

		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "spot-handler"}, actualComponent)
		r.NoError(err)
		r.Equal("spot-handler", actualComponent.Name)
		r.Equal("2.5.0", actualComponent.Spec.Version)
		r.Equal(castwarev1alpha1.ComponentMigrationYaml, actualComponent.Spec.Migration)
	})

	t.Run("should handle spot-handler with empty version label from DaemonSet", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)
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

		daemonSet := &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "spot-handler",
				Namespace: "test-namespace",
				Labels: map[string]string{
					"app.kubernetes.io/name": "castai-spot-handler",
				},
			},
		}

		testOps := newClusterTestOps(t, cluster, daemonSet)

		// spot-handler is an umbrella sub-component: the scan gate probes the
		// umbrella release (absent here) before allowing the spot-handler CR.
		expectGateUmbrellaNotInstalled(mockClient, testOps.mockHelm, cluster.Namespace)

		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   cluster.Namespace,
			ReleaseName: "castai-spot-handler",
		}).Return(nil, driver.ErrReleaseNotFound)

		mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameSpotHandler).
			Return(&castai.Component{HelmChart: helmReleaseNameSpotHandler}, nil)

		reconcile, err := testOps.sut.scanExistingComponent(ctx, mockClient, cluster, "castai-spot-handler", "spot-handler")
		r.NoError(err)
		r.True(reconcile)

		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "spot-handler"}, actualComponent)
		r.NoError(err)
		r.Equal("", actualComponent.Spec.Version)
	})

	t.Run("should not create spot-handler CR when neither helm nor DaemonSet found", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)
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

		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   cluster.Namespace,
			ReleaseName: "castai-spot-handler",
		}).Return(nil, driver.ErrReleaseNotFound)

		mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameSpotHandler).
			Return(&castai.Component{HelmChart: helmReleaseNameSpotHandler}, nil)

		reconcile, err := testOps.sut.scanExistingComponent(ctx, mockClient, cluster, "castai-spot-handler", "spot-handler")
		r.NoError(err)
		r.False(reconcile)

		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "spot-handler"}, actualComponent)
		r.Error(err)
		r.True(apierrors.IsNotFound(err))
	})
}

func TestSyncTerraformComponentsSpotHandler(t *testing.T) {
	t.Run("should detect and set spot-handler version from existing helm release in write mode", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)

		cluster := newTestCluster(t, uuid.NewString(), true)
		cluster.Spec.Terraform = true
		cluster.Spec.MigrationMode = castwarev1alpha1.ClusterMigrationModeWrite

		component := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "spot-handler",
				Namespace: cluster.Namespace,
			},
			Spec: castwarev1alpha1.ComponentSpec{
				Component:   "spot-handler",
				Cluster:     cluster.Name,
				Migration:   castwarev1alpha1.ComponentMigrationTerraform,
				Version:     "",
				ReleaseName: "castai-spot-handler",
			},
		}

		testOps := newClusterTestOps(t, cluster, component)

		helmValues := map[string]interface{}{
			"nodeSelector": map[string]string{
				"kubernetes.io/os": "linux",
			},
		}

		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   cluster.Namespace,
			ReleaseName: "castai-spot-handler",
		}).Return(&release.Release{
			Name: "spot-handler",
			Chart: &chart.Chart{
				Metadata: &chart.Metadata{
					Version: "2.1.0",
				},
			},
			Config: helmValues,
		}, nil)

		reconcile, err := testOps.sut.syncTerraformComponents(ctx, mockClient, cluster)
		r.NoError(err)
		r.True(reconcile)

		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "spot-handler"}, actualComponent)
		r.NoError(err)
		r.Equal("", actualComponent.Spec.Migration)
		r.Equal("2.1.0", actualComponent.Spec.Version)
		r.NotNil(actualComponent.Spec.Values)

		var actualValues map[string]interface{}
		err = json.Unmarshal(actualComponent.Spec.Values.Raw, &actualValues)
		r.NoError(err)
		r.Equal("linux", (actualValues["nodeSelector"]).(map[string]interface{})["kubernetes.io/os"])
	})

	t.Run("should detect spot-handler version from DaemonSet in write mode when helm not found", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)
		ctx := context.Background()

		cluster := newTestCluster(t, uuid.NewString(), true)
		cluster.Spec.Terraform = true
		cluster.Spec.MigrationMode = castwarev1alpha1.ClusterMigrationModeWrite

		component := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "spot-handler",
				Namespace: cluster.Namespace,
			},
			Spec: castwarev1alpha1.ComponentSpec{
				Component:   "spot-handler",
				Cluster:     cluster.Name,
				Migration:   castwarev1alpha1.ComponentMigrationTerraform,
				Version:     "",
				ReleaseName: "castai-spot-handler",
			},
		}

		daemonSet := &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "castai-spot-handler",
				Namespace: cluster.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/name": "castai-spot-handler",
					"helm.sh/chart":          "castai-spot-handler-2.3.0",
				},
			},
		}

		testOps := newClusterTestOps(t, cluster, component, daemonSet)

		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   cluster.Namespace,
			ReleaseName: "castai-spot-handler",
		}).Return(nil, driver.ErrReleaseNotFound)

		mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameSpotHandler).
			Return(&castai.Component{HelmChart: helmReleaseNameSpotHandler}, nil)

		reconcile, err := testOps.sut.syncTerraformComponents(ctx, mockClient, cluster)
		r.NoError(err)
		r.True(reconcile)

		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "spot-handler"}, actualComponent)
		r.NoError(err)
		r.Equal("", actualComponent.Spec.Migration)
		r.Equal("2.3.0", actualComponent.Spec.Version)
	})

	t.Run("should process both agent and spot-handler terraform migrations", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)

		cluster := newTestCluster(t, uuid.NewString(), true)
		cluster.Spec.Terraform = true
		cluster.Spec.MigrationMode = castwarev1alpha1.ClusterMigrationModeWrite

		agentComponent := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "castai-agent",
				Namespace: cluster.Namespace,
			},
			Spec: castwarev1alpha1.ComponentSpec{
				Component:   "castai-agent",
				Cluster:     cluster.Name,
				Migration:   castwarev1alpha1.ComponentMigrationTerraform,
				Version:     "",
				ReleaseName: "castai-agent",
			},
		}

		spotHandlerComponent := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "spot-handler",
				Namespace: cluster.Namespace,
			},
			Spec: castwarev1alpha1.ComponentSpec{
				Component:   "spot-handler",
				Cluster:     cluster.Name,
				Migration:   castwarev1alpha1.ComponentMigrationTerraform,
				Version:     "",
				ReleaseName: "castai-spot-handler",
			},
		}

		testOps := newClusterTestOps(t, cluster, agentComponent, spotHandlerComponent)

		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   cluster.Namespace,
			ReleaseName: "castai-agent",
		}).Return(&release.Release{
			Name: "castai-agent",
			Chart: &chart.Chart{
				Metadata: &chart.Metadata{
					Version: "1.5.0",
				},
			},
			Config: map[string]interface{}{},
		}, nil)

		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   cluster.Namespace,
			ReleaseName: "castai-spot-handler",
		}).Return(&release.Release{
			Name: "spot-handler",
			Chart: &chart.Chart{
				Metadata: &chart.Metadata{
					Version: "2.1.0",
				},
			},
			Config: map[string]interface{}{},
		}, nil)

		reconcile, err := testOps.sut.syncTerraformComponents(ctx, mockClient, cluster)
		r.NoError(err)
		r.True(reconcile)

		actualAgentComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "castai-agent"}, actualAgentComponent)
		r.NoError(err)
		r.Equal("", actualAgentComponent.Spec.Migration)
		r.Equal("1.5.0", actualAgentComponent.Spec.Version)

		actualSpotHandlerComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "spot-handler"}, actualSpotHandlerComponent)
		r.NoError(err)
		r.Equal("", actualSpotHandlerComponent.Spec.Migration)
		r.Equal("2.1.0", actualSpotHandlerComponent.Spec.Version)
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

	// Set a default test version for the castai client
	castai.SetVersion(config.CastwareOperatorVersion{
		Version:   "v0.0.1-test",
		GitCommit: "test-commit",
		GitRef:    "test-ref",
	})

	err := castwarev1alpha1.AddToScheme(scheme)
	r.NoError(err)

	err = corev1.AddToScheme(scheme)
	r.NoError(err)

	err = batchv1.AddToScheme(scheme)
	r.NoError(err)

	err = appsv1.AddToScheme(scheme)
	r.NoError(err)

	err = rbacv1.AddToScheme(scheme)
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
			Log:    &logrus.Logger{Out: io.Discard},
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

// expectGateUmbrellaNotInstalled wires up the mock expectations for the cluster
// controller's umbrella mutual-exclusivity probe when a sub-component is being
// scanned/installed and the umbrella release is NOT installed: the gate
// resolves only the umbrella release name from Mothership, then probes helm
// and finds no release. Tests that scan/install a sub-component with a castai
// mock must call this so the gate's extra mock calls are expected and the
// umbrella is treated as absent (allowing the sub-component to proceed).
func expectGateUmbrellaNotInstalled(mockClient *mock_castai.MockCastAIClient, mockHelm *mock_helm.MockClient, namespace string) {
	mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
		Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil)
	mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
		Namespace:   namespace,
		ReleaseName: components.ComponentNameUmbrella,
	}).Return(nil, driver.ErrReleaseNotFound)
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
			APIKeySecret: "test-cluster",
			Provider:     "eks",
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

			var reqBody map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			if installMethod, ok := reqBody["castware_install_method"]; ok {
				if installMethod != float64(1) {
					t.Errorf("Expected castware_install_method to be 1 (OPERATOR), got %v", installMethod)
				}
			} else {
				t.Error("Expected castware_install_method to be present in request body")
			}

			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id": "%s", "organizationId": "%s"}`, uuid.NewString(), uuid.NewString())

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

// expectGateUmbrellaInstalledCluster wires up the cluster controller's umbrella
// mutual-exclusivity probe when a sub-component is scanned/installed and the
// umbrella release IS installed: resolve only the umbrella release name from
// Mothership, then probe helm and find a deployed release (blocking).
func expectGateUmbrellaInstalledCluster(mockClient *mock_castai.MockCastAIClient, mockHelm *mock_helm.MockClient, namespace string) {
	mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
		Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil)
	mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
		Namespace:   namespace,
		ReleaseName: components.ComponentNameUmbrella,
	}).Return(&release.Release{Name: components.ComponentNameUmbrella, Info: &release.Info{Status: release.StatusDeployed}}, nil)
}

// expectGateIndividualsPresentCluster wires up the cluster controller's
// umbrella-side mutual-exclusivity probe (used by scan + handleInstall when the
// component is the umbrella): resolve all sub-component release names from
// Mothership, then probe helm for each. castai-agent is found deployed (the
// conflict); spot-handler and cluster-controller are absent.
func expectGateIndividualsPresentCluster(mockClient *mock_castai.MockCastAIClient, mockHelm *mock_helm.MockClient, namespace string) {
	mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
		Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil)
	mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameAgent).
		Return(&castai.Component{Name: components.ComponentNameAgent, ReleaseName: components.ComponentNameAgent}, nil)
	mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameSpotHandler).
		Return(&castai.Component{Name: components.ComponentNameSpotHandler, ReleaseName: components.ComponentNameSpotHandler}, nil)
	mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameClusterController).
		Return(&castai.Component{Name: components.ComponentNameClusterController, ReleaseName: components.ComponentNameClusterController}, nil)

	mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
		Namespace: namespace, ReleaseName: components.ComponentNameAgent,
	}).Return(&release.Release{Name: components.ComponentNameAgent, Info: &release.Info{Status: release.StatusDeployed}}, nil)
	mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
		Namespace: namespace, ReleaseName: components.ComponentNameSpotHandler,
	}).Return(nil, driver.ErrReleaseNotFound)
	mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
		Namespace: namespace, ReleaseName: components.ComponentNameClusterController,
	}).Return(nil, driver.ErrReleaseNotFound)
}

// TestMutualExclusivityGateCluster exercises the umbrella / individual charts
// mutual-exclusivity guard in the ClusterReconciler: scan skips creating a
// conflicting component CR, and handleInstall refuses a conflicting install
// action, acking the error back to Mothership.
func TestMutualExclusivityGateCluster(t *testing.T) {
	t.Parallel()

	t.Run("scan skips creating an individual component CR when the umbrella release is installed", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		clusterID := uuid.NewString()
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)

		cluster := &castwarev1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ClusterSpec{
				Cluster:       &castwarev1alpha1.ClusterMetadataSpec{ClusterID: clusterID},
				MigrationMode: castwarev1alpha1.ClusterMigrationModeRead,
			},
		}
		testOps := newClusterTestOps(t, cluster)

		// detectComponentVersion finds the spot-handler helm release (so the
		// scan would otherwise create a CR)...
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   cluster.Namespace,
			ReleaseName: "castai-spot-handler",
		}).Return(&release.Release{
			Name: "spot-handler",
			Chart: &chart.Chart{
				Metadata: &chart.Metadata{Version: "2.0.0"},
			},
			Config: map[string]interface{}{},
		}, nil)

		// ...but the gate detects the umbrella release is installed and blocks.
		expectGateUmbrellaInstalledCluster(mockClient, testOps.mockHelm, cluster.Namespace)

		reconciled, err := testOps.sut.scanExistingComponent(ctx, mockClient, cluster, "castai-spot-handler", "spot-handler")
		r.NoError(err)
		r.False(reconciled, "scan must not create a sub-component CR when the umbrella is installed")

		// No component CR was created.
		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "spot-handler"}, actualComponent)
		r.True(apierrors.IsNotFound(err), "spot-handler CR must not be created")
	})

	t.Run("handleInstall refuses an umbrella install action when individual releases are present", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		clusterID := uuid.NewString()
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)

		cluster := newTestCluster(t, clusterID, false)
		testOps := newClusterTestOps(t, cluster)

		// The gate resolves all sub-component release names and finds
		// castai-agent installed, so the umbrella install is refused.
		expectGateIndividualsPresentCluster(mockClient, testOps.mockHelm, cluster.Namespace)

		action := &castai.ActionInstall{
			Component: components.ComponentNameUmbrella,
			Version:   "0.1.0",
		}

		err := testOps.sut.handleInstall(ctx, mockClient, cluster, action)
		r.Error(err)
		r.Contains(err.Error(), "cannot install umbrella component")
		r.Contains(err.Error(), "individual component releases present")

		// No umbrella component CR was created.
		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: components.ComponentNameUmbrella}, actualComponent)
		r.True(apierrors.IsNotFound(err), "umbrella CR must not be created when the gate blocks the install")
	})

	t.Run("handleInstall refuses an individual install action when the umbrella release is installed", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		clusterID := uuid.NewString()
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)

		cluster := newTestCluster(t, clusterID, false)
		testOps := newClusterTestOps(t, cluster)

		// The gate detects the umbrella release is installed and refuses the
		// individual component install.
		expectGateUmbrellaInstalledCluster(mockClient, testOps.mockHelm, cluster.Namespace)

		action := &castai.ActionInstall{
			Component: components.ComponentNameAgent,
			Version:   "0.1.0",
		}

		err := testOps.sut.handleInstall(ctx, mockClient, cluster, action)
		r.Error(err)
		r.Contains(err.Error(), "cannot install individual component")
		r.Contains(err.Error(), "umbrella release")

		actualComponent := &castwarev1alpha1.Component{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: components.ComponentNameAgent}, actualComponent)
		r.True(apierrors.IsNotFound(err), "agent CR must not be created when the gate blocks the install")
	})

	t.Run("handleInstall allows a non-overlapping component install (gate does not apply)", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		clusterID := uuid.NewString()
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)

		cluster := newTestCluster(t, clusterID, false)
		testOps := newClusterTestOps(t, cluster)

		// A non-overlapping component skips the gate entirely: no Mothership or
		// helm probe is expected, and the CR is created.
		action := &castai.ActionInstall{
			Component: "test-component",
			Version:   "0.1.0",
		}

		err := testOps.sut.handleInstall(ctx, mockClient, cluster, action)
		r.NoError(err)

		actualComponent := &castwarev1alpha1.Component{}
		r.NoError(testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "test-component"}, actualComponent))
		r.Equal("test-component", actualComponent.Spec.Component)
	})
}

// expectUmbrellaInstalledNoIndividuals wires up the umbrella-first scan probe
// for the pure-umbrella case: resolve the umbrella release name from Mothership,
// find the umbrella helm release deployed, then resolve all sub-component release
// names and find every individual release absent. The umbrella component is
// resolved twice (once by ResolveUmbrellaReleaseName, once inside ResolveNames).
func expectUmbrellaInstalledNoIndividuals(mockClient *mock_castai.MockCastAIClient, mockHelm *mock_helm.MockClient, namespace, umbrellaReleaseName string) {
	// ResolveUmbrellaReleaseName and ResolveNames both resolve the umbrella.
	mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
		Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: umbrellaReleaseName}, nil).Times(2)
	mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameAgent).
		Return(&castai.Component{Name: components.ComponentNameAgent, ReleaseName: components.ComponentNameAgent}, nil)
	mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameSpotHandler).
		Return(&castai.Component{Name: components.ComponentNameSpotHandler, ReleaseName: components.ComponentNameSpotHandler}, nil)
	mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameClusterController).
		Return(&castai.Component{Name: components.ComponentNameClusterController, ReleaseName: components.ComponentNameClusterController}, nil)

	// Umbrella release present (probed by UmbrellaInstalled, then again by adopt).
	mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
		Namespace: namespace, ReleaseName: umbrellaReleaseName,
	}).Return(&release.Release{
		Name:   umbrellaReleaseName,
		Chart:  &chart.Chart{Metadata: &chart.Metadata{Version: "0.10.0"}},
		Config: map[string]interface{}{"global": map[string]interface{}{"castai": map[string]interface{}{"clusterID": "abc"}}},
		Info:   &release.Info{Status: release.StatusDeployed},
	}, nil).Times(2)
	// All sub-component releases absent.
	mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
		Namespace: namespace, ReleaseName: components.ComponentNameAgent,
	}).Return(nil, driver.ErrReleaseNotFound)
	mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
		Namespace: namespace, ReleaseName: components.ComponentNameSpotHandler,
	}).Return(nil, driver.ErrReleaseNotFound)
	mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
		Namespace: namespace, ReleaseName: components.ComponentNameClusterController,
	}).Return(nil, driver.ErrReleaseNotFound)
}

// TestScanExistingComponentsUmbrellaFirst exercises the umbrella-first decision
// tree in scanExistingComponents: a customer-installed umbrella is adopted as a
// single CR and per-component scanning is skipped; without the umbrella the
// per-component scan runs as before; hybrid configs are not migrated and warn
// Mothership once.
func TestScanExistingComponentsUmbrellaFirst(t *testing.T) {
	t.Parallel()

	t.Run("umbrella installed: adopts one umbrella CR and creates zero per-component CRs", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		clusterID := uuid.NewString()
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)

		cluster := &castwarev1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ClusterSpec{
				Cluster:       &castwarev1alpha1.ClusterMetadataSpec{ClusterID: clusterID},
				MigrationMode: castwarev1alpha1.ClusterMigrationModeWrite,
			},
		}
		testOps := newClusterTestOps(t, cluster)

		expectUmbrellaInstalledNoIndividuals(mockClient, testOps.mockHelm, cluster.Namespace, components.ComponentNameUmbrella)

		reconciled, err := testOps.sut.scanExistingComponents(ctx, mockClient, cluster)
		r.NoError(err)
		r.True(reconciled, "scan must reconcile after adopting the umbrella CR")

		// Exactly one umbrella CR was created.
		umbrella := &castwarev1alpha1.Component{}
		r.NoError(testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: components.ComponentNameUmbrella}, umbrella))
		r.Equal(components.ComponentNameUmbrella, umbrella.Spec.Component)
		r.Equal(castwarev1alpha1.ComponentMigrationHelm, umbrella.Spec.Migration, "umbrella CR must use helm migration to preserve revision history")
		r.Equal("0.10.0", umbrella.Spec.Version)
		r.Equal(components.ComponentNameUmbrella, umbrella.Spec.ReleaseName)
		r.False(umbrella.Spec.Readonly, "write mode must not mark the umbrella readonly")
		r.NotNil(umbrella.Spec.Values)

		// Zero per-component CRs: the scan was skipped.
		for _, name := range []string{components.ComponentNameAgent, components.ComponentNameSpotHandler, components.ComponentNameClusterController} {
			err := testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: name}, &castwarev1alpha1.Component{})
			r.True(apierrors.IsNotFound(err), "%s CR must not be created when the umbrella is adopted", name)
		}
	})

	t.Run("umbrella installed in read mode: adopts a readonly umbrella CR, no helm writes", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		clusterID := uuid.NewString()
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)

		cluster := &castwarev1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ClusterSpec{
				Cluster:       &castwarev1alpha1.ClusterMetadataSpec{ClusterID: clusterID},
				MigrationMode: castwarev1alpha1.ClusterMigrationModeRead,
			},
		}
		testOps := newClusterTestOps(t, cluster)

		expectUmbrellaInstalledNoIndividuals(mockClient, testOps.mockHelm, cluster.Namespace, components.ComponentNameUmbrella)

		reconciled, err := testOps.sut.scanExistingComponents(ctx, mockClient, cluster)
		r.NoError(err)
		r.True(reconciled)

		umbrella := &castwarev1alpha1.Component{}
		r.NoError(testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: components.ComponentNameUmbrella}, umbrella))
		r.True(umbrella.Spec.Readonly, "read mode must mark the umbrella readonly so the component reconciler never writes to helm")
		r.Equal(castwarev1alpha1.ComponentMigrationHelm, umbrella.Spec.Migration)

		// No helm Install/Upgrade/Uninstall was expected — only GetRelease mocks
		// were set up (the adopt path observes the release, it does not write).
	})

	t.Run("umbrella absent: per-component scan runs and adopts individual releases", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		clusterID := uuid.NewString()
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)

		cluster := &castwarev1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ClusterSpec{
				Cluster:       &castwarev1alpha1.ClusterMetadataSpec{ClusterID: clusterID},
				MigrationMode: castwarev1alpha1.ClusterMigrationModeRead,
			},
		}
		testOps := newClusterTestOps(t, cluster)

		// The umbrella is absent: resolve the umbrella release name from Mothership
		// and probe helm any number of times (the umbrella-first state probe and
		// each per-component sub-component gate re-probe it) — always not found.
		mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
			Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil).AnyTimes()
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace: cluster.Namespace, ReleaseName: components.ComponentNameUmbrella,
		}).Return(nil, driver.ErrReleaseNotFound).AnyTimes()

		// spot-handler and cluster-controller are absent as helm releases too; their
		// per-component scan falls through to YAML/deployment detection. cluster-controller
		// is a phase-2 component and is skipped unless extended permissions exist (they
		// don't here), so it is not scanned. spot-handler is scanned: detectComponentVersion
		// finds no helm release and no DaemonSet, so no CR is created. Its Mothership
		// lookup resolves the release name (called twice: scan loop + detectComponentVersion);
		// the umbrella gate re-probes the (absent) umbrella and allows it.
		mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameSpotHandler).
			Return(&castai.Component{Name: components.ComponentNameSpotHandler, ReleaseName: components.ComponentNameSpotHandler}, nil).Times(2)
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace: cluster.Namespace, ReleaseName: components.ComponentNameSpotHandler,
		}).Return(nil, driver.ErrReleaseNotFound)

		// The castai-agent helm release is present and will be adopted. Its gate
		// re-probes the (absent) umbrella and is allowed.
		mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameAgent).
			Return(&castai.Component{Name: components.ComponentNameAgent, ReleaseName: components.ComponentNameAgent}, nil)
		mockHelmAgentRelease(testOps.mockHelm, cluster.Namespace)

		reconciled, err := testOps.sut.scanExistingComponents(ctx, mockClient, cluster)
		r.NoError(err)
		r.True(reconciled, "per-component scan must reconcile after adopting castai-agent")

		// The castai-agent CR was created with helm migration (revision-preserving).
		agent := &castwarev1alpha1.Component{}
		r.NoError(testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: components.ComponentNameAgent}, agent))
		r.Equal(castwarev1alpha1.ComponentMigrationHelm, agent.Spec.Migration)

		// No umbrella CR was created.
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: components.ComponentNameUmbrella}, &castwarev1alpha1.Component{})
		r.True(apierrors.IsNotFound(err))
	})

	t.Run("hybrid config: creates zero CRs and warns Mothership once", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		clusterID := uuid.NewString()
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)

		cluster := &castwarev1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ClusterSpec{
				Cluster:       &castwarev1alpha1.ClusterMetadataSpec{ClusterID: clusterID},
				MigrationMode: castwarev1alpha1.ClusterMigrationModeWrite,
			},
		}
		testOps := newClusterTestOps(t, cluster)

		// Umbrella-first probe: umbrella resolved twice (ResolveUmbrellaReleaseName
		// + ResolveNames), umbrella helm release deployed, castai-agent deployed
		// (the hybrid conflict), spot-handler and cluster-controller absent.
		mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
			Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil).Times(2)
		mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameAgent).
			Return(&castai.Component{Name: components.ComponentNameAgent, ReleaseName: components.ComponentNameAgent}, nil)
		mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameSpotHandler).
			Return(&castai.Component{Name: components.ComponentNameSpotHandler, ReleaseName: components.ComponentNameSpotHandler}, nil)
		mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameClusterController).
			Return(&castai.Component{Name: components.ComponentNameClusterController, ReleaseName: components.ComponentNameClusterController}, nil)

		mockHelmUmbrellaDeployed(testOps.mockHelm, cluster.Namespace, components.ComponentNameUmbrella)
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace: cluster.Namespace, ReleaseName: components.ComponentNameAgent,
		}).Return(&release.Release{Name: components.ComponentNameAgent, Info: &release.Info{Status: release.StatusDeployed}}, nil)
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace: cluster.Namespace, ReleaseName: components.ComponentNameSpotHandler,
		}).Return(nil, driver.ErrReleaseNotFound)
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace: cluster.Namespace, ReleaseName: components.ComponentNameClusterController,
		}).Return(nil, driver.ErrReleaseNotFound)

		// Mothership warning is reported once for this cluster.
		mockClient.EXPECT().RecordActionResult(ctx, clusterID, gomock.Any()).
			DoAndReturn(func(_ context.Context, _ string, res *castai.ComponentActionResult) error {
				r.Equal(components.ComponentNameUmbrella, res.Name)
				r.Equal(castai.Status_ACTION_REQUIRED, res.Status, "hybrid warning must be ACTION_REQUIRED")
				r.Contains(res.Message, "hybrid configuration")
				return nil
			}).Times(1)

		reconciled, err := testOps.sut.scanExistingComponents(ctx, mockClient, cluster)
		r.NoError(err)
		r.False(reconciled, "hybrid config must not reconcile (nothing adopted)")

		// Zero CRs of any kind.
		for _, name := range []string{components.ComponentNameUmbrella, components.ComponentNameAgent, components.ComponentNameSpotHandler, components.ComponentNameClusterController} {
			err := testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: name}, &castwarev1alpha1.Component{})
			r.True(apierrors.IsNotFound(err), "%s CR must not be created for a hybrid config", name)
		}

		// Run the scan again: the dedup key is set, so no second warning.
		// Re-wire the probes (the scan re-resolves state each pass).
		mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
			Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil).Times(2)
		mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameAgent).
			Return(&castai.Component{Name: components.ComponentNameAgent, ReleaseName: components.ComponentNameAgent}, nil)
		mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameSpotHandler).
			Return(&castai.Component{Name: components.ComponentNameSpotHandler, ReleaseName: components.ComponentNameSpotHandler}, nil)
		mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameClusterController).
			Return(&castai.Component{Name: components.ComponentNameClusterController, ReleaseName: components.ComponentNameClusterController}, nil)
		mockHelmUmbrellaDeployed(testOps.mockHelm, cluster.Namespace, components.ComponentNameUmbrella)
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace: cluster.Namespace, ReleaseName: components.ComponentNameAgent,
		}).Return(&release.Release{Name: components.ComponentNameAgent, Info: &release.Info{Status: release.StatusDeployed}}, nil)
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace: cluster.Namespace, ReleaseName: components.ComponentNameSpotHandler,
		}).Return(nil, driver.ErrReleaseNotFound)
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace: cluster.Namespace, ReleaseName: components.ComponentNameClusterController,
		}).Return(nil, driver.ErrReleaseNotFound)
		// No RecordActionResult expectation for the second pass: gomock would fail
		// the test if a second warning were attempted.

		_, err = testOps.sut.scanExistingComponents(ctx, mockClient, cluster)
		r.NoError(err)
	})

	t.Run("hybrid config: failed Mothership report retries on the next scan", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		clusterID := uuid.NewString()
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)

		cluster := &castwarev1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ClusterSpec{
				Cluster:       &castwarev1alpha1.ClusterMetadataSpec{ClusterID: clusterID},
				MigrationMode: castwarev1alpha1.ClusterMigrationModeWrite,
			},
		}
		testOps := newClusterTestOps(t, cluster)

		wireHybridProbes := func() {
			mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
				Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil).Times(2)
			mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameAgent).
				Return(&castai.Component{Name: components.ComponentNameAgent, ReleaseName: components.ComponentNameAgent}, nil)
			mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameSpotHandler).
				Return(&castai.Component{Name: components.ComponentNameSpotHandler, ReleaseName: components.ComponentNameSpotHandler}, nil)
			mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameClusterController).
				Return(&castai.Component{Name: components.ComponentNameClusterController, ReleaseName: components.ComponentNameClusterController}, nil)
			mockHelmUmbrellaDeployed(testOps.mockHelm, cluster.Namespace, components.ComponentNameUmbrella)
			testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
				Namespace: cluster.Namespace, ReleaseName: components.ComponentNameAgent,
			}).Return(&release.Release{Name: components.ComponentNameAgent, Info: &release.Info{Status: release.StatusDeployed}}, nil)
			testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
				Namespace: cluster.Namespace, ReleaseName: components.ComponentNameSpotHandler,
			}).Return(nil, driver.ErrReleaseNotFound)
			testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
				Namespace: cluster.Namespace, ReleaseName: components.ComponentNameClusterController,
			}).Return(nil, driver.ErrReleaseNotFound)
		}

		// First pass: the Mothership report fails, so the dedup key is NOT advanced.
		wireHybridProbes()
		mockClient.EXPECT().RecordActionResult(ctx, clusterID, gomock.Any()).
			Return(errors.New("mothership unavailable")).Times(1)
		_, err := testOps.sut.scanExistingComponents(ctx, mockClient, cluster)
		r.NoError(err)

		// Second pass: probes re-run and the report is retried (and succeeds).
		wireHybridProbes()
		mockClient.EXPECT().RecordActionResult(ctx, clusterID, gomock.Any()).Return(nil).Times(1)
		_, err = testOps.sut.scanExistingComponents(ctx, mockClient, cluster)
		r.NoError(err)
	})

	t.Run("umbrella CR already exists: scan is a no-op", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		clusterID := uuid.NewString()
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)

		cluster := &castwarev1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ClusterSpec{
				Cluster:       &castwarev1alpha1.ClusterMetadataSpec{ClusterID: clusterID},
				MigrationMode: castwarev1alpha1.ClusterMigrationModeWrite,
			},
		}
		existingUmbrella := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      components.ComponentNameUmbrella,
				Namespace: cluster.Namespace,
			},
			Spec: castwarev1alpha1.ComponentSpec{
				Component: components.ComponentNameUmbrella,
				Cluster:   cluster.Name,
				Version:   "0.9.0",
			},
		}
		testOps := newClusterTestOps(t, cluster, existingUmbrella)

		// The umbrella-first probe still runs (it must, to decide to skip the
		// per-component scan), but adoptUmbrellaFromExistingRelease short-circuits
		// on the existing CR without a second GetRelease.
		mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
			Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil).Times(2)
		mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameAgent).
			Return(&castai.Component{Name: components.ComponentNameAgent, ReleaseName: components.ComponentNameAgent}, nil)
		mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameSpotHandler).
			Return(&castai.Component{Name: components.ComponentNameSpotHandler, ReleaseName: components.ComponentNameSpotHandler}, nil)
		mockClient.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameClusterController).
			Return(&castai.Component{Name: components.ComponentNameClusterController, ReleaseName: components.ComponentNameClusterController}, nil)
		// Umbrella deployed once (the state probe); adopt is skipped so no second probe.
		mockHelmUmbrellaDeployed(testOps.mockHelm, cluster.Namespace, components.ComponentNameUmbrella)
		// InstalledSubcomponents probes all three overlapping sub-components; all absent.
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace: cluster.Namespace, ReleaseName: components.ComponentNameAgent,
		}).Return(nil, driver.ErrReleaseNotFound)
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace: cluster.Namespace, ReleaseName: components.ComponentNameSpotHandler,
		}).Return(nil, driver.ErrReleaseNotFound)
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace: cluster.Namespace, ReleaseName: components.ComponentNameClusterController,
		}).Return(nil, driver.ErrReleaseNotFound)

		reconciled, err := testOps.sut.scanExistingComponents(ctx, mockClient, cluster)
		r.NoError(err)
		r.False(reconciled, "scan with an existing umbrella CR must be a no-op")

		// The pre-existing umbrella CR is unchanged.
		umbrella := &castwarev1alpha1.Component{}
		r.NoError(testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: components.ComponentNameUmbrella}, umbrella))
		r.Equal("0.9.0", umbrella.Spec.Version, "existing umbrella CR must not be overwritten")
	})
}

// mockHelmUmbrellaDeployed wires a single GetRelease returning a deployed
// umbrella release. Extracted so hybrid/already-exists tests can reuse it.
// nolint:unparam // releaseName is always components.ComponentNameUmbrella today; kept for readability at call sites.
func mockHelmUmbrellaDeployed(mockHelm *mock_helm.MockClient, namespace, releaseName string) {
	mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
		Namespace: namespace, ReleaseName: releaseName,
	}).Return(&release.Release{
		Name:   releaseName,
		Chart:  &chart.Chart{Metadata: &chart.Metadata{Version: "0.10.0"}},
		Config: map[string]interface{}{},
		Info:   &release.Info{Status: release.StatusDeployed},
	}, nil)
}

// mockHelmAgentRelease wires a GetRelease returning a deployed castai-agent
// release for the per-component scan path.
func mockHelmAgentRelease(mockHelm *mock_helm.MockClient, namespace string) {
	mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
		Namespace: namespace, ReleaseName: components.ComponentNameAgent,
	}).Return(&release.Release{
		Name:   components.ComponentNameAgent,
		Chart:  &chart.Chart{Metadata: &chart.Metadata{Version: "1.5.0"}},
		Config: map[string]interface{}{"image": map[string]interface{}{"tag": "v1.5.0"}},
		Info:   &release.Info{Status: release.StatusDeployed},
	}, nil)
}
