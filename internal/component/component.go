package components

import "github.com/samber/lo"

const (
	ComponentNameAgent             = "castai-agent"
	ComponentNameOperator          = "castware-operator"
	ComponentNameClusterController = "cluster-controller"
	ComponentNameSpotHandler       = "spot-handler"
)

var SupportedComponents = []string{
	ComponentNameAgent,
	ComponentNameClusterController,
	ComponentNameSpotHandler,
}

func IsSupported(name string) bool {
	// List of supported components
	supportedComponents := []string{
		ComponentNameAgent,
		ComponentNameClusterController,
		ComponentNameSpotHandler,
	}
	return lo.Contains(supportedComponents, name)
}

func RequiresExtendedPermissions(name string) bool {
	// List of components requiring extended permissions
	extendedPermissionsComponents := []string{
		ComponentNameClusterController,
	}
	return lo.Contains(extendedPermissionsComponents, name)
}
