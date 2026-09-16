package components

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTagComponentSets(t *testing.T) {
	t.Parallel()

	// Expected sets are hand-written literals for castai-umbrella v0.35.2,
	// independent of UmbrellaTagComponents itself, so a wrong constant or a
	// bad map entry cannot make the test tautologically pass.
	tests := []struct {
		name    string
		wantLen int
		expect  []string
	}{
		{
			name:    "readonly",
			wantLen: 4,
			expect: []string{
				"castai-agent",
				"castai-spot-handler",
				"castai-kvisor",
				"gpu-metrics-exporter",
			},
		},
		{
			name:    "node-autoscaler",
			wantLen: 9,
			expect: []string{
				"castai-agent",
				"castai-spot-handler",
				"castai-kvisor",
				"gpu-metrics-exporter",
				"castai-cluster-controller",
				"castai-evictor",
				"castai-pod-mutator",
				"castai-pod-pinner",
				"castai-live",
			},
		},
		{
			name:    "workload-autoscaler",
			wantLen: 9,
			expect: []string{
				"castai-agent",
				"castai-spot-handler",
				"castai-kvisor",
				"gpu-metrics-exporter",
				"castai-cluster-controller",
				"castai-evictor",
				"castai-pod-mutator",
				"castai-workload-autoscaler",
				"castai-workload-autoscaler-exporter",
			},
		},
		{
			name:    "full",
			wantLen: 11,
			expect: []string{
				"castai-agent",
				"castai-spot-handler",
				"castai-kvisor",
				"gpu-metrics-exporter",
				"castai-cluster-controller",
				"castai-evictor",
				"castai-pod-mutator",
				"castai-pod-pinner",
				"castai-live",
				"castai-workload-autoscaler",
				"castai-workload-autoscaler-exporter",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := UmbrellaTagComponents[tc.name]
			require.True(t, ok, "tag %q must be present in UmbrellaTagComponents", tc.name)
			require.Len(t, got, tc.wantLen)
			require.ElementsMatch(t, tc.expect, got)
		})
	}

	// Structural invariants between the tag sets.
	t.Run("readonly is a subset of node-autoscaler", func(t *testing.T) {
		t.Parallel()
		require.Subset(t, UmbrellaTagComponents["node-autoscaler"], UmbrellaTagComponents["readonly"])
	})
	t.Run("readonly is a subset of workload-autoscaler", func(t *testing.T) {
		t.Parallel()
		require.Subset(t, UmbrellaTagComponents["workload-autoscaler"], UmbrellaTagComponents["readonly"])
	})
	t.Run("node-autoscaler union workload-autoscaler equals full", func(t *testing.T) {
		t.Parallel()
		combined := make([]string, 0, len(UmbrellaTagComponents["node-autoscaler"])+len(UmbrellaTagComponents["workload-autoscaler"]))
		combined = append(combined, UmbrellaTagComponents["node-autoscaler"]...)
		combined = append(combined, UmbrellaTagComponents["workload-autoscaler"]...)
		require.ElementsMatch(t, UmbrellaTagComponents["full"], uniqueNames(combined))
	})
	t.Run("no tag set contains duplicates", func(t *testing.T) {
		t.Parallel()
		for tag, names := range UmbrellaTagComponents {
			require.Len(t, uniqueNames(names), len(names), "tag %q contains duplicate components", tag)
		}
	})
	t.Run("umbrella covered components equal the full set", func(t *testing.T) {
		t.Parallel()
		require.Len(t, UmbrellaCoveredComponents, 11)
		require.ElementsMatch(t, UmbrellaTagComponents["full"], UmbrellaCoveredComponents)
	})
}

