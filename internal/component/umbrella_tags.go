// Package components defines the CAST AI components this operator can manage
// and the tag → sub-component mapping used by the castai-umbrella helm chart.
package components

// Umbrella sub-component name constants.
//
// The values are the SUB-CHART (chart) names as they appear in the
// castai-umbrella chart's autoscaler sub-chart Chart.yaml dependencies, not
// necessarily the operator's own component names (e.g. the operator calls the
// spot handler "spot-handler" while its umbrella sub-chart is
// "castai-spot-handler").
//
// ComponentNameAgent, ComponentNameSpotHandler and
// ComponentNameClusterController already exist in component.go and are
// intentionally not redefined here.
const (
	ComponentNameKvisor                     = "castai-kvisor"
	ComponentNameEvictor                    = "castai-evictor"
	ComponentNamePodMutator                 = "castai-pod-mutator"
	ComponentNamePodPinner                  = "castai-pod-pinner"
	ComponentNameLive                       = "castai-live"
	ComponentNameWorkloadAutoscaler         = "castai-workload-autoscaler"
	ComponentNameWorkloadAutoscalerExporter = "castai-workload-autoscaler-exporter"
)

// Umbrella tag name constants: the mutually exclusive autoscaler profile tags
// accepted by the castai-umbrella chart (tags.<name>=true). Exactly one of
// these is expected to be set on an umbrella install.
//
// The autoscaler-anywhere and autoscaler-openshift tags are separate
// provider-profile tags and are intentionally out of scope here.
const (
	UmbrellaTagReadonly           = "readonly"
	UmbrellaTagNodeAutoscaler     = "node-autoscaler"
	UmbrellaTagWorkloadAutoscaler = "workload-autoscaler"
	UmbrellaTagFull               = "full"
)

// UmbrellaTagComponents maps each autoscaler profile tag to the exact set of
// sub-component (sub-chart) names that tag installs under the castai-umbrella
// chart (verified against the published castai chart, 0.43.227 at the time
// of writing — see umbrella_chart_integration_test.go, which fails on drift).
//
// The modes are supersets of readonly; the entries keep the chart's grouping
// order (readonly three first, then the mode-specific components) rather than
// alphabetical, so the incremental difference between tags is easy to read.
//
// Values use the umbrella SUB-CHART names ("castai-spot-handler",
// "castai-cluster-controller"), not the operator's short component names.
//
// NOTE: update this map when the umbrella chart's autoscaler sub-chart
// dependencies change (the integration test enforces this against the
// published chart).
var UmbrellaTagComponents = map[string][]string{
	UmbrellaTagReadonly: {
		ComponentNameAgent,
		"castai-spot-handler",
		ComponentNameKvisor,
	},
	UmbrellaTagNodeAutoscaler: {
		ComponentNameAgent,
		"castai-spot-handler",
		ComponentNameKvisor,
		"castai-cluster-controller",
		ComponentNameEvictor,
		ComponentNamePodMutator,
		ComponentNamePodPinner,
		ComponentNameLive,
	},
	UmbrellaTagWorkloadAutoscaler: {
		ComponentNameAgent,
		"castai-spot-handler",
		ComponentNameKvisor,
		"castai-cluster-controller",
		ComponentNameEvictor,
		ComponentNamePodMutator,
		ComponentNameWorkloadAutoscaler,
		ComponentNameWorkloadAutoscalerExporter,
	},
	UmbrellaTagFull: {
		ComponentNameAgent,
		"castai-spot-handler",
		ComponentNameKvisor,
		"castai-cluster-controller",
		ComponentNameEvictor,
		ComponentNamePodMutator,
		ComponentNamePodPinner,
		ComponentNameLive,
		ComponentNameWorkloadAutoscaler,
		ComponentNameWorkloadAutoscalerExporter,
	},
}

// UmbrellaCoveredComponents lists every sub-component any autoscaler tag of
// the castai-umbrella chart can install (the union of all tag sets, 10 in
// the published castai chart at the time of writing).
//
// The order is not alphabetical: it keeps the tag grouping (readonly four,
// then the node-autoscaler additions, then the workload-autoscaler-only
// additions) so that reading it mirrors how the chart composes the modes.
//
// NOTE: update this list when the umbrella chart's autoscaler sub-chart
// dependencies change.
var UmbrellaCoveredComponents = []string{
	// readonly
	ComponentNameAgent,
	"castai-spot-handler",
	ComponentNameKvisor,
	// + node-autoscaler
	"castai-cluster-controller",
	ComponentNameEvictor,
	ComponentNamePodMutator,
	ComponentNamePodPinner,
	ComponentNameLive,
	// + workload-autoscaler-only
	ComponentNameWorkloadAutoscaler,
	ComponentNameWorkloadAutoscalerExporter,
}

// umbrellaCoveredSet is the lookup form of UmbrellaCoveredComponents, keyed
// by the canonical (sub-chart) names.
var umbrellaCoveredSet = toSet(UmbrellaCoveredComponents)

// umbrellaNodeOnlySet holds the components installed by the node-autoscaler
// tag but not by the workload-autoscaler tag.
var umbrellaNodeOnlySet = toSet([]string{
	ComponentNamePodPinner,
	ComponentNameLive,
})

// umbrellaWorkloadOnlySet holds the components installed by the
// workload-autoscaler tag but not by the node-autoscaler tag.
var umbrellaWorkloadOnlySet = toSet([]string{
	ComponentNameWorkloadAutoscaler,
	ComponentNameWorkloadAutoscalerExporter,
})

