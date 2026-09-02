package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"
	"helm.sh/helm/v3/pkg/storage/driver"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/castai"
	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/helm"
	"github.com/castai/castware-operator/internal/migrationgate"
	"github.com/castai/castware-operator/internal/rolebindings"
	"github.com/castai/castware-operator/internal/utils"
)

// This file contains the discovery / migration scan path: umbrella-first scan,
// per-component scan, version detection, the scan-side mutual-exclusivity gate,
// and the extended-permissions classification helpers shared by the scan and
// terraform sync loops.

func (r *ClusterReconciler) scanExistingComponents(ctx context.Context, castaiClient castai.CastAIClient, cluster *castwarev1alpha1.Cluster) (bool, error) {
	if cluster.Spec.Terraform {
		// Component CR will be handled separetly and we don't want to create a new one if we are in TF.
		return false, nil
	}

	log := r.Log.WithField("action", "scan-existing-components")

	// Umbrella-first check, atomically within a single pass: decide what to adopt
	// based on the umbrella release state before touching any per-component scan,
	// so a customer-installed umbrella's rendered Deployments are never misread as
	// unmanaged individual components and adopted into per-component CRs.
	umbrellaState, umbrellaReleaseName, err := r.resolveUmbrellaScanState(ctx, castaiClient, cluster)
	if err != nil {
		// A transient Mothership/helm failure must not wedge the scan; log and skip
		// this pass so the next reconcile retries.
		log.WithError(err).Warn("Failed to resolve umbrella scan state; skipping component scan")
		return false, nil
	}

	switch umbrellaState {
	case umbrellaInstalled:
		// Adopt one castai-umbrella CR from the existing helm release (migration:
		// "helm", which observes the release without Install/Upgrade — preserving
		// its revision history) and skip the per-component scan entirely.
		return r.adoptUmbrellaFromExistingRelease(ctx, cluster, umbrellaReleaseName, log)
	case hybridConfig:
		// Umbrella release and individual sub-component releases are both present
		// (hybrid configuration). Do not migrate; warn Mothership once.
		r.warnHybridConfig(ctx, castaiClient, cluster, umbrellaReleaseName, log)
		return false, nil
	}
	// umbrellaState == umbrellaAbsent: fall through to the per-component scan.

	var reconcileNeeded bool

	for _, component := range components.SupportedComponents {
		// Migrate phase1 components first
		if requiresExtendedPermissionsByName(ctx, r.Client, log, cluster.Namespace, component) {
			continue
		}
		mothershipComponent, err := castaiClient.GetComponentByName(ctx, component)
		if err != nil {
			return false, err
		}
		reconcileComponent, err := r.scanExistingComponent(ctx, castaiClient, cluster, mothershipComponent.ReleaseName, component)
		if err != nil {
			return false, err
		}
		reconcileNeeded = reconcileNeeded || reconcileComponent
	}

	if reconcileNeeded {
		return true, nil
	}

	extendedPermsExist, err := rolebindings.CheckExtendedPermissionsExist(ctx, r.Client, cluster.Namespace)
	if err != nil {
		return false, fmt.Errorf("failed to check extended permissions: %w", err)
	}

	if extendedPermsExist {
		for _, component := range components.SupportedComponents {
			// Migrate phase2 components
			if !requiresExtendedPermissionsByName(ctx, r.Client, log, cluster.Namespace, component) {
				continue
			}

			mothershipComponent, err := castaiClient.GetComponentByName(ctx, component)
			if err != nil {
				return false, err
			}

			// Scan for cluster controller
			reconcileComponent, err := r.scanExistingComponent(ctx, castaiClient, cluster, mothershipComponent.ReleaseName, component)
			if err != nil {
				return false, err
			}
			reconcileNeeded = reconcileNeeded || reconcileComponent
		}

		return reconcileNeeded, nil
	}

	return false, nil
}

