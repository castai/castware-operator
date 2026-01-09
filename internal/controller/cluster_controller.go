package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	agentcastai "castai-agent/pkg/castai"
	"castai-agent/pkg/services/providers/aks"
	"castai-agent/pkg/services/providers/eks"
	"castai-agent/pkg/services/providers/gke"
	providers "castai-agent/pkg/services/providers/types"

	"github.com/castai/castware-operator/internal/rolebindings"
	"github.com/samber/lo"
	"github.com/sirupsen/logrus"
	"helm.sh/helm/v3/pkg/storage/driver"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/castai"
	"github.com/castai/castware-operator/internal/castai/auth"
	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/config"
	"github.com/castai/castware-operator/internal/helm"
	"github.com/castai/castware-operator/internal/utils"
)

var (
	errUnknownAction     = errors.New("unknown action")
	errComponentNotFount = errors.New("component not found")
	clusterIDRegexp      = regexp.MustCompile(`cluster_id=([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)
)

// Definitions to manage status conditions
const (
	// typeAvailableCluster represents the status when cluster resource is reconciled and works as expected.
	typeAvailableCluster = "Available"
	// typeDegradedCastware represents the status used when something went wrong with cluster reconciliation.
	typeDegradedCluster = "Degraded"
	// typeProgressingCluster represents the status when cluster is progressing (e.g., operator upgrade).
	typeProgressingCluster = "Progressing"
	// nameLabelKey represents the label key used to identify "name" label in a Kubernetes resource.
	nameLabelKey = "app.kubernetes.io/name"

	// Operator upgrade related constants
	progressingReasonOperatorUpgrading = "OperatorUpgrading"
	upgradeJobNamePrefix               = "castware-operator-upgrade"
	operatorServiceAccountName         = "castware-operator-controller-manager"
)

type existingComponentVersion struct {
	Version         string
	ComponentConfig map[string]any
	MigrationMode   string
}

// ClusterReconciler reconciles a Cluster object
type ClusterReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Log         logrus.FieldLogger
	Config      *config.Config
	HelmClient  helm.Client
	ChartLoader helm.ChartLoader
	Clientset   kubernetes.Interface
	RestConfig  *rest.Config
}

// +kubebuilder:rbac:groups=castware.cast.ai,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=castware.cast.ai,resources=clusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=castware.cast.ai,resources=clusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingwebhookconfigurations,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="apiextensions.k8s.io",resources=customresourcedefinitions,resourceNames=components.castware.cast.ai;clusters.castware.cast.ai,verbs=get;list;delete;create;patch;update

func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log

	cluster := &castwarev1alpha1.Cluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			// If the custom resource is not found then it usually means that it was deleted or not created
			// In this way, we will stop the reconciliation
			log.Info("cluster resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.WithError(err).Error("Failed to get cluster")
		return ctrl.Result{RequeueAfter: time.Minute * 5}, nil
	}

	// Check if operator upgrade is in progress
	if cluster.Status.UpgradeJobName != "" {
		log.Info("Operator upgrade in progress, checking job status")
		return r.checkUpgradeJobStatus(ctx, cluster)
	}

	log.Debug("getCastaiClient")
	castAiClient, err := r.getCastaiClient(ctx, cluster)
	if err != nil {
		log.WithError(err).Error("Failed to get castaiClient")
		return ctrl.Result{}, err
	}

	log.Debug("ensureClusterRegistration")
	clusterID, err := r.ensureClusterRegistration(ctx, cluster, castAiClient)
	if err != nil {
		return ctrl.Result{RequeueAfter: time.Minute * 1}, err
	}

	log.Debug("ensureClusterIDInSpec")
	if result, err := r.ensureClusterIDInSpec(ctx, cluster, clusterID); err != nil || result.RequeueAfter > 0 {
		return result, err
	}

	log.Debug("Completing initial setup")
	if result, err := r.completeInitialSetup(ctx, cluster, castAiClient); err != nil || result.RequeueAfter > 0 {
		return result, err
	}
	log.Debug("Initial setup completed")

	reconcile, err := r.reconcileSecret(ctx, cluster)
	if err != nil {
		return ctrl.Result{RequeueAfter: time.Minute * 5}, err
	} else if reconcile {
		return ctrl.Result{RequeueAfter: time.Second * 30}, nil
	}
	log.Debug("Secret reconciled")

	reconcile, err = r.syncTerraformComponents(ctx, castAiClient, cluster)
	if err != nil {
		log.WithError(err).Error("Failed to sync terraform components")
		// Don't block on terraform sync errors, continue to scan and poll actions
	}
	if reconcile {
		return ctrl.Result{RequeueAfter: time.Second * 30}, nil
	}
	log.Debug("Terraform Components synced")

	reconcile, err = r.scanExistingComponents(ctx, castAiClient, cluster)
	// If an error occurred while scanning existing components, we just poll actions for a minute and then retry.
	// This is to avoid that the controller gets stuck on component scanning and stops executing actions.
	if err != nil {
		log.WithError(err).Error("Failed to scan existing components")
	}
	if reconcile {
		return ctrl.Result{RequeueAfter: time.Second * 30}, nil
	}
	log.Debug("scan existing components done")

	return r.pollActions(ctx, castAiClient, cluster)
}

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

	err = castAiClient.RecordActionResult(ctx, clusterMetadata.ClusterID, &castai.ComponentActionResult{
		Name:           components.ComponentNameOperator,
		Action:         castai.Action_INSTALL,
		CurrentVersion: operatorVersion,
		Version:        operatorVersion,
		Status:         castai.Status_OK,
		ImageVersions:  nil,
		ReleaseName:    releaseName,
		Message:        "Operator installed",
	})
	if err != nil {
		log.WithError(err).Error("Failed to record action result")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
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

// syncTerraformComponents handles Component CRs that are created by Terraform with migration mode
func (r *ClusterReconciler) syncTerraformComponents(ctx context.Context, castaiClient castai.CastAIClient, cluster *castwarev1alpha1.Cluster) (bool, error) {
	// Only process if terraform flag is set
	if !cluster.Spec.Terraform {
		return false, nil
	}

	log := r.Log.WithField("action", "sync-terraform-components")

	// TODO: in components package create this var and use it from there after cluster-controller is checked and it also works
	// https://castai.atlassian.net/browse/WIRE-1904
	supportedComponents := []string{
		components.ComponentNameAgent,
		components.ComponentNameSpotHandler,
	}
	reconcileNeeded := false
	for _, componentName := range supportedComponents {
		component := &castwarev1alpha1.Component{}
		err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: componentName}, component)
		if err != nil {
			if apierrors.IsNotFound(err) {
				// CR doesn't exist yet, continue checking other components
				log.Debugf("Component CR %s not found, may be created by Terraform later", componentName)
				continue
			}
			log.WithError(err).Errorf("Failed to get component %s", componentName)
			return reconcileNeeded, err
		}

		// Component CR exists, check if it needs terraform migration handling
		if component.IsInitiliazedByTerraform() {
			log.Infof("Processing terraform migration for component %s", componentName)
			needsReconcile, err := r.handleComponentTerraformMigration(ctx, castaiClient, cluster, component)
			if err != nil {
				return reconcileNeeded, err
			}
			if needsReconcile {
				reconcileNeeded = true
			}
		}
	}

	return reconcileNeeded, nil
}

// handleComponentTerraformMigration processes a Component CR with terraform migration mode
func (r *ClusterReconciler) handleComponentTerraformMigration(ctx context.Context, castaiClient castai.CastAIClient, cluster *castwarev1alpha1.Cluster, component *castwarev1alpha1.Component) (bool, error) {
	log := r.Log.WithFields(logrus.Fields{
		"component": component.Name,
		"migration": component.Spec.Migration,
		"version":   component.Spec.Version,
	})

	updatedComponent := component.DeepCopy()

	// Case 1: Version is already set in the CR
	if component.Spec.Version != "" {
		log.Info("Version already set in terraform migration, clearing migration flag")
		updatedComponent.Spec.Migration = ""
	} else {
		// Case 2: No version set, need to detect based on cluster migration mode
		switch cluster.Spec.MigrationMode {
		case castwarev1alpha1.ClusterMigrationModeAutoupgrade:
			// Leave version empty - component controller or mutating webhook will handle it and set latest version
			updatedComponent.Spec.Migration = ""
		default:
			// Write mode (or empty/default) - must detect version from existing installation
			log.Info("Write mode: detecting version from existing installation")
			existingVersion, err := r.detectComponentVersion(ctx, log, castaiClient, cluster, component.Spec.Component)
			if err != nil {
				log.WithError(err).Warn("Failed to detect component version")
			}

			if existingVersion != nil && existingVersion.Version != "" {
				log.Infof("Detected version: %s", existingVersion.Version)
				updatedComponent.Spec.Version = existingVersion.Version

				// Also sync values if they exist and were not set by TF
				if existingVersion.ComponentConfig != nil && component.Spec.Values == nil {
					values, err := json.Marshal(existingVersion.ComponentConfig)
					if err != nil {
						log.WithError(err).Warn("Failed to marshal component config")
					} else {
						updatedComponent.Spec.Values = &v1.JSON{Raw: values}
					}
				}

				updatedComponent.Spec.Migration = ""
			} else {
				log.Info("No existing installation found for terraform migration in write mode")
				// In write mode without existing installation, leave version empty for latest
				updatedComponent.Spec.Migration = ""
			}
		}
	}

	err := r.Patch(ctx, updatedComponent, client.MergeFrom(component))
	if err != nil {
		log.WithError(err).Error("Failed to patch component")
		return false, err
	}
	log.Info("Successfully processed terraform migration")
	return true, nil
}

func (r *ClusterReconciler) scanExistingComponents(ctx context.Context, castaiClient castai.CastAIClient, cluster *castwarev1alpha1.Cluster) (bool, error) {
	if cluster.Spec.Terraform {
		// Component CR will be handled separetly and we don't want to create a new one if we are in TF.
		return false, nil
	}

	// TODO: in components package create an array and iterate it here after we test with cluster-controller as well https://castai.atlassian.net/browse/WIRE-1905
	// Scan for agent
	reconcileAgent, err := r.scanExistingComponent(ctx, castaiClient, cluster, components.ComponentNameAgent)
	if err != nil {
		return false, err
	}

	// Scan for spot-handler
	reconcileSpotHandler, err := r.scanExistingComponent(ctx, castaiClient, cluster, components.ComponentNameSpotHandler)
	if err != nil {
		return false, err
	}

	if reconcileAgent || reconcileSpotHandler {
		return true, nil
	}

	extendedPermsExist, err := rolebindings.CheckExtendedPermissionsExist(ctx, r.Client, cluster.Namespace)
	if err != nil {
		return false, fmt.Errorf("failed to check extended permissions: %w", err)
	}

	if extendedPermsExist {
		// Scan for cluster controller
		reconcileClusterController, err := r.scanExistingComponent(ctx, castaiClient, cluster, components.ComponentNameClusterController)
		if err != nil {
			return false, err
		}

		return reconcileClusterController, nil
	}

	return false, nil
}

// scanExistingComponent Checks if helm release or deployment exist for a given component, and if they do but
// there is no corresponding component CR, it creates the component CR with migration parameter configured accordingly.
func (r *ClusterReconciler) scanExistingComponent(ctx context.Context, castaiClient castai.CastAIClient, cluster *castwarev1alpha1.Cluster, componentName string) (reconcile bool, err error) {
	log := r.Log

	component := &castwarev1alpha1.Component{}
	err = r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: componentName}, component)
	if err == nil {
		// Component CR found - nothing to migrate.
		return false, nil
	}
	if !apierrors.IsNotFound(err) {
		log.WithError(err).Error("Failed to get component")
		return false, err
	}

	compVersion, err := r.detectComponentVersion(ctx, log, castaiClient, cluster, componentName)
	if err != nil {
		return false, err
	}

	if compVersion == nil {
		return false, nil
	}

	log.Info("Version found for existing component, creating new component resource")
	values, err := json.Marshal(compVersion.ComponentConfig)
	if err != nil {
		return false, err
	}
	component = newComponent(componentName, compVersion.Version, cluster)
	component.Spec.Values = &v1.JSON{Raw: values}
	component.Spec.Migration = compVersion.MigrationMode
	err = r.Create(ctx, component)
	if err != nil {
		return false, err
	}
	log.Info("component resource created")
	return true, nil
}

func (r *ClusterReconciler) detectComponentVersion(ctx context.Context, log logrus.FieldLogger, castaiClient castai.CastAIClient, cluster *castwarev1alpha1.Cluster, componentName string) (*existingComponentVersion, error) {
	agentRelease, err := r.HelmClient.GetRelease(helm.GetReleaseOptions{
		Namespace:   cluster.Namespace,
		ReleaseName: componentName,
	})

	if err == nil {
		return &existingComponentVersion{
			Version:         agentRelease.Chart.Metadata.Version,
			ComponentConfig: agentRelease.Config,
			MigrationMode:   castwarev1alpha1.ComponentMigrationHelm,
		}, nil
	}

	if !errors.Is(err, driver.ErrReleaseNotFound) {
		// If the error is not ErrReleaseNotFound, something is wrong with helm or component configuration
		log.WithError(err).Error("Failed to get helm release")
		return nil, err
	}

	// TODO: here use components.IsSupported once we test for cluster-controller as well https://castai.atlassian.net/browse/WIRE-1905
	if componentName != components.ComponentNameAgent && componentName != components.ComponentNameSpotHandler {
		log.Debugf("Component %s not found, and YAML migration is not supported for this component", componentName)
		return nil, nil
	}

	component, err := castaiClient.GetComponentByName(ctx, componentName)
	if err != nil {
		return nil, err
	}

	switch componentName {
	case components.ComponentNameAgent:
		var deploymentList appsv1.DeploymentList
		err = r.List(ctx, &deploymentList, &client.ListOptions{
			Namespace:     cluster.Namespace,
			LabelSelector: labels.SelectorFromSet(labels.Set{nameLabelKey: component.HelmChart}),
		})
		if err != nil {
			return nil, err
		}
		if len(deploymentList.Items) > 0 {
			versionLabel := deploymentList.Items[0].Labels["helm.sh/chart"]
			version := strings.TrimPrefix(versionLabel, fmt.Sprintf("%s-", component.HelmChart))
			if version == "" {
				log.Warnf("Failed to get version from deployment label, upgrading to latest version")
			}
			valueOverrides := map[string]interface{}{"replicaCount": deploymentList.Items[0].Spec.Replicas}

			return &existingComponentVersion{
				Version:         version,
				ComponentConfig: valueOverrides,
				MigrationMode:   castwarev1alpha1.ComponentMigrationYaml,
			}, nil
		}
	case components.ComponentNameSpotHandler:
		var daemonSetList appsv1.DaemonSetList
		err = r.List(ctx, &daemonSetList, &client.ListOptions{
			Namespace:     cluster.Namespace,
			LabelSelector: labels.SelectorFromSet(labels.Set{nameLabelKey: component.HelmChart}),
		})
		if err != nil {
			return nil, err
		}
		if len(daemonSetList.Items) > 0 {
			versionLabel := daemonSetList.Items[0].Labels["helm.sh/chart"]
			version := strings.TrimPrefix(versionLabel, fmt.Sprintf("%s-", component.HelmChart))
			if version == "" {
				log.Warnf("Failed to get version from daemonset label, upgrading to latest version")
			}

			valueOverrides := map[string]interface{}{}

			return &existingComponentVersion{
				Version:         version,
				ComponentConfig: valueOverrides,
				MigrationMode:   castwarev1alpha1.ComponentMigrationYaml,
			}, nil
		}
	}

	return nil, nil
}

func (r *ClusterReconciler) reconcileSecret(ctx context.Context, cluster *castwarev1alpha1.Cluster) (bool, error) {
	log := r.Log

	// Can't reconcile api key if the cluster is not there.
	if cluster.Spec.Cluster == nil {
		return false, nil
	}

	// Check if api key secret changed.
	secret := &corev1.Secret{}
	secKey := types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Spec.APIKeySecret}
	if err := r.Get(ctx, secKey, secret); err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}

	// If api key changed validate the new one.
	if secret.ResourceVersion != cluster.Status.LastSecretVersion {
		castAiClient, err := r.getCastaiClient(ctx, cluster)
		if err != nil {
			log.WithError(err).Error("Failed to get api client")
			return false, err
		}
		if _, err := castAiClient.GetCluster(ctx, cluster.Spec.Cluster.ClusterID); err != nil {
			log.WithError(err).WithField("clusterId", cluster.Spec.Cluster.ClusterID).Error("Failed to get cluster")

			// Set cluster to unavailable if GetCluster fails.
			meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
				Type:    typeAvailableCluster,
				Status:  metav1.ConditionFalse,
				Reason:  "GetClusterFailed",
				Message: fmt.Sprintf("Failed to get cluster by ID: %v", err),
			})
			err = r.Status().Update(ctx, cluster)
			if err != nil {
				log.WithError(err).Error("Failed to set available status to false")
				return false, err
			}

			return false, err
		}
		log.Info("Api key updated")

		cluster.Status.LastSecretVersion = secret.ResourceVersion
		if err := r.Status().Update(ctx, cluster); err != nil {
			return true, err
		}
	}
	return false, nil
}

func (r *ClusterReconciler) pollActions(ctx context.Context, castAiClient castai.CastAIClient, cluster *castwarev1alpha1.Cluster) (ctrl.Result, error) {
	log := r.Log

	log.Debug("Polling actions")

	actions, err := castAiClient.PollActions(ctx, cluster.Spec.Cluster.ClusterID)
	if err != nil {
		log.WithError(err).Error("Failed to poll actions")
		return ctrl.Result{RequeueAfter: time.Minute * 5}, nil
	}
	for _, action := range actions.Actions {
		var actionErr error
		switch a := action.Action().(type) {
		case *castai.ActionInstall:
			log.Infof("install action: %v", a.Component)
			actionErr = r.handleInstall(ctx, cluster, a)
		case *castai.ActionUpgrade:
			log.Infof("upgrade action: %v", a.Component)
			actionErr = r.handleUpgrade(ctx, cluster, a)
		case *castai.ActionUninstall:
			log.Infof("uninstall action: %v", a.Component)
			actionErr = r.handleUninstall(ctx, cluster, a)
		case *castai.ActionRollback:
			log.Infof("rollback action: %v", a.Component)
			actionErr = r.handleRollback(ctx, cluster, a)
		default:
			actionErr = errUnknownAction
			log.Warnf("unknown action: %v", action)
		}
		if actionErr != nil {
			log.WithError(actionErr).Errorf("Failed to handle action: %v", action)
		}

		err := castAiClient.AckAction(ctx, cluster.Spec.Cluster.ClusterID, action.Id, actionErr)
		if err != nil {
			// If action ack fails, we can't do anything about it, just process the next one.
			log.WithError(err).Error("Failed to ack action")
		}
	}

	return ctrl.Result{RequeueAfter: time.Second * 30}, nil
}

func (r *ClusterReconciler) handleInstall(ctx context.Context, cluster *castwarev1alpha1.Cluster, action *castai.ActionInstall) error {
	log := r.Log
	namespacedName := types.NamespacedName{Namespace: cluster.Namespace, Name: action.Component}

	component := &castwarev1alpha1.Component{}
	err := r.Get(ctx, namespacedName, component)
	if err == nil {
		if action.Upsert {
			upgradeAction := &castai.ActionUpgrade{
				Version:              action.Version,
				Component:            action.Component,
				ValuesOverrides:      action.ValuesOverrides,
				ResetThenReuseValues: action.ResetThenReuseValues,
			}
			return r.handleUpgrade(ctx, cluster, upgradeAction)
		}
		return errors.New("component already exists")
	} else if !apierrors.IsNotFound(err) {
		log.WithError(err).Error("Failed to get component")
		return err
	}

	component = &castwarev1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: cluster.Namespace,
			Name:      action.Component,
		},
		Spec: castwarev1alpha1.ComponentSpec{
			Component: action.Component,
			Cluster:   cluster.Name,
			Enabled:   true,
			Version:   action.Version,
		},
	}

	if action.ValuesOverrides != nil {
		values, err := utils.UnflattenMap(action.ValuesOverrides)
		if err != nil {
			return err
		}
		b, err := json.Marshal(values)
		if err != nil {
			return err
		}
		component.Spec.Values = &v1.JSON{Raw: b}
	}

	log.Debugf("creating new component: %v", component)
	return r.Create(ctx, component)
}

func (r *ClusterReconciler) handleUpgrade(ctx context.Context, cluster *castwarev1alpha1.Cluster, action *castai.ActionUpgrade) error {
	log := r.Log

	if action.Component == components.ComponentNameOperator {
		log.Infof("operator upgrade action: version %s", action.Version)
		return r.handleOperatorUpgrade(ctx, cluster, action)
	}

	namespacedName := types.NamespacedName{Namespace: cluster.Namespace, Name: action.Component}

	component := &castwarev1alpha1.Component{}
	err := r.Get(ctx, namespacedName, component)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.WithError(err).Error("Failed to get component")
			return errComponentNotFount
		}
		log.WithError(err).Error("Failed to get component")
		return fmt.Errorf("failed to get component: %w", err)
	}

	if component.Spec.Version == action.Version {
		return errors.New("component already up to date")
	}

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := r.Get(ctx, types.NamespacedName{
			Name:      component.Name,
			Namespace: component.Namespace,
		}, component); err != nil {
			return err
		}

		updatedComponent := component.DeepCopy()
		updatedComponent.Spec.Version = action.Version

		if action.ValuesOverrides != nil {
			values, err := utils.UnflattenMap(action.ValuesOverrides)
			if err != nil {
				return err
			}
			if component.Spec.Values != nil {
				currentValues := map[string]interface{}{}
				if err = json.Unmarshal(component.Spec.Values.Raw, &currentValues); err != nil {
					return err
				}
				err = utils.MergeMaps(currentValues, values)
				if err != nil {
					return err
				}
				// MergeMaps merges the second map into the first one.
				values = currentValues
			}

			b, err := json.Marshal(values)
			if err != nil {
				return err
			}
			updatedComponent.Spec.Values = &v1.JSON{Raw: b}
		}

		return r.Patch(ctx, updatedComponent, client.MergeFrom(component))
	})
}

func (r *ClusterReconciler) handleRollback(ctx context.Context, cluster *castwarev1alpha1.Cluster, action *castai.ActionRollback) error {
	log := r.Log
	namespacedName := types.NamespacedName{Namespace: cluster.Namespace, Name: action.Component}

	component := &castwarev1alpha1.Component{}
	err := r.Get(ctx, namespacedName, component)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.WithError(err).Error("Failed to get component")
			return errComponentNotFount
		}
		log.WithError(err).Error("Failed to get component")
		return fmt.Errorf("failed to get component: %w", err)
	}

	helmRelease, err := r.HelmClient.GetRelease(helm.GetReleaseOptions{
		Namespace:   component.Namespace,
		ReleaseName: component.Spec.Component,
	})
	if err != nil {
		log.WithError(err).Error("Failed to get helm release")
		return err
	}
	// Helm release version start from 1 for the first install, if version is lower than 2
	// the component has never been upgrade, hence nothing to rollback
	if helmRelease.Version < 2 {
		return ErrNothingToRollback
	}

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var latestComponent castwarev1alpha1.Component
		if err := r.Get(ctx, types.NamespacedName{
			Name:      component.Name,
			Namespace: component.Namespace,
		}, &latestComponent); err != nil {
			return err
		}

		latestComponent.Status.Rollback = true

		return r.Status().Update(ctx, &latestComponent)
	})
}

func (r *ClusterReconciler) handleUninstall(ctx context.Context, cluster *castwarev1alpha1.Cluster, action *castai.ActionUninstall) error {
	log := r.Log
	namespacedName := types.NamespacedName{Namespace: cluster.Namespace, Name: action.Component}

	component := &castwarev1alpha1.Component{}
	err := r.Get(ctx, namespacedName, component)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.WithError(err).Error("Failed to get component")
			return errComponentNotFount
		}
		log.WithError(err).Error("Failed to get component")
		return fmt.Errorf("failed to get component: %w", err)
	}

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := r.Get(ctx, types.NamespacedName{
			Name:      component.Name,
			Namespace: component.Namespace,
		}, component); err != nil {
			return err
		}

		return r.Delete(ctx, component)
	})
}

func (r *ClusterReconciler) getCastaiClient(ctx context.Context, cluster *castwarev1alpha1.Cluster) (castai.CastAIClient, error) {
	auth := auth.NewAuth(cluster.Namespace, cluster.Name)
	if err := auth.LoadApiKey(ctx, r.Client); err != nil {
		return nil, err
	}
	rest := castai.NewRestyClient(r.Config, cluster.Spec.API.APIURL, auth)

	client := castai.NewClient(nil, r.Config, rest)

	return client, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {

	updatePredicate := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			log := mgr.GetLogger()
			switch newObj := e.ObjectNew.(type) {
			case *castwarev1alpha1.Cluster:
				oldObj, ok := e.ObjectOld.(*castwarev1alpha1.Cluster)
				if !ok {
					log.Info("not updating", "name", e.ObjectOld.GetName())
					return false
				}
				// Trigger reconcile when cluster CR changes.
				return newObj.Generation != oldObj.Generation
			case *corev1.Secret:
				oldObj, ok := e.ObjectOld.(*corev1.Secret)
				if !ok {
					log.Info("not updating", "name", e.ObjectOld.GetName())
					return false
				}
				oldKey, ok := oldObj.Data["API_KEY"]
				if !ok {
					return false
				}
				newKey, ok := newObj.Data["API_KEY"]
				if !ok {
					return false
				}
				// Trigger reconcile when secret changes
				return string(oldKey) != string(newKey)
			}
			return false
		},
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&castwarev1alpha1.Cluster{}).
		WithEventFilter(updatePredicate).
		Named("cluster").
		Complete(r)
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

func newComponent(componentName, version string, cluster *castwarev1alpha1.Cluster) *castwarev1alpha1.Component {
	component := &castwarev1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: cluster.Namespace,
			Name:      componentName,
		},
		Spec: castwarev1alpha1.ComponentSpec{
			Component: componentName,
			Cluster:   cluster.Name,
			Enabled:   true,
		},
	}
	component.Spec.Readonly = cluster.Spec.MigrationMode == castwarev1alpha1.ClusterMigrationModeRead
	// If the cluster is in autoupgrade mode, we don't specify the version in the component CR so that
	// the controller will upgrade the agent to the latest version.
	if cluster.Spec.MigrationMode != castwarev1alpha1.ClusterMigrationModeAutoupgrade {
		component.Spec.Version = version
	}
	return component
}

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

	castAiClient, err := r.getCastaiClient(ctx, cluster)
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

	err = castAiClient.RecordActionResult(ctx, cluster.Spec.Cluster.ClusterID, &castai.ComponentActionResult{
		Name:           components.ComponentNameOperator,
		Action:         castai.Action_UPGRADE,
		CurrentVersion: helmRelease.Chart.Metadata.Version,
		Version:        helmRelease.Chart.Metadata.Version,
		Status:         castai.Status_PROGRESSING,
		ImageVersions:  nil,
		ReleaseName:    helmRelease.Name,
		Message:        "Operator upgrading",
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
								AllowPrivilegeEscalation: lo.ToPtr(false),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
								ReadOnlyRootFilesystem: lo.ToPtr(true),
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

// extractClusterIDFromAgentLogs extracts the cluster_id from the logs of the agent container
// in the castai-agent deployment. Returns empty string and no error if the deployment doesn't exist.
func (r *ClusterReconciler) extractClusterIDFromAgentLogs(ctx context.Context, namespace string) (string, error) {
	log := r.Log.WithField("namespace", namespace)

	deployment := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: components.ComponentNameAgent}, deployment)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Debug("castai-agent deployment not found")
			return "", nil
		}
		return "", fmt.Errorf("failed to get castai-agent deployment: %w", err)
	}

	podList := &corev1.PodList{}
	labelSelector := labels.SelectorFromSet(deployment.Spec.Selector.MatchLabels)
	err = r.List(ctx, podList, &client.ListOptions{
		Namespace:     namespace,
		LabelSelector: labelSelector,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list pods for castai-agent: %w", err)
	}

	if len(podList.Items) == 0 {
		log.Debug("no pods found for castai-agent deployment")
		return "", nil
	}

	// Try to extract cluster_id from the first running pod
	for _, pod := range podList.Items {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}

		// Find the agent container
		var agentContainer *corev1.Container
		for i := range pod.Spec.Containers {
			if pod.Spec.Containers[i].Name == "agent" {
				agentContainer = &pod.Spec.Containers[i]
				break
			}
		}

		if agentContainer == nil {
			log.WithField("pod", pod.Name).Debug("agent container not found in pod")
			continue
		}

		// Get logs from the agent container
		logReq := r.Clientset.CoreV1().Pods(namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			Container: "agent",
			TailLines: lo.ToPtr(int64(1000)),
			// Retrieve only logs for the last minute, the agent sends snapshots every 15 seconds
			// so it should be a safe interval.
			SinceSeconds: lo.ToPtr(int64(60)),
		})

		logBytes, err := r.readLogBytes(ctx, logReq)
		if err != nil {
			log.WithError(err).WithField("pod", pod.Name).Warn("failed to read logs from agent container")
			continue
		}

		// Search for cluster_id=(uuid) pattern in logs
		clusterID := extractClusterIDFromLogs(string(logBytes))
		if clusterID != "" {
			log.WithField("clusterId", clusterID).Info("extracted cluster ID from agent logs")
			return clusterID, nil
		}
	}

	log.Debug("cluster_id not found in agent logs")
	return "", nil
}

func (r *ClusterReconciler) readLogBytes(ctx context.Context, logReq *rest.Request) ([]byte, error) {
	logStream, err := logReq.Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get log stream from container: %w", err)
	}
	defer func() {
		if err := logStream.Close(); err != nil {
			r.Log.WithError(err).Warn("failed to close logs stream")
		}
	}()
	logBytes, err := io.ReadAll(logStream)
	if err != nil {
		return nil, fmt.Errorf("failed to read logs from container: %w", err)
	}
	return logBytes, nil
}

// extractClusterIDFromLogs parses logs and extracts cluster_id UUID
func extractClusterIDFromLogs(logs string) string {
	// Match cluster_id=<uuid> pattern
	matches := clusterIDRegexp.FindStringSubmatch(logs)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}
