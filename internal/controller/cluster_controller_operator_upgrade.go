package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/samber/lo"
	"github.com/sirupsen/logrus"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/castai"
	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/helm"
	"github.com/castai/castware-operator/internal/params"
)

// This file contains the operator self-upgrade path: creating the upgrade
// Kubernetes Job, polling its status, and extracting failure details from the
// job pod and its logs.

// handleOperatorUpgrade handles the operator self-upgrade by creating a Kubernetes Job
// that runs the upgrade command with the specified version.
func (r *ClusterReconciler) handleOperatorUpgrade(ctx context.Context, cluster *castwarev1alpha1.Cluster, action *castai.ActionUpgrade) error {
	log := r.Log.WithField("action", "operator-upgrade")

	// Get current operator deployment to extract image
	// TODO: use a more reliable way to get the operator image, e.g. store in helm chart or configmap
	// or currentOperatorVersion field to Cluster CR status
	operatorImage, err := r.getCurrentOperatorImage(ctx, cluster.Namespace)
	if err != nil {
		log.WithError(err).Error("Failed to get current operator image")
		return err
	}

	log.Infof("operator upgrade action: version %s, image %s", action.Version, operatorImage)

	jobName, err := r.createUpgradeJob(ctx, cluster, operatorImage, action.Version)
	if err != nil {
		log.WithError(err).Error("Failed to create upgrade job")
		return err
	}

	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:    typeProgressingCluster,
		Status:  metav1.ConditionTrue,
		Reason:  progressingReasonOperatorUpgrading,
		Message: fmt.Sprintf("Operator upgrade to version %s in progress", action.Version),
	})
	cluster.Status.UpgradeJobName = jobName

	if err := r.Status().Update(ctx, cluster); err != nil {
		log.WithError(err).Error("Failed to update cluster status")
		return err
	}

	log.Infof("Operator upgrade job created: %s", jobName)

	castAiClient, _, err := r.getCastaiClient(ctx, cluster)
	if err != nil {
		log.WithError(err).Error("Failed to get castaiClient")
		return nil
	}

	helmRelease, err := r.HelmClient.GetRelease(helm.GetReleaseOptions{
		Namespace:   r.Config.PodNamespace,
		ReleaseName: r.Config.HelmReleaseName,
	})
	if err != nil {
		log.WithError(err).Error("Failed to get helmRelease")
		return nil
	}

	// Extract operator parameters
	componentParams := params.ExtractComponentParams(
		ctx,
		log,
		components.ComponentNameOperator,
		helmRelease,
		r.Client,
		r.Config.PodNamespace,
	)

	err = castAiClient.RecordActionResult(ctx, cluster.Spec.Cluster.ClusterID, &castai.ComponentActionResult{
		Name:            components.ComponentNameOperator,
		Action:          castai.Action_UPGRADE,
		CurrentVersion:  helmRelease.Chart.Metadata.Version,
		Version:         helmRelease.Chart.Metadata.Version,
		Status:          castai.Status_PROGRESSING,
		ImageVersions:   nil,
		ReleaseName:     helmRelease.Name,
		Message:         "Operator upgrading",
		ComponentParams: componentParams,
	})
	if err != nil {
		log.WithError(err).Error("Failed to record action result")
	}

	return nil
}

// getCurrentOperatorImage retrieves the current operator image from the current operator pod.
func (r *ClusterReconciler) getCurrentOperatorImage(ctx context.Context, namespace string) (string, error) {
	podName := os.Getenv("POD_NAME")
	if podName == "" {
		return "", fmt.Errorf("POD_NAME environment variable not set")
	}

	pod := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      podName,
	}, pod)
	if err != nil {
		return "", fmt.Errorf("failed to get operator pod: %w", err)
	}

	if len(pod.Spec.Containers) == 0 {
		return "", fmt.Errorf("no containers found in operator pod")
	}

	return pod.Spec.Containers[0].Image, nil
}

