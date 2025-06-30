package controller

import (
	"castai-agent/pkg/services/providers/aks"
	"castai-agent/pkg/services/providers/eks"
	"castai-agent/pkg/services/providers/gke"
	providers "castai-agent/pkg/services/providers/types"
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/types"
	"time"

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
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// Definitions to manage status conditions
const (
	// typeAvailableCluster represents the status when cluster resource is reconciled and works as expected.
	typeAvailableCluster = "Available"
	// typeProgressingCluster represents the status when cluster resource reconciliation is in progress.
	typeProgressingCluster = "Progressing"
	// typeDegradedCastware represents the status used when something went wrong with cluster reconciliation.
	typeDegradedCluster = "Degraded"
)

// clusterReconciler reconciles a Cluster object
type clusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func NewClusterReconciler(mgr manager.Manager) *clusterReconciler {
	return &clusterReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}
}

// +kubebuilder:rbac:groups=castware.cast.ai,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=castware.cast.ai,resources=clusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=castware.cast.ai,resources=clusters/finalizers,verbs=update

func (r *clusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

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
		log.Error(err, "Failed to get cluster")
		return ctrl.Result{}, err
	}

	if err := r.setProgressing(ctx, cluster, true); err != nil {
		log.Error(err, "Failed to set progressing")
		return ctrl.Result{}, err
	}

	clusterMetadata := cluster.Spec.Cluster
	if clusterMetadata == nil || clusterMetadata.ClusterID == "" {
		p, err := GetProvider(ctx, cluster)
		if err != nil {
			// TODO: handle error
			log.Error(err, "Failed to get provider")
			return ctrl.Result{}, err
		}

		castAiClient, err := r.getCastaiClient(ctx, cluster)
		if err != nil {
			log.Error(err, "Failed to get castaiClient")
			return ctrl.Result{}, err
		}

		result, err := p.RegisterCluster(ctx, castAiClient)
		if err != nil {
			log.Error(err, "Failed to register cluster")
			meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{Type: typeDegradedCluster, Status: metav1.ConditionUnknown, Reason: err.Error(), Message: "Failed to register cluster"})
			err = r.Status().Update(ctx, cluster)
			if err != nil {
				log.Error(err, "Failed to set status")
			}
			// TODO: retry logic
			return ctrl.Result{RequeueAfter: time.Minute * 1}, err
		}
		// Set cluster ID from register cluster result
		cluster.Spec.Cluster = &castwarev1alpha1.ClusterMetadataSpec{ClusterID: result.ClusterID}
		err = r.Update(ctx, cluster)
		if err != nil {
			log.Error(err, "Failed to update cluster")
			// TODO: retry logic
			return ctrl.Result{RequeueAfter: time.Minute * 1}, err
		}
		return ctrl.Result{RequeueAfter: time.Second * 30}, nil
	}

	if clusterMetadata.ClusterID != "" && !meta.IsStatusConditionTrue(cluster.Status.Conditions, typeAvailableCluster) {
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{Type: typeAvailableCluster, Status: metav1.ConditionTrue, Reason: "ClusterID available", Message: "Cluster reconciled"})
		err = r.Status().Update(ctx, cluster)
		if err != nil {
			log.Error(err, "Failed to set available status")
			return ctrl.Result{}, err
		}
	}

	// Check if api key secret changed.
	secret := &corev1.Secret{}
	secKey := types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Spec.APIKeySecret}
	if err := r.Get(ctx, secKey, secret); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	currentSecretVersion := ""
	if secret != nil {
		currentSecretVersion = secret.ResourceVersion
	}
	// If api key changed validate the new one.
	if currentSecretVersion != cluster.Status.LastSecretVersion {
		client, err := r.getCastaiClient(ctx, cluster)
		if err != nil {
			log.Error(err, "Failed to get api client")
			return ctrl.Result{}, err
		}
		// TODO: better client validation
		client.Me(ctx)
	}
	cluster.Status.LastSecretVersion = currentSecretVersion
	if err := r.Status().Update(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.setProgressing(ctx, cluster, false); err != nil {
		log.Error(err, "Failed to set progressing")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *clusterReconciler) setProgressing(ctx context.Context, cluster *castwarev1alpha1.Cluster, isProgressing bool) error {
	status := metav1.ConditionFalse
	if isProgressing {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{Type: typeProgressingCluster, Status: status, Reason: "", Message: "Reconciling cluster"})
	return r.Status().Update(ctx, cluster)
}

func (r *clusterReconciler) getCastaiClient(ctx context.Context, cluster *castwarev1alpha1.Cluster) (castai.CastAIClient, error) {
	auth := auth.NewAuth(cluster.Namespace, cluster.Name)
	if err := auth.LoadApiKey(ctx, r.Client); err != nil {
		return nil, err
	}
	cfg, err := config.GetFromEnvironment()
	if err != nil {
		return nil, err
	}
	rest := castai.NewRestyClient(cfg, cluster.Spec.API.APIURL, auth, "")

	client := castai.NewClient(nil, rest)

	return client, nil
}

func GetProvider(ctx context.Context, cluster *castwarev1alpha1.Cluster) (providers.Provider, error) {
	// TODO: move log
	log := logrus.New()

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

// SetupWithManager sets up the controller with the Manager.
func (r *clusterReconciler) SetupWithManager(mgr ctrl.Manager) error {

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