// requiresExtendedPermissionsByName resolves whether a component requires
// extended permissions, reading the Component CR's Spec.Values when present so
// that a tag-aware component (e.g. an umbrella component with tags.readonly)
// can be satisfied by minimal permissions.
//
// If the Component CR is missing, its values are unreadable, or the lookup fails
// transiently, the function falls back to the safe default: unknown umbrella
// state is treated as extended-permissions-required, and other components use
// the name-only check. Such fallbacks are logged at warning level so they are
// visible and debuggable, since they change the permission classification.
func requiresExtendedPermissionsByName(ctx context.Context, c client.Client, log logrus.FieldLogger, namespace, name string) bool {
	component := &castwarev1alpha1.Component{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, component); err != nil {
		if apierrors.IsNotFound(err) {
			// Missing CR is an expected state (the component may not have been
			// created yet); fall back to the safe default without noise.
		} else {
			log.WithError(err).Warnf("Failed to get component %s for extended-permissions check; falling back to name-only classification", name)
		}
		return fallbackRequiresExtendedPermissions(name)
	}

	values, err := utils.UnmarshalJSON(component.Spec.Values)
	if err != nil {
		log.WithError(err).Warnf("Failed to unmarshal values for component %s; falling back to name-only classification", name)
		return fallbackRequiresExtendedPermissions(name)
	}
	return components.RequiresExtendedPermissionsForValues(name, values)
}

// requiresExtendedPermissions wraps RequiresExtendedPermissionsForValues for a
// Component that has already been fetched, used by the terraform sync loop.
func requiresExtendedPermissions(component *castwarev1alpha1.Component) bool {
	values, err := utils.UnmarshalJSON(component.Spec.Values)
	if err != nil {
		// Unreadable values default to extended-permissions-required for the
		// umbrella component (the safe choice); other components fall back to
		// the name-only check.
		if component.Spec.Component == components.ComponentNameUmbrella {
			return true
		}
		return components.RequiresExtendedPermissions(component.Spec.Component)
	}
	return components.RequiresExtendedPermissionsForValues(component.Spec.Component, values)
}

// fallbackRequiresExtendedPermissions returns the safe default classification
// when a component's tag-aware state cannot be determined: umbrella components
// default to extended-permissions-required, all others delegate to the
// name-only check.
func fallbackRequiresExtendedPermissions(name string) bool {
	if name == components.ComponentNameUmbrella {
		return true
	}
	return components.RequiresExtendedPermissions(name)
}

// umbrellaScanState is the outcome of probing the umbrella release before the
// per-component scan runs. It makes the umbrella-first decision atomic within a
// single scanExistingComponents pass.
type umbrellaScanState int

const (
	// umbrellaAbsent means no umbrella helm release is installed; the per-component
	// scan proceeds normally.
	umbrellaAbsent umbrellaScanState = iota
	// umbrellaInstalled means the umbrella helm release is installed and no
	// overlapping individual sub-component release is present. One castai-umbrella
	// CR is adopted and the per-component scan is skipped.
	umbrellaInstalled
	// hybridConfig means the umbrella release and at least one individual
	// sub-component release are both installed. The cluster is not migrated and a
	// warning is sent to Mothership.
	hybridConfig
)

// resolveUmbrellaScanState probes whether the umbrella helm release is installed
// and, when it is, whether any overlapping individual sub-component release is
// also present (the hybrid case). It resolves the umbrella presence first, and
// only pays the cost of resolving the sub-component release names when the
// umbrella is actually present — the common path (no umbrella) issues a single
// Mothership lookup and a single helm probe. The resolved umbrella release name
// is returned alongside the state so the adopt/warn callers need not re-query
// Mothership.
//
// The gate is fail-safe: a non-ErrReleaseNotFound helm error is treated as
// "present" (see migrationgate), so a helm outage surfaces as hybridConfig and
// blocks migration rather than risking a double install.
func (r *ClusterReconciler) resolveUmbrellaScanState(ctx context.Context, castaiClient castai.CastAIClient, cluster *castwarev1alpha1.Cluster) (umbrellaScanState, string, error) {
	umbrellaReleaseName, err := migrationgate.ResolveUmbrellaReleaseName(ctx, castaiClient)
	if err != nil {
		return 0, "", fmt.Errorf("resolve umbrella release name: %w", err)
	}
	if !migrationgate.UmbrellaInstalled(r.HelmClient, cluster.Namespace, umbrellaReleaseName) {
		return umbrellaAbsent, "", nil
	}

	// The umbrella is installed: check for a hybrid configuration by probing each
	// overlapping sub-component release. ResolveNames fails closed on unknown
	// Mothership errors so an outage cannot empty the release map and let the
	// operator proceed as if no individuals were present.
	names, err := migrationgate.ResolveNames(ctx, castaiClient)
	if err != nil {
		return 0, "", fmt.Errorf("resolve sub-component release names for hybrid check: %w", err)
	}
	present := migrationgate.InstalledSubcomponents(r.HelmClient, cluster.Namespace, names.SubcomponentReleases)
	if len(present) > 0 {
		return hybridConfig, umbrellaReleaseName, nil
	}
	return umbrellaInstalled, umbrellaReleaseName, nil
}

