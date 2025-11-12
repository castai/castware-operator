//nolint:goconst
package controller

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/castai"
	mock_castai "github.com/castai/castware-operator/internal/castai/mock"
	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/helm"
	"github.com/golang/mock/gomock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestOperatorUpgrade(t *testing.T) {
	t.Run("should create upgrade job when operator upgrade action is received", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)
		ctx := context.Background()
		clusterID := uuid.NewString()
		now := time.Now()

		// Set POD_NAME environment variable (required for getCurrentOperatorImage)
		podName := "test-operator-pod"
		r.NoError(os.Setenv("POD_NAME", podName))
		t.Cleanup(func() {
			_ = os.Unsetenv("POD_NAME")
		})

		cluster := newTestCluster(t, clusterID, false)
		operatorPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podName,
				Namespace: cluster.Namespace,
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "manager",
						Image: "castai/castware-operator:v0.0.1",
					},
				},
			},
		}

		testOps := newClusterTestOps(t, cluster, operatorPod)

		action := &castai.Action{
			Id:         uuid.NewString(),
			CreateTime: &now,
			ActionUpgrade: &castai.ActionUpgrade{
				Component: components.ComponentNameOperator,
				Version:   "v0.0.2",
			},
		}

		mockClient.EXPECT().PollActions(gomock.Any(), clusterID).Return(&castai.PollActionsResponse{
			Actions: []*castai.Action{action},
		}, nil)
		mockClient.EXPECT().AckAction(gomock.Any(), clusterID, action.Id, nil).Return(nil)

		_, err := testOps.sut.pollActions(ctx, mockClient, cluster)
		r.NoError(err)

		// Verify cluster status was updated to Progressing
		actualCluster := &castwarev1alpha1.Cluster{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, actualCluster)
		r.NoError(err)

		progressingCondition := meta.FindStatusCondition(actualCluster.Status.Conditions, typeProgressingCluster)
		r.NotNil(progressingCondition)
		r.Equal(metav1.ConditionTrue, progressingCondition.Status)
		r.Equal(progressingReasonOperatorUpgrading, progressingCondition.Reason)
		r.Contains(progressingCondition.Message, "v0.0.2")

		// Verify upgrade job was created
		r.NotEmpty(actualCluster.Status.UpgradeJobName)

		// Verify job exists
		job := &batchv1.Job{}
		err = testOps.sut.Get(ctx, client.ObjectKey{
			Namespace: cluster.Namespace,
			Name:      actualCluster.Status.UpgradeJobName,
		}, job)
		r.NoError(err)

		// Verify job has correct args
		r.Len(job.Spec.Template.Spec.Containers, 1)
		container := job.Spec.Template.Spec.Containers[0]
		r.Equal("castai/castware-operator:v0.0.1", container.Image)
		r.Contains(container.Args, "upgrade")
		r.Contains(container.Args, "--version")
		r.Contains(container.Args, "v0.0.2")
		r.Contains(container.Args, "--cluster-cr-name")
		r.Contains(container.Args, cluster.Name)
		r.Contains(container.Args, "--cluster-cr-namespace")
		r.Contains(container.Args, cluster.Namespace)
	})

	t.Run("should return error when POD_NAME is not set", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)
		ctx := context.Background()
		clusterID := uuid.NewString()
		now := time.Now()

		// Ensure POD_NAME is not set
		_ = os.Unsetenv("POD_NAME")

		cluster := newTestCluster(t, clusterID, false)
		testOps := newClusterTestOps(t, cluster)

		action := &castai.Action{
			Id:         uuid.NewString(),
			CreateTime: &now,
			ActionUpgrade: &castai.ActionUpgrade{
				Component: components.ComponentNameOperator,
				Version:   "v0.0.2",
			},
		}

		mockClient.EXPECT().PollActions(gomock.Any(), clusterID).Return(&castai.PollActionsResponse{
			Actions: []*castai.Action{action},
		}, nil)
		mockClient.EXPECT().AckAction(gomock.Any(), clusterID, action.Id, gomock.Any()).Return(nil)

		_, err := testOps.sut.pollActions(ctx, mockClient, cluster)
		r.NoError(err)

		// Verify no job was created and no status was updated
		actualCluster := &castwarev1alpha1.Cluster{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, actualCluster)
		r.NoError(err)
		r.Empty(actualCluster.Status.UpgradeJobName)
	})

	t.Run("should pause reconciliation when upgrade is in progress", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()
		clusterID := uuid.NewString()

		jobName := "test-upgrade-job"

		cluster := newTestCluster(t, clusterID, false)
		cluster.Status.UpgradeJobName = jobName

		// Create a running job
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      jobName,
				Namespace: cluster.Namespace,
			},
			Status: batchv1.JobStatus{
				Active: 1,
			},
		}

		testOps := newClusterTestOps(t, cluster, job)

		result, err := testOps.sut.checkUpgradeJobStatus(ctx, cluster)
		r.NoError(err)
		r.Equal(time.Second*10, result.RequeueAfter)

		// Verify cluster still has upgradeJobName (not cleared)
		actualCluster := &castwarev1alpha1.Cluster{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, actualCluster)
		r.NoError(err)
		r.Equal(jobName, actualCluster.Status.UpgradeJobName)
	})

	t.Run("should set status to completed when upgrade job succeeds", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()
		clusterID := uuid.NewString()

		cluster := newTestCluster(t, clusterID, false)
		cluster.Status.UpgradeJobName = "test-upgrade-job"

		// Create a succeeded job
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-upgrade-job",
				Namespace: cluster.Namespace,
			},
			Status: batchv1.JobStatus{
				Succeeded: 1,
			},
		}

		testOps := newClusterTestOps(t, cluster, job)

		result, err := testOps.sut.checkUpgradeJobStatus(ctx, cluster)
		r.NoError(err)
		r.Equal(time.Second*5, result.RequeueAfter)

		// Verify cluster status updated
		actualCluster := &castwarev1alpha1.Cluster{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, actualCluster)
		r.NoError(err)
		r.Empty(actualCluster.Status.UpgradeJobName)

		progressingCondition := meta.FindStatusCondition(actualCluster.Status.Conditions, typeProgressingCluster)
		r.NotNil(progressingCondition)
		r.Equal(metav1.ConditionFalse, progressingCondition.Status)
		r.Equal("UpgradeCompleted", progressingCondition.Reason)
	})

	t.Run("should set status to degraded when upgrade job fails", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()
		clusterID := uuid.NewString()

		cluster := newTestCluster(t, clusterID, false)
		cluster.Status.UpgradeJobName = "test-upgrade-job"

		// Create a failed job with pod
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-upgrade-job",
				Namespace: cluster.Namespace,
				Labels: map[string]string{
					"job-name": "test-upgrade-job",
				},
			},
			Spec: batchv1.JobSpec{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"job-name": "test-upgrade-job",
					},
				},
			},
			Status: batchv1.JobStatus{
				Failed: 1,
			},
		}

		// Create failed pod for the job
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-upgrade-job-pod",
				Namespace: cluster.Namespace,
				Labels: map[string]string{
					"job-name": "test-upgrade-job",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodFailed,
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name: "upgrade",
						State: corev1.ContainerState{
							Terminated: &corev1.ContainerStateTerminated{
								ExitCode: 1,
								Reason:   "Error",
								Message:  "Upgrade failed",
							},
						},
					},
				},
			},
		}

		testOps := newClusterTestOps(t, cluster, job, pod)

		result, err := testOps.sut.checkUpgradeJobStatus(ctx, cluster)
		r.Error(err)
		r.Equal(time.Duration(0), result.RequeueAfter)

		// Verify cluster status updated to Degraded
		actualCluster := &castwarev1alpha1.Cluster{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, actualCluster)
		r.NoError(err)
		r.Empty(actualCluster.Status.UpgradeJobName)

		degradedCondition := meta.FindStatusCondition(actualCluster.Status.Conditions, typeDegradedCluster)
		r.NotNil(degradedCondition)
		r.Equal(metav1.ConditionTrue, degradedCondition.Status)
		r.Equal("UpgradeFailed", degradedCondition.Reason)
		r.Contains(degradedCondition.Message, "container exited with code 1")

		progressingCondition := meta.FindStatusCondition(actualCluster.Status.Conditions, typeProgressingCluster)
		r.NotNil(progressingCondition)
		r.Equal(metav1.ConditionFalse, progressingCondition.Status)
		r.Equal("UpgradeFailed", progressingCondition.Reason)
	})

	t.Run("should clear upgrade job name when job not found", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()
		clusterID := uuid.NewString()

		cluster := newTestCluster(t, clusterID, false)
		cluster.Status.UpgradeJobName = "non-existent-job"

		testOps := newClusterTestOps(t, cluster)

		result, err := testOps.sut.checkUpgradeJobStatus(ctx, cluster)
		r.NoError(err)
		r.Equal(time.Second*30, result.RequeueAfter)

		// Verify cluster status cleared
		actualCluster := &castwarev1alpha1.Cluster{}
		err = testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, actualCluster)
		r.NoError(err)
		r.Empty(actualCluster.Status.UpgradeJobName)

		progressingCondition := meta.FindStatusCondition(actualCluster.Status.Conditions, typeProgressingCluster)
		r.NotNil(progressingCondition)
		r.Equal(metav1.ConditionFalse, progressingCondition.Status)
		r.Equal("UpgradeJobNotFound", progressingCondition.Reason)
	})

	t.Run("should not allow duplicate upgrade jobs", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()
		clusterID := uuid.NewString()

		podName := "test-operator-pod"
		r.NoError(os.Setenv("POD_NAME", podName))
		t.Cleanup(func() {
			_ = os.Unsetenv("POD_NAME")
		})

		cluster := newTestCluster(t, clusterID, false)
		cluster.Status.UpgradeJobName = "existing-upgrade-job"
		cluster.Spec.APIKeySecret = "api-key-secret"
		// Set cluster to Available status to avoid Reconcile setting it and requeueing
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:   typeAvailableCluster,
			Status: metav1.ConditionTrue,
			Reason: "ClusterIdAvailable",
		})

		// Create API key secret that Reconcile method will look up
		apiKeySecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "api-key-secret",
				Namespace: cluster.Namespace,
			},
			Data: map[string][]byte{
				"API_KEY": []byte("test-api-key"),
			},
		}

		existingJob := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "existing-upgrade-job",
				Namespace: cluster.Namespace,
			},
			Status: batchv1.JobStatus{
				Active: 1,
			},
		}

		testOps := newClusterTestOps(t, cluster, apiKeySecret, existingJob)

		// Update cluster's LastSecretVersion to match the secret's ResourceVersion
		// This prevents reconcileSecret from trying to validate the API key
		actualSecret := &corev1.Secret{}
		err := testOps.sut.Get(ctx, client.ObjectKey{
			Namespace: cluster.Namespace,
			Name:      "api-key-secret",
		}, actualSecret)
		r.NoError(err)

		actualCluster := &castwarev1alpha1.Cluster{}
		err2 := testOps.sut.Get(ctx, client.ObjectKey{
			Namespace: cluster.Namespace,
			Name:      cluster.Name,
		}, actualCluster)
		r.NoError(err2)
		actualCluster.Status.LastSecretVersion = actualSecret.ResourceVersion
		err2 = testOps.sut.Status().Update(ctx, actualCluster)
		r.NoError(err2)

		// Should not call PollActions because upgrade is already in progress
		// The reconciler should skip pollActions when upgradeJobName is set
		result, err3 := testOps.sut.Reconcile(ctx, reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      cluster.Name,
				Namespace: cluster.Namespace,
			},
		})
		r.NoError(err3)
		r.Equal(time.Second*10, result.RequeueAfter)

		// Verify still only one job exists
		actualCluster2 := &castwarev1alpha1.Cluster{}
		err4 := testOps.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, actualCluster2)
		r.NoError(err4)
		r.Equal("existing-upgrade-job", actualCluster2.Status.UpgradeJobName)
	})
}

