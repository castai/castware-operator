package controller

import (
	"castai-agent/pkg/services/providers/aks"
	"castai-agent/pkg/services/providers/eks"
	"castai-agent/pkg/services/providers/gke"
	providers "castai-agent/pkg/services/providers/types"
	"context"
	"fmt"
	"github.com/samber/lo"
	"time"

	"k8s.io/apimachinery/pkg/types"

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

		castAiClient, err := r.getCastaiClient(ctx, cluster)
		if err != nil {
			log.WithError(err).Error("Failed to get castaiClient")
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

	if err := r.reconcileSecret(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}

	// If everything is reconciled, we can listen for cluster hub events.
	castaiClient, err := r.getCastaiClient(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	actions, err := castaiClient.GetActions(ctx, "", cluster.Spec.Cluster.ClusterID)
	if err != nil {
		log.WithError(err).Error("Failed to get actions")
		return ctrl.Result{}, err
	}
	log.Infof("Got %d actions", len(actions))
	for _, action := range actions {
		if !action.IsValid() {
			log.Errorf("Invalid action: %v", action.GetType())
			err := castaiClient.AckAction(ctx, action.ID, cluster.Spec.Cluster.ClusterID, lo.ToPtr(fmt.Sprintf("invalid action: %s", action.GetType())))
			if err != nil {
				log.WithError(err).Error("Failed to ack action")
			}
			log.Infof("Acked invalid action: %v", action.GetType())
			continue
		}
		switch t := action.Data().(type) {
		case *castai.ActionChartUpsert:
			log.Infof("Got chart upsert action: %v", t)
			component := &castwarev1alpha1.Component{}
			// TODO: handle errors
			err := r.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: t.ReleaseName}, component)
			if err != nil {
				log.WithError(err).Error("Failed to get component")
				return ctrl.Result{}, err
			}
			if component.Spec.Version != t.ChartSource.Version {
				log.Infof("Component version changed, reconciling")
				updatedComponent := component.DeepCopy()
				updatedComponent.Spec.Version = t.ChartSource.Version
				err = r.Client.Patch(ctx, updatedComponent, client.MergeFrom(component))
				if err != nil {
					log.WithError(err).Error("Failed to patch component CR")
					return ctrl.Result{}, err
				}
			} else {
				log.Infof("Component version not changed")
			}

			err = castaiClient.AckAction(ctx, action.ID, cluster.Spec.Cluster.ClusterID, nil)
			if err != nil {
				log.WithError(err).Error("Failed to ack action")
				return ctrl.Result{}, err
			}
			log.Infof("Acked chart upsert action: %v", t)
		default:
			log.Infof("Got unsupported action: %v", t)
			err := castaiClient.AckAction(ctx, action.ID, cluster.Spec.Cluster.ClusterID, lo.ToPtr(fmt.Sprintf("unsupported action: %s", action.GetType())))
			if err != nil {
				log.WithError(err).Error("Failed to ack action")
			}
			log.Infof("Acked unsupported action: %v", t)
		}
	}

	// TODO: change to higher value like 5 mins
	return ctrl.Result{RequeueAfter: time.Second * 30}, nil
}

func (r *ClusterReconciler) reconcileSecret(ctx context.Context, cluster *castwarev1alpha1.Cluster) error {
	log := r.Log

	// Can't reconcile api key if the cluster is not there.
	if cluster.Spec.Cluster == nil {
		return nil
	}

	// Check if api key secret changed.
	secret := &corev1.Secret{}
	secKey := types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Spec.APIKeySecret}
	if err := r.Get(ctx, secKey, secret); err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	// If api key changed validate the new one.
	if secret.ResourceVersion != cluster.Status.LastSecretVersion {
		castAiClient, err := r.getCastaiClient(ctx, cluster)
		if err != nil {
			log.WithError(err).Error("Failed to get api client")
			return err
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
				return err
			}

			return err
		}
		log.Info("Api key updated")

		cluster.Status.LastSecretVersion = secret.ResourceVersion
		if err := r.Status().Update(ctx, cluster); err != nil {
			return err
		}
	}
	return nil
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
				return true
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
