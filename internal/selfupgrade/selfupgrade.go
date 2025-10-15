package selfupgrade

import (
	"context"
	"errors"
	"fmt"
	"time"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/castai"
	"github.com/castai/castware-operator/internal/castai/auth"
	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/config"
	"github.com/castai/castware-operator/internal/helm"
	"github.com/sirupsen/logrus"
	"helm.sh/helm/v3/pkg/release"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	errUninstalled   = errors.New("helm release is uninstalled")
	errStatusUnknown = errors.New("helm release status is unknown")
)

func NewService(client client.Client, helmClient helm.Client, config *config.Config, log logrus.FieldLogger, clusterCrName string, clusterCrNamespace string) *Service {
	return &Service{Client: client, helmClient: helmClient, config: config, log: log, clusterCrName: clusterCrName, clusterCrNamespace: clusterCrNamespace}
}

type Service struct {
	client.Client
	helmClient         helm.Client
	config             *config.Config
	log                logrus.FieldLogger
	clusterCrName      string
	clusterCrNamespace string
}

func (s *Service) Run(ctx context.Context, targetVersion string) error {
	log := s.log.WithField("cluster", s.clusterCrName)
	cluster := &castwarev1alpha1.Cluster{}
	err := s.Get(ctx, client.ObjectKey{Name: s.clusterCrName, Namespace: s.clusterCrNamespace}, cluster)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("cluster resource not found")
			return err
		}
		// Error reading the object - requeue the request.
		log.WithError(err).Error("Failed to get cluster")
		return err
	}

	auth := auth.NewAuth(cluster.Namespace, cluster.Name)
	if err := auth.LoadApiKey(ctx, s.Client); err != nil {
		return fmt.Errorf("failed to load api key: %w", err)
	}
	castAiClient := castai.NewClient(s.log, castai.NewRestyClient(s.config, cluster.Spec.API.APIURL, auth))
	component, err := castAiClient.GetComponentByName(ctx, components.ComponentNameOperator)
	if err != nil {
		return fmt.Errorf("failed to get component: %w", err)
	}
	if targetVersion == "" {
		log.Infof("target version not specified, defaulting to latest version: %s", component.LatestVersion)
		targetVersion = component.LatestVersion
	}
	log = s.log.WithField("target_version", targetVersion)
	getReleaseOptions := helm.GetReleaseOptions{
		Namespace:   cluster.Namespace,
		ReleaseName: component.Name,
	}
	helmRelease, err := s.helmClient.GetRelease(getReleaseOptions)
	if err != nil {
		return fmt.Errorf("failed to get helm release: %w", err)
	}
	upgradeOptions := helm.UpgradeOptions{
		ChartSource: &helm.ChartSource{
			RepoURL: cluster.Spec.HelmRepoURL,
			Name:    helmRelease.Chart.Metadata.Name,
			Version: targetVersion,
		},
		Release:              helmRelease,
		ResetThenReuseValues: true,
		DryRun:               true,
		Recreate:             false,
		// If the tag was overridden, auto upgrade won't work unless we reset it to helm default.
		ValuesOverrides: map[string]interface{}{"image": map[string]interface{}{"tag": ""}},
	}
	_, err = s.helmClient.Upgrade(ctx, upgradeOptions)
	if err != nil {
		log.WithError(err).Error("Upgrade dry run failed")
		return fmt.Errorf("upgrade dry run failed: %w", err)
	}

	// Dry run was successful, proceed with upgrade
	upgradeOptions.DryRun = false
	previousVersion := helmRelease.Chart.Metadata.Version
	helmRelease, err = s.helmClient.Upgrade(ctx, upgradeOptions)
	if err != nil {
		// If upgrade fails we log the error check for status, if the upgrade didn't start there's
		// no reason to rollback, but if the helm release is in failed state we rollback.
		log.WithError(err).Error("Upgrade run failed")
	} else {
		log.Infof("Upgrade started, release name: %s -> %s", previousVersion, helmRelease.Chart.Metadata.Version)
	}
	err = s.checkReleaseStatus(ctx, log, getReleaseOptions)
	if err != nil {
		// If context was canceled or timed out, return the error and do not attempt to rollback
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			log.Warn("Upgrade canceled for timeout, won't rollback")
			return err
		} else if errors.Is(err, errUninstalled) {
			log.Warn("Upgrade failed, helm release is uninstalled, won't rollback")
			return err
		}

		log.WithError(err).Warn("Upgrade failed, trying to rollback")
		rollbackErr := s.helmClient.Rollback(helm.RollbackOptions{
			Namespace:   getReleaseOptions.Namespace,
			ReleaseName: getReleaseOptions.ReleaseName,
		})
		if rollbackErr != nil {
			// TODO: p3 - error for RecordActionResult
			// err = errors.Join(err, rollbackErr)
			log.WithError(rollbackErr).Error("Rollback failed")
		}
		return err
	}

	// TODO: p3 RecordActionResult if action id was specified
	// TODO: p3 set cluster CR status to upgraded (remove upgrade job id)
	return nil
}
func (s *Service) checkReleaseStatus(ctx context.Context, log *logrus.Entry, getReleaseOptions helm.GetReleaseOptions) error {
	t := time.NewTicker(time.Second * 5)
	defer t.Stop()
	var (
		helmRelease *release.Release
		err         error
	)

	for {
		select {
		case <-t.C:
			err = wait.ExponentialBackoff(retry.DefaultBackoff, func() (bool, error) {
				helmRelease, err = s.helmClient.GetRelease(getReleaseOptions)
				if err != nil {
					log.WithError(err).Error("Failed to get helm release")
					return false, err
				}
				return true, nil
			})
			if err != nil {
				return err
			}

			switch helmRelease.Info.Status {
			case release.StatusDeployed:
				log.Info("Helm release deployed, verifying pods are ready")
				if err := s.waitForPodsReady(ctx, log, getReleaseOptions); err != nil {
					return fmt.Errorf("upgrade deployed but pods failed to start: %w", err)
				}
				log.Info("Upgrade successful")
				// TODO: p4 - check pod readiness
				return nil
			case release.StatusFailed:
				return fmt.Errorf("helm is in failed status: %s", helmRelease.Info.Description)
			case release.StatusUninstalled, release.StatusUninstalling:
				log.Warnf("Upgrade failed, helm release is uninstalled (%s)", helmRelease.Info.Description)
				return errUninstalled
			case release.StatusSuperseded:
				// Nothing to do, just wait for the ticker and get the latest status
			case release.StatusPendingUpgrade, release.StatusPendingInstall, release.StatusPendingRollback:
				log.Info("Upgrade still in progress, waiting for it to complete")
			case release.StatusUnknown:
				// Status is unknown, it shouldn't happen, but if it does,
				// the only thing we can do is trying to rollback.
				log.Warn("Helm release status is unknown")
				return errStatusUnknown
			}
		case <-ctx.Done():
			log.Error("Upgrade canceled for timeout")
			return ctx.Err()
		}
	}
}

