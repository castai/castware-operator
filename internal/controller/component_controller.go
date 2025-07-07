package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"helm.sh/helm/v3/pkg/storage/driver"

	"k8s.io/client-go/util/retry"

	"helm.sh/helm/v3/pkg/release"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/castai/castware-operator/internal/helm"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Definitions to manage status conditions
const (
	// typeAvailableComponent represents the status when component resource is reconciled, installed and works as expected.
	typeAvailableComponent = "Available"

	// typeProgressingComponent represent the status when a component is installing
	typeProgressingComponent = "Progressing"
)

// +kubebuilder:rbac:groups=castware.cast.ai,resources=components,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=castware.cast.ai,resources=components/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=castware.cast.ai,resources=components/finalizers,verbs=update
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=get
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;create
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
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=create;get;list;watch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=create;get;list;watch

// ComponentReconciler reconciles a Component object
type ComponentReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Log        logrus.FieldLogger
	HelmClient helm.Client
}

func (r *ComponentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log

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
		return ctrl.Result{RequeueAfter: time.Minute * 5}, err
	}
	log = log.WithField("component", component.Name).WithField("cluster", component.Spec.Cluster)

	// If installation/upgrade is in progress we wait for completion or timeout.
	if progressingCondition := meta.FindStatusCondition(component.Status.Conditions, typeProgressingComponent); progressingCondition != nil &&
		progressingCondition.Status == metav1.ConditionTrue {
		log.WithField("action", progressingCondition.Reason).Info("Helm installation is in progress")

		// TODO: proper helm install timeout
		if time.Now().After(progressingCondition.LastTransitionTime.Add(10 * time.Minute)) {
			// Helm install got stuck, try to rollback or mark component as failed.
			log.Warn("Helm installation timeout exceeded")
			return ctrl.Result{}, nil
		}

		return r.checkInstallProgress(ctx, log, component)
	}

	// This should not happen as we set version in mutating webhook, just keeping it here as precaution.
	if component.Spec.Version == "" {
		return ctrl.Result{}, errors.New("version not set for component")
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
	if component.Status.CurrentVersion == "" && !meta.IsStatusConditionTrue(component.Status.Conditions, typeProgressingComponent) {
		log.Infof("Component is not installed, installing version %s", component.Spec.Version)
		return r.installComponent(ctx, log, component)
	}

	// TODO: (WIRE-1440) compare actual version to desired version to upgrade/downgrade
	// TODO: (WIRE-1441) delete component when crd is deleted

	return ctrl.Result{}, nil
}

func (r *ComponentReconciler) installComponent(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component) (ctrl.Result, error) {
	cluster := &castwarev1alpha1.Cluster{}
	err := r.Get(ctx, types.NamespacedName{Namespace: component.Namespace, Name: component.Spec.Cluster}, cluster)
	if err != nil {
		log.WithError(err).Error("Failed to get cluster")
		return ctrl.Result{RequeueAfter: time.Minute * 5}, err
	}

	// TODO: (after mvp) validate json overrides in webhook?
	overrides := map[string]string{}
	if component.Spec.Values != nil {
		if err := json.Unmarshal(component.Spec.Values.Raw, &overrides); err != nil {
			log.WithError(err).Error("Failed to unmarshal values")
			return ctrl.Result{}, err
		}
	}
	overrides["apiURL"] = cluster.Spec.API.APIURL
	overrides["apiKeySecretRef"] = cluster.Spec.APIKeySecret
	overrides["provider"] = cluster.Spec.Provider
	overrides["createNamespace"] = "false"

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
		})
		if err != nil {
			log.WithError(err).Error("Failed to install chart")
			return ctrl.Result{RequeueAfter: time.Minute * 5}, nil
		}
	}

	meta.SetStatusCondition(&component.Status.Conditions, metav1.Condition{
		Type:    typeProgressingComponent,
		Status:  metav1.ConditionTrue,
		Reason:  "Installing",
		Message: fmt.Sprintf("Installing component: %s", component.Spec.Version),
	})
	err = r.updateStatus(ctx, component)
	if err != nil {
		log.WithError(err).Errorf("Failed to set '%s' status", typeProgressingComponent)
	}

	return ctrl.Result{RequeueAfter: time.Second * 30}, nil
}

func (r *ComponentReconciler) checkInstallProgress(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component) (ctrl.Result, error) {
	helmRelease, err := r.HelmClient.GetRelease(helm.GetReleaseOptions{
		Namespace:   component.Namespace,
		ReleaseName: component.Spec.Component,
	})
	if err != nil {
		log.WithError(err).Error("Failed to get helm release")
		return ctrl.Result{RequeueAfter: time.Minute * 5}, err
	}

	progressingCondition := metav1.Condition{
		Type:   typeProgressingComponent,
		Status: metav1.ConditionFalse,
	}

	switch helmRelease.Info.Status {
	case release.StatusDeployed:
		log.Info("Helm chart deployed")
		progressingCondition.Reason = "Completed"
		progressingCondition.Message = "Helm release install successful"

		// Component successfully deployed, we can safely set available status and current version.
		meta.SetStatusCondition(&component.Status.Conditions, metav1.Condition{
			Type:    typeAvailableComponent,
			Status:  metav1.ConditionTrue,
			Reason:  "Installed",
			Message: progressingCondition.Message,
		})
		component.Status.CurrentVersion = component.Spec.Version
	case release.StatusFailed:
		log.Warnf("Helm install failed: %s", helmRelease.Info.Description)
		progressingCondition.Reason = "Failed"
		progressingCondition.Message = fmt.Sprintf("Helm release install failed: %s", helmRelease.Info.Description)
	default:
		// Install still in progress, wait and check again.
		return ctrl.Result{RequeueAfter: time.Second * 30}, err
	}

	meta.SetStatusCondition(&component.Status.Conditions, progressingCondition)
	err = r.updateStatus(ctx, component)
	if err != nil {
		log.WithError(err).Error("Failed to set component status")
	}

	// TODO: how to deal with degraded components/failed installs

	return ctrl.Result{RequeueAfter: time.Minute * 5}, err
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
	return ctrl.NewControllerManagedBy(mgr).
		For(&castwarev1alpha1.Component{}).
		Named("component").
		Complete(r)
}
