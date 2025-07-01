package v1alpha1

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/castai/castware-operator/internal/castai"
	"github.com/sirupsen/logrus"

	"github.com/castai/castware-operator/internal/castai/auth"
	"github.com/castai/castware-operator/internal/config"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
)

// nolint:unused
// log is for logging in this package.
var clusterlog = logf.Log.WithName("cluster-resource")

// SetupClusterWebhookWithManager registers the webhook for Cluster in the manager.
func SetupClusterWebhookWithManager(mgr ctrl.Manager) error {
	cfg, err := config.GetFromEnvironment()
	if err != nil {
		return fmt.Errorf("unable to load config from environment: %w", err)
	}

	return ctrl.NewWebhookManagedBy(mgr).For(&castwarev1alpha1.Cluster{}).
		WithValidator(&ClusterCustomValidator{client: mgr.GetClient(), config: cfg}).
		WithDefaulter(&ClusterCustomDefaulter{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-castware-cast-ai-v1alpha1-cluster,mutating=true,failurePolicy=fail,sideEffects=None,groups=castware.cast.ai,resources=clusters,verbs=create;update,versions=v1alpha1,name=mcluster-v1alpha1.kb.io,admissionReviewVersions=v1

// ClusterCustomDefaulter struct is responsible for setting default values on the custom resource of the
// Kind Cluster when those are created or updated.
type ClusterCustomDefaulter struct {
	// TODO(user): Add more fields as needed for defaulting
}

var _ webhook.CustomDefaulter = &ClusterCustomDefaulter{}

// Default implements webhook.CustomDefaulter so a webhook will be registered for the Kind Cluster.
func (d *ClusterCustomDefaulter) Default(ctx context.Context, obj runtime.Object) error {
	cluster, ok := obj.(*castwarev1alpha1.Cluster)
	if !ok {
		return fmt.Errorf("expected a Cluster object but got %T", obj)
	}
	apiURL, err := url.Parse(cluster.Spec.API.APIURL)
	if err != nil {
		return fmt.Errorf("invalid api url: %w", err)
	}
	baseUrl := strings.Split(apiURL.Host, ".")
	if len(baseUrl) < 2 {
		return fmt.Errorf("invalid api url: %s", cluster.Spec.API.APIURL)
	}
	baseUrl = baseUrl[1:]

	if cluster.Spec.API.GrpcURL == "" {
		cluster.Spec.API.GrpcURL = strings.Join(append([]string{"grpc"}, baseUrl...), ".")
		clusterlog.Info("setting GrpcURL from ApiURL", "url", cluster.Spec.API.GrpcURL)
	}
	if cluster.Spec.API.KvisorGrpcURL == "" {
		cluster.Spec.API.KvisorGrpcURL = strings.Join(append([]string{"kvisor"}, baseUrl...), ".")
		clusterlog.Info("setting KvisorGrpcURL from ApiURL", "url", cluster.Spec.API.KvisorGrpcURL)
	}

	return nil
}

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-castware-cast-ai-v1alpha1-cluster,mutating=false,failurePolicy=fail,sideEffects=None,groups=castware.cast.ai,resources=clusters,verbs=create;update,versions=v1alpha1,name=vcluster-v1alpha1.kb.io,admissionReviewVersions=v1

// ClusterCustomValidator struct is responsible for validating the Cluster resource
// when it is created, updated, or deleted.
type ClusterCustomValidator struct {
	client client.Client
	config *config.Config
}

var _ webhook.CustomValidator = &ClusterCustomValidator{}

func (v *ClusterCustomValidator) validateApiKey(ctx context.Context, cluster *castwarev1alpha1.Cluster) error {
	auth := auth.NewAuthFromCR(cluster)

	err := auth.LoadApiKey(ctx, v.client)
	if err != nil {
		return fmt.Errorf("unable to load api key: %w", err)
	}

	restClient := castai.NewRestyClient(v.config, cluster.Spec.API.APIURL, auth)

	_, err = castai.NewClient(logrus.New(), restClient).Me(ctx)
	if err != nil {
		return err
	}

	return nil
}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type Cluster.
func (v *ClusterCustomValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	cluster, ok := obj.(*castwarev1alpha1.Cluster)
	if !ok {
		return nil, fmt.Errorf("expected a Cluster object but got %T", obj)
	}

	if cluster.Spec.Provider == "" {
		return nil, fmt.Errorf("provider must be specified")
	}

	err := v.validateApiKey(ctx, cluster)
	if err != nil {
		return nil, err
	}

	clusterlog.Info("New cluster validated", "name", cluster.GetName())
	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type Cluster.
func (v *ClusterCustomValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	cluster, ok := newObj.(*castwarev1alpha1.Cluster)
	if !ok {
		return nil, fmt.Errorf("expected a Cluster object for the newObj but got %T", newObj)
	}
	oldCluster, ok := oldObj.(*castwarev1alpha1.Cluster)
	if !ok {
		return nil, fmt.Errorf("expected a Cluster object for the newObj but got %T", newObj)
	}

	if oldCluster.Spec.Provider != cluster.Spec.Provider {
		return nil, fmt.Errorf("provider cannot be modified")
	}

	if oldCluster.Spec.Cluster != nil && cluster.Spec.Cluster != nil {
		oldClusterSpec := oldCluster.Spec.Cluster
		newClusterSpec := cluster.Spec.Cluster
		if oldClusterSpec.ClusterID != newClusterSpec.ClusterID ||
			oldClusterSpec.ClusterName != newClusterSpec.ClusterName ||
			oldClusterSpec.ProjectID != newClusterSpec.ProjectID ||
			oldClusterSpec.Location != newClusterSpec.Location {
			return nil, fmt.Errorf("cluster spec cannot be modified after onboarding")
		}
	}
	if oldCluster.Spec.Cluster != nil && cluster.Spec.Cluster == nil {
		return nil, fmt.Errorf("cluster spec cannot be deleted")
	}

	if oldCluster.Spec.APIKeySecret != cluster.Spec.APIKeySecret {
		err := v.validateApiKey(ctx, cluster)
		if err != nil {
			return nil, err
		}
	}

	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type Cluster.
func (v *ClusterCustomValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	cluster, ok := obj.(*castwarev1alpha1.Cluster)
	if !ok {
		return nil, fmt.Errorf("expected a Cluster object but got %T", obj)
	}
	clusterlog.Info("Validation for Cluster upon deletion", "name", cluster.GetName())

	// TODO(user): fill in your validation logic upon object deletion.

	return nil, nil
}
