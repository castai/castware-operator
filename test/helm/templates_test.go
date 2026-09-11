package helmtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const chartPath = "../../charts/castai-castware-operator"

// renderChart runs `helm template` with the given --set flags and returns the
// rendered manifest as a string.
func renderChart(t *testing.T, sets ...string) string {
	t.Helper()
	abs, err := filepath.Abs(chartPath)
	if err != nil {
		t.Fatalf("resolve chart path: %v", err)
	}
	args := []string{"template", abs, "--set", "apiKeySecret.apiKey=test"} //nolint:prealloc
	args = append(args, sets...)
	cmd := exec.Command("helm", args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template failed: %v", err)
	}
	return string(out)
}

// extractComponentManifests pulls out the post-install Component CRs (hook-weight "10")
// from a full helm template render.
func extractComponentManifests(t *testing.T, rendered string) []string {
	t.Helper()
	docs := strings.Split(rendered, "\n---\n")
	var components []string
	for _, doc := range docs {
		if strings.Contains(doc, "kind: Component") && strings.Contains(doc, `hook-weight: "10"`) {
			components = append(components, doc)
		}
	}
	return components
}

func TestUmbrellaPath_MinimalPermissions(t *testing.T) {
	rendered := renderChart(t,
		"--set", "defaultComponents.umbrella.enabled=true",
		"--set", "extendedPermissions=false",
	)

	components := extractComponentManifests(t, rendered)
	if len(components) != 1 {
		t.Fatalf("expected 1 Component CR, got %d", len(components))
	}

	comp := components[0]
	if !strings.Contains(comp, `name: castai-umbrella`) {
		t.Errorf("expected Component name castai-umbrella, got:\n%s", comp)
	}
	if !strings.Contains(comp, "component: \"castai-umbrella\"") {
		t.Errorf("expected spec.component castai-umbrella")
	}
	if !strings.Contains(comp, "readonly: true") {
		t.Errorf("expected tags.readonly=true for minimal permissions")
	}
	if strings.Contains(comp, "full: true") {
		t.Errorf("tags.full should not be set for minimal permissions")
	}
}

func TestUmbrellaPath_ExtendedPermissions(t *testing.T) {
	rendered := renderChart(t,
		"--set", "defaultComponents.umbrella.enabled=true",
		"--set", "extendedPermissions=true",
	)

	components := extractComponentManifests(t, rendered)
	if len(components) != 1 {
		t.Fatalf("expected 1 Component CR, got %d", len(components))
	}

	comp := components[0]
	if !strings.Contains(comp, "full: true") {
		t.Errorf("expected tags.full=true for extended permissions")
	}
	if strings.Contains(comp, "readonly: true") {
		t.Errorf("tags.readonly should not be set for extended permissions")
	}
}

func TestUmbrellaPath_DefaultExtendedPermissionsUnset(t *testing.T) {
	// When extendedPermissions is not set at all, it defaults to false,
	// so the umbrella should get tags.readonly=true.
	rendered := renderChart(t,
		"--set", "defaultComponents.umbrella.enabled=true",
	)

	components := extractComponentManifests(t, rendered)
	if len(components) != 1 {
		t.Fatalf("expected 1 Component CR, got %d", len(components))
	}

	comp := components[0]
	if !strings.Contains(comp, "readonly: true") {
		t.Errorf("expected tags.readonly=true when extendedPermissions is unset (defaults to false)")
	}
	if strings.Contains(comp, "full: true") {
		t.Errorf("tags.full should not be set when extendedPermissions is unset")
	}
}

func TestUmbrellaPath_CustomTagsOverride(t *testing.T) {
	rendered := renderChart(t,
		"--set", "defaultComponents.umbrella.enabled=true",
		"--set", "extendedPermissions=true",
		"--set", "defaultComponents.umbrella.tags.readonly=true",
	)

	components := extractComponentManifests(t, rendered)
	if len(components) != 1 {
		t.Fatalf("expected 1 Component CR, got %d", len(components))
	}

	comp := components[0]
	if !strings.Contains(comp, "readonly: true") {
		t.Errorf("expected custom tags.readonly=true to override extendedPermissions")
	}
	if strings.Contains(comp, "full: true") {
		t.Errorf("tags.full should not appear when custom tags override")
	}
}

func TestUmbrellaPath_CustomComponentAndCluster(t *testing.T) {
	rendered := renderChart(t,
		"--set", "defaultComponents.umbrella.enabled=true",
		"--set", "defaultComponents.umbrella.component=custom-umbrella",
		"--set", "defaultComponents.umbrella.cluster=custom-cluster",
	)

	components := extractComponentManifests(t, rendered)
	if len(components) != 1 {
		t.Fatalf("expected 1 Component CR, got %d", len(components))
	}

	comp := components[0]
	if !strings.Contains(comp, `name: custom-umbrella`) {
		t.Errorf("expected Component name custom-umbrella, got:\n%s", comp)
	}
	if !strings.Contains(comp, "component: \"custom-umbrella\"") {
		t.Errorf("expected spec.component custom-umbrella")
	}
	if !strings.Contains(comp, "cluster: \"custom-cluster\"") {
		t.Errorf("expected spec.cluster custom-cluster")
	}
}

