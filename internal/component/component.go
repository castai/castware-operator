package components

import "github.com/samber/lo"

const (
	ComponentNameAgent    = "castai-agent"
	ComponentNameOperator = "castware-operator"
)

func IsSupported(name string) bool {
	// List of supported components
	supportedComponents := []string{
		ComponentNameAgent,
	}
	return lo.Contains(supportedComponents, name)
}