// umbrellaNodeSideSet holds the components that count as "node side" for
// MinimalCoveringTag: everything the node-autoscaler tag adds on top of
// readonly. castai-cluster-controller, castai-evictor and castai-pod-mutator
// are shared with the workload side.
var umbrellaNodeSideSet = toSet([]string{
	"castai-cluster-controller",
	ComponentNameEvictor,
	ComponentNamePodMutator,
	ComponentNamePodPinner,
	ComponentNameLive,
})

// umbrellaWorkloadSideSet holds the components that count as "workload side"
// for MinimalCoveringTag: everything the workload-autoscaler tag adds on top
// of readonly. castai-cluster-controller, castai-evictor and
// castai-pod-mutator are shared with the node side.
var umbrellaWorkloadSideSet = toSet([]string{
	"castai-cluster-controller",
	ComponentNameEvictor,
	ComponentNamePodMutator,
	ComponentNameWorkloadAutoscaler,
	ComponentNameWorkloadAutoscalerExporter,
})

// IsUmbrellaCoveredComponent reports whether name is one of the sub-components
// the castai-umbrella chart's autoscaler tags can install.
//
// Both naming forms are accepted, because names reach the operator from two
// sources: helm chart matching yields the umbrella sub-chart names (e.g.
// "castai-spot-handler", "castai-kvisor") while the Mothership uses the
// operator's short component names (e.g. "spot-handler",
// "cluster-controller"). Unknown names report false.
func IsUmbrellaCoveredComponent(name string) bool {
	_, ok := canonicalUmbrellaComponent(name)
	return ok
}

// MinimalCoveringTag returns the narrowest castai-umbrella autoscaler tag
// whose component set covers every component in present.
//
// This is the migration's tag derivation: given the set of components a
// cluster already has installed standalone (names may arrive as umbrella
// sub-chart names or as the operator's short component names; unknown names
// are ignored, not errors), it picks the single tag to set on the umbrella so
// that nothing currently installed is lost.
//
// Because tags act at mode granularity, the minimal tag may render components
// that were not present standalone (e.g. a cluster with only the agent and the
// pod-pinner is covered by node-autoscaler, which also renders the
// cluster-controller, evictor, pod-mutator and castai-live).
//
// Semantics:
//   - empty or all-unknown input → readonly (the smallest, always-present
//     baseline);
//   - components exclusive to node-autoscaler (pod-pinner, castai-live) AND
//     components exclusive to workload-autoscaler (workload-autoscaler,
//     workload-autoscaler-exporter) present → full;
//   - else any workload-autoscaler-exclusive component (workload-autoscaler,
//     workload-autoscaler-exporter) → workload-autoscaler (the
//     node-autoscaler tag does not cover those, even when shared node-side
//     components are also present);
//   - else any node-side component (cluster-controller, evictor,
//     pod-mutator, pod-pinner, castai-live) → node-autoscaler;
//   - else any workload-side component (cluster-controller, evictor,
//     pod-mutator, workload-autoscaler, workload-autoscaler-exporter) →
//     workload-autoscaler;
//   - else → readonly.
//
// cluster-controller, evictor and pod-mutator count for both the node and
// the workload side: with only those shared components present the node
// side wins (its check comes first), but they lose to the
// workload-autoscaler-exclusive components, which the node-autoscaler tag
// would not cover.
func MinimalCoveringTag(present []string) string {
	var nodeOnly, workloadOnly, nodeSide, workloadSide bool
	for _, name := range present {
		canonical, ok := canonicalUmbrellaComponent(name)
		if !ok {
			continue
		}
		if umbrellaNodeOnlySet[canonical] {
			nodeOnly = true
		}
		if umbrellaWorkloadOnlySet[canonical] {
			workloadOnly = true
		}
		if umbrellaNodeSideSet[canonical] {
			nodeSide = true
		}
		if umbrellaWorkloadSideSet[canonical] {
			workloadSide = true
		}
	}

	switch {
	case nodeOnly && workloadOnly:
		return UmbrellaTagFull
	case workloadOnly:
		// A workload-autoscaler-exclusive component is not covered by the
		// node-autoscaler tag, so it wins over shared node-side components
		// (cluster-controller, evictor, pod-mutator).
		return UmbrellaTagWorkloadAutoscaler
	case nodeSide:
		return UmbrellaTagNodeAutoscaler
	case workloadSide:
		return UmbrellaTagWorkloadAutoscaler
	default:
		return UmbrellaTagReadonly
	}
}

// canonicalUmbrellaComponent normalizes a component name into the umbrella
// sub-chart (chart) name used as the key everywhere in this file, and reports
// whether the result is one of the covered components.
//
// Only the two historical operator short names differ from their sub-chart
// names: "spot-handler" → "castai-spot-handler" and "cluster-controller" →
// "castai-cluster-controller". Every other name maps to itself; names outside
// the covered set return ("", false).
func canonicalUmbrellaComponent(name string) (string, bool) {
	canonical := name
	switch name {
	case ComponentNameSpotHandler:
		canonical = "castai-spot-handler"
	case ComponentNameClusterController:
		canonical = "castai-cluster-controller"
	}
	if umbrellaCoveredSet[canonical] {
		return canonical, true
	}
	return "", false
}

// toSet builds the lookup form of a component name list.
func toSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return set
}
