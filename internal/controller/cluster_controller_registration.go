package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	agentcastai "castai-agent/pkg/castai"
	"castai-agent/pkg/services/providers/aks"
	"castai-agent/pkg/services/providers/eks"
	"castai-agent/pkg/services/providers/gke"
	providers "castai-agent/pkg/services/providers/types"

	"github.com/sirupsen/logrus"
	"helm.sh/helm/v3/pkg/storage/driver"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/castai"
	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/helm"
	"github.com/castai/castware-operator/internal/params"
)

// This file contains the cluster registration / initial-setup lifecycle:
// registering the cluster with Mothership, persisting the cluster ID, completing
// initial setup (reporting the operator install), and detecting/reporting
// operator helm revision changes.

func isClusterIDMissing(clusterMetadata *castwarev1alpha1.ClusterMetadataSpec) bool {
	return clusterMetadata == nil || clusterMetadata.ClusterID == ""
}

func (r *ClusterReconciler) ensureClusterRegistration(ctx context.Context, cluster *castwarev1alpha1.Cluster, castAiClient castai.CastAIClient) (clusterID string, err error) {
	log := r.Log
	clusterMetadata := cluster.Spec.Cluster

	needsRegistration := cluster.Status.LastRegistrationVersion == ""

	if isClusterIDMissing(clusterMetadata) {
		clusterID, err = r.extractClusterIDFromAgentLogs(ctx, cluster.Namespace)
		if err != nil {
			log.WithError(err).Warn("Failed to extract cluster id from agent logs, registering cluster")
			needsRegistration = true
		}

		if clusterID != "" {
			log.Infof("Cluster already registered by the agent, cluster id: %v", clusterID)
		}
	}

	if !needsRegistration {
		return clusterID, nil
	}

	if clusterID != "" {
		log.Infof("Re-registering cluster, cluster id: %v", clusterID)
	}

	p, err := GetProvider(ctx, r.Log, cluster)
	if err != nil {
		log.WithError(err).Error("Failed to get provider")
		return "", err
	}

	installMethod := agentcastai.CastwareInstallMethodOperator
	result, err := p.RegisterClusterWithInstallMethod(ctx, castAiClient, &installMethod)
	if err != nil {
		log.WithError(err).Error("Failed to register cluster")
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:    typeDegradedCluster,
			Status:  metav1.ConditionUnknown,
			Reason:  "ClusterRegistrationFailed",
			Message: fmt.Sprintf("Failed to register cluster: %v", err),
		})
		if statusErr := r.Status().Update(ctx, cluster); statusErr != nil {
			log.WithError(statusErr).Errorf("Failed to set '%s' status", typeDegradedCluster)
		}
		return "", err
	}

	if clusterID == "" {
		clusterID = result.ClusterID
	}

	operatorVersion := strings.TrimPrefix(castai.GetVersion().Version, "v")
	cluster.Status.LastRegistrationVersion = operatorVersion
	if err := r.Status().Update(ctx, cluster); err != nil {
		log.WithError(err).Error("Failed to update LastRegistrationVersion in status")
		return "", err
	}

	return clusterID, nil
}

func (r *ClusterReconciler) ensureClusterIDInSpec(ctx context.Context, cluster *castwarev1alpha1.Cluster, clusterID string) (ctrl.Result, error) {
	log := r.Log

	if !isClusterIDMissing(cluster.Spec.Cluster) {
		return ctrl.Result{}, nil
	}

	updatedCluster := cluster.DeepCopy()
	updatedCluster.Spec.Cluster = &castwarev1alpha1.ClusterMetadataSpec{ClusterID: clusterID}
	err := r.Patch(ctx, updatedCluster, client.MergeFrom(cluster))
	if err != nil {
		log.WithError(err).Error("Failed to set cluster id")
		return ctrl.Result{RequeueAfter: time.Minute * 1}, err
	}
	return ctrl.Result{RequeueAfter: time.Second * 30}, nil
}

