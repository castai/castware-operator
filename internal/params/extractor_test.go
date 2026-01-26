package params

import (
	"context"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	components "github.com/castai/castware-operator/internal/component"
)

func TestExtractOperatorParams_WithExtendedPermissions(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = rbacv1.AddToScheme(scheme)

	// Create RoleBinding with extended permissions label
	roleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-rolebinding",
			Namespace: "test-namespace",
			Labels: map[string]string{
				"castware.cast.ai/extended-permissions": "true",
			},
		},
	}

	// Create ClusterRoleBinding with extended permissions label
	clusterRoleBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-clusterrolebinding",
			Labels: map[string]string{
				"castware.cast.ai/extended-permissions": "true",
			},
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(roleBinding, clusterRoleBinding).
		Build()

	log := logrus.New()
	params := extractOperatorParams(context.Background(), log, k8sClient, "test-namespace")

	assert.NotNil(t, params)
	assert.Equal(t, true, params["extendedPermissions"])
}

func TestExtractOperatorParams_WithoutExtendedPermissions(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = rbacv1.AddToScheme(scheme)

	// Create RoleBinding without extended permissions label
	roleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-rolebinding",
			Namespace: "test-namespace",
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(roleBinding).
		Build()

	log := logrus.New()
	params := extractOperatorParams(context.Background(), log, k8sClient, "test-namespace")

	assert.NotNil(t, params)
	assert.Equal(t, false, params["extendedPermissions"])
}

func TestExtractSpotHandlerParams_Phase2Enabled(t *testing.T) {
	helmRelease := &release.Release{
		Config: map[string]interface{}{
			"phase2Permissions": true,
		},
	}

	params := extractSpotHandlerParams(helmRelease)

	assert.NotNil(t, params)
	assert.Equal(t, true, params["phase2Permissions"])
}

func TestExtractSpotHandlerParams_Phase2Disabled(t *testing.T) {
	helmRelease := &release.Release{
		Config: map[string]interface{}{
			"phase2Permissions": false,
		},
	}

	params := extractSpotHandlerParams(helmRelease)

	assert.NotNil(t, params)
	assert.Equal(t, false, params["phase2Permissions"])
}

func TestExtractSpotHandlerParams_NilHelmRelease(t *testing.T) {
	params := extractSpotHandlerParams(nil)

	assert.NotNil(t, params)
	assert.Empty(t, params)
}

func TestExtractSpotHandlerParams_MissingPhase2(t *testing.T) {
	helmRelease := &release.Release{
		Config: map[string]interface{}{
			"someOtherConfig": "value",
		},
	}

	params := extractSpotHandlerParams(helmRelease)

	assert.NotNil(t, params)
	assert.Empty(t, params)
}

func TestExtractClusterControllerParams_WithAutoscalingFromChart(t *testing.T) {
	helmRelease := &release.Release{
		Chart: &chart.Chart{
			Values: map[string]interface{}{
				"autoscaling": map[string]interface{}{
					"enabled": true,
				},
			},
		},
	}

	params := extractClusterControllerParams(helmRelease)

	assert.NotNil(t, params)
	assert.Contains(t, params, "autoscaling")

	autoscaling := params["autoscaling"].(map[string]interface{})
	assert.Equal(t, true, autoscaling["enabled"])
}

func TestExtractClusterControllerParams_WithAutoscalingFromConfig(t *testing.T) {
	helmRelease := &release.Release{
		Config: map[string]interface{}{
			"autoscaling": map[string]interface{}{
				"enabled": true,
			},
		},
	}

	params := extractClusterControllerParams(helmRelease)

	assert.NotNil(t, params)
	assert.Contains(t, params, "autoscaling")

	autoscaling := params["autoscaling"].(map[string]interface{})
	assert.Equal(t, true, autoscaling["enabled"])
}

func TestExtractClusterControllerParams_WithAutoscalingOverrides(t *testing.T) {
	helmRelease := &release.Release{
		Chart: &chart.Chart{
			Values: map[string]interface{}{
				"autoscaling": map[string]interface{}{
					"enabled": false,
				},
			},
		},
		Config: map[string]interface{}{
			"autoscaling": map[string]interface{}{
				"enabled": true,
			},
		},
	}

	params := extractClusterControllerParams(helmRelease)

	assert.NotNil(t, params)
	assert.Contains(t, params, "autoscaling")

	autoscaling := params["autoscaling"].(map[string]interface{})
	assert.Equal(t, true, autoscaling["enabled"])
}