// adoptUmbrellaFromExistingRelease adopts a single castai-umbrella Component CR
// from an existing helm release, skipping the per-component scan entirely. The
// CR is created with migration: "helm", which routes the component reconciler
// through its observe-only path (it sets a Progressing "Migrating" condition and
// then watches the existing release via GetRelease) — so the release's revision
// history is preserved and no Install (which would reset revision to 1) or
// Upgrade is performed. Under MigrationMode == "read", newComponent sets
// Readonly=true so the component reconciler never writes to helm either.
//
// Idempotent: if a castai-umbrella CR already exists, adoption is a no-op.
func (r *ClusterReconciler) adoptUmbrellaFromExistingRelease(ctx context.Context, cluster *castwarev1alpha1.Cluster, releaseName string, log logrus.FieldLogger) (bool, error) {
	component := &castwarev1alpha1.Component{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: components.ComponentNameUmbrella}, component); err == nil {
		// Umbrella CR already exists - nothing to adopt.
		return false, nil
	} else if !apierrors.IsNotFound(err) {
		log.WithError(err).Error("Failed to get umbrella component")
		return false, err
	}

	helmRelease, err := r.HelmClient.GetRelease(helm.GetReleaseOptions{
		Namespace:   cluster.Namespace,
		ReleaseName: releaseName,
	})
	if err != nil {
		// The release vanished between the probe and the adopt (race) or helm is
		// transiently failing. Surface the error so the scan is retried next pass.
		return false, fmt.Errorf("get umbrella release for adoption: %w", err)
	}

	log.Info("Umbrella release found, creating castai-umbrella component resource")
	values, err := json.Marshal(helmRelease.Config)
	if err != nil {
		return false, err
	}
	component = newComponent(components.ComponentNameUmbrella, helmRelease.Chart.Metadata.Version, cluster)
	component.Spec.Values = &v1.JSON{Raw: values}
	component.Spec.Migration = castwarev1alpha1.ComponentMigrationHelm
	component.Spec.ReleaseName = releaseName
	if err := r.Create(ctx, component); err != nil {
		return false, err
	}
	log.Info("Umbrella component resource created")
	return true, nil
}

// warnHybridConfig reports a hybrid configuration (umbrella release and
// individual sub-component releases both installed) to Mothership once per
// operator process per cluster. Such clusters are not migrated: no Component CR
// is created. The only Mothership warning channel is RecordActionResult, which is
// component-scoped, so the warning is attached to castai-umbrella with
// Status_ACTION_REQUIRED. Deduped in-memory via lastHybridWarnClusterID so the
// ~30s reconcile cadence does not spam Mothership; a failed report is retried on
// the next scan because the dedup key is only advanced on success.
func (r *ClusterReconciler) warnHybridConfig(ctx context.Context, castaiClient castai.CastAIClient, cluster *castwarev1alpha1.Cluster, releaseName string, log logrus.FieldLogger) {
	clusterID := ""
	if cluster.Spec.Cluster != nil {
		clusterID = cluster.Spec.Cluster.ClusterID
	}
	if clusterID == "" {
		log.Warn("Hybrid configuration detected but cluster ID is not set; cannot report to Mothership yet")
		return
	}
	if r.lastHybridWarnClusterID == clusterID {
		return
	}

	err := castaiClient.RecordActionResult(ctx, clusterID, &castai.ComponentActionResult{
		Name:        components.ComponentNameUmbrella,
		Action:      castai.Action_INSTALL,
		Status:      castai.Status_ACTION_REQUIRED,
		ReleaseName: releaseName,
		Message: "hybrid configuration detected: the castai-umbrella helm release is installed alongside " +
			"individual component helm releases (castai-agent, spot-handler, cluster-controller). " +
			"Remove one side (uninstall the umbrella or the individual charts) so the operator can manage this cluster.",
	})
	if err != nil {
		log.WithError(err).Warn("Failed to report hybrid configuration to Mothership; will retry on next scan")
		return
	}
	r.lastHybridWarnClusterID = clusterID
	log.Info("Reported hybrid configuration to Mothership")
}

