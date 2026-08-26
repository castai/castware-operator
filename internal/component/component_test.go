package components

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequiresExtendedPermissions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want bool
	}{
		{ComponentNameClusterController, true},
		{ComponentNameAgent, false},
		{ComponentNameSpotHandler, false},
		{ComponentNameUmbrella, false},
		{ComponentNameOperator, false},
		{"unknown", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, RequiresExtendedPermissions(tc.name))
		})
	}
}

func TestRequiresExtendedPermissionsForValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		values  map[string]any
		want    bool
		comment string
	}{
		// Non-umbrella components delegate to RequiresExtendedPermissions and
		// ignore the values entirely.
		{name: ComponentNameClusterController, values: nil, want: true, comment: "cluster-controller always extended"},
		{name: ComponentNameAgent, values: nil, want: false, comment: "agent never extended"},
		{name: ComponentNameSpotHandler, values: nil, want: false, comment: "spot-handler never extended"},

		// Umbrella is tag-aware.
		{name: ComponentNameUmbrella, values: nil, want: true, comment: "umbrella with no tags requires extended"},
		{name: ComponentNameUmbrella, values: map[string]any{}, want: true, comment: "umbrella with empty tags requires extended"},
		{name: ComponentNameUmbrella, values: map[string]any{"tags": map[string]any{}}, want: true, comment: "umbrella with tags but no readonly requires extended"},
		{name: ComponentNameUmbrella, values: map[string]any{"tags": map[string]any{"readonly": true}}, want: false, comment: "umbrella readonly is minimal-perm"},
		{name: ComponentNameUmbrella, values: map[string]any{"tags": map[string]any{"readonly": false}}, want: true, comment: "umbrella readonly=false requires extended"},
		{name: ComponentNameUmbrella, values: map[string]any{"tags": map[string]any{"full": true}}, want: true, comment: "umbrella full requires extended"},
		{name: ComponentNameUmbrella, values: map[string]any{"tags": map[string]any{"node-autoscaler": true}}, want: true, comment: "umbrella node-autoscaler requires extended"},
		{name: ComponentNameUmbrella, values: map[string]any{"tags": map[string]any{"readonly": "true"}}, want: true, comment: "umbrella readonly as string (not bool) requires extended"},
		{name: ComponentNameUmbrella, values: map[string]any{"tags": "not-a-map"}, want: true, comment: "umbrella malformed tags requires extended"},
	}
	for _, tc := range tests {
		t.Run(tc.comment, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, RequiresExtendedPermissionsForValues(tc.name, tc.values), tc.comment)
		})
	}
}
