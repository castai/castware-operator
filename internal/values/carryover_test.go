package values

import (
	"testing"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	components "github.com/castai/castware-operator/internal/component"
)

func jsonValues(raw string) *apiextensionsv1.JSON {
	if raw == "" {
		return nil
	}
	return &apiextensionsv1.JSON{Raw: []byte(raw)}
}

func component(name, rawValues string) *castwarev1alpha1.Component {
	return &castwarev1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "castai-agent"},
		Spec: castwarev1alpha1.ComponentSpec{
			Component: name,
			Values:    jsonValues(rawValues),
		},
	}
}

func TestCarryOverIndividualValues_AgentOnly(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	individuals := map[string]*castwarev1alpha1.Component{
		components.ComponentNameAgent: component(components.ComponentNameAgent, `{
			"topologySpreadConstraints": [{"maxSkew": 1, "topologyKey": "kubernetes.io/hostname"}],
			"additionalEnv": {"GKE_PROJECT_ID": "proj-1"}
		}`),
	}

	out := CarryOverIndividualValues(individuals, "eks")

	r.NotNil(out)
	as, ok := out["autoscaler"].(map[string]any)
	r.True(ok, "parent key should be autoscaler for eks")
	agent, ok := as["castai-agent"].(map[string]any)
	r.True(ok, "agent values nested under autoscaler.castai-agent")
	r.Len(agent["topologySpreadConstraints"].([]any), 1)
	r.Equal("proj-1", agent["additionalEnv"].(map[string]any)["GKE_PROJECT_ID"])
}

func TestCarryOverIndividualValues_MultipleSubcomponents(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	individuals := map[string]*castwarev1alpha1.Component{
		components.ComponentNameAgent:             component(components.ComponentNameAgent, `{"additionalEnv": {"EKS_REGION": "us-east-1"}}`),
		components.ComponentNameSpotHandler:       component(components.ComponentNameSpotHandler, `{"tolerations": [{"key": "spot"}]}`),
		components.ComponentNameClusterController: component(components.ComponentNameClusterController, `{"resources": {"limits": {"cpu": "1"}}}`),
	}

	out := CarryOverIndividualValues(individuals, "gke")

	r.NotNil(out)
	as := out["autoscaler"].(map[string]any)
	r.Contains(as, "castai-agent")
	r.Contains(as, "castai-spot-handler")
	r.Contains(as, "castai-cluster-controller")
}

func TestCarryOverIndividualValues_StripsUmbrellaManagedKeys(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	// A user who put credential-like keys directly in their agent CR spec.values.
	// These must NOT carry into autoscaler.castai-agent — the umbrella provisions
	// them via global.castai.* + the castai-credentials Secret, and carrying
	// them re-trips subchart template validation failures.
	individuals := map[string]*castwarev1alpha1.Component{
		components.ComponentNameAgent: component(components.ComponentNameAgent, `{
			"apiKey": "shh",
			"apiURL": "https://api.cast.ai",
			"provider": "eks",
			"clusterID": "abc",
			"castai": {"apiKey": "nested-shh"},
			"additionalEnv": {"EKS_REGION": "us-east-1"}
		}`),
	}

	out := CarryOverIndividualValues(individuals, "eks")
	as := out["autoscaler"].(map[string]any)
	agent := as["castai-agent"].(map[string]any)

	r.NotContains(agent, "apiKey")
	r.NotContains(agent, "apiURL")
	r.NotContains(agent, "provider")
	r.NotContains(agent, "clusterID")
	r.NotContains(agent, "castai")
	r.Contains(agent, "additionalEnv", "user value should survive stripping")
}

func TestCarryOverIndividualValues_StripsNestedManagedKeys(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	individuals := map[string]*castwarev1alpha1.Component{
		components.ComponentNameAgent: component(components.ComponentNameAgent, `{
			"image": {"repository": "castai/agent"},
			"castai": {"apiURL": "https://api.cast.ai", "nested": {"deep": true}}
		}`),
	}

	out := CarryOverIndividualValues(individuals, "eks")
	as := out["autoscaler"].(map[string]any)
	agent := as["castai-agent"].(map[string]any)

	r.NotContains(agent, "castai", "nested castai credentials map stripped entirely")
	r.Contains(agent, "image", "non-managed nested map preserved")
}

