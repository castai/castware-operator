package params

import (
	"context"

	"github.com/sirupsen/logrus"
	"helm.sh/helm/v3/pkg/release"
	"sigs.k8s.io/controller-runtime/pkg/client"

	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/rolebindings"
)

// ExtractComponentParams extracts component-specific parameters to send to Mothership.
// Only extracts specific, non-sensitive parameters (not all Helm values).
func ExtractComponentParams(
	ctx context.Context,
	log logrus.FieldLogger,
	componentName string,
	helmRelease *release.Release,
	k8sClient client.Client,
	namespace string,
) map[string]interface{} {

	switch componentName {
	case components.ComponentNameOperator:
		return extractOperatorParams(ctx, log, k8sClient, namespace)
	case components.ComponentNameSpotHandler:
		return extractSpotHandlerParams(log, helmRelease)
	case components.ComponentNameClusterController:
		return extractClusterControllerParams(log, helmRelease)
	default:
		return make(map[string]interface{})
	}
}

func extractOperatorParams(ctx context.Context, log logrus.FieldLogger, k8sClient client.Client, namespace string) map[string]interface{} {
	params := make(map[string]interface{})

	extendedPerms, err := rolebindings.CheckExtendedPermissionsExist(ctx, k8sClient, namespace)
	if err != nil {
		log.WithError(err).Warn("Failed to check extended permissions, continuing without this parameter")
		return params
	}

	params["extendedPermissions"] = extendedPerms
	return params
}

func extractSpotHandlerParams(log logrus.FieldLogger, helmRelease *release.Release) map[string]interface{} {
	params := make(map[string]interface{})

	if helmRelease == nil || helmRelease.Config == nil {
		return params
	}

	if phase2, ok := helmRelease.Config["phase2Permissions"].(bool); ok {
		params["phase2Permissions"] = phase2
	} else {
		log.Debug("phase2Permissions not found in Helm values")
	}

	return params
}

func extractClusterControllerParams(_ logrus.FieldLogger, helmRelease *release.Release) map[string]interface{} {
	params := make(map[string]interface{})

	if helmRelease == nil || helmRelease.Config == nil {
		return params
	}

	if autoscaling, ok := helmRelease.Config["autoscaling"].(map[string]interface{}); ok {
		autoscalingParams := make(map[string]interface{})

		if enabled, ok := autoscaling["enabled"].(bool); ok {
			autoscalingParams["enabled"] = enabled
		}

		if len(autoscalingParams) > 0 {
			params["autoscaling"] = autoscalingParams
		}
	}

	if workloadAutoscaling, ok := helmRelease.Config["workloadAutoscaling"].(map[string]interface{}); ok {
		workloadAutoscalingParams := make(map[string]interface{})

		if enabled, ok := workloadAutoscaling["enabled"].(bool); ok {
			workloadAutoscalingParams["enabled"] = enabled
		}

		if len(workloadAutoscalingParams) > 0 {
			params["workloadAutoscaling"] = workloadAutoscalingParams
		}
	}

	return params
}
