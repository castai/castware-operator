// Package migrationgate implements the umbrella / individual charts mutual
// exclusivity gate and the umbrella-covered standalone release detection.
// The umbrella chart (castai-umbrella) renders the same workloads as the
// individual component charts (castai-agent, spot-handler,
// cluster-controller). Running both produces duplicate Deployments, duplicate
// CRDs and conflicting Helm ownership, so any ambiguity resolves to blocking
// rather than double-installing.
//
// It also detects standalone releases of the umbrella-covered charts the
// operator does not manage (UmbrellaCoveredCharts): the migration absorbs
// those releases — uninstalls them and re-renders them under the umbrella —
// so the detection returns the full release details (name, chart, version,
// user config) rather than a bare presence signal.
//
// The gate is fail-safe: a helm lookup that returns driver.ErrReleaseNotFound
// means "not present", while any other error (helm unreachable, permission
// denied, ...) is treated as "present/unknown" so that the gate blocks rather
// than risks a double install.
package migrationgate

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"helm.sh/helm/v3/pkg/storage/driver"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/castai/castware-operator/internal/castai"
	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/helm"
	"github.com/castai/castware-operator/internal/utils"
)

// Subcomponents are the individual component names whose charts overlap with
// the umbrella chart's rendered workloads. Order is stable so error messages
// and tests are deterministic.
var Subcomponents = []string{
	components.ComponentNameAgent,
	components.ComponentNameSpotHandler,
	components.ComponentNameClusterController,
}

// UmbrellaCoveredCharts are the umbrella-covered sub-chart names the operator
// does NOT support as individual components. A standalone release of one of
// these in the cluster namespace is invisible to Subcomponents-based probing
// yet collides with the umbrella install: Helm's TakeOwnership silently
// absorbs name-matching resources — leaving a ghost release the migration
// rollback would then delete — while name-mismatches render duplicate
// workloads. Standalone releases of these charts are detected by chart
// identity and absorbed (uninstalled + re-rendered under the umbrella) by the
// CID-1053 migration rather than merely blocked.
//
// Sourced from the umbrella chart's autoscaler profile subchart list minus the
// operator's SupportedComponents. kent-profile charts (castai-kentroller,
// castai-chart-upgrader, metrics-server) are excluded: the migration never
// enables the kent profile. The set must NOT include castai-agent,
// castai-spot-handler or castai-cluster-controller — those are operator
// components, detected via Mothership-resolved release names instead. Update
// when the umbrella chart's tag sets change.
var UmbrellaCoveredCharts = []string{
	components.ComponentNameKvisor,                     // readonly + full
	components.ComponentNameGPUMetricsExporter,         // readonly + full
	components.ComponentNameEvictor,                    // full
	components.ComponentNamePodMutator,                 // full
	components.ComponentNamePodPinner,                  // full
	components.ComponentNameLive,                       // full
	components.ComponentNameWorkloadAutoscaler,         // full
	components.ComponentNameWorkloadAutoscalerExporter, // full
}

// IsUmbrellaOrSubcomponent reports whether name is the umbrella component or
// one of the individual components whose chart overlaps with the umbrella. The
// mutual-exclusivity gate only applies to these; any other component renders
// disjoint workloads and cannot conflict.
func IsUmbrellaOrSubcomponent(name string) bool {
	if name == components.ComponentNameUmbrella {
		return true
	}
	for _, sub := range Subcomponents {
		if name == sub {
			return true
		}
	}
	return false
}

// Names holds the Mothership-resolved release names used to probe helm for the
// umbrella release and each overlapping sub-component.
type Names struct {
	UmbrellaReleaseName string
	// SubcomponentReleases maps each present sub-component's release name.
	SubcomponentReleases map[string]string
}

// ResolveUmbrellaReleaseName queries Mothership for the umbrella component's
// release name. It is a leaner alternative to ResolveNames for call sites that
// only need to probe whether the umbrella is installed. Falls back to the
// umbrella component name when Mothership returns an empty release name,
// matching getReleaseName semantics.
func ResolveUmbrellaReleaseName(ctx context.Context, c castai.CastAIClient) (string, error) {
	umbrella, err := c.GetComponentByName(ctx, components.ComponentNameUmbrella)
	if err != nil {
		return "", err
	}
	if umbrella.ReleaseName == "" {
		return components.ComponentNameUmbrella, nil
	}
	return umbrella.ReleaseName, nil
}

