package values

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
)

func testCluster() *castwarev1alpha1.Cluster {
	return &castwarev1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "castai", Namespace: "castai-agent"},
		Spec: castwarev1alpha1.ClusterSpec{
			Provider:     "gke",
			APIKeySecret: "castware-api-key",
			API:          castwarev1alpha1.APISpec{APIURL: "https://api.cast.ai"},
		},
	}
}

func liveEnabled(m map[string]any) any {
	autoscaler, _ := m["autoscaler"].(map[string]any)
	live, _ := autoscaler["castai-live"].(map[string]any)
	return live["enabled"]
}

func TestUmbrellaValues_LiveDisabledByDefault(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	umbrella := component("castai-umbrella", "")
	out, err := UmbrellaValues(umbrella, testCluster(), nil)
	r.NoError(err)

	r.Equal(false, liveEnabled(out), "castai-live must be disabled by default (opt-in)")
}

func TestUmbrellaValues_LiveEnabledViaUserValues(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	umbrella := component("castai-umbrella", `{"autoscaler":{"castai-live":{"enabled":true}}}`)
	out, err := UmbrellaValues(umbrella, testCluster(), nil)
	r.NoError(err)

	r.Equal(true, liveEnabled(out), "user values must be able to opt into castai-live")
}

func TestUmbrellaValues_UserValuesWinOverExtraOverrides(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// extraOverrides (e.g. migration-derived tag values) merge UNDER the user's
	// own spec.values, so an explicit user value must win.
	umbrella := component("castai-umbrella", `{"autoscaler":{"castai-live":{"enabled":false}}}`)
	out, err := UmbrellaValues(umbrella, testCluster(), map[string]any{
		"autoscaler": map[string]any{"castai-live": map[string]any{"enabled": true}},
	})
	r.NoError(err)

	r.Equal(false, liveEnabled(out), "user-supplied values must override derived extraOverrides")
}

func TestUmbrellaValues_CoreCastAIValuesSet(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	cluster := testCluster()
	cluster.Spec.API.GrpcURL = "grpc.cast.ai:443"
	out, err := UmbrellaValues(component("castai-umbrella", ""), cluster, nil)
	r.NoError(err)

	global, ok := out["global"].(map[string]any)
	r.True(ok)
	castai, ok := global["castai"].(map[string]any)
	r.True(ok)
	r.Equal("https://api.cast.ai", castai["apiURL"])
	r.Equal("gke", castai["provider"])
	r.Equal("castware-api-key", castai["apiKeySecretRef"])
	r.Equal("grpc.cast.ai:443", castai["grpcURL"])
}

// kentSub returns the nested kent.<sub>.<key...> entry of the built values.
func kentSub(m map[string]any, sub string) map[string]any {
	kent, _ := m["kent"].(map[string]any)
	s, _ := kent[sub].(map[string]any)
	return s
}

func TestUmbrellaValues_KentMode_UserValuesArmKent(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	cluster := testCluster()
	cluster.Spec.Cluster = &castwarev1alpha1.ClusterMetadataSpec{ClusterID: "cluster-id"}
	cluster.Spec.API.KvisorGrpcURL = "kvisor.cast.ai:443"
	umbrella := component("castai-umbrella", `{"kent":{"enabled":true}}`)

	out, err := UmbrellaValues(umbrella, cluster, nil)
	r.NoError(err)

	r.NotContains(out, "autoscaler", "kent mode must not emit the autoscaler path")
	kent, ok := out["kent"].(map[string]any)
	r.True(ok, "kent block must be present")
	r.Equal(true, kent["enabled"])

	// The kvisor wiring is neutralized under kent.castai-kvisor.castai.
	kvisor := kentSub(out, "castai-kvisor")
	kvisorCastai, ok := kvisor["castai"].(map[string]any)
	r.True(ok, "kent.castai-kvisor.castai must be present")
	r.Equal("kvisor.cast.ai:443", kvisorCastai["grpcAddr"])
	r.Equal(map[string]any{"name": ""}, kvisorCastai["clusterIdConfigMapKeyRef"])
	r.Equal(map[string]any{"name": ""}, kvisorCastai["clusterIdSecretKeyRef"])

	// The chart-upgrader cronjob is force-disabled (operator owns upgrades).
	r.Equal(false, kentSub(out, "castai-chart-upgrader")["enabled"])
}