// createUpgradeJob creates a Kubernetes Job that runs the operator upgrade command.
func (r *ClusterReconciler) createUpgradeJob(ctx context.Context, cluster *castwarev1alpha1.Cluster, operatorImage, targetVersion string) (string, error) {
	log := r.Log

	jobName := fmt.Sprintf("%s-%d", upgradeJobNamePrefix, time.Now().Unix())

	clusterCrName := cluster.Name
	clusterCrNamespace := cluster.Namespace
	if clusterCrNamespace == "" {
		clusterCrNamespace = os.Getenv("POD_NAMESPACE")
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "castware-operator",
				"app.kubernetes.io/component": "upgrade-job",
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: operatorServiceAccountName,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: lo.ToPtr(true),
						RunAsUser:    lo.ToPtr(int64(1000)),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "helm-cache",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{
									SizeLimit: lo.ToPtr(resource.MustParse("512Mi")),
								},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:    "upgrade",
							Image:   operatorImage,
							Command: []string{"/manager"},
							Args:    []string{"upgrade", "--version", targetVersion, "--cluster-cr-name", clusterCrName, "--cluster-cr-namespace", clusterCrNamespace},
							Env: []corev1.EnvVar{
								{
									Name: "POD_NAME",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "metadata.name",
										},
									},
								},
								{
									Name: "POD_NAMESPACE",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "metadata.namespace",
										},
									},
								},
								{
									Name:  "HELM_CACHE_HOME",
									Value: "/.cache/helm",
								},
							},
							Ports: []corev1.ContainerPort{
								{
									Name:          "health-probe",
									ContainerPort: 8081,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromInt32(8081),
									},
								},
								InitialDelaySeconds: 15,
								PeriodSeconds:       20,
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/readyz",
										Port: intstr.FromInt32(8081),
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       10,
							},
							SecurityContext: &corev1.SecurityContext{
								RunAsNonRoot:             lo.ToPtr(true),
								RunAsUser:                lo.ToPtr(int64(1000)),
								AllowPrivilegeEscalation: lo.ToPtr(false),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
								ReadOnlyRootFilesystem: lo.ToPtr(true),
								SeccompProfile: &corev1.SeccompProfile{
									Type: corev1.SeccompProfileTypeRuntimeDefault,
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "helm-cache",
									MountPath: "/.cache/helm",
								},
							},
						},
					},
				},
			},
			BackoffLimit: lo.ToPtr(int32(0)),
		},
	}

	if err := r.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			log.Warn("Upgrade job already exists")
			return jobName, nil
		}
		return "", fmt.Errorf("failed to create job: %w", err)
	}

	return jobName, nil
}

// checkUpgradeJobStatus checks the status of the operator upgrade job and updates cluster status accordingly.
func (r *ClusterReconciler) checkUpgradeJobStatus(ctx context.Context, cluster *castwarev1alpha1.Cluster) (ctrl.Result, error) {
	log := r.Log.WithField("upgradeJob", cluster.Status.UpgradeJobName)

	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{
		Namespace: cluster.Namespace,
		Name:      cluster.Status.UpgradeJobName,
	}, job)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Warn("Upgrade job not found, clearing status")
			// Job was deleted, clear the status
			cluster.Status.UpgradeJobName = ""
			meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
				Type:    typeProgressingCluster,
				Status:  metav1.ConditionFalse,
				Reason:  "UpgradeJobNotFound",
				Message: "Upgrade job not found",
			})
			if err := r.Status().Update(ctx, cluster); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: time.Second * 30}, nil
		}
		return ctrl.Result{}, err
	}

	if job.Status.Succeeded > 0 {
		log.Info("Upgrade job succeeded")

		// Record the successful upgrade to Mothership with component parameters
		castAiClient, _, err := r.getCastaiClient(ctx, cluster)
		if err != nil {
			log.WithError(err).Error("Failed to get castaiClient to record upgrade completion")
		} else {
			// Get the current operator Helm release to determine the version
			helmRelease, err := r.HelmClient.GetRelease(helm.GetReleaseOptions{
				Namespace:   r.Config.PodNamespace,
				ReleaseName: r.Config.HelmReleaseName,
			})
			if err != nil {
				log.WithError(err).Error("Failed to get operator helm release to record upgrade completion")
			} else {
				// Extract operator parameters
				componentParams := params.ExtractComponentParams(
					ctx,
					log,
					components.ComponentNameOperator,
					helmRelease,
					r.Client,
					r.Config.PodNamespace,
				)

				// Record the upgrade completion with OK status and component parameters
				err = castAiClient.RecordActionResult(ctx, cluster.Spec.Cluster.ClusterID, &castai.ComponentActionResult{
					Name:            components.ComponentNameOperator,
					Action:          castai.Action_UPGRADE,
					CurrentVersion:  helmRelease.Chart.Metadata.Version,
					Version:         helmRelease.Chart.Metadata.Version,
					Status:          castai.Status_OK,
					ReleaseName:     helmRelease.Name,
					Message:         "Operator upgrade completed successfully",
					ComponentParams: componentParams,
				})
				if err != nil {
					log.WithError(err).Error("Failed to record operator upgrade completion")
				} else {
					log.Info("Recorded operator upgrade completion to Mothership")
					// Track the reported revision to prevent duplicate reporting
					cluster.Status.LastReportedHelmRevision = helmRelease.Version
				}
			}
		}

		// Clean up the job
		if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
			log.WithError(err).Warn("Failed to delete upgrade job")
		}

		// Update cluster status
		cluster.Status.UpgradeJobName = ""
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:    typeProgressingCluster,
			Status:  metav1.ConditionFalse,
			Reason:  "UpgradeCompleted",
			Message: "Operator upgrade completed successfully",
		})
		if err := r.Status().Update(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: time.Second * 5}, nil
	}

	if job.Status.Failed > 0 {
		// Get failure details from job pod logs
		failureMessage := r.getUpgradeJobFailureDetails(ctx, job)
		log.WithField("failureMessage", failureMessage).Error("Upgrade job failed with details")

		// Clean up the job
		if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
			log.WithError(err).Warn("Failed to delete upgrade job")
		}

		cluster.Status.UpgradeJobName = ""
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:    typeDegradedCluster,
			Status:  metav1.ConditionTrue,
			Reason:  "UpgradeFailed",
			Message: fmt.Sprintf("Operator upgrade failed: %s", failureMessage),
		})
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:    typeProgressingCluster,
			Status:  metav1.ConditionFalse,
			Reason:  "UpgradeFailed",
			Message: fmt.Sprintf("Operator upgrade failed: %s", failureMessage),
		})
		if err := r.Status().Update(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, errors.New(failureMessage)
	}

	log.Info("Operator upgrade job still running")
	return ctrl.Result{RequeueAfter: time.Second * 10}, nil
}