func TestMinimalCoveringTag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		present []string
		want    string
	}{
		// Empty or all-unknown input maps to the readonly baseline.
		{name: "empty slice", present: []string{}, want: "readonly"},
		{name: "nil slice", present: nil, want: "readonly"},
		{name: "only unknown components", present: []string{"castware-operator", "does-not-exist"}, want: "readonly"},
		{name: "unknown mixed with known readonly component", present: []string{"castai-agent", "unknown"}, want: "readonly"},

		// Readonly-level components only: nothing beyond the baseline.
		{name: "agent only", present: []string{"castai-agent"}, want: "readonly"},
		{name: "readonly subset", present: []string{"castai-agent", "castai-kvisor", "gpu-metrics-exporter"}, want: "readonly"},
		{name: "spot-handler sub-chart name", present: []string{"castai-spot-handler"}, want: "readonly"},

		// Mothership short-form names are accepted and canonicalized.
		{name: "spot-handler short form", present: []string{"spot-handler"}, want: "readonly"},
		{name: "cluster-controller short form", present: []string{"cluster-controller"}, want: "node-autoscaler"},
		{name: "spot-handler and cluster-controller short forms", present: []string{"spot-handler", "cluster-controller"}, want: "node-autoscaler"},

		// Node-autoscaler-level components.
		{name: "agent plus pod-pinner", present: []string{"castai-agent", "castai-pod-pinner"}, want: "node-autoscaler"},
		{name: "castai-live only", present: []string{"castai-live"}, want: "node-autoscaler"},
		{name: "duplicate entries", present: []string{"castai-live", "castai-live"}, want: "node-autoscaler"},

		// Workload-autoscaler-level components.
		{name: "workload-autoscaler only", present: []string{"castai-workload-autoscaler"}, want: "workload-autoscaler"},
		{name: "agent plus workload-autoscaler-exporter", present: []string{"castai-agent", "castai-workload-autoscaler-exporter"}, want: "workload-autoscaler"},

		// A shared node-side component together with a workload-only
		// component is covered by the workload tag, not the node tag.
		{name: "shared cluster-controller plus workload-autoscaler", present: []string{"cluster-controller", "castai-workload-autoscaler"}, want: "workload-autoscaler"},

		// Node-only and workload-only components together need the full tag.
		{name: "pod-pinner plus workload-autoscaler", present: []string{"castai-pod-pinner", "castai-workload-autoscaler"}, want: "full"},
		{name: "castai-live plus workload-autoscaler-exporter", present: []string{"castai-live", "castai-workload-autoscaler-exporter"}, want: "full"},

		// Every covered component present at once.
		{
			name: "all covered components",
			present: []string{
				"castai-agent",
				"castai-spot-handler",
				"castai-kvisor",
				"gpu-metrics-exporter",
				"castai-cluster-controller",
				"castai-evictor",
				"castai-pod-mutator",
				"castai-pod-pinner",
				"castai-live",
				"castai-workload-autoscaler",
				"castai-workload-autoscaler-exporter",
			},
			want: "full",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, MinimalCoveringTag(tc.present), tc.name)
		})
	}
}

func TestIsUmbrellaCoveredComponent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want bool
	}{
		// All 11 umbrella sub-chart names are covered.
		{"castai-agent", true},
		{"castai-spot-handler", true},
		{"castai-kvisor", true},
		{"gpu-metrics-exporter", true},
		{"castai-cluster-controller", true},
		{"castai-evictor", true},
		{"castai-pod-mutator", true},
		{"castai-pod-pinner", true},
		{"castai-live", true},
		{"castai-workload-autoscaler", true},
		{"castai-workload-autoscaler-exporter", true},

		// The Mothership short forms are accepted too.
		{"spot-handler", true},
		{"cluster-controller", true},

		// The umbrella chart itself, this operator and unrelated or
		// malformed names are not covered.
		{"castai-umbrella", false},
		{"castware-operator", false},
		{"", false},
		{"agent", false},
		{"Castai-Agent", false},
		{"does-not-exist", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, IsUmbrellaCoveredComponent(tc.name))
		})
	}
}

func TestUmbrellaCoveredComponentsOrderIsDeterministic(t *testing.T) {
	t.Parallel()

	// 11 entries, none duplicated: the list must match its own
	// deduplicated copy.
	require.Len(t, UmbrellaCoveredComponents, 11)
	require.ElementsMatch(t, uniqueNames(UmbrellaCoveredComponents), UmbrellaCoveredComponents)

	// The documented grouping order: readonly four first, then the
	// node-autoscaler additions, then the workload-autoscaler-only
	// additions.
	require.Equal(t, []string{
		// readonly
		"castai-agent",
		"castai-spot-handler",
		"castai-kvisor",
		"gpu-metrics-exporter",
		// + node-autoscaler
		"castai-cluster-controller",
		"castai-evictor",
		"castai-pod-mutator",
		"castai-pod-pinner",
		"castai-live",
		// + workload-autoscaler-only
		"castai-workload-autoscaler",
		"castai-workload-autoscaler-exporter",
	}, UmbrellaCoveredComponents)
}

// uniqueNames returns names with duplicates removed, preserving the order of
// first occurrence.
func uniqueNames(names []string) []string {
	seen := make(map[string]bool, len(names))
	unique := make([]string, 0, len(names))
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		unique = append(unique, name)
	}
	return unique
}