func TestCarryOverIndividualValues_OnlyManagedKeys_SkipsSubchart(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	// Every key is umbrella-managed → after stripping, nothing remains. The
	// subchart entry must be omitted (writing nil under autoscaler.<alias> makes
	// Helm's value coalescer panic).
	individuals := map[string]*castwarev1alpha1.Component{
		components.ComponentNameAgent:       component(components.ComponentNameAgent, `{"apiKey": "shh", "apiURL": "u", "provider": "eks"}`),
		components.ComponentNameSpotHandler: component(components.ComponentNameSpotHandler, `{"additionalEnv": {"A": "B"}}`),
	}

	out := CarryOverIndividualValues(individuals, "eks")
	as := out["autoscaler"].(map[string]any)
	r.NotContains(as, "castai-agent", "agent with only managed keys is skipped")
	r.Contains(as, "castai-spot-handler", "spot-handler with a real value is kept")
}

func TestCarryOverIndividualValues_EmptyAndNilValues(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	cases := []struct {
		name        string
		individuals map[string]*castwarev1alpha1.Component
	}{
		{"nil map", nil},
		{"empty map", map[string]*castwarev1alpha1.Component{}},
		{"nil spec.values", map[string]*castwarev1alpha1.Component{
			components.ComponentNameAgent: component(components.ComponentNameAgent, ""),
		}},
		{"empty json object", map[string]*castwarev1alpha1.Component{
			components.ComponentNameAgent: component(components.ComponentNameAgent, `{}`),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := CarryOverIndividualValues(tc.individuals, "eks")
			r.Nil(out, "no user-relevant values → nil result (caller skips merging)")
		})
	}
}

func TestCarryOverIndividualValues_UnknownComponentSkipped(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	individuals := map[string]*castwarev1alpha1.Component{
		"some-other-component":        component("some-other-component", `{"foo": "bar"}`),
		components.ComponentNameAgent: component(components.ComponentNameAgent, `{"additionalEnv": {"X": "Y"}}`),
	}

	out := CarryOverIndividualValues(individuals, "eks")
	as := out["autoscaler"].(map[string]any)
	r.NotContains(as, "some-other-component")
	r.Contains(as, "castai-agent")
}

func TestCarryOverIndividualValues_AnywhereProviderParentKey(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	individuals := map[string]*castwarev1alpha1.Component{
		components.ComponentNameAgent: component(components.ComponentNameAgent, `{"additionalEnv": {"ANYWHERE_CLUSTER_NAME": "c"}}`),
	}

	out := CarryOverIndividualValues(individuals, "anywhere")

	r.NotNil(out)
	r.Contains(out, "autoscaler-anywhere", "parent key should be autoscaler-anywhere for anywhere provider")
	r.NotContains(out, "autoscaler")
	as := out["autoscaler-anywhere"].(map[string]any)
	r.Contains(as, "castai-agent")
}

func TestCarryOverIndividualValues_MalformedJSONSkipped(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	individuals := map[string]*castwarev1alpha1.Component{
		components.ComponentNameAgent:       component(components.ComponentNameAgent, `{not valid json`),
		components.ComponentNameSpotHandler: component(components.ComponentNameSpotHandler, `{"additionalEnv": {"A": "B"}}`),
	}

	out := CarryOverIndividualValues(individuals, "eks")
	as := out["autoscaler"].(map[string]any)
	r.NotContains(as, "castai-agent", "malformed agent values skipped, not fatal")
	r.Contains(as, "castai-spot-handler", "valid spot-handler values still carried")
}

func TestStripUmbrellaManagedKeys_DeepCopyIsolatesCaller(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	original := map[string]any{
		"castai": map[string]any{"apiKey": "shh"},
		"image":  map[string]any{"repository": "r"},
	}

	stripped := stripUmbrellaManagedKeys(original)
	r.NotContains(stripped, "castai")
	// Caller's map is untouched.
	r.Contains(original, "castai", "stripUmbrellaManagedKeys must deep-copy, not mutate input")
}
