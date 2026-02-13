package controller

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/castai"
	mock_castai "github.com/castai/castware-operator/internal/castai/mock"
	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/config"
	"github.com/castai/castware-operator/internal/helm"
	mock_helm "github.com/castai/castware-operator/internal/helm/mock"
)

func TestDetectAndReportOperatorHelmRevisionChange(t *testing.T) {
	t.Run("should detect and report operator revision change when helm is upgraded without version change", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		mockHelm := mock_helm.NewMockClient(ctrl)
		mockCastAI := mock_castai.NewMockCastAIClient(ctrl)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testCluster.Status.LastReportedHelmRevision = 1
		meta.SetStatusCondition(&testCluster.Status.Conditions, metav1.Condition{
			Type:   typeAvailableCluster,
			Status: metav1.ConditionTrue,
			Reason: "ClusterIdAvailable",
		})

		// Create fake k8s client with extended permissions
		scheme := runtime.NewScheme()
		_ = castwarev1alpha1.AddToScheme(scheme)
		_ = rbacv1.AddToScheme(scheme)

		roleBinding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-rb",
				Namespace: "castai-agent",
				Labels: map[string]string{
					"castware.cast.ai/extended-permissions": "true",
				},
			},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(testCluster, roleBinding).WithStatusSubresource(testCluster).Build()

		reconciler := &ClusterReconciler{
			Client:     fakeClient,
			HelmClient: mockHelm,
			Config: &config.Config{
				PodNamespace:    "castai-agent",
				HelmReleaseName: "castware-operator",
			},
			Log: &logrus.Logger{Out: io.Discard},
		}

		// Mock helm release with incremented revision but same version
		mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   "castai-agent",
			ReleaseName: "castware-operator",
		}).Return(&release.Release{
			Name:    "castware-operator",
			Version: 2, // Revision changed from 1 to 2
			Chart: &chart.Chart{
				Metadata: &chart.Metadata{
					Version: "0.3.0", // Same version
				},
			},
			Config: map[string]interface{}{
				// Simulate extendedPermissions change
				"extendedPermissions": true,
			},
		}, nil)

		// Expect RecordActionResult call with updated parameters
		mockCastAI.EXPECT().RecordActionResult(
			gomock.Any(),
			testCluster.Spec.Cluster.ClusterID,
			gomock.Any(),
		).DoAndReturn(func(ctx context.Context, clusterID string, result *castai.ComponentActionResult) error {
			r.Equal(components.ComponentNameOperator, result.Name)
			r.Equal(castai.Action_UPGRADE, result.Action)
			r.Equal("0.3.0", result.Version)
			r.Equal(castai.Status_OK, result.Status)
			// Verify ComponentParams are extracted
			r.NotNil(result.ComponentParams)
			extendedPerms, ok := result.ComponentParams["extendedPermissions"]
			r.True(ok, "extendedPermissions should be present in ComponentParams")
			// The value depends on k8s rolebindings - just verify it's a bool
			r.IsType(false, extendedPerms)
			return nil
		})

		// Call the detection function
		err := reconciler.detectAndReportOperatorHelmRevisionChange(ctx, testCluster, mockCastAI)
		r.NoError(err)

		// Verify LastReportedHelmRevision was updated
		r.Equal(2, testCluster.Status.LastReportedHelmRevision)
	})

	t.Run("should not report when revision has not changed", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		mockHelm := mock_helm.NewMockClient(ctrl)
		mockCastAI := mock_castai.NewMockCastAIClient(ctrl)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testCluster.Status.LastReportedHelmRevision = 1
		meta.SetStatusCondition(&testCluster.Status.Conditions, metav1.Condition{
			Type:   typeAvailableCluster,
			Status: metav1.ConditionTrue,
			Reason: "ClusterIdAvailable",
		})

		reconciler := &ClusterReconciler{
			HelmClient: mockHelm,
			Config: &config.Config{
				PodNamespace:    "castai-agent",
				HelmReleaseName: "castware-operator",
			},
			Log: &logrus.Logger{Out: io.Discard},
		}

		// Mock helm release with same revision
		mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   "castai-agent",
			ReleaseName: "castware-operator",
		}).Return(&release.Release{
			Name:    "castware-operator",
			Version: 1, // Same revision
			Chart: &chart.Chart{
				Metadata: &chart.Metadata{
					Version: "0.3.0",
				},
			},
		}, nil)

		// Should NOT call RecordActionResult
		// mockCastAI has no expectations set, so any call would fail the test

		err := reconciler.detectAndReportOperatorHelmRevisionChange(ctx, testCluster, mockCastAI)
		r.NoError(err)

		// Verify LastReportedHelmRevision unchanged
		r.Equal(1, testCluster.Status.LastReportedHelmRevision)
	})

	t.Run("should skip detection when upgrade job is in progress", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		mockHelm := mock_helm.NewMockClient(ctrl)
		mockCastAI := mock_castai.NewMockCastAIClient(ctrl)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testCluster.Status.UpgradeJobName = "upgrade-job-123"
		testCluster.Status.LastReportedHelmRevision = 1
		meta.SetStatusCondition(&testCluster.Status.Conditions, metav1.Condition{
			Type:   typeAvailableCluster,
			Status: metav1.ConditionTrue,
			Reason: "ClusterIdAvailable",
		})

		reconciler := &ClusterReconciler{
			HelmClient: mockHelm,
			Config: &config.Config{
				PodNamespace:    "castai-agent",
				HelmReleaseName: "castware-operator",
			},
			Log: &logrus.Logger{Out: io.Discard},
		}

		// Should NOT call GetRelease or RecordActionResult when upgrade job is active
		// mockHelm and mockCastAI have no expectations set

		err := reconciler.detectAndReportOperatorHelmRevisionChange(ctx, testCluster, mockCastAI)
		r.NoError(err)
	})

	t.Run("should skip detection when helm release not found (manifest-based install)", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		mockHelm := mock_helm.NewMockClient(ctrl)
		mockCastAI := mock_castai.NewMockCastAIClient(ctrl)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testCluster.Status.LastReportedHelmRevision = 0
		meta.SetStatusCondition(&testCluster.Status.Conditions, metav1.Condition{
			Type:   typeAvailableCluster,
			Status: metav1.ConditionTrue,
			Reason: "ClusterIdAvailable",
		})

		reconciler := &ClusterReconciler{
			HelmClient: mockHelm,
			Config: &config.Config{
				PodNamespace:    "castai-agent",
				HelmReleaseName: "castware-operator",
			},
			Log: &logrus.Logger{Out: io.Discard},
		}

		// Mock helm release not found (manifest-based installation)
		mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   "castai-agent",
			ReleaseName: "castware-operator",
		}).Return(nil, driver.ErrReleaseNotFound)

		// Should NOT call RecordActionResult for manifest-based installs
		// mockCastAI has no expectations set

		err := reconciler.detectAndReportOperatorHelmRevisionChange(ctx, testCluster, mockCastAI)
		r.NoError(err)
	})

	t.Run("should handle GetRelease errors gracefully", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		mockHelm := mock_helm.NewMockClient(ctrl)
		mockCastAI := mock_castai.NewMockCastAIClient(ctrl)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testCluster.Status.LastReportedHelmRevision = 1
		meta.SetStatusCondition(&testCluster.Status.Conditions, metav1.Condition{
			Type:   typeAvailableCluster,
			Status: metav1.ConditionTrue,
			Reason: "ClusterIdAvailable",
		})

		reconciler := &ClusterReconciler{
			HelmClient: mockHelm,
			Config: &config.Config{
				PodNamespace:    "castai-agent",
				HelmReleaseName: "castware-operator",
			},
			Log: &logrus.Logger{Out: io.Discard},
		}

		// Mock helm error
		mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   "castai-agent",
			ReleaseName: "castware-operator",
		}).Return(nil, errors.New("helm connection error"))

		// Should return error but not panic
		err := reconciler.detectAndReportOperatorHelmRevisionChange(ctx, testCluster, mockCastAI)
		r.Error(err)
		r.Contains(err.Error(), "failed to get Helm release")
	})

	t.Run("should not double-report after first installation", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		mockHelm := mock_helm.NewMockClient(ctrl)
		mockCastAI := mock_castai.NewMockCastAIClient(ctrl)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		// Simulate first installation scenario where completeInitialSetup() already set LastReportedHelmRevision
		testCluster.Status.LastReportedHelmRevision = 1
		meta.SetStatusCondition(&testCluster.Status.Conditions, metav1.Condition{
			Type:   typeAvailableCluster,
			Status: metav1.ConditionTrue,
			Reason: "ClusterIdAvailable",
		})

		scheme := runtime.NewScheme()
		_ = castwarev1alpha1.AddToScheme(scheme)
		_ = rbacv1.AddToScheme(scheme)
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(testCluster).WithStatusSubresource(testCluster).Build()

		reconciler := &ClusterReconciler{
			Client:     fakeClient,
			HelmClient: mockHelm,
			Config: &config.Config{
				PodNamespace:    "castai-agent",
				HelmReleaseName: "castware-operator",
			},
			Log: &logrus.Logger{Out: io.Discard},
		}

		// Mock helm release with same revision as reported (no change)
		mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   "castai-agent",
			ReleaseName: "castware-operator",
		}).Return(&release.Release{
			Name:    "castware-operator",
			Version: 1, // Same as LastReportedHelmRevision set by completeInitialSetup
			Chart: &chart.Chart{
				Metadata: &chart.Metadata{
					Version: "0.3.0",
				},
			},
		}, nil)

		// Should NOT call RecordActionResult since revision hasn't changed
		// mockCastAI has no expectations set, so any call would fail the test

		err := reconciler.detectAndReportOperatorHelmRevisionChange(ctx, testCluster, mockCastAI)
		r.NoError(err)

		// Verify LastReportedHelmRevision unchanged
		r.Equal(1, testCluster.Status.LastReportedHelmRevision)
	})
}
