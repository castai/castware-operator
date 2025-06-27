package controller

import (
	"castai-agent/pkg/services/providers/aks"
	"castai-agent/pkg/services/providers/eks"
	"castai-agent/pkg/services/providers/gke"
	"castai-agent/pkg/services/providers/types"
	"context"
	"fmt"
	"time"

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

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Cluster object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/reconcile
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

	clusterMetadata := cluster.Spec.Cluster
	if clusterMetadata == nil || clusterMetadata.ClusterID == "" {
		p, err := GetProvider(ctx, cluster)
		if err != nil {
			// TODO: handle error
			log.Error(err, "Failed to get provider")
			return ctrl.Result{}, err
		}

		client, err := r.getCastaiClient(ctx, cluster)
		if err != nil {
			log.Error(err, "Failed to get castaiClient")
			return ctrl.Result{}, err
		}

		result, err := p.RegisterCluster(ctx, client)
		if err != nil {
			log.Error(err, "Failed to register cluster")
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
		return ctrl.Result{Requeue: true}, nil
	}

	return ctrl.Result{}, nil
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

func GetProvider(ctx context.Context, cluster *castwarev1alpha1.Cluster) (types.Provider, error) {
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

	// TODO: when implementing reconciler add a watcher to monitor api key secret changes and refresh cache

	return ctrl.NewControllerManagedBy(mgr).
		For(&castwarev1alpha1.Cluster{}).
		Named("cluster").
		Complete(r)
}