func TestIndividualPath_NoUmbrellaCR(t *testing.T) {
	rendered := renderChart(t,
		"--set", "defaultComponents.umbrella.enabled=false",
	)

	components := extractComponentManifests(t, rendered)
	// Default values.yaml has castai-agent and spot-handler
	if len(components) != 2 {
		t.Fatalf("expected 2 individual Component CRs (castai-agent, spot-handler), got %d", len(components))
	}

	for _, comp := range components {
		if strings.Contains(comp, "castai-umbrella") {
			t.Errorf("umbrella CR should not be rendered when umbrella.enabled=false")
		}
	}

	// Verify castai-agent is present
	foundAgent := false
	foundSpotHandler := false
	for _, comp := range components {
		if strings.Contains(comp, "component: \"castai-agent\"") {
			foundAgent = true
		}
		if strings.Contains(comp, "component: \"spot-handler\"") {
			foundSpotHandler = true
		}
	}
	if !foundAgent {
		t.Errorf("expected castai-agent in individual path defaults")
	}
	if !foundSpotHandler {
		t.Errorf("expected spot-handler in individual path defaults")
	}
}

func TestIndividualPath_ExplicitComponents(t *testing.T) {
	// Use a values file to override components entirely, since --set cannot
	// clear a map. This tests that the individual path renders custom
	// components correctly.
	valuesFile, err := os.CreateTemp("", "test-values-*.yaml")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer func() {
		_ = os.Remove(valuesFile.Name())
	}()

	valuesContent := `
defaultComponents:
  umbrella:
    enabled: false
  components:
    castai-agent:
      component: "castai-agent"
      cluster: "castai"
      enabled: false
    spot-handler:
      component: "spot-handler"
      cluster: "castai"
      enabled: false
    cluster-controller:
      component: "cluster-controller"
      cluster: "castai"
      enabled: true
`
	if _, err := valuesFile.WriteString(valuesContent); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	_ = valuesFile.Close()

	abs, err := filepath.Abs(chartPath)
	if err != nil {
		t.Fatalf("resolve chart path: %v", err)
	}
	cmd := exec.Command("helm", "template", abs,
		"--set", "apiKeySecret.apiKey=test",
		"-f", valuesFile.Name(),
	)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template failed: %v", err)
	}

	components := extractComponentManifests(t, string(out))
	// The template renders all entries in the components map, including
	// disabled ones (with enabled: false in spec). We expect 3 CRs total:
	// castai-agent (disabled), spot-handler (disabled), cluster-controller (enabled).
	if len(components) != 3 {
		t.Fatalf("expected 3 Component CRs (2 disabled + 1 enabled), got %d", len(components))
	}

	// Verify only cluster-controller is enabled.
	var foundEnabled bool
	for _, comp := range components {
		if strings.Contains(comp, "component: \"cluster-controller\"") {
			if !strings.Contains(comp, "enabled: true") {
				t.Errorf("cluster-controller should be enabled")
			}
			foundEnabled = true
		}
	}
	if !foundEnabled {
		t.Errorf("expected cluster-controller component in rendered output")
	}
}

func TestUmbrellaPath_NoIndividualComponentCRs(t *testing.T) {
	rendered := renderChart(t,
		"--set", "defaultComponents.umbrella.enabled=true",
		"--set", "extendedPermissions=true",
	)

	components := extractComponentManifests(t, rendered)
	if len(components) != 1 {
		t.Fatalf("expected exactly 1 umbrella Component CR, got %d", len(components))
	}

	comp := components[0]
	for _, name := range []string{"castai-agent", "spot-handler"} {
		if strings.Contains(comp, "component: \""+name+"\"") {
			t.Errorf("individual component %q should not be rendered in umbrella path", name)
		}
	}
}

func TestDisabled_NoComponentCRs(t *testing.T) {
	rendered := renderChart(t,
		"--set", "defaultComponents.enabled=false",
	)

	components := extractComponentManifests(t, rendered)
	if len(components) != 0 {
		t.Fatalf("expected 0 Component CRs when defaultComponents.enabled=false, got %d", len(components))
	}
}

func TestHelmLint_UmbrellaPath(t *testing.T) {
	t.Helper()
	abs, err := filepath.Abs(chartPath)
	if err != nil {
		t.Fatalf("resolve chart path: %v", err)
	}
	cmd := exec.Command("helm", "lint", abs,
		"--set", "apiKeySecret.apiKey=test",
		"--set", "defaultComponents.umbrella.enabled=true",
		"--set", "extendedPermissions=true",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm lint failed: %v\n%s", err, out)
	}
	res := string(out)
	if !strings.Contains(res, "0 chart(s) failed") {
		t.Fatalf("helm lint reported failures:\n%s", res)
	}
}

func TestHelmLint_IndividualPath(t *testing.T) {
	t.Helper()
	abs, err := filepath.Abs(chartPath)
	if err != nil {
		t.Fatalf("resolve chart path: %v", err)
	}
	cmd := exec.Command("helm", "lint", abs,
		"--set", "apiKeySecret.apiKey=test",
		"--set", "defaultComponents.umbrella.enabled=false",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm lint failed: %v\n%s", err, out)
	}
	res := string(out)
	if !strings.Contains(res, "0 chart(s) failed") {
		t.Fatalf("helm lint reported failures:\n%s", res)
	}
}
