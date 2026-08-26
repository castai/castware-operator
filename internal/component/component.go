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
// the operator's minimal (base) RBAC. Any other tag (or no tag) pulls in
// cluster-controller/evictor/pod-pinner, which need extended permissions. All
// non-umbrella components delegate to RequiresExtendedPermissions.
func RequiresExtendedPermissionsForValues(name string, values map[string]any) bool {
	if name != ComponentNameUmbrella {
		return RequiresExtendedPermissions(name)
	}
	tags, _ := values["tags"].(map[string]any)
	readonly, _ := tags["readonly"].(bool)
	return !readonly
}
