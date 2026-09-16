package params

import (
	"context"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/sirupsen/logrus"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/release"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/rolebindings"
)

// Labels used for the umbrella live-workload version detection. Kept in sync
// with Mothership's umbrella detection for non-operator-managed clusters
// (installedcomponents service): sub-component workloads are identified by
// app.kubernetes.io/name and, under the umbrella chart, carry the umbrella
// chart version in helm.sh/chart — the actual component version is the
// app.kubernetes.io/version label.
const (
	labelAppName    = "app.kubernetes.io/name"
	labelAppVersion = "app.kubernetes.io/version"
	// helmReleaseNameAnnotation is set by Helm on every resource it owns.
	helmReleaseNameAnnotation = "meta.helm.sh/release-name"
)

// ExtractComponentParams extracts component-specific parameters to send to Mothership.
// Only extracts specific, non-sensitive parameters (not all Helm values).
func ExtractComponentParams(
	ctx context.Context,
	log logrus.FieldLogger,
	componentName string,
	helmRelease *release.Release,
	k8sClient client.Client,
	namespace string,
) map[string]interface{} {

	switch componentName {
	case components.ComponentNameOperator:
		return extractOperatorParams(ctx, log, k8sClient, namespace)
	case components.ComponentNameSpotHandler:
		return extractSpotHandlerParams(helmRelease)
	case components.ComponentNameClusterController:
		return extractClusterControllerParams(helmRelease)
	case components.ComponentNameUmbrella:
		return extractUmbrellaParams(ctx, log, helmRelease, k8sClient, namespace)
	default:
		return make(map[string]interface{})
	}
}

func extractOperatorParams(ctx context.Context, log logrus.FieldLogger, k8sClient client.Client, namespace string) map[string]interface{} {
	params := make(map[string]interface{})

	extendedPerms, err := rolebindings.CheckExtendedPermissionsExist(ctx, k8sClient, namespace)
	if err != nil {
		log.WithError(err).Warn("Failed to check extended permissions, continuing without this parameter")
		return params
	}

	params["extendedPermissions"] = extendedPerms
	return params
}

func extractSpotHandlerParams(helmRelease *release.Release) map[string]interface{} {
	params := make(map[string]interface{})

	if helmRelease == nil {
		return params
	}

	var paramsLookup []map[string]any

	// first check for defaults then overrides
	if helmRelease.Chart != nil && helmRelease.Chart.Values != nil {
		paramsLookup = append(paramsLookup, helmRelease.Chart.Values)
	}

	if helmRelease.Config != nil {
		paramsLookup = append(paramsLookup, helmRelease.Config)
	}

	for _, helmParams := range paramsLookup {
		if phase2, ok := helmParams["phase2Permissions"].(bool); ok {
			params["phase2Permissions"] = phase2
		}
	}

	return params
}

func extractClusterControllerParams(helmRelease *release.Release) map[string]interface{} {
	params := make(map[string]interface{})
	if helmRelease == nil {
		return params
	}

	var paramsLookup []map[string]any

	// first check for defaults then overrides
	if helmRelease.Chart != nil && helmRelease.Chart.Values != nil {
		paramsLookup = append(paramsLookup, helmRelease.Chart.Values)
	}

	if helmRelease.Config != nil {
		paramsLookup = append(paramsLookup, helmRelease.Config)
	}

	for _, helmParams := range paramsLookup {
		if autoscaling, ok := helmParams["autoscaling"].(map[string]interface{}); ok {
			autoscalingParams := make(map[string]interface{})

			if enabled, ok := autoscaling["enabled"].(bool); ok {
				autoscalingParams["enabled"] = enabled
			}

			if len(autoscalingParams) > 0 {
				params["autoscaling"] = autoscalingParams
			}
		}

		if workloadAutoscaling, ok := helmParams["workloadAutoscaling"].(map[string]interface{}); ok {
			workloadAutoscalingParams := make(map[string]interface{})

			if enabled, ok := workloadAutoscaling["enabled"].(bool); ok {
				workloadAutoscalingParams["enabled"] = enabled
			}

			if len(workloadAutoscalingParams) > 0 {
				params["workloadAutoscaling"] = workloadAutoscalingParams
			}
		}
	}

	return params
}

