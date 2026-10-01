package components

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKentComponents(t *testing.T) {
	t.Parallel()

	// Hand-written literal from the kent sub-chart's Chart.yaml (0.15.0),
	// independent of KentComponents so a wrong constant cannot pass tautologically.
	require.Len(t, KentComponents, 11)
	require.Equal(t, []string{
		// kent sub-chart Chart.yaml dependency order.
		"castai-agent",
		"castai-cluster-controller",
		"castai-kentroller",
		"castai-workload-autoscaler",
		"castai-workload-autoscaler-exporter",
		"castai-live",
		"castai-pod-mutator",
		"castai-spot-handler",
		"metrics-server",
		"castai-kvisor",
		"castai-chart-upgrader",
	}, KentComponents)

	// No duplicates.
	require.ElementsMatch(t, uniqueNames(KentComponents), KentComponents)

	// The autoscaler-only extras are NOT part of the kent profile.
	require.NotContains(t, KentComponents, ComponentNameEvictor)
	require.NotContains(t, KentComponents, ComponentNamePodPinner)

	// Every kent-only component is a member of the full kent set.
	for _, name := range KentOnlyComponents {
		require.Contains(t, KentComponents, name)
	}
}

func TestKentOnlyComponents(t *testing.T) {
	t.Parallel()

	require.Equal(t, []string{
		"castai-kentroller",
		"castai-chart-upgrader",
		"metrics-server",
	}, KentOnlyComponents)

	for _, tc := range []struct {
		name string
		want bool
	}{
		{"castai-kentroller", true},
		{"castai-chart-upgrader", true},
		{"metrics-server", true},

		// Shared components (also under autoscaler tags) and anything else
		// are not kent-only.
		{"castai-agent", false},
		{"castai-cluster-controller", false},
		{"castai-kvisor", false},
		{"castai-live", false},
		{"castai-evictor", false},
		{"kent", false},
		{"", false},
		{"does-not-exist", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, IsKentOnlyComponent(tc.name))
		})
	}
}

func TestKentEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		values map[string]any
		want   bool
	}{
		{
			name:   "kent enabled",
			values: map[string]any{"kent": map[string]any{"enabled": true}},
			want:   true,
		},
		{
			name:   "kent disabled",
			values: map[string]any{"kent": map[string]any{"enabled": false}},
			want:   false,
		},
		{
			name:   "no kent key",
			values: map[string]any{"tags": map[string]any{"readonly": true}},
			want:   false,
		},
		{
			name:   "nil values",
			values: nil,
			want:   false,
		},
		{
			name:   "kent not a map",
			values: map[string]any{"kent": true},
			want:   false,
		},
		{
			name:   "enabled not a bool",
			values: map[string]any{"kent": map[string]any{"enabled": "true"}},
			want:   false,
		},
		{
			name: "kent block with other knobs",
			values: map[string]any{"kent": map[string]any{
				"enabled":          true,
				"managedKarpenter": true,
			}},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, KentEnabled(tc.values), tc.name)
		})
	}
}

func TestMinimalCoveringProfile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		present []string
		want    string
	}{
		// Delegation to the tag derivation: empty or all-unknown input maps
		// to the readonly baseline.
		{name: "empty slice", present: []string{}, want: "readonly"},
		{name: "nil slice", present: nil, want: "readonly"},
		{name: "only unknown components", present: []string{"castware-operator", "does-not-exist"}, want: "readonly"},
		{name: "agent only", present: []string{"castai-agent"}, want: "readonly"},

		// Any kent-only component forces the kent profile branch, alone or
		// mixed with anything else.
		{name: "kentroller only", present: []string{"castai-kentroller"}, want: "kent"},
		{name: "chart-upgrader only", present: []string{"castai-chart-upgrader"}, want: "kent"},
		{name: "metrics-server only", present: []string{"metrics-server"}, want: "kent"},
		{
			name:    "all kent-only components",
			present: []string{"castai-kentroller", "castai-chart-upgrader", "metrics-server"},
			want:    "kent",
		},
		{
			// Edge: autoscaler-only extras (evictor, pod-pinner are NOT in the
			// kent profile) do not override the kent-only presence.
			name:    "kentroller plus evictor and pod-pinner",
			present: []string{"castai-kentroller", "castai-evictor", "castai-pod-pinner"},
			want:    "kent",
		},
		{
			name:    "kentroller plus full autoscaler set",
			present: append([]string{"castai-kentroller"}, UmbrellaCoveredComponents...),
			want:    "kent",
		},

		// Tag-level derivations are unchanged when no kent-only component is
		// present.
		{name: "pod-pinner only", present: []string{"castai-pod-pinner"}, want: "node-autoscaler"},
		{name: "workload-autoscaler only", present: []string{"castai-workload-autoscaler"}, want: "workload-autoscaler"},
		{
			name:    "node-only plus workload-only",
			present: []string{"castai-live", "castai-workload-autoscaler-exporter"},
			want:    "full",
		},

		// Short-form names still canonicalize in the tag branch.
		{name: "spot-handler short form", present: []string{"spot-handler"}, want: "readonly"},
		{name: "cluster-controller short form", present: []string{"cluster-controller"}, want: "node-autoscaler"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, MinimalCoveringProfile(tc.present), tc.name)
		})
	}
}