// scanExistingComponent Checks if helm release or deployment exist for a given component, and if they do but
// there is no corresponding component CR, it creates the component CR with migration parameter configured accordingly.
func (r *ClusterReconciler) scanExistingComponent(ctx context.Context, castaiClient castai.CastAIClient, cluster *castwarev1alpha1.Cluster, releaseName, componentName string) (reconcile bool, err error) {
	log := r.Log

	component := &castwarev1alpha1.Component{}
	err = r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: componentName}, component)
	if err == nil {
		// Component CR found - nothing to migrate.
		return false, nil
	}
	if !apierrors.IsNotFound(err) {
		log.WithError(err).Error("Failed to get component")
		return false, err
	}

	compVersion, err := r.detectComponentVersion(ctx, log, castaiClient, cluster, releaseName, componentName)
	if err != nil {
		return false, err
	}

	if compVersion == nil {
		return false, nil
	}

	// Umbrella / individual charts mutual-exclusivity gate. Before creating a
	// Component CR from a detected release, make sure we are not introducing the
	// conflicting installation side:
	//   - an individual component CR must not be created over an installed
	//     umbrella release;
	//   - an umbrella CR must not be created over installed individual
	//     component releases (unless spec.migrate is set, which a scan-created
	//     CR never sets, so this always blocks by default).
	if blocked, err := r.scanBlockedByMutualExclusivity(ctx, castaiClient, cluster, componentName); err != nil {
		// A transient Mothership/helm lookup failure must not wedge the whole
		// scan; log and skip creating this component so the next scan retries.
		log.WithError(err).Warnf("Failed to evaluate mutual-exclusivity for %s; skipping", componentName)
		return false, nil
	} else if blocked {
		log.Infof("Skipping creation of %s CR due to umbrella/individual mutual-exclusivity gate", componentName)
		return false, nil
	}

	log.Info("Version found for existing component, creating new component resource")
	values, err := json.Marshal(compVersion.ComponentConfig)
	if err != nil {
		return false, err
	}
	component = newComponent(componentName, compVersion.Version, cluster)
	component.Spec.Values = &v1.JSON{Raw: values}
	component.Spec.Migration = compVersion.MigrationMode
	err = r.Create(ctx, component)
	if err != nil {
		return false, err
	}
	log.Info("component resource created")
	return true, nil
}

// scanBlockedByMutualExclusivity reports whether creating a Component CR for
// componentName would violate the umbrella / individual charts mutual-
// exclusivity gate. It blocks when:
//   - componentName is an individual (non-umbrella) component and the umbrella
//     helm release is installed;
//   - componentName is the umbrella component and any individual component
//     release is installed (a scan-created umbrella CR never sets migrate, so
//     the default block-on-conflict applies).
func (r *ClusterReconciler) scanBlockedByMutualExclusivity(ctx context.Context, castaiClient castai.CastAIClient, cluster *castwarev1alpha1.Cluster, componentName string) (bool, error) {
	// The gate only applies to the umbrella and its overlapping sub-components;
	// other components render disjoint workloads and cannot conflict.
	if !migrationgate.IsUmbrellaOrSubcomponent(componentName) {
		return false, nil
	}

	// Umbrella side: refuse if any individual sub-component release is present.
	// This needs all sub-component release names.
	if componentName == components.ComponentNameUmbrella {
		names, err := migrationgate.ResolveNames(ctx, castaiClient)
		if err != nil {
			return false, fmt.Errorf("resolve release names: %w", err)
		}
		present := migrationgate.InstalledSubcomponents(r.HelmClient, cluster.Namespace, names.SubcomponentReleases)
		return len(present) > 0, nil
	}

	// Sub-component side: only the umbrella release name is needed to decide
	// whether the umbrella is installed. Resolving just the umbrella avoids
	// re-querying Mothership for the component being scanned (which
	// detectComponentVersion already queried).
	umbrellaReleaseName, err := migrationgate.ResolveUmbrellaReleaseName(ctx, castaiClient)
	if err != nil {
		return false, fmt.Errorf("resolve umbrella release name: %w", err)
	}
	return migrationgate.UmbrellaInstalled(r.HelmClient, cluster.Namespace, umbrellaReleaseName), nil
}

