package components

// Kent-only sub-chart names: rendered only by the umbrella's kent profile,
// so their presence forces the kent branch of the migration's mode
// derivation.
const (
	ComponentNameKentroller    = "castai-kentroller"
	ComponentNameChartUpgrader = "castai-chart-upgrader"
	ComponentNameMetricsServer = "metrics-server"
)

// UmbrellaProfileKent is the kent profile identifier. Kent is a profile, not
// a tag: it is driven by kent.enabled=true and is mutually exclusive with
// tags.* (the admission webhook rejects the mix).
const UmbrellaProfileKent = "kent"

// KentComponents lists every sub-chart the kent profile renders, in the kent
// sub-chart's Chart.yaml dependency order. Validated against the published
// chart by umbrella_chart_integration_test.go — update on drift.
var KentComponents = []string{
	ComponentNameAgent,
	UmbrellaSubchartClusterController,
	ComponentNameKentroller,
	ComponentNameWorkloadAutoscaler,
	ComponentNameWorkloadAutoscalerExporter,
	ComponentNameLive,
	ComponentNamePodMutator,
	UmbrellaSubchartSpotHandler,
	ComponentNameMetricsServer,
	ComponentNameKvisor,
	ComponentNameChartUpgrader,
}

// KentOnlyComponents are the sub-charts only the kent profile renders.
var KentOnlyComponents = []string{
	ComponentNameKentroller,
	ComponentNameChartUpgrader,
	ComponentNameMetricsServer,
}

// kentOnlySet is the lookup form of KentOnlyComponents.
var kentOnlySet = toSet(KentOnlyComponents)

// KentEnabled reports whether umbrella values enable the kent profile.
// Tolerant of a missing or malformed kent block.
func KentEnabled(values map[string]any) bool {
	if values == nil {
		return false
	}
	kent, ok := values["kent"].(map[string]any)
	if !ok {
		return false
	}
	enabled, _ := kent["enabled"].(bool)
	return enabled
}

// IsKentOnlyComponent reports whether name is a kent-only sub-chart.
func IsKentOnlyComponent(name string) bool {
	return kentOnlySet[name]
}

// MinimalCoveringProfile is the profile-aware counterpart of
// MinimalCoveringTag used by the migration's mode derivation. Any kent-only
// component present forces the kent profile (no autoscaler tag renders them),
// regardless of what else is present; otherwise the minimal covering tag
// applies.
//
// Edge: a set mixing kent-only and autoscaler-only extras (evictor,
// pod-pinner are NOT in the kent profile) still derives kent — the extras are
// absorbed but not re-rendered, since no mode covers both.
func MinimalCoveringProfile(present []string) string {
	for _, name := range present {
		if IsKentOnlyComponent(name) {
			return UmbrellaProfileKent
		}
	}
	return MinimalCoveringTag(present)
}