// ResolveNames queries Mothership for the umbrella and sub-component release
// names. The umbrella release name is required and its lookup failure is
// surfaced as an error.
//
// Sub-component resolution is fail-safe: a sub-component Mothership does not
// know about (castai.ErrNotFound) is genuinely absent and is skipped — there is
// no chart to probe. Any other error (timeout, 5xx, auth, ...) means "unknown",
// not "absent"; to keep the gate from silently passing when it cannot see the
// sub-component releases, such errors are recorded and surfaced as an error if
// resolution ends with no resolvable sub-components. This prevents an
// unreachable-Mothership outage from emptying the release map and letting an
// umbrella install proceed over individual releases that are actually present.
func ResolveNames(ctx context.Context, c castai.CastAIClient) (*Names, error) {
	umbrella, err := c.GetComponentByName(ctx, components.ComponentNameUmbrella)
	if err != nil {
		return nil, err
	}
	if umbrella.ReleaseName == "" {
		// Fall back to the component name, matching getReleaseName semantics.
		umbrella.ReleaseName = components.ComponentNameUmbrella
	}

	names := &Names{
		UmbrellaReleaseName:  umbrella.ReleaseName,
		SubcomponentReleases: map[string]string{},
	}
	var resolveErrs []error
	for _, sub := range Subcomponents {
		mc, err := c.GetComponentByName(ctx, sub)
		if err != nil {
			if errors.Is(err, castai.ErrNotFound) {
				// Mothership has no record of this component: there is no chart
				// to probe, so skipping is safe and does not weaken the gate.
				continue
			}
			// Unknown resolution failure: record it so resolution can fail
			// rather than return an empty map that would let the gate pass.
			resolveErrs = append(resolveErrs, fmt.Errorf("resolve %s: %w", sub, err))
			continue
		}
		if mc.ReleaseName == "" {
			mc.ReleaseName = sub
		}
		names.SubcomponentReleases[sub] = mc.ReleaseName
	}

	// If at least one sub-component resolved, the helm probe has something to
	// inspect and the gate can block on a present release. If none resolved and
	// we hit unknown errors, fail closed: the gate must not pass when it cannot
	// see the individual releases it is meant to guard against.
	if len(names.SubcomponentReleases) == 0 && len(resolveErrs) > 0 {
		return nil, fmt.Errorf("resolve sub-component release names: %w", errors.Join(resolveErrs...))
	}
	return names, nil
}

// UmbrellaInstalled reports whether the umbrella helm release is present in the
// given namespace. Fail-safe: any non-not-found error is treated as present so
// the gate blocks rather than risks a double install.
func UmbrellaInstalled(hc helm.Client, namespace, umbrellaReleaseName string) bool {
	if umbrellaReleaseName == "" {
		return false
	}
	return releasePresent(hc, namespace, umbrellaReleaseName)
}

// InstalledSubcomponents returns the sub-component names whose helm releases
// are currently present in the given namespace, in Subcomponents order.
// Fail-safe: a sub-component whose lookup returns a non-not-found error is
// treated as present, again so the gate blocks.
func InstalledSubcomponents(hc helm.Client, namespace string, releases map[string]string) []string {
	var present []string
	for _, sub := range Subcomponents {
		releaseName, ok := releases[sub]
		if !ok || releaseName == "" {
			continue
		}
		if releasePresent(hc, namespace, releaseName) {
			present = append(present, sub)
		}
	}
	return present
}

// releasePresent reports whether a helm release exists, treating
// driver.ErrReleaseNotFound as absent and any other error as present
// (fail-safe toward blocking).
func releasePresent(hc helm.Client, namespace, releaseName string) bool {
	_, err := hc.GetRelease(helm.GetReleaseOptions{
		Namespace:   namespace,
		ReleaseName: releaseName,
	})
	if err == nil {
		return true
	}
	return !errors.Is(err, driver.ErrReleaseNotFound)
}

// ValidateInstallPermissions asks Mothership whether the umbrella install for
// the migration is permitted (components:validateInstallation). The umbrella
// chart renders a broader RBAC surface than the phase1/phase2 individual
// charts, so the operator's service account may be under-permissioned for it.
//
// Returns the validation response so the caller can surface the block reason on
// the CR. A transport/API error is returned as-is (transient, retry-worthy);
// a denial arrives as Allowed=false + BlockReason, not as an error.
func ValidateInstallPermissions(ctx context.Context, c castai.CastAIClient, clusterID, componentName, targetVersion string, componentParams map[string]any) (*castai.ValidateComponentInstallResponse, error) {
	return c.ValidateComponentInstall(ctx, &castai.ValidateComponentInstallRequest{
		ClusterID:       clusterID,
		ComponentName:   componentName,
		TargetVersion:   targetVersion,
		ComponentParams: componentParams,
	})
}