// umbrellaSubcomponent is one inventory entry: a sub-chart of the umbrella
// release with its resolved enablement and running version. Field names align
// with the tags map Mothership already flattens for umbrella conditions.
type umbrellaSubcomponent struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Enabled bool   `json:"enabled"`
}

// extractUmbrellaParams reports the umbrella's effective configuration to
// Mothership as two views plus the legacy flat flags:
//
//   - tags: the active tags.* mode(s), as resolved for this release. The shape
//     matches what Mothership already consumes for umbrella clusters: nested
//     tags maps are flattened to conditions (tags.node-autoscaler=true) and
//     false values are the neutral opt-out.
//   - inventory: per-sub-component entries (name, running version, enabled).
//     The operator sets no per-component overrides, so the enabled set is
//     whatever the active tag mode defines plus any user overrides — the
//     inventory, not the operator's config, is the source of truth for what
//     runs. Versions prefer the live workloads (actual) over the release's
//     resolved sub-chart versions (requested), so a stale release record
//     cannot report a version the cluster no longer runs.
//   - flat flags (extendedPermissions, phase2Permissions, autoscaling.enabled,
//     workloadAutoscaling.enabled): kept alongside for existing consumers.
func extractUmbrellaParams(ctx context.Context, log logrus.FieldLogger, helmRelease *release.Release, k8sClient client.Client, namespace string) map[string]interface{} {
	params := make(map[string]interface{})
	if helmRelease == nil || helmRelease.Chart == nil || helmRelease.Chart.Metadata == nil {
		return params
	}

	// Requested view: the chart defaults coalesced with the release's
	// user-supplied values — the values Helm actually resolved for this
	// release.
	coalesced := map[string]interface{}{}
	if c, err := chartutil.CoalesceValues(helmRelease.Chart, helmRelease.Config); err != nil {
		log.WithError(err).Warn("Failed to coalesce umbrella values; falling back to the release config")
		if helmRelease.Config != nil {
			coalesced = helmRelease.Config
		}
	} else {
		coalesced = c
	}

	// Active tag modes, sanitized to bools only: Mothership flattens tags
	// into umbrella conditions, so non-bool entries (strings, nil, nested
	// maps) are dropped to keep the map[string]bool contract.
	if raw, ok := coalesced["tags"].(map[string]interface{}); ok {
		tags := make(map[string]bool, len(raw))
		for name, value := range raw {
			if b, ok := value.(bool); ok {
				tags[name] = b
			}
		}
		if len(tags) > 0 {
			params["tags"] = tags
		}
	}

	// Actual view: live workload versions of the umbrella's sub-components.
	live := liveUmbrellaWorkloadVersions(ctx, log, k8sClient, namespace, helmRelease.Name)

	// Per-sub-component inventory from the release's dependency tree.
	params["inventory"] = umbrellaInventory(helmRelease.Chart, coalesced, live)

	// Flat flags for existing consumers, read from the coalesced (nested)
	// sub-chart values.
	if v, ok := lookupBool(coalesced, "autoscaler.castai-spot-handler.phase2Permissions"); ok {
		params["phase2Permissions"] = v
	}
	if v, ok := lookupBool(coalesced, "autoscaler.castai-cluster-controller.autoscaling.enabled"); ok {
		params["autoscaling"] = map[string]interface{}{"enabled": v}
	}
	if v, ok := lookupBool(coalesced, "autoscaler.castai-cluster-controller.workloadAutoscaling.enabled"); ok {
		params["workloadAutoscaling"] = map[string]interface{}{"enabled": v}
	}
	// Same derivation the admission webhook uses, so the reported flag cannot
	// disagree with what the operator itself would gate on.
	params["extendedPermissions"] = components.RequiresExtendedPermissionsForValues(components.ComponentNameUmbrella, coalesced)

	return params
}

