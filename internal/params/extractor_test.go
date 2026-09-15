package params

import (
	"context"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
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

// umbrellaTestChart builds a chart tree mirroring the castai umbrella chart's
// structure: the root with the autoscaler and kent profiles, and the
// autoscaler profile's sub-components carrying the mode tags.
func umbrellaTestChart() *chart.Chart {
	modeTags := []string{"readonly", "node-autoscaler", "workload-autoscaler", "full"}
	ccTags := []string{"node-autoscaler", "workload-autoscaler", "full"}

	sub := func(name, version string, values map[string]interface{}) *chart.Chart {
		return &chart.Chart{
			Metadata: &chart.Metadata{Name: name, Version: version},
			Values:   values,
		}
	}

	agent := sub("castai-agent", "0.161.0", map[string]interface{}{})
	spot := sub("castai-spot-handler", "0.35.2", map[string]interface{}{"phase2Permissions": false})
	kvisor := sub("castai-kvisor", "1.164.5", map[string]interface{}{})
	cc := sub("castai-cluster-controller", "0.92.4", map[string]interface{}{
		"autoscaling":         map[string]interface{}{"enabled": false},
		"workloadAutoscaling": map[string]interface{}{"enabled": false},
	})
	kentSub := sub("castai-kentroller", "0.1.56", map[string]interface{}{})

	autoscaler := &chart.Chart{
		Metadata: &chart.Metadata{
			Name:    "autoscaler",
			Version: "0.2.0",
			Dependencies: []*chart.Dependency{
				{Name: "castai-agent", Version: "0.161.0", Condition: "castai-agent.enabled", Tags: modeTags},
				{Name: "castai-spot-handler", Version: "0.35.2", Condition: "castai-spot-handler.enabled", Tags: modeTags},
				{Name: "castai-kvisor", Version: "1.164.5", Condition: "castai-kvisor.enabled", Tags: modeTags},
				{Name: "castai-cluster-controller", Version: "0.92.4", Condition: "castai-cluster-controller.enabled", Tags: ccTags},
			},
		},
		Values: map[string]interface{}{},
	}
	autoscaler.AddDependency(agent, spot, kvisor, cc)

	kent := &chart.Chart{
		Metadata: &chart.Metadata{
			Name:    "kent",
			Version: "0.15.0",
			Dependencies: []*chart.Dependency{
				{Name: "castai-kentroller", Version: "0.1.56", Condition: "castai-kentroller.enabled"},
			},
		},
		Values: map[string]interface{}{},
	}
	kent.AddDependency(kentSub)

	root := &chart.Chart{
		Metadata: &chart.Metadata{
			Name:    "castai",
			Version: "0.38.16",
			Dependencies: []*chart.Dependency{
				{Name: "kent", Version: "0.15.0", Condition: "kent.enabled"},
				{Name: "autoscaler", Version: "0.2.0"},
			},
		},
		Values: map[string]interface{}{
			"tags": map[string]interface{}{
				"readonly":            false,
				"full":                false,
				"node-autoscaler":     false,
				"workload-autoscaler": false,
			},
			"kent": map[string]interface{}{"enabled": false},
		},
	}
	root.AddDependency(kent, autoscaler)
	return root
}

func umbrellaTestRelease(userValues map[string]interface{}) *release.Release {
	return &release.Release{
		Name:   "castai",
		Chart:  umbrellaTestChart(),
		Config: userValues,
	}
}

func inventoryEntry(t *testing.T, params map[string]interface{}, name string) umbrellaSubcomponent {
	t.Helper()
	inventory, ok := params["inventory"].([]umbrellaSubcomponent)
	if !ok {
		t.Fatalf("expected inventory to be []umbrellaSubcomponent, got %T", params["inventory"])
	}
	for _, entry := range inventory {
		if entry.Name == name {
			return entry
		}
	}
	t.Fatalf("no inventory entry for %s in %+v", name, inventory)
	return umbrellaSubcomponent{}
}

func TestExtractUmbrellaParams_TagsAndInventory(t *testing.T) {
	log := logrus.New()
	rel := umbrellaTestRelease(map[string]interface{}{
		"tags": map[string]interface{}{"node-autoscaler": true},
	})

	params := extractUmbrellaParams(context.Background(), log, rel, nil, "test-namespace")

	// Active tag mode, sanitized to bools only.
	tags, ok := params["tags"].(map[string]bool)
	assert.True(t, ok, "tags must be reported")
	assert.Equal(t, true, tags["node-autoscaler"])
	assert.Equal(t, false, tags["readonly"])
	assert.Len(t, tags, 4, "the chart-default tag modes are reported alongside the active one")

	// Inventory: the mode-tagged sub-components are enabled, the others are
	// not; versions are the release-resolved (requested) sub-chart versions.
	agent := inventoryEntry(t, params, "castai-agent")
	assert.True(t, agent.Enabled)
	assert.Equal(t, "0.161.0", agent.Version)

	spot := inventoryEntry(t, params, "castai-spot-handler")
	assert.True(t, spot.Enabled)
	assert.Equal(t, "0.35.2", spot.Version)

	kvisor := inventoryEntry(t, params, "castai-kvisor")
	assert.True(t, kvisor.Enabled)

	cc := inventoryEntry(t, params, "castai-cluster-controller")
	assert.True(t, cc.Enabled, "node-autoscaler enables the cluster-controller")
	assert.Equal(t, "0.92.4", cc.Version)

	// The kent profile is disabled by its chart default condition.
	kent := inventoryEntry(t, params, "kent")
	assert.False(t, kent.Enabled)
	kentroller := inventoryEntry(t, params, "castai-kentroller")
	assert.False(t, kentroller.Enabled, "disabled parent gates the sub-chart")

	// Flat flags: extended permissions because the cluster-controller is
	// enabled by the mode; the rest from the coalesced sub-chart values.
	assert.Equal(t, true, params["extendedPermissions"])
	autoscaling, ok := params["autoscaling"].(map[string]interface{})
	assert.True(t, ok, "autoscaling flag must be reported")
	assert.Equal(t, false, autoscaling["enabled"])
	workloadAutoscaling, ok := params["workloadAutoscaling"].(map[string]interface{})
	assert.True(t, ok, "workloadAutoscaling flag must be reported")
	assert.Equal(t, false, workloadAutoscaling["enabled"])
	_, hasPhase2 := params["phase2Permissions"]
	assert.True(t, hasPhase2, "phase2Permissions flag must be reported")
}

func TestExtractUmbrellaParams_ReadonlyMode(t *testing.T) {
	log := logrus.New()
	rel := umbrellaTestRelease(map[string]interface{}{
		"tags": map[string]interface{}{"readonly": true},
	})

	params := extractUmbrellaParams(context.Background(), log, rel, nil, "test-namespace")

	agent := inventoryEntry(t, params, "castai-agent")
	assert.True(t, agent.Enabled)
	cc := inventoryEntry(t, params, "castai-cluster-controller")
	assert.False(t, cc.Enabled, "readonly does not enable the cluster-controller")
	assert.Equal(t, false, params["extendedPermissions"])
}

func TestExtractUmbrellaParams_ExplicitOverridesBeatTagMode(t *testing.T) {
	log := logrus.New()
	rel := umbrellaTestRelease(map[string]interface{}{
		"tags": map[string]interface{}{"node-autoscaler": true},
		"autoscaler": map[string]interface{}{
			"castai-kvisor":             map[string]interface{}{"enabled": false},
			"castai-cluster-controller": map[string]interface{}{"enabled": true},
		},
	})

	params := extractUmbrellaParams(context.Background(), log, rel, nil, "test-namespace")

	kvisor := inventoryEntry(t, params, "castai-kvisor")
	assert.False(t, kvisor.Enabled, "explicit false wins over the active tag mode")

	// The cluster-controller is enabled here by the tag mode anyway; the
	// explicit-true-beats-mode case is covered by the readonly variant below.
}

func TestExtractUmbrellaParams_ExplicitTrueBeatsReadonlyMode(t *testing.T) {
	log := logrus.New()
	rel := umbrellaTestRelease(map[string]interface{}{
		"tags": map[string]interface{}{"readonly": true},
		"autoscaler": map[string]interface{}{
			"castai-cluster-controller": map[string]interface{}{"enabled": true},
		},
	})

	params := extractUmbrellaParams(context.Background(), log, rel, nil, "test-namespace")

	cc := inventoryEntry(t, params, "castai-cluster-controller")
	assert.True(t, cc.Enabled, "explicit true wins over the readonly tag mode")
	assert.Equal(t, true, params["extendedPermissions"])
}

func TestExtractUmbrellaParams_LiveWorkloadVersionWins(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)

	// A live agent Deployment owned by the umbrella release, running a newer
	// version than the release's resolved sub-chart (a stale release record).
	agentDeployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "castai-agent",
			Namespace: "test-namespace",
			Labels: map[string]string{
				labelAppName:    "castai-agent",
				labelAppVersion: "v0.170.0",
			},
			Annotations: map[string]string{
				helmReleaseNameAnnotation: "castai",
			},
		},
	}
	// A stale kvisor DaemonSet owned by the same release.
	kvisorDaemonSet := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "castai-kvisor",
			Namespace: "test-namespace",
			Labels: map[string]string{
				labelAppName:    "castai-kvisor",
				labelAppVersion: "v1.170.0",
			},
			Annotations: map[string]string{
				helmReleaseNameAnnotation: "castai",
			},
		},
	}
	// A standalone agent Deployment from a DIFFERENT release: must not shadow
	// the umbrella-owned workload's version.
	standaloneAgent := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "castai-agent-standalone",
			Namespace: "test-namespace",
			Labels: map[string]string{
				labelAppName:    "castai-agent",
				labelAppVersion: "v0.0.1",
			},
			Annotations: map[string]string{
				helmReleaseNameAnnotation: "some-other-release",
			},
		},
	}
	// A kentroller StatefulSet owned by the umbrella release, running a newer
	// version than the release's resolved sub-chart.
	kentrollerStatefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "castai-kentroller",
			Namespace: "test-namespace",
			Labels: map[string]string{
				labelAppName:    "castai-kentroller",
				labelAppVersion: "v0.2.0",
			},
			Annotations: map[string]string{
				helmReleaseNameAnnotation: "castai",
			},
		},
	}
	// A chart-rendered Job owned by the umbrella release: Jobs are transient
	// and must not contribute a (possibly stale) version to the inventory.
	transientJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "castai-kentroller-migrate",
			Namespace: "test-namespace",
			Labels: map[string]string{
				labelAppName:    "castai-kentroller",
				labelAppVersion: "v9.9.9",
			},
			Annotations: map[string]string{
				helmReleaseNameAnnotation: "castai",
			},
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(agentDeployment, kvisorDaemonSet, standaloneAgent, kentrollerStatefulSet, transientJob).
		Build()

	log := logrus.New()
	rel := umbrellaTestRelease(map[string]interface{}{
		"tags": map[string]interface{}{"node-autoscaler": true},
	})

	params := extractUmbrellaParams(context.Background(), log, rel, k8sClient, "test-namespace")

	agent := inventoryEntry(t, params, "castai-agent")
	assert.Equal(t, "0.170.0", agent.Version,
		"the live workload version must win over the stale release-resolved value")
	kvisor := inventoryEntry(t, params, "castai-kvisor")
	assert.Equal(t, "1.170.0", kvisor.Version, "daemonset workloads are included")
	kentroller := inventoryEntry(t, params, "castai-kentroller")
	assert.Equal(t, "0.2.0", kentroller.Version, "statefulset workloads are included")
	assert.NotEqual(t, "9.9.9", kentroller.Version, "transient job workloads must not contribute a version")
	cc := inventoryEntry(t, params, "castai-cluster-controller")
	assert.Equal(t, "0.92.4", cc.Version, "no live workload: requested version reported")
}

