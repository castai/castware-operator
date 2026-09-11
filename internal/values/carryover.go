package values

// This file carries over individual (standalone) component chart values into the
// umbrella chart's values layout during standalone→umbrella migration (CID-1047).
//
// The umbrella chart mounts each overlapping sub-component under the
// autoscaler.<alias> values key (confirmed in the castai-umbrella chart's
// values.yaml: autoscaler.castai-agent, autoscaler.castai-cluster-controller,
// autoscaler.castai-spot-handler). So a user's per-component customization that
// lived at the top level of the individual chart's values — e.g. the agent CR's
// spec.values.topologySpreadConstraints / spec.values.additionalEnv — must be
// placed under autoscaler.<alias> for the umbrella's subchart to pick it up.
//
// This mirrors the reference castctl migration's buildExtraValues +
// stripUmbrellaManagedKeys (castctl/internal/cli/cluster/migrate/command.go),
// adapted to the operator's data source: the operator reads the individual
// Component CRs' spec.values directly (user customizations only), whereas castctl
// reads the full coalesced helm release values (which include operator-injected
// credentials) and must strip them. Stripping is still applied here as
// defense-in-depth: a user may have set credential-like keys in their CR
// spec.values, and carrying those into autoscaler.<alias> re-trips the subchart
// template validation failures the umbrella-managed key set guards against.
//
// The carried values are merged UNDER the umbrella CR's own spec.values (see
// UmbrellaValues' merge order), so an explicit umbrella value still wins over a
// carried-over individual value.

import (
	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/utils"
)

// standaloneToUmbrellaSubchart maps an individual component name to the alias
// under which the umbrella chart mounts the corresponding subchart (under the
// autoscaler.* values key for eks/gke/aks profiles, autoscaler-anywhere.* for
// anywhere clusters). Mirrors castctl installer.StandaloneToUmbrellaSubchart
// for the three components the operator's migration gate covers.
var standaloneToUmbrellaSubchart = map[string]string{
	components.ComponentNameAgent:             "castai-agent",
	components.ComponentNameSpotHandler:       "castai-spot-handler",
	components.ComponentNameClusterController: "castai-cluster-controller",
}

// umbrellaManagedKeys is the set of keys the umbrella chart provisions itself
// (via global.castai.* and the castai-credentials Secret). They must not be
// carried over from individual values or the umbrella install fails validation
// in the subchart templates. Copied from castctl's umbrellaManagedKeys
// (command.go:679): subchart-local credentials wiring (apiKey/apiURL/provider/
// clusterID and their secret/configmap ref variants) plus the nested castai map
// (some subcharts read .Values.castai.* rather than .Values.global.castai.*).
var umbrellaManagedKeys = map[string]struct{}{
	"apiKey":                {},
	"apiKeySecretRef":       {},
	"clusterID":             {},
	"clusterIdSecretRef":    {},
	"clusterIdSecretKeyRef": {},
	"configMapRef":          {},
	"apiURL":                {},
	"provider":              {},
	// Nested credentials wiring: stripping the nested map entirely covers both
	// .Values.castai.* and .Values.global.castai.* layouts.
	"castai": {},
}

// parentKeyForProvider returns the umbrella values key under which per-subchart
// values are nested: "autoscaler-anywhere" for anywhere clusters, "autoscaler"
// otherwise (eks/gke/aks). Mirrors castctl buildExtraValues (command.go:644).
func parentKeyForProvider(provider string) string {
	if provider == "anywhere" {
		return "autoscaler-anywhere"
	}
	return "autoscaler"
}

// CarryOverIndividualValues wraps each present individual component CR's
// user-supplied spec.values into the umbrella chart's autoscaler.<alias> layout.
// It returns nil when no individual carries any user-relevant value, so the
// caller can skip merging an empty overrides entry (writing a nil subchart
// value makes Helm's value coalescer panic — see castctl command.go:658).
//
// The returned map is intended to be merged into the extraOverrides passed to
// UmbrellaValues, which merges it UNDER the umbrella CR's own spec.values so an
// explicit umbrella value still wins over a carried-over individual value.
func CarryOverIndividualValues(individuals map[string]*castwarev1alpha1.Component, provider string) map[string]any {
	if len(individuals) == 0 {
		return nil
	}

	subcharts := map[string]any{}
	for name, ind := range individuals {
		alias, ok := standaloneToUmbrellaSubchart[name]
		if !ok {
			// Unknown component — nothing to carry over.
			continue
		}
		if ind == nil || ind.Spec.Values == nil {
			continue
		}
		raw, err := utils.UnmarshalJSON(ind.Spec.Values)
		if err != nil {
			// A malformed individual values block should not abort the migration;
			// skip carrying it over. The umbrella install proceeds with defaults
			// for this subchart, matching the pre-carryover behavior.
			continue
		}
		stripped := stripUmbrellaManagedKeys(raw)
		if len(stripped) == 0 {
			// Only umbrella-managed keys were present — skip rather than write a
			// nil entry under autoscaler.<alias>.
			continue
		}
		subcharts[alias] = stripped
	}
	if len(subcharts) == 0 {
		return nil
	}

	return map[string]any{
		parentKeyForProvider(provider): subcharts,
	}
}

// stripUmbrellaManagedKeys returns a deep copy of m with umbrella-managed keys
// removed (recursively, so nested credentials maps are also dropped). Returns
// nil when nothing remains, so callers can skip writing an empty subchart
// entry. Mirrors castctl stripUmbrellaManagedKeys (command.go:698).
func stripUmbrellaManagedKeys(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if _, managed := umbrellaManagedKeys[k]; managed {
			continue
		}
		if nested, ok := v.(map[string]any); ok {
			out[k] = stripUmbrellaManagedKeys(nested)
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
