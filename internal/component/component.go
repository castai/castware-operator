package component

import "github.com/samber/lo"

func IsSupported(name string) bool {
	// List of supported components
	supportedComponents := []string{
		"castai-agent",
	}
	return lo.Contains(supportedComponents, name)
}