func TestGetCurrentOperatorImage(t *testing.T) {
	t.Run("should extract image from current pod", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()

		podName := "test-operator-pod"
		namespace := "test-namespace"
		expectedImage := "castai/castware-operator:v0.0.1"

		r.NoError(os.Setenv("POD_NAME", podName))
		t.Cleanup(func() {
			_ = os.Unsetenv("POD_NAME")
		})

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podName,
				Namespace: namespace,
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "manager",
						Image: expectedImage,
					},
				},
			},
		}

		testOps := newClusterTestOps(t, pod)

		image, err := testOps.sut.getCurrentOperatorImage(ctx, namespace)
		r.NoError(err)
		r.Equal(expectedImage, image)
	})

	t.Run("should return error when POD_NAME not set", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()

		_ = os.Unsetenv("POD_NAME")

		testOps := newClusterTestOps(t)

		_, err := testOps.sut.getCurrentOperatorImage(ctx, "test-namespace")
		r.Error(err)
		r.Contains(err.Error(), "POD_NAME environment variable not set")
	})

	t.Run("should return error when pod not found", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()

		r.NoError(os.Setenv("POD_NAME", "non-existent-pod"))
		t.Cleanup(func() {
			_ = os.Unsetenv("POD_NAME")
		})

		testOps := newClusterTestOps(t)

		_, err := testOps.sut.getCurrentOperatorImage(ctx, "test-namespace")
		r.Error(err)
		r.Contains(err.Error(), "failed to get operator pod")
	})
}

