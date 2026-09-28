// Package values builds Helm values for the castai-umbrella component.
//
// The builder translates the Cluster spec into global.castai.* values plus
// the active profile's sub-component defaults, then merges the user-supplied
// Component.Spec.Values on top. It is shared by the component reconciler and
// the migration controller, so both derive the umbrella values from one
// source of truth.
//
// The chart has two profile layouts, selected by the kent flag:
//
//   - autoscaler (default): sub-components under autoscaler.* (or
//     autoscaler-anywhere.* on anywhere clusters, threaded by the migration's
//     carry-over).
//   - kent: sub-components under kent.* (detected from the user values or
//     the caller's extraOverrides).
//
// Merge order, later wins:
//
//  1. operator defaults (global.castai.* and the profile's sub-components);
//  2. extraOverrides (the migration's derived mode and carry-over);
//  3. the user's Component.Spec.Values;
//  4. operator-managed force-disables: in kent mode the chart-upgrader cronjob
//     (kent.castai-chart-upgrader.enabled=false) is hard disabled — the
//     operator owns umbrella upgrades, and this is the one builder value that
//     deliberately beats an explicit user setting.
package values

import (
	"fmt"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/utils"
)

// UmbrellaValues builds the Helm values for the castai-umbrella component
// from the Cluster spec. Users can tune individual sub-components and
// override any builder-provided value — except the kent chart-upgrader
// force-disable (see the package comment).
//
// extraOverrides, when non-nil, is merged underneath the user values so a
// caller (the migration controller) can inject derived mode values that the
// user's own values can still override.
func UmbrellaValues(component *castwarev1alpha1.Component, cluster *castwarev1alpha1.Cluster, extraOverrides map[string]any) (map[string]any, error) {
	globalCastai := map[string]any{
		"apiURL":          cluster.Spec.API.APIURL,
		"provider":        cluster.Spec.Provider,
		"apiKeySecretRef": cluster.Spec.APIKeySecret,
	}

	if cluster.Spec.API.GrpcURL != "" {
		globalCastai["grpcURL"] = cluster.Spec.API.GrpcURL
	}

	kvisorCastai := map[string]any{}
	if cluster.Spec.API.KvisorGrpcURL != "" {
		kvisorCastai["grpcAddr"] = cluster.Spec.API.KvisorGrpcURL
	}

	// Inject the cluster ID directly and neutralize the kvisor sub-chart's
	// default clusterIdConfigMapKeyRef/clusterIdSecretKeyRef: the umbrella
	// chart forbids a direct clusterID next to those refs.
	if cluster.Spec.Cluster != nil && cluster.Spec.Cluster.ClusterID != "" {
		globalCastai["clusterID"] = cluster.Spec.Cluster.ClusterID
		kvisorCastai["clusterIdConfigMapKeyRef"] = map[string]any{"name": ""}
		kvisorCastai["clusterIdSecretKeyRef"] = map[string]any{"name": ""}
	}

	// Unmarshal the user values up front: the kent-mode detection needs them
	// alongside the caller's extraOverrides.
	userValues := map[string]any{}
	if component.Spec.Values != nil {
		parsed, err := utils.UnmarshalJSON(component.Spec.Values)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal umbrella values: %w", err)
		}
		userValues = parsed
	}

	// Kent mode: sub-components live under kent.* instead of autoscaler.*.
	// The builder only carries the kvisor wiring — the kent profile's own
	// defaults apply for the rest (castai-live enabled with CLM off,
	// metrics-server off, ...), so the autoscaler-only castai-live opt-out is
	// not emitted.
	kentMode := components.KentEnabled(userValues) || components.KentEnabled(extraOverrides)

	subchartParent := "autoscaler"
	profileValues := map[string]any{}
	if kentMode {
		subchartParent = components.UmbrellaProfileKent
	} else {
		profileValues[components.ComponentNameLive] = map[string]any{
			// castai-live is opt-in: the live chart requires extra cluster-scoped
			// RBAC (cluster-scope secrets read, PriorityClasses,
			// ValidatingAdmissionPolicies).
			"enabled": false,
		}
	}
	if len(kvisorCastai) > 0 {
		profileValues[components.ComponentNameKvisor] = map[string]any{
			"castai": kvisorCastai,
		}
	}

	values := map[string]any{
		"global": map[string]any{
			"castai": globalCastai,
		},
		subchartParent: profileValues,
	}

	if len(extraOverrides) > 0 {
		if err := utils.MergeMaps(values, extraOverrides); err != nil {
			return nil, fmt.Errorf("failed to merge umbrella derived overrides: %w", err)
		}
	}

	if len(userValues) > 0 {
		if err := utils.MergeMaps(values, userValues); err != nil {
			return nil, fmt.Errorf("failed to merge umbrella values: %w", err)
		}
	}

	// Operator-managed kent overrides go in LAST, after the user values: the
	// operator owns umbrella upgrades, so the chart-upgrader cronjob is hard
	// force-disabled — it would race the operator's own release management.
	if kentMode {
		forceDisable := map[string]any{
			components.UmbrellaProfileKent: map[string]any{
				components.ComponentNameChartUpgrader: map[string]any{
					"enabled": false,
				},
			},
		}
		if err := utils.MergeMaps(values, forceDisable); err != nil {
			return nil, fmt.Errorf("failed to merge umbrella kent overrides: %w", err)
		}
	}

	return values, nil
}
