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