// ValidateUmbrellaInstallPermissions runs the Mothership permission gate for
// an umbrella install: it sends the umbrella CR's user-supplied install values
// (spec.values) as component_params so the server can compare the umbrella's
// required RBAC surface against the operator's installed conditions.
//
// The payload is deliberately the full user-supplied values — not the
// component_params whitelist params.ExtractComponentParams builds from an
// installed release — because the RBAC surface is not determined by tags
// alone (an explicitly enabled cluster-controller sub-component forces the
// broader surface even with tags.readonly=true, and spot-handler presence is
// values-driven, not tag-driven), and pre-install there is no release to
// extract from anyway.
//
// derivedTag, when non-empty, is the umbrella tag mode the migration derived
// from the present standalone set (components.MinimalCoveringTag). It is
// folded into the payload as tags.<derivedTag>=true so the gate validates the
// EFFECTIVE install surface — exactly what values.UmbrellaValues will render —
// rather than the user-only one: a migration deriving a broader tag must be
// permission-checked against that broader surface before anything is
// uninstalled. The merge mirrors UmbrellaValues' extraOverrides slot (user
// spec.values on top), so an explicit user tag choice remains in effect: the
// derived tag adds its own key under tags, it never replaces the user's. An
// empty derivedTag (the component controller's call site, or a migration with
// nothing to derive) leaves the user values untouched.
//
// The response's BlockReason is normalized to "missing permissions" when the
// server returns none, so callers surface one consistent message.
// Transport/API errors are returned as-is so each caller wraps them with its
// own context. A nil userValues (or empty raw) is sent as no params.
func ValidateUmbrellaInstallPermissions(ctx context.Context, c castai.CastAIClient, clusterID, targetVersion, derivedTag string, userValues *apiextensionsv1.JSON) (*castai.ValidateComponentInstallResponse, error) {
	componentParams, err := utils.UnmarshalJSON(userValues)
	if err != nil {
		return nil, fmt.Errorf("unmarshal umbrella values for permission gate: %w", err)
	}

	if derivedTag != "" {
		derived := map[string]any{"tags": map[string]any{derivedTag: true}}
		if err := utils.MergeMaps(componentParams, derived); err != nil {
			return nil, fmt.Errorf("merge derived tag %q into permission gate values: %w", derivedTag, err)
		}
	}

	validation, err := ValidateInstallPermissions(ctx, c, clusterID, components.ComponentNameUmbrella, targetVersion, componentParams)
	if err != nil {
		return nil, err
	}

	if !validation.Allowed && validation.BlockReason == "" {
		validation.BlockReason = "missing permissions"
	}
	return validation, nil
}

// CoveredStandaloneRelease describes a present standalone helm release whose
// chart is one of UmbrellaCoveredCharts.
type CoveredStandaloneRelease struct {
	ReleaseName  string         // helm release name (release may be installed under any name)
	ChartName    string         // canonical umbrella sub-chart (chart identity)
	ChartVersion string         // resolved chart version
	Config       map[string]any // user-supplied release values (release config)
}

// InstalledCoveredStandaloneReleases returns the standalone releases present
// in the namespace whose chart is one of UmbrellaCoveredCharts, matched by
// chart identity (rel.Chart.Metadata.Name), not by release name. Result is
// deterministic: sorted by ReleaseName. Fail-safe: a helm listing error is
// returned so callers block rather than proceed on unknown state. Releases
// with nil Chart or nil Chart.Metadata are skipped defensively.
func InstalledCoveredStandaloneReleases(hc helm.Client, namespace string) ([]CoveredStandaloneRelease, error) {
	rels, err := hc.ListReleases(helm.ListReleasesOptions{Namespace: namespace})
	if err != nil {
		return nil, err
	}
	coveredSet := make(map[string]bool, len(UmbrellaCoveredCharts))
	for _, chart := range UmbrellaCoveredCharts {
		coveredSet[chart] = true
	}
	var matched []CoveredStandaloneRelease
	for _, rel := range rels {
		if rel == nil || rel.Chart == nil || rel.Chart.Metadata == nil {
			continue
		}
		if !coveredSet[rel.Chart.Metadata.Name] {
			continue
		}
		matched = append(matched, CoveredStandaloneRelease{
			ReleaseName:  rel.Name,
			ChartName:    rel.Chart.Metadata.Name,
			ChartVersion: rel.Chart.Metadata.Version,
			Config:       rel.Config, // may be nil: kept as-is, not substituted with an empty map
		})
	}
	// A single chart may have several standalone releases; all are returned.
	sort.Slice(matched, func(i, j int) bool {
		return matched[i].ReleaseName < matched[j].ReleaseName
	})
	return matched, nil
}