// getUpgradeJobFailureDetails retrieves failure details from the upgrade job's pod.
// It checks pod status conditions and container logs to determine why the job failed.
func (r *ClusterReconciler) getUpgradeJobFailureDetails(ctx context.Context, job *batchv1.Job) string {
	log := r.Log.WithField("job", job.Name)

	// List pods for this job
	podList := &corev1.PodList{}
	labelSelector := labels.SelectorFromSet(job.Spec.Selector.MatchLabels)
	err := r.List(ctx, podList, &client.ListOptions{
		Namespace:     job.Namespace,
		LabelSelector: labelSelector,
	})
	if err != nil {
		log.WithError(err).Warn("Failed to list pods for upgrade job")
		return "failed to retrieve pod details"
	}

	if len(podList.Items) == 0 {
		return "no pods found for upgrade job"
	}

	// Check the most recent pod
	pod := podList.Items[len(podList.Items)-1]

	// Check pod status conditions for failure reasons
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
			return fmt.Sprintf("pod scheduling failed: %s", condition.Message)
		}
	}

	// Check container statuses
	if len(pod.Status.ContainerStatuses) > 0 {
		containerStatus := pod.Status.ContainerStatuses[0]

		// Check if container failed
		if containerStatus.State.Terminated != nil {
			terminated := containerStatus.State.Terminated
			if terminated.ExitCode != 0 {
				failureMsg := fmt.Sprintf("container exited with code %d: %s", terminated.ExitCode, terminated.Reason)

				// Try to get logs for more details
				if logs := r.getUpgradeJobPodLogs(ctx, pod.Namespace, pod.Name, "upgrade"); logs != "" {
					// Limit log length to avoid overwhelming the status message
					if len(logs) > 500 {
						logs = logs[:500] + "... (truncated)"
					}
					failureMsg = fmt.Sprintf("%s. Logs: %s", failureMsg, logs)
				}
				return failureMsg
			}
		}

		// Check if container is waiting with error
		if containerStatus.State.Waiting != nil {
			waiting := containerStatus.State.Waiting
			return fmt.Sprintf("container waiting: %s - %s", waiting.Reason, waiting.Message)
		}
	}

	// Check pod phase
	if pod.Status.Phase == corev1.PodFailed {
		return fmt.Sprintf("pod failed with reason: %s, message: %s", pod.Status.Reason, pod.Status.Message)
	}

	return "unknown failure reason"
}

// getUpgradeJobPodLogs retrieves logs from a specific pod/container.
// Returns empty string if logs cannot be retrieved.
func (r *ClusterReconciler) getUpgradeJobPodLogs(ctx context.Context, namespace, podName, containerName string) string {
	logReq := r.Clientset.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: containerName,
		TailLines: lo.ToPtr(int64(50)), // Get last 50 lines
	})

	logBytes, err := r.readLogBytes(ctx, logReq)
	if err != nil {
		r.Log.WithError(err).WithFields(logrus.Fields{
			"pod":       podName,
			"container": containerName,
		}).Warn("Failed to read logs from upgrade job pod")
		return ""
	}

	return string(logBytes)
}