// umbrellaInventory walks the release chart's dependency tree and resolves, per
// sub-chart, the enablement and running version. A sub-chart can be declared
// under several profiles (autoscaler, kent, ...): it runs if any enabled
// parent's declaration resolves enabled, and the reported version prefers an
// enabled declaration's resolution. Output is sorted by name for determinism.
func umbrellaInventory(root *chart.Chart, values map[string]interface{}, live map[string]string) []umbrellaSubcomponent {
	entries := map[string]*umbrellaSubcomponent{}

	var walk func(ch *chart.Chart, parentPath string, parentEnabled bool)
	walk = func(ch *chart.Chart, parentPath string, parentEnabled bool) {
		if ch == nil || ch.Metadata == nil {
			return
		}
		loaded := map[string]*chart.Chart{}
		for _, sub := range ch.Dependencies() {
			if sub != nil && sub.Metadata != nil {
				loaded[sub.Metadata.Name] = sub
			}
		}
		for _, dep := range ch.Metadata.Dependencies {
			if dep == nil || dep.Name == "" {
				continue
			}
			key := dep.Name
			if dep.Alias != "" {
				key = dep.Alias
			}
			depPath := key
			if parentPath != "" {
				depPath = parentPath + "." + key
			}
			enabled := parentEnabled && subchartEnabled(dep, parentPath, depPath, values)

			// Requested (release-resolved) sub-chart version; the live
			// workload version wins when one is running.
			version := ""
			if sub := loaded[dep.Name]; sub != nil && sub.Metadata != nil {
				version = sub.Metadata.Version
			}
			if v, ok := live[dep.Name]; ok {
				version = v
			}

			entry, seen := entries[dep.Name]
			if !seen {
				entries[dep.Name] = &umbrellaSubcomponent{Name: dep.Name, Version: version, Enabled: enabled}
			} else if enabled {
				// Prefer the enabled declaration's view: a disabled entry may
				// carry the version of a profile that is not running.
				entry.Enabled = true
				if version != "" {
					entry.Version = version
				}
			}
			if sub := loaded[dep.Name]; sub != nil {
				walk(sub, depPath, enabled)
			}
		}
	}
	walk(root, "", true)

	inventory := make([]umbrellaSubcomponent, 0, len(entries))
	for _, entry := range entries {
		inventory = append(inventory, *entry)
	}
	sort.Slice(inventory, func(i, j int) bool { return inventory[i].Name < inventory[j].Name })
	return inventory
}

// subchartEnabled resolves whether the umbrella chart renders the sub-chart
// declared at depPath, replicating the chart's own helper precedence: an
// explicit enabled value wins (false or true — either direction beats the tag
// mode), then the declaration's condition, then the active tag mode, then a
// tag-less declaration defaults to enabled (Helm semantics). The parent's
// enablement is applied by the caller.
func subchartEnabled(dep *chart.Dependency, parentPath, depPath string, values map[string]interface{}) bool {
	// Explicit <path>.enabled — the umbrella helper convention
	// (autoscaler.castai-cluster-controller.enabled) and the common user
	// override for any sub-chart.
	if b, ok := lookupBool(values, depPath+".enabled"); ok {
		return b
	}
	// The declaration's condition, resolved against the parent's values scope
	// (Helm semantics), e.g. condition "kent.enabled" on the root's kent
	// dependency reads the top-level kent.enabled default.
	if dep.Condition != "" {
		condPath := dep.Condition
		if parentPath != "" {
			condPath = parentPath + "." + dep.Condition
		}
		if b, ok := lookupBool(values, condPath); ok {
			return b
		}
	}
	// The active tag mode: enabled when any of the declaration's tags is set
	// to true; a tag-bearing declaration with no active tag is disabled.
	if len(dep.Tags) > 0 {
		for _, tag := range dep.Tags {
			if b, ok := lookupBool(values, "tags."+tag); ok && b {
				return true
			}
		}
		return false
	}
	return true
}