func TestUmbrellaValues_KentMode_ExtraOverridesArmKent(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// The migration arms kent mode via extraOverrides (derived profile for a
	// kent-shaped standalone set) while the user's own values stay empty.
	umbrella := component("castai-umbrella", "")
	out, err := UmbrellaValues(umbrella, testCluster(), map[string]any{
		"kent": map[string]any{"enabled": true},
	})
	r.NoError(err)

	r.NotContains(out, "autoscaler", "kent mode must not emit the autoscaler path")
	kent, ok := out["kent"].(map[string]any)
	r.True(ok)
	r.Equal(true, kent["enabled"])
	r.Equal(false, kentSub(out, "castai-chart-upgrader")["enabled"])
}

func TestUmbrellaValues_KentMode_UserValuesStillWin(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// User tuning under kent.* (and global) survives: user values merge on
	// top of the builder defaults and the extraOverrides.
	umbrella := component("castai-umbrella", `{
		"kent": {
			"enabled": true,
			"castai-live": {"controller": {"replicaCount": 2}},
			"metrics-server": {"enabled": true}
		},
		"global": {"castai": {"apiURL": "https://user.cast.ai"}}
	}`)
	out, err := UmbrellaValues(umbrella, testCluster(), map[string]any{
		"kent": map[string]any{"enabled": true},
	})
	r.NoError(err)

	r.Equal(float64(2), kentSub(out, "castai-live")["controller"].(map[string]any)["replicaCount"])
	r.Equal(true, kentSub(out, "metrics-server")["enabled"])

	global := out["global"].(map[string]any)
	castai := global["castai"].(map[string]any)
	r.Equal("https://user.cast.ai", castai["apiURL"], "user global values must win")
}

func TestUmbrellaValues_KentMode_ChartUpgraderForceDisableBeatsUser(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// The chart-upgrader cronjob is the one value that beats an explicit user
	// setting: it would race the operator's own upgrades.
	umbrella := component("castai-umbrella", `{
		"kent": {
			"enabled": true,
			"castai-chart-upgrader": {"enabled": true, "chart": {"name": "castai"}}
		}
	}`)
	out, err := UmbrellaValues(umbrella, testCluster(), nil)
	r.NoError(err)

	upgrader := kentSub(out, "castai-chart-upgrader")
	r.Equal(false, upgrader["enabled"], "the chart-upgrader force-disable must beat the user's enable")
	// The user's other keys under the same sub-chart survive the deep merge.
	r.Equal("castai", upgrader["chart"].(map[string]any)["name"])
}

func TestUmbrellaValues_NonKentMode_NoKentPathEmitted(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// Autoscaler mode: the builder emits no kent.* path and no
	// force-disable. A user's inert kent block (kent.enabled=false) is
	// carried through verbatim.
	tests := []struct {
		raw  string
		kent bool // whether the user's own values carry a kent block
	}{
		{raw: ""},
		{raw: `{"tags":{"readonly":true}}`},
		{raw: `{"kent":{"enabled":false}}`, kent: true},
	}
	for _, tc := range tests {
		out, err := UmbrellaValues(component("castai-umbrella", tc.raw), testCluster(), nil)
		r.NoError(err)
		r.NotNil(out["autoscaler"], "autoscaler defaults must be emitted (values %q)", tc.raw)
		r.Equal(false, liveEnabled(out))
		if tc.kent {
			// The user's inert kent block survives, but the builder must not add
			// the chart-upgrader force-disable to it.
			kent, ok := out["kent"].(map[string]any)
			r.True(ok, "user's inert kent block must be carried through (values %q)", tc.raw)
			r.Equal(false, kent["enabled"])
			r.NotContains(kent, "castai-chart-upgrader", "no force-disable outside kent mode")
		} else {
			r.NotContains(out, "kent", "non-kent mode must not emit the kent path (values %q)", tc.raw)
		}
	}
}
