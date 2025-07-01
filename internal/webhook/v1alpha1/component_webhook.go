package v1alpha1

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/castai"
	"github.com/castai/castware-operator/internal/castai/auth"
	"github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/config"
	"github.com/sirupsen/logrus"
)

// nolint:unused
// log is for logging in this package.
var componentlog = logf.Log.WithName("component-resource")

// SetupComponentWebhookWithManager registers the webhook for Component in the manager.
func SetupComponentWebhookWithManager(mgr ctrl.Manager, version *config.CastwareOperatorVersion) error {
	cfg, err := config.GetFromEnvironment()
	if err != nil {
		return fmt.Errorf("unable to load config from environment: %w", err)
	}

	return ctrl.NewWebhookManagedBy(mgr).For(&castwarev1alpha1.Component{}).
		WithValidator(&ComponentCustomValidator{client: mgr.GetClient(), config: cfg, version: version}).
		WithDefaulter(&ComponentCustomDefaulter{}).
		Complete()
}

// TODO(user): EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!

// +kubebuilder:webhook:path=/mutate-castware-cast-ai-v1alpha1-component,mutating=true,failurePolicy=fail,sideEffects=None,groups=castware.cast.ai,resources=components,verbs=create;update,versions=v1alpha1,name=mcomponent-v1alpha1.kb.io,admissionReviewVersions=v1

// ComponentCustomDefaulter struct is responsible for setting default values on the custom resource of the
// Kind Component when those are created or updated.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as it is used only for temporary operations and does not need to be deeply copied.
type ComponentCustomDefaulter struct {
	// TODO(user): Add more fields as needed for defaulting
}

var _ webhook.CustomDefaulter = &ComponentCustomDefaulter{}

// Default implements webhook.CustomDefaulter so a webhook will be registered for the Kind Component.
func (d *ComponentCustomDefaulter) Default(ctx context.Context, obj runtime.Object) error {
	component, ok := obj.(*castwarev1alpha1.Component)

	if !ok {
		return fmt.Errorf("expected an Component object but got %T", obj)
	}
	componentlog.Info("Defaulting for Component", "name", component.GetName())

	// TODO(user): fill in your defaulting logic.

	return nil
}

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-castware-cast-ai-v1alpha1-component,mutating=false,failurePolicy=fail,sideEffects=None,groups=castware.cast.ai,resources=components,verbs=create;update,versions=v1alpha1,name=vcomponent-v1alpha1.kb.io,admissionReviewVersions=v1

// ComponentCustomValidator struct is responsible for validating the Component resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type ComponentCustomValidator struct {
	client  client.Client
	config  *config.Config
	version *config.CastwareOperatorVersion
}

var _ webhook.CustomValidator = &ComponentCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type Component.
func (v *ComponentCustomValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	c, ok := obj.(*castwarev1alpha1.Component)
	if !ok {
		return nil, fmt.Errorf("expected an Component object but got %T", obj)
	}

	// validate that config supports the component
	if !component.IsSupported(c.Spec.Component) {
		return nil, fmt.Errorf("component '%s' is not supported", c.Spec.Component)
	}

	// validate that cluster exists
	cluster := &castwarev1alpha1.Cluster{}
	err := v.client.Get(ctx, client.ObjectKey{Namespace: c.GetNamespace(), Name: c.Spec.Cluster}, cluster)
	if err != nil {
		return nil, fmt.Errorf("cluster '%s' does not exist", c.Spec.Cluster)
	}

	// validate that CAST.AI supports the component
	auth := auth.NewAuthFromCR(cluster)

	err = auth.LoadApiKey(ctx, v.client)
	if err != nil {
		return nil, fmt.Errorf("unable to load api key: %w", err)
	}

	restClient := castai.NewRestyClient(v.config, cluster.Spec.API.APIURL, auth, v.version.Version)

	_, err = castai.NewClient(logrus.New(), restClient).GetComponentByName(ctx, c.Spec.Component)
	if err != nil {
		if errors.Is(err, castai.ErrNotFound) {
			return nil, fmt.Errorf("component '%s' is not supported by CAST.AI", c.Spec.Component)
		}
		return nil, err
	}
	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type Component.
func (v *ComponentCustomValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	component, ok := newObj.(*castwarev1alpha1.Component)
	if !ok {
		return nil, fmt.Errorf("expected a Component object for the newObj but got %T", newObj)
	}
	componentlog.Info("Validation for Component upon update", "name", component.GetName())

	// TODO(user): fill in your validation logic upon object update.

	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type Component.
func (v *ComponentCustomValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	component, ok := obj.(*castwarev1alpha1.Component)
	if !ok {
		return nil, fmt.Errorf("expected a Component object but got %T", obj)
	}
	componentlog.Info("Validation for Component upon deletion", "name", component.GetName())

	// TODO(user): fill in your validation logic upon object deletion.

	return nil, nil
}