// liveUmbrellaWorkloadVersions returns the running version of every live
// workload owned by the given umbrella helm release, keyed by the workload's
// app.kubernetes.io/name label (the sub-chart's name). This mirrors
// Mothership's umbrella detection for non-operator-managed clusters: under the
// umbrella chart, the helm.sh/chart label carries the umbrella's version, so
// the component version is read from app.kubernetes.io/version. Only workloads
// owned by the umbrella release are considered, so a standalone release of a
// sub-chart in the same namespace does not shadow it.
//
// When several workloads owned by the release share an app name but run
// different versions (a mid-rollout snapshot or a duplicate workload), the
// highest semantic version wins and the conflict is logged.
func liveUmbrellaWorkloadVersions(ctx context.Context, log logrus.FieldLogger, k8sClient client.Client, namespace, releaseName string) map[string]string {
	versions := map[string]string{}
	if k8sClient == nil || releaseName == "" {
		return versions
	}

	collect := func(obj metav1.Object) {
		annos := obj.GetAnnotations()
		if annos[helmReleaseNameAnnotation] != releaseName {
			return
		}
		labels := obj.GetLabels()
		name := labels[labelAppName]
		version := strings.TrimPrefix(labels[labelAppVersion], "v")
		if name == "" || version == "" {
			return
		}
		existing, seen := versions[name]
		if !seen || existing == version {
			versions[name] = version
			return
		}
		// Same app name, different running versions: a mid-rollout snapshot
		// or a duplicate workload. Keep the deterministic winner and surface
		// the conflict so the duplicate can be cleaned up.
		if newerVersion(version, existing) {
			versions[name] = version
		}
		log.WithFields(logrus.Fields{
			"app":          name,
			"version":      version,
			"kept_version": versions[name],
		}).Warn("Umbrella workloads share an app name but run different versions")
	}

	// One list call per kind; a kind with no matching workloads simply
	// returns an empty list. A list failure logs and leaves that kind's
	// workloads unreported (requested versions win).
	workloadKinds := []struct {
		name string
		list client.ObjectList
	}{
		{"deployments", &appsv1.DeploymentList{}},
		{"daemonsets", &appsv1.DaemonSetList{}},
		{"statefulsets", &appsv1.StatefulSetList{}},
	}
	for _, kind := range workloadKinds {
		if err := k8sClient.List(ctx, kind.list, client.InNamespace(namespace)); err != nil {
			log.WithError(err).Warnf("Failed to list %s for umbrella inventory; using requested versions", kind.name)
			continue
		}
		if err := meta.EachListItem(kind.list, func(obj runtime.Object) error {
			if m, ok := obj.(metav1.Object); ok {
				collect(m)
			}
			return nil
		}); err != nil {
			log.WithError(err).Warnf("Failed to walk %s for umbrella inventory; using requested versions", kind.name)
		}
	}

	return versions
}

// newerVersion reports whether version should displace other as the
// reported running version
func newerVersion(version, other string) bool {
	v, err := semver.NewVersion(version)
	if err != nil {
		return false
	}
	o, err := semver.NewVersion(other)
	if err != nil {
		return true
	}
	return v.GreaterThan(o)
}

// lookupBool resolves a dot-separated path in nested values maps to a bool. A
// missing path or a non-bool value reports ok=false.
func lookupBool(values map[string]interface{}, path string) (bool, bool) {
	var current interface{} = values
	for _, segment := range strings.Split(path, ".") {
		m, ok := current.(map[string]interface{})
		if !ok {
			return false, false
		}
		current, ok = m[segment]
		if !ok {
			return false, false
		}
	}
	b, ok := current.(bool)
	return b, ok
}