func TestExtractUmbrellaParams_TagsNonBoolValuesDropped(t *testing.T) {
	log := logrus.New()
	rel := umbrellaTestRelease(map[string]interface{}{
		"tags": map[string]interface{}{
			"node-autoscaler": true,
			"readonly":        "yes", // string: dropped
			"full":            nil,   // nil: dropped
			"workload-autoscaler": map[string]interface{}{ // map: dropped
				"nested": true,
			},
		},
	})

	params := extractUmbrellaParams(context.Background(), log, rel, nil, "test-namespace")

	// Only bool entries survive the sanitization — including entries whose
	// non-bool user override replaced the chart's bool default — so
	// Mothership keeps receiving the map[string]bool contract it flattens
	// into umbrella conditions.
	tags, ok := params["tags"].(map[string]bool)
	assert.True(t, ok, "tags must be reported")
	assert.Equal(t, map[string]bool{"node-autoscaler": true}, tags)
}

func TestUmbrellaInventory_MalformedChartNodes(t *testing.T) {
	// A root without metadata: the walk must return early instead of
	// panicking on the ch.Metadata.Dependencies dereference.
	assert.NotPanics(t, func() {
		assert.Empty(t, umbrellaInventory(&chart.Chart{}, map[string]interface{}{}, nil))
	})

	// A metadata-less node in the dependency tree is skipped; the healthy
	// sibling is still inventoried. (Helm's own AddDependency rejects nil
	// charts, so only the metadata-less shape is reachable here.)
	root := &chart.Chart{
		Metadata: &chart.Metadata{
			Name:    "castai",
			Version: "0.38.16",
			Dependencies: []*chart.Dependency{
				{Name: "castai-agent", Version: "0.161.0"},
			},
		},
	}
	root.AddDependency(&chart.Chart{}, &chart.Chart{
		Metadata: &chart.Metadata{Name: "castai-agent", Version: "0.161.0"},
	})

	assert.NotPanics(t, func() {
		assert.Equal(t, []umbrellaSubcomponent{
			{Name: "castai-agent", Version: "0.161.0", Enabled: true},
		}, umbrellaInventory(root, map[string]interface{}{}, nil))
	})
}

func TestExtractUmbrellaParams_NilRelease(t *testing.T) {
	log := logrus.New()
	params := extractUmbrellaParams(context.Background(), log, nil, nil, "test-namespace")
	assert.Empty(t, params)
}
