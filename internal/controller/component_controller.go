package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/castai"
	"github.com/castai/castware-operator/internal/castai/auth"
	"github.com/castai/castware-operator/internal/config"
	"github.com/castai/castware-operator/internal/helm"
)

// Definitions to manage status conditions
const (
	componentFinalizer = "castware.cast.ai/cleanup-helm"

	// typeAvailableComponent represents the status when component resource is reconciled, installed and works as expected.
	typeAvailableComponent = "Available"

	// typeProgressingComponent represent the status when a component is installing
	typeProgressingComponent = "Progressing"

	progressingReasonInstalling = "Installing"
	progressingReasonUpgrading  = "Upgrading"
	progressingReasonMigrating  = "Migrating"

	reasonInstalled       = "Installed"
	reasonInstallFailed   = "InstallFailed"
	reasonRollbackFailed  = "RollbackFailed"
	reasonRollbackStarted = "RollbackStarted"
	reasonUpgradeFailed   = "UpgradeFailed"
	reasonUpgradeStarted  = "UpgradeStarted"
	reasonMigrationFailed = "MigrationFailed"
)

var ErrNothingToRollback = errors.New("nothing to rollback")

// +kubebuilder:rbac:groups=castware.cast.ai,resources=components,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=castware.cast.ai,resources=components/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=castware.cast.ai,resources=components/finalizers,verbs=update
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings;clusterroles;clusterrolebindings,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods;nodes;services;events;configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets;replicasets,verbs=get;list;watch

// +kubebuilder:rbac:groups="",resources=limitranges;namespaces;persistentvolumeclaims;persistentvolumes;replicationcontrollers;resourcequotas,verbs=get;list;watch
// +kubebuilder:rbac:groups=argoproj.io,resources=rollouts,verbs=get;list;watch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch
// +kubebuilder:rbac:groups=autoscaling.cast.ai,resources=recommendations,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=cronjobs;jobs,verbs=get;list;watch
// +kubebuilder:rbac:groups=datadoghq.com,resources=extendeddaemonsetreplicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups=karpenter.k8s.aws,resources=awsnodetemplates;ec2nodeclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=karpenter.sh,resources=machines;nodeclaims;nodepools;provisioners,verbs=get;list;watch
// +kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses;networkpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch
// +kubebuilder:rbac:groups=runbooks.cast.ai,resources=recommendationsyncs,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=csinodes;storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=create;patch;get;list;watch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=create;patch;get;list;watch
// +kubebuilder:rbac:groups=pod-mutations.cast.ai,resources=podmutations,verbs=create;update;get;list;watch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=delete,resourceNames=castai-agent

// ComponentReconciler reconciles a Component object
type ComponentReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Log        logrus.FieldLogger
	HelmClient helm.Client
	Recorder   record.EventRecorder
	Config     *config.Config
}