func (r *ClusterReconciler) completeInitialSetup(ctx context.Context, cluster *castwarev1alpha1.Cluster, castAiClient castai.CastAIClient) (ctrl.Result, error) {
	log := r.Log
	clusterMetadata := cluster.Spec.Cluster

	if clusterMetadata.ClusterID == "" || meta.IsStatusConditionTrue(cluster.Status.Conditions, typeAvailableCluster) {
		return ctrl.Result{}, nil
	}

	// Try to get Helm release. If not found (e.g., in E2E tests where operator is deployed via manifests),
	// fall back to using the version from the binary.
	var operatorVersion string
	var releaseName string

	helmRelease, err := r.HelmClient.GetRelease(helm.GetReleaseOptions{
		Namespace:   r.Config.PodNamespace,
		ReleaseName: r.Config.HelmReleaseName,
	})
	if err != nil {
		if errors.Is(err, driver.ErrReleaseNotFound) {
			operatorVersion = strings.TrimPrefix(castai.GetVersion().Version, "v")
			releaseName = components.ComponentNameOperator
			log.WithField("version", operatorVersion).Warn("Helm release not found, using version from binary")
		} else {
			log.WithError(err).Error("Failed to get Helm release")
			return ctrl.Result{}, err
		}
	} else {
		operatorVersion = helmRelease.Chart.Metadata.Version
		releaseName = helmRelease.Name
	}

	// Extract operator parameters
	componentParams := params.ExtractComponentParams(
		ctx,
		log,
		components.ComponentNameOperator,
		nil, // helmRelease may be nil if installed via manifests
		r.Client,
		r.Config.PodNamespace,
	)

	err = castAiClient.RecordActionResult(ctx, clusterMetadata.ClusterID, &castai.ComponentActionResult{
		Name:            components.ComponentNameOperator,
		Action:          castai.Action_INSTALL,
		CurrentVersion:  operatorVersion,
		Version:         operatorVersion,
		Status:          castai.Status_OK,
		ImageVersions:   nil,
		ReleaseName:     releaseName,
		Message:         "Operator installed",
		ComponentParams: componentParams,
	})
	if err != nil {
		log.WithError(err).Error("Failed to record action result")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	// Initialize LastReportedHelmRevision if we have a helm release
	// This prevents duplicate reporting on the next reconcile loop
	if helmRelease != nil {
		cluster.Status.LastReportedHelmRevision = helmRelease.Version
	}

	log.Info("Cluster reconciled")
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{Type: typeAvailableCluster, Status: metav1.ConditionTrue, Reason: "ClusterIdAvailable", Message: "Cluster reconciled"})
	err = r.Status().Update(ctx, cluster)
	if err != nil {
		log.WithError(err).Error("Failed to set available status")
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: time.Second * 5}, nil
}

// detectAndReportOperatorHelmRevisionChange detects if the operator helm release revision has changed
// (e.g., from a direct helm upgrade without version change) and reports updated parameters to Mothership.
// This only triggers if there's NO active upgrade job (to avoid double-reporting with Mothership-initiated upgrades).
func (r *ClusterReconciler) detectAndReportOperatorHelmRevisionChange(ctx context.Context, cluster *castwarev1alpha1.Cluster, castAiClient castai.CastAIClient) error {
	log := r.Log.WithField("action", "detect-operator-revision-change")

	// Skip if there's an active upgrade job (Mothership-initiated upgrade in progress)
	if cluster.Status.UpgradeJobName != "" {
		log.Debug("Skipping operator revision detection: upgrade job in progress")
		return nil
	}

	// Skip if cluster is not yet fully set up
	if cluster.Spec.Cluster.ClusterID == "" || !meta.IsStatusConditionTrue(cluster.Status.Conditions, typeAvailableCluster) {
		return nil
	}

	// Get current operator helm release
	helmRelease, err := r.HelmClient.GetRelease(helm.GetReleaseOptions{
		Namespace:   r.Config.PodNamespace,
		ReleaseName: r.Config.HelmReleaseName,
	})
	if err != nil {
		// If Helm release not found (e.g., manifests-based install), skip revision detection
		if errors.Is(err, driver.ErrReleaseNotFound) {
			log.Debug("Helm release not found, skipping operator revision detection")
			return nil
		}
		return fmt.Errorf("failed to get Helm release: %w", err)
	}

	currentRevision := helmRelease.Version
	lastReportedRevision := cluster.Status.LastReportedHelmRevision

	// Check if revision has changed
	if currentRevision == lastReportedRevision {
		return nil
	}

	// Revision changed! This could be from:
	// 1. Version upgrade (already handled by upgrade job)
	// 2. Parameter-only change (e.g., helm upgrade --set extendedPermissions=true without version change)
	// 3. Direct helm upgrade outside operator control
	log.WithFields(logrus.Fields{
		"currentRevision":      currentRevision,
		"lastReportedRevision": lastReportedRevision,
		"operatorVersion":      helmRelease.Chart.Metadata.Version,
	}).Info("Detected operator helm revision change, reporting to Mothership")

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
		Status:          castai.Status_OK,
		ReleaseName:     helmRelease.Name,
		Message:         "Operator helm revision change detected",
		ComponentParams: componentParams,
	})
	if err != nil {
		return fmt.Errorf("failed to record operator revision change: %w", err)
	}

	// Update the tracking field
	cluster.Status.LastReportedHelmRevision = currentRevision
	if err := r.Status().Update(ctx, cluster); err != nil {
		return fmt.Errorf("failed to update LastReportedHelmRevision: %w", err)
	}

	log.WithField("params", componentParams).Info("Successfully reported operator helm revision change to Mothership")
	return nil
}

func GetProvider(ctx context.Context, log logrus.FieldLogger, cluster *castwarev1alpha1.Cluster) (providers.Provider, error) {
	switch cluster.Spec.Provider {
	case eks.Name:
		return eks.New(ctx, log.WithField("provider", gke.Name), false)
	case gke.Name:
		return gke.New(log.WithField("provider", gke.Name))
	case aks.Name:
		return aks.New(log.WithField("provider", aks.Name))
	default:
		return nil, fmt.Errorf("unsupported provider: %s", cluster.Spec.Provider)
	}
}