func (r *ClusterReconciler) detectComponentVersion(ctx context.Context, log logrus.FieldLogger, castaiClient castai.CastAIClient, cluster *castwarev1alpha1.Cluster, releaseName, componentName string) (*existingComponentVersion, error) {
	helmRelease, err := r.HelmClient.GetRelease(helm.GetReleaseOptions{
		Namespace:   cluster.Namespace,
		ReleaseName: releaseName,
	})

	if err == nil {
		return &existingComponentVersion{
			Version:         helmRelease.Chart.Metadata.Version,
			ComponentConfig: helmRelease.Config,
			MigrationMode:   castwarev1alpha1.ComponentMigrationHelm,
		}, nil
	}

	if !errors.Is(err, driver.ErrReleaseNotFound) {
		// If the error is not ErrReleaseNotFound, something is wrong with helm or component configuration
		log.WithError(err).Error("Failed to get helm release")
		return nil, err
	}

	if !components.IsSupported(componentName) {
		log.Debugf("Component %s not found, and YAML migration is not supported for this component", componentName)
		return nil, nil
	}

	component, err := castaiClient.GetComponentByName(ctx, componentName)
	if err != nil {
		return nil, err
	}

	switch componentName {
	case components.ComponentNameAgent, components.ComponentNameClusterController:
		var deploymentList appsv1.DeploymentList
		err = r.List(ctx, &deploymentList, &client.ListOptions{
			Namespace:     cluster.Namespace,
			LabelSelector: labels.SelectorFromSet(labels.Set{nameLabelKey: component.HelmChart}),
		})
		if err != nil {
			return nil, err
		}
		if len(deploymentList.Items) > 0 {
			versionLabel := deploymentList.Items[0].Labels["helm.sh/chart"]
			version := strings.TrimPrefix(versionLabel, fmt.Sprintf("%s-", component.HelmChart))
			if version == "" {
				log.Warnf("Failed to get version from deployment label, upgrading to latest version")
			}
			valueOverrides := map[string]interface{}{"replicaCount": deploymentList.Items[0].Spec.Replicas}

			return &existingComponentVersion{
				Version:         version,
				ComponentConfig: valueOverrides,
				MigrationMode:   castwarev1alpha1.ComponentMigrationYaml,
			}, nil
		}
	case components.ComponentNameSpotHandler:
		var daemonSetList appsv1.DaemonSetList
		err = r.List(ctx, &daemonSetList, &client.ListOptions{
			Namespace:     cluster.Namespace,
			LabelSelector: labels.SelectorFromSet(labels.Set{nameLabelKey: component.HelmChart}),
		})
		if err != nil {
			return nil, err
		}
		if len(daemonSetList.Items) > 0 {
			versionLabel := daemonSetList.Items[0].Labels["helm.sh/chart"]
			version := strings.TrimPrefix(versionLabel, fmt.Sprintf("%s-", component.HelmChart))
			if version == "" {
				log.Warnf("Failed to get version from daemonset label, upgrading to latest version")
			}

			valueOverrides := map[string]interface{}{}

			return &existingComponentVersion{
				Version:         version,
				ComponentConfig: valueOverrides,
				MigrationMode:   castwarev1alpha1.ComponentMigrationYaml,
			}, nil
		}
	}

	return nil, nil
}

func newComponent(componentName, version string, cluster *castwarev1alpha1.Cluster) *castwarev1alpha1.Component {
	component := &castwarev1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: cluster.Namespace,
			Name:      componentName,
		},
		Spec: castwarev1alpha1.ComponentSpec{
			Component: componentName,
			Cluster:   cluster.Name,
			Enabled:   true,
		},
	}
	component.Spec.Readonly = cluster.Spec.MigrationMode == castwarev1alpha1.ClusterMigrationModeRead
	// If the cluster is in autoupgrade mode, we don't specify the version in the component CR so that
	// the controller will upgrade the agent to the latest version.
	if cluster.Spec.MigrationMode != castwarev1alpha1.ClusterMigrationModeAutoupgrade {
		component.Spec.Version = version
	}
	return component
}