// nolint:gocyclo
func (r *ComponentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, retErr error) {
	log := r.Log.WithField("component", req.NamespacedName.String())
	log.Debug("Reconciling Component")

	component := &castwarev1alpha1.Component{}
	err := r.Get(ctx, req.NamespacedName, component)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// If the custom resource is not found then it usually means that it was deleted or not created
			// In this way, we will stop the reconciliation
			log.Info("component resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.WithError(err).Error("Failed to get component")
		return ctrl.Result{RequeueAfter: time.Minute * 5}, nil
	}
	log = log.WithField("cluster", component.Spec.Cluster)

	if component.DeletionTimestamp != nil && !component.DeletionTimestamp.IsZero() {
		return r.deleteComponent(ctx, log.WithField("action", "delete"), component)
	}

	if !controllerutil.ContainsFinalizer(component, componentFinalizer) {
		controllerutil.AddFinalizer(component, componentFinalizer)
		if err := r.Update(ctx, component); err != nil {
			return ctrl.Result{}, err
		}
	}

	progressingCondition := meta.FindStatusCondition(component.Status.Conditions, typeProgressingComponent)
	if progressingCondition == nil && component.Spec.Migration == castwarev1alpha1.ComponentMigrationHelm {
		// Component was migrated from an existing helm chart, we set progressing condition to true
		// and in the next reconcile loop we check the helm chart status and ser CR status accordingly.
		log.Info("Migrating component from an existing helm chart")
		meta.SetStatusCondition(&component.Status.Conditions, metav1.Condition{
			Type:    typeProgressingComponent,
			Status:  metav1.ConditionTrue,
			Reason:  progressingReasonMigrating,
			Message: fmt.Sprintf("Migrating component from %s", component.Spec.Migration),
		})
		if err := r.updateStatus(ctx, component); err != nil {
			log.WithError(err).Error("Failed to update status")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, err
	}

	if progressingCondition == nil && component.Spec.Migration == castwarev1alpha1.ComponentMigrationYaml {
		// yaml migration is supported only for the agent (validation is in webhooks).
		// running "helm install" takes over the existing resources that become managed by helm
		// and tied to the component custom resource.
		log.Infof("Migrating component from yaml, version: %s", component.Spec.Version)

		// Before migrating the resources we try a dry run to check if the migration is possible.
		isDryRun := true
		_, err = r.installComponent(ctx, log.WithField("action", "migrate"), component, isDryRun)
		if err != nil {
			log.WithError(err).Error("Failed to dry run migration from yaml")
			meta.SetStatusCondition(&component.Status.Conditions, metav1.Condition{
				Type:    typeAvailableComponent,
				Status:  metav1.ConditionFalse,
				Reason:  reasonMigrationFailed,
				Message: fmt.Sprintf("Failed migration: %v", err),
			})

			err = r.updateStatus(ctx, component)
			if err != nil {
				log.WithError(err).Errorf("Failed to set '%s' status", typeProgressingComponent)
			}

			return ctrl.Result{RequeueAfter: time.Minute * 30}, err
		}

		isDryRun = false
		return r.installComponent(ctx, log.WithField("action", "migrate"), component, isDryRun)
	}

	// If installation/upgrade is in progress we wait for completion or timeout.
	if progressingCondition != nil && progressingCondition.Status == metav1.ConditionTrue {
		log.WithField("action", progressingCondition.Reason).Info("Helm install in progress")

		if time.Now().After(progressingCondition.LastTransitionTime.Add(10 * time.Minute)) {
			// Helm install got stuck, try to rollback or mark component as failed.

			// defer as we want this action to be recorded after rollback action has been recorded
			defer r.recordActionResult(ctx, log, component, actionFromProgressingReason(progressingCondition.Reason), errors.New("helm install timeout exceeded"))

			// If migration times out something was already wrong with the helm chart before starting,
			// we set the custom resource as readonly to allow the user to fix the chart without
			// interference from the operator.
			if progressingCondition.Reason == progressingReasonMigrating {
				log.Warn("Migration timeout exceeded, the component will be set as readonly")
				updatedComponent := component.DeepCopy()
				updatedComponent.Spec.Readonly = true
				err = r.Client.Patch(ctx, updatedComponent, client.MergeFrom(component))
				// Patch failure is a recoverable error.
				if err != nil {
					log.WithError(err).Error("Failed to patch component")
					return ctrl.Result{RequeueAfter: time.Minute}, nil
				}
				return ctrl.Result{RequeueAfter: time.Minute}, nil
			}
			log.Warn("Helm install timeout exceeded")
			log := log.WithField("action", "rollback")

			result, err := r.rollback(ctx, log, component)
			if err != nil {
				// TODO: (WIRE-1341) delete component if it's failed and there is nothing to rollback
				if errors.Is(err, ErrNothingToRollback) {
					// Nothing to rollback, marking the component as unavailable and progressing as false
					progressingCondition = &metav1.Condition{
						Type:    typeProgressingComponent,
						Status:  metav1.ConditionFalse,
						Reason:  progressingReasonInstalling,
						Message: fmt.Sprintf("Rollback failed: %v", err),
					}
					meta.SetStatusCondition(&component.Status.Conditions, *progressingCondition)
					meta.SetStatusCondition(&component.Status.Conditions, metav1.Condition{
						Type:    typeAvailableComponent,
						Status:  metav1.ConditionFalse,
						Reason:  progressingCondition.Reason,
						Message: progressingCondition.Message,
					})
					if err := r.updateStatus(ctx, component); err != nil {
						log.WithError(err).Error("Failed to update status")
						return ctrl.Result{}, err
					}
					return ctrl.Result{}, err
				}
				log.WithError(err).Error("Failed to rollback")
				return ctrl.Result{}, err
			}

			return result, nil
		}

		return r.checkHelmProgress(ctx, log, component)
	}

	// This should not happen as we set version in mutating webhook, just keeping it here as precaution.
	if component.Spec.Version == "" {
		log.Error("Version not set for component")
		return ctrl.Result{}, nil
	}

	// If component is not enabled in the crd and not installed we have nothing to do,
	// if the component is installed we can patch the helm chart to set replicas to zero.
	if !component.Spec.Enabled {
		if !meta.IsStatusConditionTrue(component.Status.Conditions, typeAvailableComponent) {
			log.Debug("Component not installed and not enabled, nothing to do")
			return ctrl.Result{RequeueAfter: time.Minute * 5}, nil
		}

		// TODO: (after mvp) if component is already installed scale it to 0
		log.Info("Component is disabled, not installing it")
	}

	// If CurrentVersion is empty the component has to be installed for the first time.
	if component.Status.CurrentVersion == "" && !meta.IsStatusConditionTrue(component.Status.Conditions, typeProgressingComponent) && component.Spec.Migration == "" {
		log.Infof("Component is not installed, installing version %s", component.Spec.Version)
		return r.installComponent(ctx, log.WithField("action", "install"), component, false)
	}

	// If current version is not empty, but it's different from the one specified in the CRD the component
	// must be upgraded or downgraded unless the controller is already performing another action
	if component.Status.CurrentVersion != "" && component.Status.CurrentVersion != component.Spec.Version &&
		!meta.IsStatusConditionTrue(component.Status.Conditions, typeProgressingComponent) {
		return r.upgradeComponent(ctx, log.WithField("action", "upgrade"), component)
	}

	// If the component is in rollback mode we disable it and try to rollback.
	if component.Status.Rollback {
		log.Info("Component is in rollback mode, rolling back")
		updatedComponent := component.DeepCopy()
		updatedComponent.Status.Rollback = false
		err = r.updateStatus(ctx, updatedComponent)
		if err != nil {
			log.WithError(err).Error("Failed to reset component CRD version")
			return ctrl.Result{}, err
		}
		return r.rollback(ctx, log, updatedComponent)
	}

	log.Debug("Component reconciled")

	return ctrl.Result{RequeueAfter: time.Minute * 15}, nil
}

func (r *ComponentReconciler) valueOverrides(component *castwarev1alpha1.Component, cluster *castwarev1alpha1.Cluster) (map[string]any, error) {
	overrides := map[string]any{}
	if component.Spec.Values != nil {
		if err := json.Unmarshal(component.Spec.Values.Raw, &overrides); err != nil {
			return nil, err
		}
	}
	overrides["apiURL"] = cluster.Spec.API.APIURL
	overrides["apiKeySecretRef"] = cluster.Spec.APIKeySecret
	overrides["provider"] = cluster.Spec.Provider
	overrides["createNamespace"] = false
	return overrides, nil
}

// nolint:unparam
func (r *ComponentReconciler) deleteComponent(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component) (_ ctrl.Result, err error) {
	defer func() {
		r.recordActionResult(ctx, log, component, castai.Action_DELETE, err)
	}()

	log.Info("Component is being deleted")
	if controllerutil.ContainsFinalizer(component, componentFinalizer) {
		log.Info("Uninstalling Helm release")
		_, err := r.HelmClient.Uninstall(helm.UninstallOptions{
			Namespace:   component.Namespace,
			ReleaseName: component.Spec.Component,
			Wait:        true,
			// If the helm release is not found there is nothing to uninstall,
			// hence we can safely remove the finalizer.
			IgnoreNotFound: true,
		})
		if err != nil {
			log.WithError(err).Error("Failed to uninstall Helm release")
			return ctrl.Result{}, err
		}
		// If the helm chart is successfully uninstalled the finalizer is removed and the CR deleted.
		controllerutil.RemoveFinalizer(component, componentFinalizer)
		if err := r.Update(ctx, component); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Stop reconciliation, deletion in progress.
	return ctrl.Result{}, nil
}

func (r *ComponentReconciler) installComponent(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component, dryRun bool) (_ ctrl.Result, err error) {
	log = log.WithField("dryRun", dryRun)
	var recordErr error
	defer func() {
		if !dryRun {
			if recordErr == nil {
				recordErr = err
			}
			r.recordActionResult(ctx, log, component, castai.Action_INSTALL, recordErr, withDefaultStatus(castai.Status_PROGRESSING))
		}
	}()

	cluster := &castwarev1alpha1.Cluster{}
	err = r.Get(ctx, types.NamespacedName{Namespace: component.Namespace, Name: component.Spec.Cluster}, cluster)
	if err != nil {
		recordErr = fmt.Errorf("failed to get cluster: %w", err)
		log.WithError(err).Error("Failed to get cluster")
		// TODO: retry once after 5 min and if it still fails fail permanently
		return ctrl.Result{RequeueAfter: time.Minute * 5}, nil
	}

	// TODO: (after mvp) validate json overrides in webhook?
	overrides, err := r.valueOverrides(component, cluster)
	if err != nil {
		recordErr = fmt.Errorf("failed to set value overrides: %w", err)
		log.WithError(err).Error("Failed to set helm value overrides")
		return ctrl.Result{}, nil
	}

	_, err = r.HelmClient.GetRelease(helm.GetReleaseOptions{
		Namespace:   component.Namespace,
		ReleaseName: component.Spec.Component,
	})
	// If release is not found we install it, otherwise we just set status as progressing and wait for completion.
	// This could happen if we start to install but fail to set progressing
	// status condition (for example because the operator is restarted).
	if err != nil {
		if !errors.Is(err, driver.ErrReleaseNotFound) {
			log.WithError(err).Error("Failed to get release")
			return ctrl.Result{}, err
		}
		log.Info("Helm release not found, installing component")
		_, err = r.HelmClient.Install(ctx, helm.InstallOptions{
			ChartSource: &helm.ChartSource{
				RepoURL: cluster.Spec.HelmRepoURL,
				Name:    component.Spec.Component,
				Version: component.Spec.Version,
			},
			Namespace:       component.Namespace,
			CreateNamespace: false,
			ReleaseName:     component.Spec.Component,
			ValuesOverrides: overrides,
			DryRun:          dryRun,
		})
		if err != nil {
			recordErr = fmt.Errorf("failed to install chart: %w", err)
			log.WithError(err).Error("Failed to install chart")
			// TODO: retry once after 5 min and if it still fails fail permanently
			if dryRun {
				// For dry run we just return the error and let the caller handle it.
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: time.Minute * 5}, nil
		}
	}

	if !dryRun {
		meta.SetStatusCondition(&component.Status.Conditions, metav1.Condition{
			Type:    typeProgressingComponent,
			Status:  metav1.ConditionTrue,
			Reason:  progressingReasonInstalling,
			Message: fmt.Sprintf("Installing component: %s", component.Spec.Version),
		})
		err = r.updateStatus(ctx, component)
		if err != nil {
			log.WithError(err).Errorf("Failed to set '%s' status", typeProgressingComponent)
		}
	}

	return ctrl.Result{RequeueAfter: time.Second * 30}, nil
}

func (r *ComponentReconciler) upgradeComponent(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component) (_ ctrl.Result, err error) {
	var recordErr error
	defer func() {
		if err != nil {
			r.Recorder.Eventf(component, v1.EventTypeWarning, reasonUpgradeFailed, "Upgrade failed: %v", err)
		}
		if recordErr == nil {
			recordErr = err
		}
		r.recordActionResult(ctx, log, component, castai.Action_UPGRADE, recordErr, withDefaultStatus(castai.Status_PROGRESSING))
	}()

	cluster := &castwarev1alpha1.Cluster{}
	err = r.Get(ctx, types.NamespacedName{Namespace: component.Namespace, Name: component.Spec.Cluster}, cluster)
	if err != nil {
		recordErr = fmt.Errorf("failed to get cluster: %w", err)
		log.WithError(err).Error("Failed to get cluster")
		// TODO: retry once after 5 min and if it still fails fail permanently
		return ctrl.Result{RequeueAfter: time.Minute * 5}, nil
	}

	helmRelease, err := r.HelmClient.GetRelease(helm.GetReleaseOptions{
		Namespace:   component.Namespace,
		ReleaseName: component.Spec.Component,
	})
	if err != nil {
		log.WithError(err).Error("Failed to get helm release")
		return ctrl.Result{}, err
	}

	overrides, err := r.valueOverrides(component, cluster)
	if err != nil {
		recordErr = fmt.Errorf("failed to set value overrides: %w", err)
		log.WithError(err).Error("Failed to set helm value overrides")
		return ctrl.Result{}, nil
	}

	_, err = r.HelmClient.Upgrade(ctx, helm.UpgradeOptions{
		ChartSource: &helm.ChartSource{
			RepoURL: cluster.Spec.HelmRepoURL,
			Name:    helmRelease.Chart.Metadata.Name,
			Version: component.Spec.Version,
		},
		Release:              helmRelease,
		ValuesOverrides:      overrides,
		ResetThenReuseValues: true,
		Recreate:             true,
	})
	if err != nil {
		recordErr = fmt.Errorf("failed to upgrade chart: %w", err)
		// If new version does not exist reset to the currently installed version.
		if errors.Is(err, helm.ErrChartNotFound) {
			log.Warnf("Helm release not found, reverting CRD version to %s", helmRelease.Chart.Metadata.Version)
			updatedComponent := component.DeepCopy()
			updatedComponent.Spec.Version = helmRelease.Chart.Metadata.Version
			err = r.Client.Patch(ctx, updatedComponent, client.MergeFrom(component))
			// Patch failure is a recoverable error.
			if err != nil {
				recordErr = fmt.Errorf("failed to reset component CRD version: %w", err)
				log.WithError(err).Error("Failed to patch component")
				return ctrl.Result{RequeueAfter: time.Minute}, nil
			}
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}

		log.WithError(err).Error("Failed to upgrade chart")
		return r.rollback(ctx, log, component)
	}

	r.Recorder.Eventf(component, v1.EventTypeNormal, reasonUpgradeStarted, "Upgrade started: %s -> %s", component.Status.CurrentVersion, component.Spec.Version)

	meta.SetStatusCondition(&component.Status.Conditions, metav1.Condition{
		Type:    typeProgressingComponent,
		Status:  metav1.ConditionTrue,
		Reason:  progressingReasonUpgrading,
		Message: fmt.Sprintf("Upgrading component: %s -> %s", component.Status.CurrentVersion, component.Spec.Version),
	})
	err = r.updateStatus(ctx, component)
	if err != nil {
		// Update status errors are recoverable, if it happens we just requeue and end up here again.
		log.WithError(err).Errorf("Failed to set '%s' status", typeProgressingComponent)
	}
	return ctrl.Result{RequeueAfter: time.Second * 30}, nil
}

// TODO: this should set progressing status and we should check if it succeeded or not in next loops
func (r *ComponentReconciler) rollback(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component) (_ ctrl.Result, err error) {
	defer func() {
		if err != nil {
			r.Recorder.Eventf(component, v1.EventTypeWarning, reasonRollbackFailed, "Rollback failed: %v", err)

			if errors.Is(err, ErrNothingToRollback) {
				// if there is nothing to rollback we don't want to mark the action as failed
				r.recordActionResult(ctx, log, component, castai.Action_ROLLBACK, nil, withDefaultMessage("nothing to rollback"))
				return
			}
		}

		r.recordActionResult(ctx, log, component, castai.Action_ROLLBACK, err)
	}()

	helmRelease, err := r.HelmClient.GetRelease(helm.GetReleaseOptions{
		Namespace:   component.Namespace,
		ReleaseName: component.Spec.Component,
	})
	if err != nil {
		log.WithError(err).Error("Failed to get helm release")
		return ctrl.Result{}, err
	}
	// Helm release version start from 1 for the first install, if version is lower than 2
	// the component has never been upgrade, hence nothing to rollback
	if helmRelease.Version < 2 {
		return ctrl.Result{}, ErrNothingToRollback
	}
	previousRelease, err := r.HelmClient.GetRelease(helm.GetReleaseOptions{
		Namespace:   component.Namespace,
		ReleaseName: component.Spec.Component,
		Version:     helmRelease.Version - 1,
	})
	if err != nil {
		log.WithError(err).Error("Failed to get previous helm release")
		return ctrl.Result{}, err
	}
	// Before proceeding with rollback we set the desired version to the one we want,
	// otherwise the reconcile loop will try upgrade it again.
	updatedComponent := component.DeepCopy()
	updatedComponent.Spec.Version = previousRelease.Chart.Metadata.Version
	err = r.Client.Patch(ctx, updatedComponent, client.MergeFrom(component))
	if err != nil {
		log.WithError(err).Error("Failed to reset component CRD version")
		return ctrl.Result{}, err
	}

	err = r.HelmClient.Rollback(helm.RollbackOptions{
		Namespace:   component.Namespace,
		ReleaseName: component.Spec.Component,
	})
	if err != nil {
		log.WithError(err).Error("Failed to rollback component")
		return ctrl.Result{}, err
	}

	log.WithField("previous_version", previousRelease.Chart.Metadata.Version).
		WithField("failed_version", component.Spec.Version).
		Info("rollback in progress")
	r.Recorder.Eventf(component, v1.EventTypeNormal, reasonRollbackStarted, "Rollback to version %s started", previousRelease.Chart.Metadata.Version)
	return ctrl.Result{RequeueAfter: time.Second * 30}, nil
}

// TODO: it should consider install/upgrade/rollback and log/raise events accordingly
func (r *ComponentReconciler) checkHelmProgress(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component) (_ ctrl.Result, err error) {
	progressingCondition := meta.FindStatusCondition(component.Status.Conditions, typeProgressingComponent)
	if progressingCondition == nil {
		// this should not happen
		return ctrl.Result{}, errors.New("progressing condition not found")
	}
	log.Debug("Checking helm install progress")

	helmRelease, err := r.HelmClient.GetRelease(helm.GetReleaseOptions{
		Namespace:   component.Namespace,
		ReleaseName: component.Spec.Component,
	})
	if err != nil {
		r.recordActionResult(ctx, log, component, actionFromProgressingReason(progressingCondition.Reason), fmt.Errorf("failed to get helm release: %v", err))
		log.WithError(err).Error("Failed to get helm release")
		return ctrl.Result{RequeueAfter: time.Minute * 5}, nil
	}

	switch helmRelease.Info.Status {
	case release.StatusDeployed:
		r.recordActionResult(ctx, log, component, actionFromProgressingReason(progressingCondition.Reason), nil)

		log.WithField("component_version", component.Spec.Version).Info("Helm chart deployed")

		switch progressingCondition.Reason {
		case progressingReasonUpgrading:
			progressingCondition.Message = "Helm release upgrade successful"
		case progressingReasonMigrating:
			progressingCondition.Message = "Component migration successful"
		default:
			progressingCondition.Message = "Helm release install successful"
		}

		progressingCondition.Reason = "Completed"
		progressingCondition.Status = metav1.ConditionFalse

		// TODO: (after mvp) check component logs for known errors and rollback

		// Component successfully deployed, we can safely set available status and current version.
		meta.SetStatusCondition(&component.Status.Conditions, metav1.Condition{
			Type:    typeAvailableComponent,
			Status:  metav1.ConditionTrue,
			Reason:  reasonInstalled,
			Message: progressingCondition.Message,
		})
		component.Status.CurrentVersion = helmRelease.Chart.Metadata.Version
		r.Recorder.Eventf(component, v1.EventTypeNormal, reasonInstalled, "Version %s installed successfully", component.Status.CurrentVersion)
	case release.StatusFailed:
		// defer as we want this action to be recorded after rollback action has been recorded
		defer r.recordActionResult(ctx, log, component, actionFromProgressingReason(progressingCondition.Reason), fmt.Errorf("helm install failed: %s", helmRelease.Info.Description))

		log.Warnf("Helm install failed: %s", helmRelease.Info.Description)
		r.Recorder.Eventf(component, v1.EventTypeWarning, reasonInstallFailed, "Version %s install failed", component.Spec.Version)

		if progressingCondition.Reason == progressingReasonUpgrading {
			log.Warn("Helm chart upgrade failed, trying to rollback")
			// If an upgrade attempt failed rollback to the previous version.
			if component.Status.CurrentVersion != "" {
				return r.rollback(ctx, log.WithField("action", "rollback"), component)
			}
		}
		// If the component was installed for the first time we can't roll back,
		// the only thing we can do is setting available and progressing status conditions to false.
		progressingCondition.Reason = "Failed"
		progressingCondition.Status = metav1.ConditionFalse
		progressingCondition.Message = fmt.Sprintf("Helm release install failed: %s - %s", component.Spec.Version, helmRelease.Info.Description)

		meta.SetStatusCondition(&component.Status.Conditions, metav1.Condition{
			Type:    typeAvailableComponent,
			Status:  metav1.ConditionFalse,
			Reason:  "InstallFailed",
			Message: progressingCondition.Message,
		})
	default:
		// Install still in progress, wait and check again.
		return ctrl.Result{RequeueAfter: time.Second * 30}, nil
	}

	meta.SetStatusCondition(&component.Status.Conditions, *progressingCondition)
	err = r.updateStatus(ctx, component)
	if err != nil {
		log.WithError(err).Error("Failed to set component status")
		return ctrl.Result{RequeueAfter: time.Second * 30}, nil
	}

	// TODO: how to deal with degraded components/failed installs

	return ctrl.Result{}, nil
}

func (r *ComponentReconciler) updateStatus(ctx context.Context, component *castwarev1alpha1.Component) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var latestComponent castwarev1alpha1.Component
		if err := r.Get(ctx, types.NamespacedName{
			Name:      component.Name,
			Namespace: component.Namespace,
		}, &latestComponent); err != nil {
			return err
		}

		// Modify latestComponent.Status here
		latestComponent.Status = component.Status

		return r.Status().Update(ctx, &latestComponent)
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *ComponentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("component-controller")
	return ctrl.NewControllerManagedBy(mgr).
		For(&castwarev1alpha1.Component{}).
		Named("component").
		Complete(r)
}

type componentActionResultOption func(*castai.ComponentActionResult)

func withDefaultStatus(status castai.Status) componentActionResultOption {
	return func(r *castai.ComponentActionResult) {
		r.Status = status
	}
}

func withDefaultMessage(message string) componentActionResultOption {
	return func(r *castai.ComponentActionResult) {
		r.Message = message
	}
}

func (r *ComponentReconciler) recordActionResult(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component, action castai.ActionType, actionErr error, opts ...componentActionResultOption) {
	cluster := &castwarev1alpha1.Cluster{}
	err := r.Get(ctx, types.NamespacedName{Namespace: component.Namespace, Name: component.Spec.Cluster}, cluster)
	if err != nil {
		log.WithError(err).Error("Failed to get cluster")
		return
	}

	castAiClient, err := r.getCastaiClient(ctx, cluster)
	if err != nil {
		log.WithError(err).Error("Failed to get castaiClient")
		return
	}

	req := &castai.ComponentActionResult{
		Name:           component.Spec.Component,
		Action:         action,
		CurrentVersion: component.Status.CurrentVersion,
		Version:        component.Spec.Version,
		Status:         castai.Status_OK,
		ReleaseName:    component.Spec.Component,
		Message:        "",
	}

	for _, opt := range opts {
		opt(req)
	}

	if actionErr != nil {
		req.Status = castai.Status_ERROR
		req.Message = actionErr.Error()
	}

	if err := castAiClient.RecordActionResult(ctx, cluster.Spec.Cluster.ClusterID, req); err != nil {
		log.WithError(err).Error("Failed to record action result")
		return
	}

	log.WithFields(logrus.Fields{"request": fmt.Sprintf("%+v", req)}).Info("Recorded action result for component")
}

func (r *ComponentReconciler) getCastaiClient(ctx context.Context, cluster *castwarev1alpha1.Cluster) (castai.CastAIClient, error) {
	auth := auth.NewAuth(cluster.Namespace, cluster.Name)
	if err := auth.LoadApiKey(ctx, r.Client); err != nil {
		return nil, err
	}
	rest := castai.NewRestyClient(r.Config, cluster.Spec.API.APIURL, auth)

	client := castai.NewClient(nil, rest)

	return client, nil
}

func actionFromProgressingReason(reason string) castai.ActionType {
	switch reason {
	case progressingReasonInstalling, progressingReasonMigrating:
		return castai.Action_INSTALL
	case progressingReasonUpgrading:
		return castai.Action_UPGRADE
	default:
		return ""
	}
}
