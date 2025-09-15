package controller

import (
	"castai-agent/pkg/services/providers/aks"
	"castai-agent/pkg/services/providers/eks"
	"castai-agent/pkg/services/providers/gke"
	providers "castai-agent/pkg/services/providers/types"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/castai/castware-operator/internal/utils"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/castai/castware-operator/internal/config"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/castai"
	"github.com/castai/castware-operator/internal/castai/auth"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var unknownActionError = fmt.Errorf("unknown action")

// Definitions to manage status conditions
const (
	// typeAvailableCluster represents the status when cluster resource is reconciled and works as expected.
	typeAvailableCluster = "Available"
	// typeDegradedCastware represents the status used when something went wrong with cluster reconciliation.
	typeDegradedCluster = "Degraded"
)

// ClusterReconciler reconciles a Cluster object
type ClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logrus.FieldLogger
	Config *config.Config
}

// +kubebuilder:rbac:groups=castware.cast.ai,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=castware.cast.ai,resources=clusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=castware.cast.ai,resources=clusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingwebhookconfigurations,verbs=get;list;watch;patch;update

func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log

	cluster := &castwarev1alpha1.Cluster{}
	err := r.Get(ctx, req.NamespacedName, cluster)
	if err != nil {
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

	castAiClient, err := r.getCastaiClient(ctx, cluster)
	if err != nil {
		log.WithError(err).Error("Failed to get castaiClient")
		return ctrl.Result{}, err
	}

	clusterMetadata := cluster.Spec.Cluster
	if clusterMetadata == nil || clusterMetadata.ClusterID == "" {
		p, err := GetProvider(ctx, r.Log, cluster)
		if err != nil {
			// TODO: handle error
			log.WithError(err).Error("Failed to get provider")
			return ctrl.Result{}, err
		}

		result, err := p.RegisterCluster(ctx, castAiClient)
		if err != nil {
			log.WithError(err).Error("Failed to register cluster")
			meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
				Type:    typeDegradedCluster,
				Status:  metav1.ConditionUnknown,
				Reason:  "ClusterRegistrationFailed",
				Message: fmt.Sprintf("Failed to register cluster: %v", err),
			})
			err = r.Status().Update(ctx, cluster)
			if err != nil {
				log.WithError(err).Errorf("Failed to set '%s' status", typeDegradedCluster)
			}
			// TODO: retry logic
			return ctrl.Result{RequeueAfter: time.Minute * 1}, err
		}
		// Set cluster ID from register cluster result
		updatedCluster := cluster.DeepCopy()
		updatedCluster.Spec.Cluster = &castwarev1alpha1.ClusterMetadataSpec{ClusterID: result.ClusterID}
		err = r.Client.Patch(ctx, updatedCluster, client.MergeFrom(cluster))
		if err != nil {
			log.WithError(err).Error("Failed to set cluster id")
			// TODO: retry logic
			return ctrl.Result{RequeueAfter: time.Minute * 1}, err
		}
		return ctrl.Result{RequeueAfter: time.Second * 30}, nil
	}

	if clusterMetadata.ClusterID != "" && !meta.IsStatusConditionTrue(cluster.Status.Conditions, typeAvailableCluster) {
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{Type: typeAvailableCluster, Status: metav1.ConditionTrue, Reason: "ClusterIdAvailable", Message: "Cluster reconciled"})
		err = r.Status().Update(ctx, cluster)
		if err != nil {
			log.WithError(err).Error("Failed to set available status")
			return ctrl.Result{}, err
		}
	}

	reconcile, err := r.reconcileSecret(ctx, cluster)
	if err != nil {
		return ctrl.Result{RequeueAfter: time.Minute * 5}, err
	} else if reconcile {
		return ctrl.Result{RequeueAfter: time.Second * 30}, nil
	}

	return r.pollActions(ctx, castAiClient, cluster)
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
		case *castai.ActionRollback:
			log.Infof("rollback action: %v", a.Component)
		default:
			actionErr = unknownActionError
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
	namespacedName := types.NamespacedName{Namespace: cluster.Namespace, Name: action.Component}

	component := &castwarev1alpha1.Component{}
	err := r.Get(ctx, namespacedName, component)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.WithError(err).Error("Failed to get component")
			return errors.New("component not found")
		}
		log.WithError(err).Error("Failed to get component")
		return fmt.Errorf("failed to get component: %w", err)
	}

	if component.Spec.Version == action.Version {
		return errors.New("component already up to date")
	}

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var latestComponent castwarev1alpha1.Component
		if err := r.Get(ctx, types.NamespacedName{
			Name:      component.Name,
			Namespace: component.Namespace,
		}, &latestComponent); err != nil {
			return err
		}

		updatedComponent := latestComponent.DeepCopy()
		updatedComponent.Spec.Version = action.Version

		if action.ValuesOverrides != nil {
			values, err := utils.UnflattenMap(action.ValuesOverrides)
			if err != nil {
				return err
			}
			if latestComponent.Spec.Values != nil {
				currentValues := map[string]interface{}{}
				if err = json.Unmarshal(latestComponent.Spec.Values.Raw, &currentValues); err != nil {
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

		return r.Client.Patch(ctx, updatedComponent, client.MergeFrom(component))
	})
}

func (r *ClusterReconciler) getCastaiClient(ctx context.Context, cluster *castwarev1alpha1.Cluster) (castai.CastAIClient, error) {
	auth := auth.NewAuth(cluster.Namespace, cluster.Name)
	if err := auth.LoadApiKey(ctx, r.Client); err != nil {
		return nil, err
	}
	rest := castai.NewRestyClient(r.Config, cluster.Spec.API.APIURL, auth)

	client := castai.NewClient(nil, rest)

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
