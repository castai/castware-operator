// Package values builds Helm values for the castai-umbrella component.
//
// The umbrella component has a dedicated builder that translates the Cluster
// spec into the umbrella chart's global.castai.* values and then merges the
// user-supplied Component.Spec.Values on top. It is shared by the component
// reconciler (which owns the umbrella CR's ongoing reconcile) and the migration
// controller (which installs the umbrella once during standalone→umbrella
// migration), so both paths derive the umbrella values from one source of truth.
package values

import (
	"fmt"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/utils"
)

// UmbrellaValues builds the Helm values for the castai-umbrella component from
// the Cluster spec. It populates global.castai.{apiURL,grpcURL,provider,
// clusterID,apiKeySecretRef} and then deep-merges the user-supplied
// Component.Spec.Values on top, so users can disable or tune individual
// sub-components (e.g. autoscaler.castai-evictor.enabled=false) and override
// any builder-provided value.
//
// extraOverrides, when non-nil, is merged underneath the user values so a
// caller (the migration controller) can inject derived tag-mode values that the
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
	// chart forbids a direct clusterID next to those refs, and clearing the
	// refs lets kvisor consume the injected cluster ID immediately instead of
	// waiting for the agent to publish "castai-agent-metadata".
	if cluster.Spec.Cluster != nil && cluster.Spec.Cluster.ClusterID != "" {
		globalCastai["clusterID"] = cluster.Spec.Cluster.ClusterID
		kvisorCastai["clusterIdConfigMapKeyRef"] = map[string]any{"name": ""}
		kvisorCastai["clusterIdSecretKeyRef"] = map[string]any{"name": ""}
	}

	values := map[string]any{
		"global": map[string]any{
			"castai": globalCastai,
		},
	}

	if len(kvisorCastai) > 0 {
		values["autoscaler"] = map[string]any{
			"castai-kvisor": map[string]any{
				"castai": kvisorCastai,
			},
		}
	}

	// Caller-provided derived overrides (e.g. tag mode from present individuals)
	// go in first, under the user values, so user-supplied values still win.
	if len(extraOverrides) > 0 {
		if err := utils.MergeMaps(values, extraOverrides); err != nil {
			return nil, fmt.Errorf("failed to merge umbrella derived overrides: %w", err)
		}
	}

	// Merge the user-supplied values on top of the builder output so that
	// operator-managed global.castai.* fields act as defaults and the user can
	// override them or tune individual sub-components.
	if component.Spec.Values != nil {
		userValues, err := utils.UnmarshalJSON(component.Spec.Values)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal umbrella values: %w", err)
		}
		if err := utils.MergeMaps(values, userValues); err != nil {
			return nil, fmt.Errorf("failed to merge umbrella values: %w", err)
		}
	}

	return values, nil
}
