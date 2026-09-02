package controller

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/castai"
	"github.com/castai/castware-operator/internal/castai/auth"
	"github.com/castai/castware-operator/internal/config"
	"github.com/castai/castware-operator/internal/helm"
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
	// LogIngest, when set, receives the cluster identity (clusterID, apiURL, apiKey)
	// once the cluster is registered, enabling structured log shipping to the
	// mothership. Optional: nil-safe (no-op when unset, e.g. in tests).
	LogIngest logIngestStateUpdater

	// lastHybridWarnClusterID dedupes the hybrid-config Mothership warning so a
	// cluster with the umbrella release and individual sub-component releases both
	// installed is reported once per operator process rather than on every ~30s
	// reconcile. It is in-memory only: a restart re-fires the warning once. Guarded
	// by Reconcile's single-threaded execution; no mutex needed.
	lastHybridWarnClusterID string
}

// logIngestStateUpdater is the subset of the logingest hook needed by the
// reconciler. Declared here to avoid importing the logingest package (which
// would create an import cycle only if reversed; kept local for clarity and
// testability).
type logIngestStateUpdater interface {
	UpdateState(clusterID, apiURL, apiKey, provider string)
}

// +kubebuilder:rbac:groups=castware.cast.ai,resources=*,verbs=get;list;watch
// +kubebuilder:rbac:groups=castware.cast.ai,resources=clusters,verbs=create;update;patch;delete
// +kubebuilder:rbac:groups=castware.cast.ai,resources=clusters/status,verbs=update;patch
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

	// Attach the cluster's provider to the log context, matching other CAST AI
	// components (e.g. the agent logs provider=gke). It then flows into both
	// stdout and the shipped IngestLogs fields.
	if cluster.Spec.Provider != "" {
		log = log.WithField("provider", cluster.Spec.Provider)
	}

	// Check if operator upgrade is in progress
	if cluster.Status.UpgradeJobName != "" {
		log.Info("Operator upgrade in progress, checking job status")
		return r.checkUpgradeJobStatus(ctx, cluster)
	}

	log.Debug("getCastaiClient")
	castAiClient, apiKeyAuth, err := r.getCastaiClient(ctx, cluster)
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

	// Once the cluster is registered and its ID is known, enable structured log
	// shipping to the mothership. Idempotent: the hook's UpdateState is cheap to
	// repeat, and re-running on each reconcile picks up API key rotations and
	// provider changes. A no-op when LogIngest is unset (e.g. in tests).
	if cluster.Spec.Cluster != nil && cluster.Spec.Cluster.ClusterID != "" && r.LogIngest != nil && apiKeyAuth != nil {
		r.LogIngest.UpdateState(cluster.Spec.Cluster.ClusterID, cluster.Spec.API.APIURL, apiKeyAuth.ApiKey(), cluster.Spec.Provider)
	}

	log.Debug("Completing initial setup")
	if result, err := r.completeInitialSetup(ctx, cluster, castAiClient); err != nil || result.RequeueAfter > 0 {
		return result, err
	}
	log.Debug("Initial setup completed")

	// Detect and report operator helm revision changes (e.g., from direct helm upgrades)
	if err := r.detectAndReportOperatorHelmRevisionChange(ctx, cluster, castAiClient); err != nil {
		log.WithError(err).Error("Failed to detect operator helm revision change")
	}

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

func (r *ClusterReconciler) getCastaiClient(ctx context.Context, cluster *castwarev1alpha1.Cluster) (castai.CastAIClient, auth.Auth, error) {
	auth := auth.NewAuth(cluster.Namespace, cluster.Name)
	if err := auth.LoadApiKey(ctx, r.Client); err != nil {
		return nil, nil, err
	}
	rest := castai.NewRestyClient(r.Config, cluster.Spec.API.APIURL, auth)

	client := castai.NewClient(nil, r.Config, rest)

	return client, auth, nil
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