// waitForPodsReady waits for all pods associated with the Helm release to be ready.
// This ensures that the upgrade is truly successful and not just deployed with failing pods.
func (s *Service) waitForPodsReady(ctx context.Context, log *logrus.Entry, getReleaseOptions helm.GetReleaseOptions) error {
	timeout := 5 * time.Minute
	checkInterval := 5 * time.Second

	// Create a context with timeout for pod readiness check
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	// Helm uses app.kubernetes.io/instance label to identify release resources
	labelSelector := client.MatchingLabels{
		"app.kubernetes.io/instance": getReleaseOptions.ReleaseName,
	}

	for {
		select {
		case <-ticker.C:
			podList := &corev1.PodList{}
			if err := s.List(checkCtx, podList, client.InNamespace(getReleaseOptions.Namespace), labelSelector); err != nil {
				log.WithError(err).Warn("Failed to list pods for release")
				continue
			}

			if len(podList.Items) == 0 {
				log.Debug("No pods found for release yet, waiting...")
				continue
			}

			allReady, failedPods := checkPodsReadiness(podList)

			if len(failedPods) > 0 {
				// Log details about failed pods
				for _, pod := range failedPods {
					log.WithFields(logrus.Fields{
						"pod":    pod.Name,
						"phase":  pod.Status.Phase,
						"reason": pod.Status.Reason,
					}).Warn("Pod in failed state")
				}
				return fmt.Errorf("pods failed to start: %d pod(s) in failed state", len(failedPods))
			}

			if allReady {
				log.Infof("All %d pod(s) are ready", len(podList.Items))
				return nil
			}

			log.Debugf("Waiting for pods to become ready (%d total)", len(podList.Items))

		case <-checkCtx.Done():
			if errors.Is(checkCtx.Err(), context.DeadlineExceeded) {
				// Get current pod status for error message
				podList := &corev1.PodList{}
				if err := s.List(ctx, podList, client.InNamespace(getReleaseOptions.Namespace), labelSelector); err == nil {
					for _, pod := range podList.Items {
						log.WithFields(logrus.Fields{
							"pod":   pod.Name,
							"phase": pod.Status.Phase,
							"ready": isPodReady(&pod),
						}).Warn("Pod status at timeout")
					}
				}
				return fmt.Errorf("timeout waiting for pods to become ready after %v", timeout)
			}
			return checkCtx.Err()
		}
	}
}

// checkPodsReadiness checks if all pods are ready and returns failed pods
func checkPodsReadiness(podList *corev1.PodList) (allReady bool, failedPods []corev1.Pod) {
	allReady = true
	failedPods = []corev1.Pod{}

	for _, pod := range podList.Items {
		// Check for permanent failure states
		if pod.Status.Phase == corev1.PodFailed {
			failedPods = append(failedPods, pod)
			allReady = false
			continue
		}

		// Check for image pull errors or crash loops
		for _, containerStatus := range pod.Status.ContainerStatuses {
			if containerStatus.State.Waiting != nil {
				reason := containerStatus.State.Waiting.Reason
				// These are permanent failures that won't recover
				if reason == "ImagePullBackOff" || reason == "ErrImagePull" || reason == "CrashLoopBackOff" {
					failedPods = append(failedPods, pod)
					allReady = false
					break
				}
			}
		}

		// Check if pod is ready
		if !isPodReady(&pod) {
			allReady = false
		}
	}

	return allReady, failedPods
}

// isPodReady checks if a pod has the Ready condition set to True
func isPodReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}
