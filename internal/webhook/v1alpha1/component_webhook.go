package v1alpha1

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"helm.sh/helm/v3/pkg/release"

	"github.com/castai/castware-operator/internal/helm"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/sirupsen/logrus"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/castai"
	"github.com/castai/castware-operator/internal/castai/auth"
	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/config"
)

const (
	extendedPermissionsLabel = "castware.cast.ai/extended-permissions"
	extendedPermissionsValue = "true"
)

// nolint:unused
// log is for logging in this package.
var componentlog = logf.Log.WithName("component-resource")

var errComponentReadonly = errors.New("readonly components cannot be modified")

// SetupComponentWebhookWithManager registers the webhook for Component in the manager.
func SetupComponentWebhookWithManager(mgr ctrl.Manager, log logrus.FieldLogger, chartLoader helm.ChartLoader, helmClient helm.Client) error {
	cfg, err := config.GetFromEnvironment()
	if err != nil {
		return fmt.Errorf("unable to load config from environment: %w", err)
	}

	return ctrl.NewWebhookManagedBy(mgr).For(&castwarev1alpha1.Component{}).
		WithValidator(&ComponentCustomValidator{client: mgr.GetClient(), config: cfg, log: log, chartLoader: chartLoader, helmClient: helmClient}).
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

	// If version is empty we set it to the latest available.
	if component.Spec.Version == "" && !component.IsInitiliazedByTerraform() {
		if c.LatestVersion == "" {
			log.Error("component latest version not returned by api")
			return errors.New("component latest version not returned by api")
		}
		component.Spec.Version = c.LatestVersion
	}

	if component.Labels == nil {
		component.Labels = map[string]string{}
	}

	if component.Labels[castwarev1alpha1.LabelHelmChart] == "" {
		component.Labels[castwarev1alpha1.LabelHelmChart] = c.HelmChart
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

	client := castai.NewClient(log, cfg, rest)

	return client, nil
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
	helmClient  helm.Client
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
	if !components.IsSupported(c.Spec.Component) {
		return nil, fmt.Errorf("component '%s' is not supported", c.Spec.Component)
	}

	if components.RequiresExtendedPermissions(c.Spec.Component) {
		ok, err := v.checkExtendedPermissionsExist(ctx, c.Namespace)
		if err != nil {
			return nil, fmt.Errorf("failed to check extended permissions: %w", err)
		}
		if !ok {
			return nil, fmt.Errorf("component '%s' requires extended permissions, please run "+
				"`helm upgrade castware-operator -n castai-agent --set extendedPermissions=\"true\" --reuse-values castai-helm/castware-operator`",
				c.Spec.Component)
		}
	}

	// validate that cluster exists
	cluster := &castwarev1alpha1.Cluster{}
	err := v.client.Get(ctx, client.ObjectKey{Namespace: c.GetNamespace(), Name: c.Spec.Cluster}, cluster)
	if err != nil {
		return nil, fmt.Errorf("cluster '%s' does not exist", c.Spec.Cluster)
	}

	if err := v.validateTerraformMigration(c, cluster); err != nil {
		return nil, err
	}

	castAiClient, err := v.getCastAIClient(ctx, cluster)
	if err != nil {
		return nil, err
	}

	// validate that CAST.AI supports the component
	castComponent, err := v.getComponentByName(ctx, castAiClient, c.Spec.Component)
	if err != nil {
		return nil, err
	}

	// we skip if initialized by terraform and version is empty as validation will fail as version is empty
	if !(c.IsInitiliazedByTerraform() && c.Spec.Version == "") {
		// check that the version exists
		if err := v.validateVersion(ctx, castComponent.HelmChart, c); err != nil {
			return nil, fmt.Errorf("failed to validate version %s for chart '%s': %w", c.Spec.Version, castComponent.HelmChart, err)
		}
	}

	// check that the helm chart for the component to migrate is already installed.
	if c.Spec.Migration == castwarev1alpha1.ComponentMigrationHelm {
		if err := v.validateHelmRelease(c); err != nil {
			return nil, fmt.Errorf("failed to validate existing helm release: %w", err)
		}
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
	if component.Spec.Migration != "" && oldComponent.Spec.Migration != component.Spec.Migration {
		return nil, fmt.Errorf("components can be migrated only during resource creation")
	}

	cluster := &castwarev1alpha1.Cluster{}
	err := v.client.Get(ctx, client.ObjectKey{Namespace: component.GetNamespace(), Name: component.Spec.Cluster}, cluster)
	if err != nil {
		return nil, fmt.Errorf("cluster '%s' does not exist", component.Spec.Cluster)
	}

	castAiClient, err := v.getCastAIClient(ctx, cluster)
	if err != nil {
		return nil, err
	}

	castComponent, err := v.getComponentByName(ctx, castAiClient, component.Spec.Component)
	if err != nil {
		return nil, err
	}

	if err := v.validateVersion(ctx, castComponent.HelmChart, component); err != nil {
		return nil, fmt.Errorf("failed to validate version %s for chart '%s': %w", component.Spec.Version, castComponent.HelmChart, err)
	}

	// Validate component upgrade when version changes
	if oldComponent.Spec.Version != component.Spec.Version {
		if err := v.validateComponentUpgrade(ctx, castAiClient, cluster, component); err != nil {
			return nil, fmt.Errorf("component upgrade validation failed: %w", err)
		}
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
func (v *ComponentCustomValidator) getCastAIClient(ctx context.Context, cluster *castwarev1alpha1.Cluster) (castai.CastAIClient, error) {
	auth := auth.NewAuthFromCR(cluster)

	err := auth.LoadApiKey(ctx, v.client)
	if err != nil {
		return nil, fmt.Errorf("unable to load api key: %w", err)
	}

	restClient := castai.NewRestyClient(v.config, cluster.Spec.API.APIURL, auth)
	return castai.NewClient(v.log, v.config, restClient), nil
}

func (v *ComponentCustomValidator) getComponentByName(ctx context.Context, client castai.CastAIClient, componentName string) (*castai.Component, error) {
	resp, err := client.GetComponentByName(ctx, componentName)
	if err != nil {
		if errors.Is(err, castai.ErrNotFound) {
			return nil, fmt.Errorf("component '%s' is not supported by CAST.AI", componentName)
		}
		return nil, err
	}

	return resp, nil
}

// validateHelmRelease checks that the given component has a valid helm release installed.
func (v *ComponentCustomValidator) validateHelmRelease(component *castwarev1alpha1.Component) error {
	helmRelease, err := v.helmClient.GetRelease(helm.GetReleaseOptions{
		Namespace:   component.Namespace,
		ReleaseName: component.Spec.Component,
	})
	if err != nil {
		return err
	}
	switch helmRelease.Info.Status {
	case release.StatusUninstalled:
		return errors.New("component is not installed")
	case release.StatusUninstalling:
		return errors.New("component is uninstalling")
	case release.StatusFailed:
		return errors.New("helm chart is in failed status")

	}
	return nil
}

// validateComponentUpgrade validates if a component upgrade can proceed based on RBAC checksum changes.
func (v *ComponentCustomValidator) validateComponentUpgrade(ctx context.Context, castAiClient castai.CastAIClient, cluster *castwarev1alpha1.Cluster, component *castwarev1alpha1.Component) error {
	validation, err := castAiClient.ValidateComponentUpgrade(ctx, &castai.ValidateComponentUpgradeRequest{
		ClusterID:     cluster.Spec.Cluster.ClusterID,
		ComponentName: component.Spec.Component,
		TargetVersion: component.Spec.Version,
	})
	if err != nil {
		return fmt.Errorf("failed to validate component upgrade: %w", err)
	}

	if !validation.Allowed {
		blockReason := "Component upgrade blocked"
		if validation.BlockReason != "" {
			blockReason = validation.BlockReason
		}
		return errors.New(blockReason)
	}

	return nil
}

// checkExtendedPermissionsExist checks if RoleBinding and ClusterRoleBinding with
// 'castware.cast.ai/extended-permissions: "true"' label exist in the given namespace.
// Returns (roleBindingExists, clusterRoleBindingExists, error).
func (v *ComponentCustomValidator) checkExtendedPermissionsExist(ctx context.Context, namespace string) (bool, error) {

	// Check for RoleBindings with the extended-permissions label
	roleBindingList := &rbacv1.RoleBindingList{}
	if err := v.client.List(ctx, roleBindingList,
		client.InNamespace(namespace),
		client.MatchingLabels{extendedPermissionsLabel: extendedPermissionsValue},
	); err != nil {
		return false, fmt.Errorf("failed to list RoleBindings: %w", err)
	}

	// Check for ClusterRoleBindings with the extended-permissions label
	clusterRoleBindingList := &rbacv1.ClusterRoleBindingList{}
	if err := v.client.List(ctx, clusterRoleBindingList,
		client.MatchingLabels{extendedPermissionsLabel: extendedPermissionsValue},
	); err != nil {
		return false, fmt.Errorf("failed to list ClusterRoleBindings: %w", err)
	}

	roleBindingExists := len(roleBindingList.Items) > 0
	clusterRoleBindingExists := len(clusterRoleBindingList.Items) > 0

	return roleBindingExists && clusterRoleBindingExists, nil
}

// validateTerraformMigration validates the combination of terraform migration, cluster mode, and version
func (v *ComponentCustomValidator) validateTerraformMigration(component *castwarev1alpha1.Component, cluster *castwarev1alpha1.Cluster) error {
	// Only validate if migration is terraform
	if !component.IsInitiliazedByTerraform() || !cluster.Spec.Terraform {
		return nil
	}

	// If migration is terraform AND version is set AND cluster is in autoupgrade mode -> reject
	if component.Spec.Version != "" && cluster.Spec.MigrationMode == castwarev1alpha1.ClusterMigrationModeAutoupgrade {
		return fmt.Errorf(
			"cannot set explicit version in terraform with autoupgrade mode true: " +
				"in autoupgrade mode, the operator automatically detects and uses the latest version. " +
				"Either remove the version field from the Component CR or change the Cluster migration mode to 'write'",
		)
	}

	return nil
}
