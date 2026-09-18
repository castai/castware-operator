package components

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
)

// The tag→component mapping in umbrella_tags.go is hand-curated from the
// published castai umbrella chart's autoscaler sub-chart. Nothing at runtime
// validates it against the chart the migration actually installs: if the chart
// changes and the mapping is not updated, migrations derive wrong tags —
// dropping components or requesting unneeded permissions. This test is that
// guard: it downloads the published umbrella chart and diffs its autoscaler
// sub-chart dependencies against UmbrellaTagComponents and
// UmbrellaCoveredComponents.
//
// It validates the LATEST published castai chart by default, so a chart
// release that changes the tag sets fails this test — that is the intended
// forcing function to update the mapping (or to consciously re-verify and
// pin via UMBRELLA_CHART_TEST_VERSION). Requires network access; when the
// repo cannot be reached the test skips loudly (CI runs it for real).

const (
	umbrellaChartRepoIndex = "https://castai.github.io/helm-charts/index.yaml"
	// umbrellaChartName is the published umbrella chart ("castai"); the legacy
	// "castai-umbrella" entry in the same repo is a different, older chart
	// without the autoscaler profile.
	umbrellaChartName = "castai"
)

// umbrellaChartIndex is the minimal slice of the helm repo index this test needs.
type umbrellaChartIndex struct {
	Entries map[string][]struct {
		Version string   `json:"version"`
		URLs    []string `json:"urls"`
	} `json:"entries"`
}

// TestUmbrellaTagComponentsMatchPublishedChart downloads the published
// umbrella chart and asserts the hand-curated tag→component mapping in this
// package matches the chart's autoscaler sub-chart dependencies exactly: the
// same tags, and per tag the same component sets.
func TestUmbrellaTagComponentsMatchPublishedChart(t *testing.T) {
	t.Parallel()

	version, urls := publishedUmbrellaChart(t)
	require.NotEmpty(t, urls, "index entry for chart %s version %s has no urls", umbrellaChartName, version)

	umbrella := downloadChart(t, urls[0], version)

	// The autoscaler profile sub-chart holds the mode dependencies.
	var autoscaler *chart.Chart
	for _, dep := range umbrella.Dependencies() {
		if dep != nil && dep.Metadata != nil && dep.Metadata.Name == "autoscaler" {
			autoscaler = dep
			break
		}
	}
	require.NotNil(t, autoscaler, "published chart %s has no autoscaler sub-chart (renamed?)", version)

	// Rebuild the chart's tag→component sets from the declared dependencies.
	fromChart := map[string][]string{}
	for _, dep := range autoscaler.Metadata.Dependencies {
		require.NotNil(t, dep, "nil dependency in the autoscaler sub-chart")
		require.NotEmpty(t, dep.Name, "dependency without a name in the autoscaler sub-chart")
		require.NotEmpty(t, dep.Tags, "dependency %s has no tags — the mapping cannot be derived", dep.Name)
		for _, tag := range dep.Tags {
			fromChart[tag] = append(fromChart[tag], dep.Name)
		}
	}

	// Same tags, exactly.
	require.ElementsMatch(t,
		[]string{UmbrellaTagReadonly, UmbrellaTagNodeAutoscaler, UmbrellaTagWorkloadAutoscaler, UmbrellaTagFull},
		keysOf(fromChart),
		"the autoscaler sub-chart of %s defines different tags than umbrella_tags.go — update the mapping (or the tag constants) for the new chart", version)

	// Same component sets per tag, exactly.
	for tag, want := range UmbrellaTagComponents {
		require.ElementsMatch(t, want, fromChart[tag],
			"tag %q differs from the published chart %s — update umbrella_tags.go: chart=%v compiled=%v",
			tag, version, fromChart[tag], want)
	}

	// The covered list is the union of all the chart's tag sets (every
	// component any autoscaler tag can install).
	union := map[string]bool{}
	for _, names := range fromChart {
		for _, name := range names {
			union[name] = true
		}
	}
	require.ElementsMatch(t, UmbrellaCoveredComponents, keysOf(union),
		"UmbrellaCoveredComponents differs from the union of the published chart %s's tag sets: chart=%v compiled=%v",
		version, keysOf(union), UmbrellaCoveredComponents)
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// publishedUmbrellaChart resolves which chart version to validate: the
// UMBRELLA_CHART_TEST_VERSION pin when set, otherwise the latest published
// entry. Returns the version and its download urls.
func publishedUmbrellaChart(t *testing.T) (string, []string) {
	t.Helper()

	body := httpGet(t, umbrellaChartRepoIndex)
	var idx umbrellaChartIndex
	if err := yaml.Unmarshal(body, &idx); err != nil {
		t.Fatalf("parse helm repo index: %v", err)
	}
	entries := idx.Entries[umbrellaChartName]
	if len(entries) == 0 {
		t.Fatalf("helm repo index has no %q chart", umbrellaChartName)
	}

	if pin := os.Getenv("UMBRELLA_CHART_TEST_VERSION"); pin != "" {
		for _, e := range entries {
			if e.Version == pin {
				return e.Version, e.URLs
			}
		}
		t.Fatalf("UMBRELLA_CHART_TEST_VERSION=%s not found in the %s repo index", pin, umbrellaChartName)
	}

	// Index entries are ordered newest-first; the latest is the drift guard.
	latest := entries[0]
	return latest.Version, latest.URLs
}

// downloadChart fetches and loads a chart archive.
func downloadChart(t *testing.T, url, version string) *chart.Chart {
	t.Helper()

	body := httpGet(t, url)
	ch, err := loader.LoadArchive(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("load chart %s archive from %s: %v", version, url, err)
	}
	if ch.Metadata == nil || ch.Metadata.Name != umbrellaChartName {
		t.Fatalf("archive at %s is chart %q, want %q", url, chartNameOr(ch), umbrellaChartName)
	}
	return ch
}

func chartNameOr(ch *chart.Chart) string {
	if ch.Metadata != nil {
		return ch.Metadata.Name
	}
	return "<nil metadata>"
}

// httpGet fetches url, skipping the test loudly when the network is
// unavailable so offline local runs are not blocked (CI runs it for real).
func httpGet(t *testing.T, url string) []byte {
	t.Helper()

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Skipf("cannot reach %s (%v) — skipping the umbrella chart drift check; it runs in CI", url, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("close response body from %s: %v", url, err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("unexpected status %d from %s — skipping the umbrella chart drift check; it runs in CI", resp.StatusCode, url)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Skipf("read response from %s: %v — skipping the umbrella chart drift check", url, err)
	}
	return body
}