func TestExtractClusterControllerParams_WithWorkloadAutoscaling(t *testing.T) {
	helmRelease := &release.Release{
		Config: map[string]interface{}{
			"workloadAutoscaling": map[string]interface{}{
				"enabled": true,
			},
		},
	}

	params := extractClusterControllerParams(helmRelease)

	assert.NotNil(t, params)
	assert.Contains(t, params, "workloadAutoscaling")

	workloadAutoscaling := params["workloadAutoscaling"].(map[string]interface{})
	assert.Equal(t, true, workloadAutoscaling["enabled"])
}

func TestExtractClusterControllerParams_NilHelmRelease(t *testing.T) {
	params := extractClusterControllerParams(nil)

	assert.NotNil(t, params)
	assert.Empty(t, params)
}

func TestExtractComponentParams_UnknownComponent(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = rbacv1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	log := logrus.New()
	params := ExtractComponentParams(
		context.Background(),
		log,
		"unknown-component",
		nil,
		k8sClient,
		"test-namespace",
	)

	assert.NotNil(t, params)
	assert.Empty(t, params)
}

func TestExtractComponentParams_Operator(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = rbacv1.AddToScheme(scheme)

	// Create RoleBinding with extended permissions label
	roleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-rolebinding",
			Namespace: "test-namespace",
			Labels: map[string]string{
				"castware.cast.ai/extended-permissions": "true",
			},
		},
	}

	// Create ClusterRoleBinding with extended permissions label
	clusterRoleBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-clusterrolebinding",
			Labels: map[string]string{
				"castware.cast.ai/extended-permissions": "true",
			},
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(roleBinding, clusterRoleBinding).
		Build()

	log := logrus.New()
	params := ExtractComponentParams(
		context.Background(),
		log,
		components.ComponentNameOperator,
		nil,
		k8sClient,
		"test-namespace",
	)

	assert.NotNil(t, params)
	assert.Equal(t, true, params["extendedPermissions"])
}

func TestExtractComponentParams_SpotHandler(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = rbacv1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	helmRelease := &release.Release{
		Config: map[string]interface{}{
			"phase2Permissions": true,
		},
	}

	log := logrus.New()
	params := ExtractComponentParams(
		context.Background(),
		log,
		components.ComponentNameSpotHandler,
		helmRelease,
		k8sClient,
		"test-namespace",
	)

	assert.NotNil(t, params)
	assert.Equal(t, true, params["phase2Permissions"])
}

func TestExtractComponentParams_Agent(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = rbacv1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	helmRelease := &release.Release{
		Config: map[string]interface{}{
			"autoscaling": map[string]interface{}{
				"enabled":     true,
				"minReplicas": float64(3),
				"maxReplicas": float64(5),
			},
		},
	}

	log := logrus.New()
	params := ExtractComponentParams(
		context.Background(),
		log,
		components.ComponentNameAgent,
		helmRelease,
		k8sClient,
		"test-namespace",
	)

	assert.NotNil(t, params)
	assert.Empty(t, params)
}

func TestExtractComponentParams_ClusterController(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = rbacv1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	helmRelease := &release.Release{
		Config: map[string]interface{}{
			"autoscaling": map[string]interface{}{
				"enabled": true,
			},
			"workloadAutoscaling": map[string]interface{}{
				"enabled": false,
			},
		},
	}

	log := logrus.New()
	params := ExtractComponentParams(
		context.Background(),
		log,
		components.ComponentNameClusterController,
		helmRelease,
		k8sClient,
		"test-namespace",
	)

	assert.NotNil(t, params)
	assert.Contains(t, params, "autoscaling")
	assert.Contains(t, params, "workloadAutoscaling")

	autoscaling := params["autoscaling"].(map[string]interface{})
	assert.Equal(t, true, autoscaling["enabled"])

	workloadAutoscaling := params["workloadAutoscaling"].(map[string]interface{})
	assert.Equal(t, false, workloadAutoscaling["enabled"])
}
