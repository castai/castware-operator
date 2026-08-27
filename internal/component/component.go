package components

import "github.com/samber/lo"

const (
	ComponentNameAgent             = "castai-agent"
	ComponentNameOperator          = "castware-operator"
	ComponentNameClusterController = "cluster-controller"
	ComponentNameSpotHandler       = "spot-handler"
	ComponentNameUmbrella          = "castai-umbrella"
)

// SupportedComponents List of supported components
var SupportedComponents = []string{
	ComponentNameUmbrella,
	ComponentNameAgent,
	ComponentNameSpotHandler,
	ComponentNameClusterController,
	// Add new components here, phase2 components must be added after cluster controller
}

func IsSupported(name string) bool {
	return lo.Contains(SupportedComponents, name)
}

func RequiresExtendedPermissions(name string) bool {
	// List of components requiring extended permissions
	extendedPermissionsComponents := []string{
		ComponentNameClusterController,
	}
	return lo.Contains(extendedPermissionsComponents, name)
}

// RequiresExtendedPermissionsForValues reports whether the component requires
// extended permissions given its already-unmarshaled user values.
//
// The umbrella component is special: its tags.readonly profile installs only
// the agent, spot-handler and kvisor sub-components, which are satisfiable with
// the operator's minimal (base) RBAC. Any other tag (or no tag) pulls in the
// cluster-controller ("woop"), which needs extended permissions.
//
// The umbrella chart allows a user to combine tags.readonly=true with an
// explicitly enabled cluster-controller sub-component
// (autoscaler.castai-cluster-controller.enabled=true). We cannot prevent that
// combination, so the gate treats the cluster-controller as authoritative:
// whenever it is enabled — by explicit opt-in or by a non-readonly tag — the
// umbrella requires extended permissions, even if tags.readonly is also set.
// All non-umbrella components delegate to RequiresExtendedPermissions.
func RequiresExtendedPermissionsForValues(name string, values map[string]any) bool {
	if name != ComponentNameUmbrella {
		return RequiresExtendedPermissions(name)
	}
	// An explicitly enabled cluster-controller always requires extended
	// permissions, regardless of the readonly tag.
	if umbrellaClusterControllerEnabled(values) {
		return true
	}
	tags, _ := values["tags"].(map[string]any)
	readonly, _ := tags["readonly"].(bool)
	return !readonly
}

// umbrellaClusterControllerEnabled reports whether the umbrella chart would
// enable its cluster-controller sub-component, mirroring the chart's own
// "umbrella.castai-cluster-controller.enabled" helper: an explicit
// autoscaler.castai-cluster-controller.enabled value wins; otherwise the
// node-autoscaler, workload-autoscaler and full tags enable it.
func umbrellaClusterControllerEnabled(values map[string]any) bool {
	autoscaler, _ := values["autoscaler"].(map[string]any)
	cc, _ := autoscaler["castai-cluster-controller"].(map[string]any)
	if enabled, ok := cc["enabled"].(bool); ok {
		return enabled
	}
	tags, _ := values["tags"].(map[string]any)
	if tags == nil {
		return false
	}
	for _, tag := range []string{"node-autoscaler", "workload-autoscaler", "full"} {
		if v, _ := tags[tag].(bool); v {
			return true
		}
	}
	return false
}
