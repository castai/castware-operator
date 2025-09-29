package v1alpha1

import (
	"context"
	"errors"
	"fmt"

	"github.com/castai/castware-operator/internal/helm"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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

var errComponentReadonly = errors.New("readonly components cannot be modified")

// SetupComponentWebhookWithManager registers the webhook for Component in the manager.
func SetupComponentWebhookWithManager(mgr ctrl.Manager, log logrus.FieldLogger, chartLoader helm.ChartLoader) error {
	cfg, err := config.GetFromEnvironment()
	if err != nil {
		return fmt.Errorf("unable to load config from environment: %w", err)
	}

	return ctrl.NewWebhookManagedBy(mgr).For(&castwarev1alpha1.Component{}).
		WithValidator(&ComponentCustomValidator{client: mgr.GetClient(), config: cfg, log: log, chartLoader: chartLoader}).
		WithDefaulter(&ComponentCustomDefaulter{client: mgr.GetClient(), log: log}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-castware-cast-ai-v1alpha1-component,mutating=true,failurePolicy=fail,sideEffects=None,groups=castware.cast.ai,resources=components,verbs=create;update,versions=v1alpha1,name=mcomponent-v1alpha1.kb.io,admissionReviewVersions=v1

// ComponentCustomDefaulter struct is responsible for setting default values on the custom resource of the
// Kind Component when those are created or updated.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as it is used only for temporary operations and does not need to be deeply copied.
type ComponentCustomDefaulter struct {
	client client.Client
	log    logrus.FieldLogger
}

var _ webhook.CustomDefaulter = &ComponentCustomDefaulter{}

// Default implements webhook.CustomDefaulter so a webhook will be registered for the Kind Component.
func (d *ComponentCustomDefaulter) Default(ctx context.Context, obj runtime.Object) error {
	component, ok := obj.(*castwarev1alpha1.Component)
	if !ok {
		return fmt.Errorf("expected an Component object but got %T", obj)
	}
	log := d.log.WithField("component", component.GetName())

	// If version is empty we set it to the latest available.
	if component.Spec.Version == "" {
		if err := d.setLatestVersion(ctx, log, component); err != nil {
			return err
		}
	}

	return nil
}

func (d *ComponentCustomDefaulter) getCastaiClient(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component) (castai.CastAIClient, error) {
	cluster := &castwarev1alpha1.Cluster{}
	err := d.client.Get(ctx, types.NamespacedName{Namespace: component.Namespace, Name: component.Spec.Cluster}, cluster)
	if err != nil {
		return nil, err
	}

	auth := auth.NewAuth(component.Namespace, component.Spec.Cluster)
	if err := auth.LoadApiKey(ctx, d.client); err != nil {
		log.WithError(err).Error("Failed to load api key")
		return nil, err
	}
	cfg, err := config.GetFromEnvironment()
	if err != nil {
		log.WithError(err).Error("Failed to get config")
		return nil, err
	}
	rest := castai.NewRestyClient(cfg, cluster.Spec.API.APIURL, auth)

	client := castai.NewClient(log, rest)

	return client, nil
}

func (d *ComponentCustomDefaulter) setLatestVersion(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component) error {
	log.Info("Component version not found, installing latest version")

	castAiClient, err := d.getCastaiClient(ctx, log, component)
	if err != nil {
		log.WithError(err).Error("Failed to get castaiClient")
		return err
	}
	c, err := castAiClient.GetComponentByName(ctx, component.Spec.Component)
	if err != nil {
		log.WithError(err).Error("Failed to get component")
		return err
	}

	if c.LatestVersion == "" {
		log.Error("component latest version not returned by api")
		return errors.New("component latest version not returned by api")
	}

	component.Spec.Version = c.LatestVersion

	return nil
}

// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-castware-cast-ai-v1alpha1-component,mutating=false,failurePolicy=fail,sideEffects=None,groups=castware.cast.ai,resources=components,verbs=create;update,versions=v1alpha1,name=vcomponent-v1alpha1.kb.io,admissionReviewVersions=v1

// ComponentCustomValidator struct is responsible for validating the Component resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type ComponentCustomValidator struct {
	client      client.Client
	config      *config.Config
	chartLoader helm.ChartLoader
	log         logrus.FieldLogger
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
	castComponent, err := v.getComponentByName(ctx, cluster, c.Spec.Component)
	if err != nil {
		return nil, err
	}

	// check that the version exists
	if err := v.validateVersion(ctx, castComponent.HelmChart, c); err != nil {
		return nil, fmt.Errorf("failed to validate version %s for chart '%s': %w", c.Spec.Version, castComponent.HelmChart, err)
	}

	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type Component.
func (v *ComponentCustomValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	component, ok := newObj.(*castwarev1alpha1.Component)
	if !ok {
		return nil, fmt.Errorf("expected a Component object for the newObj but got %T", newObj)
	}
	oldComponent, ok := oldObj.(*castwarev1alpha1.Component)
	if !ok {
		return nil, fmt.Errorf("expected a Component object for the oldObj but got %T", newObj)
	}

	// Component is readonly, no changes allowed until the readonly flag is set to false.
	if component.Spec.Readonly && oldComponent.Spec.Readonly {
		// Component was already readonly, no changes allowed except for the readonly flag.
		return nil, errComponentReadonly
	} else if component.Spec.Readonly && !oldComponent.Spec.Readonly {
		// Component was not readonly, now it is, check that version didn't change.

		// Readonly changed, but for the sake of comparing with the old spec,
		// we need to make a copy of the new spec with readonly false.
		newSpecCopy := component.Spec.DeepCopy()
		newSpecCopy.Readonly = false

		diff := cmp.Diff(
			oldComponent.Spec,
			*newSpecCopy,
			// common & useful options:
			cmpopts.EquateEmpty(), // nil vs empty slice/map treated equal
		)
		if diff != "" {
			return nil, errComponentReadonly
		}

		return nil, nil
	}

	if oldComponent.Spec.Component != component.Spec.Component {
		return nil, fmt.Errorf("component name cannot be modified")
	}
	if oldComponent.Spec.Cluster != component.Spec.Cluster {
		return nil, fmt.Errorf("referenced cluster CRD cannot be modified")
	}

	cluster := &castwarev1alpha1.Cluster{}
	err := v.client.Get(ctx, client.ObjectKey{Namespace: component.GetNamespace(), Name: component.Spec.Cluster}, cluster)
	if err != nil {
		return nil, fmt.Errorf("cluster '%s' does not exist", component.Spec.Cluster)
	}

	castComponent, err := v.getComponentByName(ctx, cluster, component.Spec.Component)
	if err != nil {
		return nil, err
	}

	if err := v.validateVersion(ctx, castComponent.HelmChart, component); err != nil {
		return nil, fmt.Errorf("failed to validate version %s for chart '%s': %w", component.Spec.Version, castComponent.HelmChart, err)
	}

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

func (v *ComponentCustomValidator) validateVersion(ctx context.Context, helmChart string, component *castwarev1alpha1.Component) error {
	cluster := &castwarev1alpha1.Cluster{}
	err := v.client.Get(ctx, client.ObjectKey{Namespace: component.Namespace, Name: component.Spec.Cluster}, cluster)
	if err != nil {
		return err
	}
	_, err = v.chartLoader.Load(ctx, &helm.ChartSource{
		RepoURL: cluster.Spec.HelmRepoURL,
		Name:    helmChart,
		Version: component.Spec.Version,
	})
	return err
}
func (v *ComponentCustomValidator) getComponentByName(ctx context.Context, cluster *castwarev1alpha1.Cluster, componentName string) (*castai.Component, error) {
	auth := auth.NewAuthFromCR(cluster)

	err := auth.LoadApiKey(ctx, v.client)
	if err != nil {
		return nil, fmt.Errorf("unable to load api key: %w", err)
	}

	restClient := castai.NewRestyClient(v.config, cluster.Spec.API.APIURL, auth)

	resp, err := castai.NewClient(logrus.New(), restClient).GetComponentByName(ctx, componentName)
	if err != nil {
		if errors.Is(err, castai.ErrNotFound) {
			return nil, fmt.Errorf("component '%s' is not supported by CAST.AI", componentName)
		}
		return nil, err
	}

	return resp, nil
}
