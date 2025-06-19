package v1alpha1

import (
	"context"
	"fmt"

	"github.com/castai/castware-operator/internal/castai"
	"github.com/castai/castware-operator/internal/castai/auth"
	"github.com/castai/castware-operator/internal/config"
	"github.com/sirupsen/logrus"
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

// TODO(user): EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!

// +kubebuilder:webhook:path=/mutate-castware-cast-ai-v1alpha1-cluster,mutating=true,failurePolicy=fail,sideEffects=None,groups=castware.cast.ai,resources=clusters,verbs=create;update,versions=v1alpha1,name=mcluster-v1alpha1.kb.io,admissionReviewVersions=v1

// ClusterCustomDefaulter struct is responsible for setting default values on the custom resource of the
// Kind Cluster when those are created or updated.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as it is used only for temporary operations and does not need to be deeply copied.
type ClusterCustomDefaulter struct {
	// TODO(user): Add more fields as needed for defaulting
}

var _ webhook.CustomDefaulter = &ClusterCustomDefaulter{}

// Default implements webhook.CustomDefaulter so a webhook will be registered for the Kind Cluster.
func (d *ClusterCustomDefaulter) Default(ctx context.Context, obj runtime.Object) error {
	cluster, ok := obj.(*castwarev1alpha1.Cluster)

	if !ok {
		return fmt.Errorf("expected an Cluster object but got %T", obj)
	}
	clusterlog.Info("Defaulting for Cluster", "name", cluster.GetName())

	// TODO(user): fill in your defaulting logic.

	return nil
}

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-castware-cast-ai-v1alpha1-cluster,mutating=false,failurePolicy=fail,sideEffects=None,groups=castware.cast.ai,resources=clusters,verbs=create;update,versions=v1alpha1,name=vcluster-v1alpha1.kb.io,admissionReviewVersions=v1

// ClusterCustomValidator struct is responsible for validating the Cluster resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type ClusterCustomValidator struct {
	client client.Client
	config *config.Config
}

var _ webhook.CustomValidator = &ClusterCustomValidator{}

func (v *ClusterCustomValidator) validateApiKey(ctx context.Context, cluster *castwarev1alpha1.Cluster) error {
	auth := auth.NewAuth(cluster.Namespace, cluster.Name)

	err := auth.LoadApiKey(ctx, v.client)
	if err != nil {
		return fmt.Errorf("unable to load api key: %w", err)
	}

	restClient := castai.NewRestyClient(v.config, cluster.Spec.API.APIURL, auth, "v0.1")

	_, err = castai.NewClient(logrus.New(), restClient).Me(ctx)
	if err != nil {
		return err
	}

	// TODO: WIRE-1334 - make an api call to check that the token is valid

	return nil
}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type Cluster.
func (v *ClusterCustomValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	cluster, ok := obj.(*castwarev1alpha1.Cluster)
	if !ok {
		return nil, fmt.Errorf("expected a Cluster object but got %T", obj)
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
	clusterlog.Info("Validation for Cluster upon update", "name", cluster.GetName())
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