func TestUpgradeJobFailureDetails(t *testing.T) {
	t.Run("should extract failure details from terminated container", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()

		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-job",
				Namespace: "test-namespace",
				Labels: map[string]string{
					"job-name": "test-job",
				},
			},
			Spec: batchv1.JobSpec{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"job-name": "test-job",
					},
				},
			},
		}

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-job-pod",
				Namespace: "test-namespace",
				Labels: map[string]string{
					"job-name": "test-job",
				},
			},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{
					{
						State: corev1.ContainerState{
							Terminated: &corev1.ContainerStateTerminated{
								ExitCode: 127,
								Reason:   "CommandNotFound",
								Message:  "upgrade command not found",
							},
						},
					},
				},
			},
		}

		testOps := newClusterTestOps(t, job, pod)

		message := testOps.sut.getUpgradeJobFailureDetails(ctx, job)
		r.Contains(message, "container exited with code 127")
		r.Contains(message, "CommandNotFound")
	})

	t.Run("should handle pod scheduling failures", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()

		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-job",
				Namespace: "test-namespace",
				Labels: map[string]string{
					"job-name": "test-job",
				},
			},
			Spec: batchv1.JobSpec{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"job-name": "test-job",
					},
				},
			},
		}

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-job-pod",
				Namespace: "test-namespace",
				Labels: map[string]string{
					"job-name": "test-job",
				},
			},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{
						Type:    corev1.PodScheduled,
						Status:  corev1.ConditionFalse,
						Message: "0/3 nodes available: insufficient memory",
					},
				},
			},
		}

		testOps := newClusterTestOps(t, job, pod)

		message := testOps.sut.getUpgradeJobFailureDetails(ctx, job)
		r.Contains(message, "pod scheduling failed")
		r.Contains(message, "insufficient memory")
	})
}
func TestRecordOperatorUpgradeProgressing(t *testing.T) {
	t.Run("should record progressing action result successfully", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)
		ctx := context.Background()
		clusterID := uuid.NewString()

		cluster := newTestCluster(t, clusterID, false)
		cluster.Spec.APIKeySecret = "api-key-secret"

		apiKeySecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "api-key-secret",
				Namespace: cluster.Namespace,
			},
			Data: map[string][]byte{"API_KEY": []byte("test-api-key")},
		}

		testOps := newClusterTestOps(t, cluster, apiKeySecret)

		helmRelease := &release.Release{
			Name: "castware-operator",
			Chart: &chart.Chart{
				Metadata: &chart.Metadata{
					Version: "v1.0.0",
				},
			},
		}

		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   testOps.sut.Config.PodNamespace,
			ReleaseName: testOps.sut.Config.HelmReleaseName,
		}).Return(helmRelease, nil)

		mockClient.EXPECT().RecordActionResult(
			gomock.Any(),
			clusterID,
			&castai.ComponentActionResult{
				Name:           components.ComponentNameOperator,
				Action:         castai.Action_UPGRADE,
				CurrentVersion: "v1.0.0",
				Version:        "v1.0.0",
				Status:         castai.Status_PROGRESSING,
				ImageVersions:  nil,
				ReleaseName:    "castware-operator",
				Message:        "Operator upgrading",
			},
		).Return(nil)

		castAiClient := mockClient
		helmReleaseResult, err := testOps.sut.HelmClient.GetRelease(helm.GetReleaseOptions{
			Namespace:   testOps.sut.Config.PodNamespace,
			ReleaseName: testOps.sut.Config.HelmReleaseName,
		})
		r.NoError(err)

		err = castAiClient.RecordActionResult(ctx, cluster.Spec.Cluster.ClusterID, &castai.ComponentActionResult{
			Name:           components.ComponentNameOperator,
			Action:         castai.Action_UPGRADE,
			CurrentVersion: helmReleaseResult.Chart.Metadata.Version,
			Version:        helmReleaseResult.Chart.Metadata.Version,
			Status:         castai.Status_PROGRESSING,
			ImageVersions:  nil,
			ReleaseName:    helmReleaseResult.Name,
			Message:        "Operator upgrading",
		})
		r.NoError(err)
	})

	t.Run("should handle getCastaiClient error", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		clusterID := uuid.NewString()

		cluster := newTestCluster(t, clusterID, false)

		testOps := newClusterTestOps(t, cluster)

		_, err := testOps.sut.getCastaiClient(ctx, cluster)
		r.Error(err)
	})

	t.Run("should handle helm GetRelease error", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		clusterID := uuid.NewString()

		cluster := newTestCluster(t, clusterID, false)
		cluster.Spec.APIKeySecret = "api-key-secret"

		apiKeySecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "api-key-secret",
				Namespace: cluster.Namespace,
			},
			Data: map[string][]byte{"API_KEY": []byte("test-api-key")},
		}

		testOps := newClusterTestOps(t, cluster, apiKeySecret)

		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   testOps.sut.Config.PodNamespace,
			ReleaseName: testOps.sut.Config.HelmReleaseName,
		}).Return(nil, errors.New("not found"))

		_, err := testOps.sut.HelmClient.GetRelease(helm.GetReleaseOptions{
			Namespace:   testOps.sut.Config.PodNamespace,
			ReleaseName: testOps.sut.Config.HelmReleaseName,
		})
		r.Error(err)
	})

	t.Run("should log error when RecordActionResult fails", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)
		ctx := context.Background()
		clusterID := uuid.NewString()

		cluster := newTestCluster(t, clusterID, false)
		cluster.Spec.APIKeySecret = "api-key-secret"

		apiKeySecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "api-key-secret",
				Namespace: cluster.Namespace,
			},
			Data: map[string][]byte{"API_KEY": []byte("test-api-key")},
		}

		testOps := newClusterTestOps(t, cluster, apiKeySecret)

		helmRelease := &release.Release{
			Name: "castware-operator",
			Chart: &chart.Chart{
				Metadata: &chart.Metadata{
					Version: "v1.0.0",
				},
			},
		}

		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   testOps.sut.Config.PodNamespace,
			ReleaseName: testOps.sut.Config.HelmReleaseName,
		}).Return(helmRelease, nil)

		mockClient.EXPECT().RecordActionResult(
			gomock.Any(),
			clusterID,
			gomock.Any(),
		).Return(errors.New("Unauthorizes"))

		castAiClient := mockClient
		helmReleaseResult, err := testOps.sut.HelmClient.GetRelease(helm.GetReleaseOptions{
			Namespace:   testOps.sut.Config.PodNamespace,
			ReleaseName: testOps.sut.Config.HelmReleaseName,
		})
		r.NoError(err)

		err = castAiClient.RecordActionResult(ctx, cluster.Spec.Cluster.ClusterID, &castai.ComponentActionResult{
			Name:           components.ComponentNameOperator,
			Action:         castai.Action_UPGRADE,
			CurrentVersion: helmReleaseResult.Chart.Metadata.Version,
			Version:        helmReleaseResult.Chart.Metadata.Version,
			Status:         castai.Status_PROGRESSING,
			ImageVersions:  nil,
			ReleaseName:    helmReleaseResult.Name,
			Message:        "Operator upgrading",
		})
		r.Error(err)
	})

	t.Run("should use correct config values for namespace and release name", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mockClient := mock_castai.NewMockCastAIClient(ctrl)
		ctx := context.Background()
		clusterID := uuid.NewString()

		cluster := newTestCluster(t, clusterID, false)
		cluster.Spec.APIKeySecret = "api-key-secret"

		apiKeySecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "api-key-secret",
				Namespace: cluster.Namespace,
			},
			Data: map[string][]byte{"API_KEY": []byte("test-api-key")},
		}

		testOps := newClusterTestOps(t, cluster, apiKeySecret)

		helmRelease := &release.Release{
			Name: testOps.sut.Config.HelmReleaseName,
			Chart: &chart.Chart{
				Metadata: &chart.Metadata{
					Version: "v1.0.0",
				},
			},
		}

		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   testOps.sut.Config.PodNamespace,
			ReleaseName: testOps.sut.Config.HelmReleaseName,
		}).Return(helmRelease, nil)

		mockClient.EXPECT().RecordActionResult(
			gomock.Any(),
			clusterID,
			gomock.Any(),
		).Return(nil)

		castAiClient := mockClient
		helmReleaseResult, err := testOps.sut.HelmClient.GetRelease(helm.GetReleaseOptions{
			Namespace:   testOps.sut.Config.PodNamespace,
			ReleaseName: testOps.sut.Config.HelmReleaseName,
		})
		r.NoError(err)
		r.Equal(testOps.sut.Config.HelmReleaseName, helmReleaseResult.Name)

		err = castAiClient.RecordActionResult(ctx, cluster.Spec.Cluster.ClusterID, &castai.ComponentActionResult{
			Name:           components.ComponentNameOperator,
			Action:         castai.Action_UPGRADE,
			CurrentVersion: helmReleaseResult.Chart.Metadata.Version,
			Version:        helmReleaseResult.Chart.Metadata.Version,
			Status:         castai.Status_PROGRESSING,
			ImageVersions:  nil,
			ReleaseName:    helmReleaseResult.Name,
			Message:        "Operator upgrading",
		})
		r.NoError(err)
	})
}
