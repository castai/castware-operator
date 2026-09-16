package migrationgate

import (
	"context"
	"errors"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/castai/castware-operator/internal/castai"
	mock_castai "github.com/castai/castware-operator/internal/castai/mock"
	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/helm"
	mock_helm "github.com/castai/castware-operator/internal/helm/mock"
)

func TestIsUmbrellaOrSubcomponent(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		components.ComponentNameUmbrella:          true,
		components.ComponentNameAgent:             true,
		components.ComponentNameSpotHandler:       true,
		components.ComponentNameClusterController: true,
		components.ComponentNameOperator:          false,
		"some-other-component":                    false,
		"":                                        false,
	}
	for name, want := range cases {
		require.Equal(t, want, IsUmbrellaOrSubcomponent(name), "name=%q", name)
	}
}

func TestResolveUmbrellaReleaseName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("uses mothership release name when present", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mc := mock_castai.NewMockCastAIClient(ctrl)
		mc.EXPECT().GetComponentByName(ctx, components.ComponentNameUmbrella).
			Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: "umbrella-rel"}, nil)

		name, err := ResolveUmbrellaReleaseName(ctx, mc)
		r.NoError(err)
		r.Equal("umbrella-rel", name)
	})

	t.Run("falls back to component name when release name empty", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mc := mock_castai.NewMockCastAIClient(ctrl)
		mc.EXPECT().GetComponentByName(ctx, components.ComponentNameUmbrella).
			Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: ""}, nil)

		name, err := ResolveUmbrellaReleaseName(ctx, mc)
		r.NoError(err)
		r.Equal(components.ComponentNameUmbrella, name)
	})

	t.Run("surfaces mothership error", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mc := mock_castai.NewMockCastAIClient(ctrl)
		mc.EXPECT().GetComponentByName(ctx, components.ComponentNameUmbrella).
			Return(nil, errors.New("boom"))

		_, err := ResolveUmbrellaReleaseName(ctx, mc)
		r.Error(err)
	})
}

func TestUmbrellaInstalled(t *testing.T) {
	t.Parallel()
	ns := "castai-agent"

	t.Run("true when release found", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		hc := mock_helm.NewMockClient(ctrl)
		hc.EXPECT().GetRelease(helm.GetReleaseOptions{Namespace: ns, ReleaseName: "umbrella-rel"}).Return(&release.Release{}, nil)

		r.True(UmbrellaInstalled(hc, ns, "umbrella-rel"))
	})

	t.Run("false when release not found", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		hc := mock_helm.NewMockClient(ctrl)
		hc.EXPECT().GetRelease(helm.GetReleaseOptions{Namespace: ns, ReleaseName: "umbrella-rel"}).Return(nil, driver.ErrReleaseNotFound)

		r.False(UmbrellaInstalled(hc, ns, "umbrella-rel"))
	})

	t.Run("fail-safe true on unexpected helm error", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		hc := mock_helm.NewMockClient(ctrl)
		hc.EXPECT().GetRelease(helm.GetReleaseOptions{Namespace: ns, ReleaseName: "umbrella-rel"}).Return(nil, errors.New("helm unreachable"))

		// Any non-not-found error is treated as present so the gate blocks.
		r.True(UmbrellaInstalled(hc, ns, "umbrella-rel"))
	})

	t.Run("false when release name empty", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		hc := mock_helm.NewMockClient(ctrl)

		r.False(UmbrellaInstalled(hc, ns, ""))
	})
}

func TestInstalledSubcomponents(t *testing.T) {
	t.Parallel()
	ns := "castai-agent"

	t.Run("returns present sub-components in stable order", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		hc := mock_helm.NewMockClient(ctrl)

		releases := map[string]string{
			components.ComponentNameAgent:             "castai-agent",
			components.ComponentNameSpotHandler:       "castai-spot-handler",
			components.ComponentNameClusterController: "cluster-controller",
		}
		// Agent and cluster-controller present, spot-handler absent.
		hc.EXPECT().GetRelease(helm.GetReleaseOptions{Namespace: ns, ReleaseName: "castai-agent"}).Return(&release.Release{}, nil)
		hc.EXPECT().GetRelease(helm.GetReleaseOptions{Namespace: ns, ReleaseName: "castai-spot-handler"}).Return(nil, driver.ErrReleaseNotFound)
		hc.EXPECT().GetRelease(helm.GetReleaseOptions{Namespace: ns, ReleaseName: "cluster-controller"}).Return(&release.Release{}, nil)

		present := InstalledSubcomponents(hc, ns, releases)
		r.Equal([]string{components.ComponentNameAgent, components.ComponentNameClusterController}, present)
	})

	t.Run("skips sub-components with no resolved release name", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		hc := mock_helm.NewMockClient(ctrl)

		releases := map[string]string{
			components.ComponentNameAgent: "castai-agent",
			// spot-handler and cluster-controller unresolved.
		}
		hc.EXPECT().GetRelease(helm.GetReleaseOptions{Namespace: ns, ReleaseName: "castai-agent"}).Return(nil, driver.ErrReleaseNotFound)

		r.Empty(InstalledSubcomponents(hc, ns, releases))
	})
}

func TestResolveNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// subMocks sets up GetComponentByName expectations for the umbrella (always
	// called first) plus the given sub-component responses. A nil response error
	// means "return this component"; a non-nil error is returned as-is.
	subMocks := func(mc *mock_castai.MockCastAIClient, subs map[string]struct {
		comp *castai.Component
		err  error
	}) {
		mc.EXPECT().GetComponentByName(ctx, components.ComponentNameUmbrella).
			Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil)
		for _, sub := range Subcomponents {
			s, ok := subs[sub]
			if !ok {
				continue
			}
			mc.EXPECT().GetComponentByName(ctx, sub).Return(s.comp, s.err)
		}
	}

	t.Run("returns all resolved release names", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mc := mock_castai.NewMockCastAIClient(ctrl)
		subMocks(mc, map[string]struct {
			comp *castai.Component
			err  error
		}{
			components.ComponentNameAgent:             {comp: &castai.Component{ReleaseName: "castai-agent"}},
			components.ComponentNameSpotHandler:       {comp: &castai.Component{ReleaseName: "castai-spot-handler"}},
			components.ComponentNameClusterController: {comp: &castai.Component{ReleaseName: "cluster-controller"}},
		})

		names, err := ResolveNames(ctx, mc)
		r.NoError(err)
		r.Equal(components.ComponentNameUmbrella, names.UmbrellaReleaseName)
		r.Len(names.SubcomponentReleases, 3)
		r.Equal("castai-agent", names.SubcomponentReleases[components.ComponentNameAgent])
	})

	t.Run("surfaces umbrella lookup error", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mc := mock_castai.NewMockCastAIClient(ctrl)
		mc.EXPECT().GetComponentByName(ctx, components.ComponentNameUmbrella).Return(nil, errors.New("boom"))

		_, err := ResolveNames(ctx, mc)
		r.Error(err)
	})

	t.Run("skips genuinely-absent sub-components (ErrNotFound) and returns no error", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mc := mock_castai.NewMockCastAIClient(ctrl)
		subMocks(mc, map[string]struct {
			comp *castai.Component
			err  error
		}{
			components.ComponentNameAgent:             {err: castai.ErrNotFound},
			components.ComponentNameSpotHandler:       {err: castai.ErrNotFound},
			components.ComponentNameClusterController: {err: castai.ErrNotFound},
		})

		names, err := ResolveNames(ctx, mc)
		r.NoError(err, "ErrNotFound means genuinely absent, not unknown")
		r.Empty(names.SubcomponentReleases)
	})

	t.Run("fails closed when all sub-components hit unknown errors", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mc := mock_castai.NewMockCastAIClient(ctrl)
		subMocks(mc, map[string]struct {
			comp *castai.Component
			err  error
		}{
			components.ComponentNameAgent:             {err: errors.New("timeout")},
			components.ComponentNameSpotHandler:       {err: errors.New("timeout")},
			components.ComponentNameClusterController: {err: errors.New("timeout")},
		})

		_, err := ResolveNames(ctx, mc)
		r.Error(err, "unknown Mothership failures must not let the gate pass on an empty map")
	})

	t.Run("returns partial map when at least one sub-component resolves", func(t *testing.T) {
		// Even if other sub-components hit unknown errors, a partial map lets
		// the helm-layer fail-safe decide (it treats present releases as
		// blocking). Resolution must not error here.
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mc := mock_castai.NewMockCastAIClient(ctrl)
		subMocks(mc, map[string]struct {
			comp *castai.Component
			err  error
		}{
			components.ComponentNameAgent:             {comp: &castai.Component{ReleaseName: "castai-agent"}},
			components.ComponentNameSpotHandler:       {err: errors.New("timeout")},
			components.ComponentNameClusterController: {err: errors.New("timeout")},
		})

		names, err := ResolveNames(ctx, mc)
		r.NoError(err)
		r.Len(names.SubcomponentReleases, 1)
		r.Contains(names.SubcomponentReleases, components.ComponentNameAgent)
	})
}

// relWithChartDetails builds a release record with an explicit release name,
// chart name, chart version and release config (user-supplied values), for
// tests that exercise the detailed covered-release detection. A nil config
// stays nil: the migration keeps it rather than substituting an empty map.
func relWithChartDetails(releaseName, chartName, chartVersion string, config map[string]any) *release.Release {
	return &release.Release{
		Name:   releaseName,
		Chart:  &chart.Chart{Metadata: &chart.Metadata{Name: chartName, Version: chartVersion}},
		Config: config,
	}
}

// relWithChart builds a release record identified by the given chart name,
// with an arbitrary release name — InstalledUmbrellaOnlyCharts and
// InstalledCoveredStandaloneReleases must match on chart identity, not
// release name.
func relWithChart(chartName string) *release.Release {
	return relWithChartDetails("arbitrary-release-name", chartName, "1.0.0", nil)
}

func TestInstalledUmbrellaOnlyCharts(t *testing.T) {
	t.Parallel()
	ns := "castai-agent"

	t.Run("returns matching charts in list order", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		hc := mock_helm.NewMockClient(ctrl)

		// A kvisor and an evictor standalone under arbitrary release names,
		// plus unrelated releases that must not match (including the
		// operator-supported castai-agent, whose standalone release is the
		// migration's normal starting point, not a conflict).
		hc.EXPECT().ListReleases(helm.ListReleasesOptions{Namespace: ns}).Return([]*release.Release{
			relWithChart("castai-agent"),
			relWithChart("castai-kvisor"),
			relWithChart("metrics-server"),
			relWithChart("castai-evictor"),
		}, nil)

		present, err := InstalledUmbrellaOnlyCharts(hc, ns)
		r.NoError(err)
		r.Equal([]string{"castai-kvisor", "castai-evictor"}, present)
	})

	t.Run("no standalone umbrella-managed releases present", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		hc := mock_helm.NewMockClient(ctrl)

		hc.EXPECT().ListReleases(helm.ListReleasesOptions{Namespace: ns}).Return([]*release.Release{
			relWithChart("castai-agent"),
			relWithChart("castai-spot-handler"),
		}, nil)

		present, err := InstalledUmbrellaOnlyCharts(hc, ns)
		r.NoError(err)
		r.Empty(present)
	})

	t.Run("detects gpu-metrics-exporter standalone", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		hc := mock_helm.NewMockClient(ctrl)

		// Latent-gap fix: even the readonly umbrella mode renders
		// gpu-metrics-exporter, so a standalone release of it must be detected
		// before the umbrella install overlaps it.
		hc.EXPECT().ListReleases(helm.ListReleasesOptions{Namespace: ns}).Return([]*release.Release{
			relWithChart("castai-agent"),
			relWithChart(components.ComponentNameGPUMetricsExporter),
		}, nil)

		present, err := InstalledUmbrellaOnlyCharts(hc, ns)
		r.NoError(err)
		r.Equal([]string{components.ComponentNameGPUMetricsExporter}, present)
	})

	t.Run("result order follows UmbrellaCoveredCharts, not the release list", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		hc := mock_helm.NewMockClient(ctrl)

		// Feed the matching releases in an order that is the reverse of the
		// chart list order: the result must stay deterministic in
		// UmbrellaCoveredCharts order so callers' messaging is stable.
		hc.EXPECT().ListReleases(helm.ListReleasesOptions{Namespace: ns}).Return([]*release.Release{
			relWithChart("castai-live"),
			relWithChart(components.ComponentNameEvictor),
			relWithChart(components.ComponentNameGPUMetricsExporter),
		}, nil)

		present, err := InstalledUmbrellaOnlyCharts(hc, ns)
		r.NoError(err)
		r.Equal([]string{
			components.ComponentNameGPUMetricsExporter,
			components.ComponentNameEvictor,
			components.ComponentNameLive,
		}, present)
	})

	t.Run("helm listing error is returned so callers block (fail-safe)", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		hc := mock_helm.NewMockClient(ctrl)

		hc.EXPECT().ListReleases(helm.ListReleasesOptions{Namespace: ns}).Return(nil, errors.New("helm unreachable"))

		present, err := InstalledUmbrellaOnlyCharts(hc, ns)
		r.Error(err)
		r.Nil(present)
	})
}

func TestUmbrellaCoveredCharts(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	covered := map[string]bool{}
	for _, chart := range UmbrellaCoveredCharts {
		r.False(covered[chart], "duplicate chart %q", chart)
		covered[chart] = true
	}
	r.Len(covered, 8, "expected the 8 non-operator covered charts")

	// Operator-managed components are detected through Mothership-resolved
	// release names instead, so their presence here would make the covered-set
	// detection double-count them.
	for _, op := range []string{components.ComponentNameAgent, components.ComponentNameSpotHandler, components.ComponentNameClusterController} {
		r.False(covered[op], "%s must not be in UmbrellaCoveredCharts", op)
	}

	// gpu-metrics-exporter is rendered even in readonly umbrella mode, so its
	// standalone release must be covered (CID-1053 latent-gap fix).
	r.True(covered[components.ComponentNameGPUMetricsExporter], "gpu-metrics-exporter must be covered")

	// Every entry must be a chart the umbrella's autoscaler tags can render —
	// anything else is drift against components.UmbrellaCoveredComponents.
	for _, chart := range UmbrellaCoveredCharts {
		r.True(components.IsUmbrellaCoveredComponent(chart), "%s is not umbrella-covered", chart)
	}
}

func TestInstalledCoveredStandaloneReleases(t *testing.T) {
	t.Parallel()
	ns := "castai-agent"

	t.Run("returns full details for matching releases under arbitrary names", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		hc := mock_helm.NewMockClient(ctrl)

		kvisorConfig := map[string]any{"kvisor": map[string]any{"scanJobsEnabled": true}}
		hc.EXPECT().ListReleases(helm.ListReleasesOptions{Namespace: ns}).Return([]*release.Release{
			relWithChartDetails("alpha-kvisor", components.ComponentNameKvisor, "1.2.3", kvisorConfig),
			relWithChartDetails("zulu-exporter", components.ComponentNameWorkloadAutoscalerExporter, "0.9.1", nil),
		}, nil)

		releases, err := InstalledCoveredStandaloneReleases(hc, ns)
		r.NoError(err)
		r.Len(releases, 2)
		r.Equal(CoveredStandaloneRelease{
			ReleaseName:  "alpha-kvisor",
			ChartName:    components.ComponentNameKvisor,
			ChartVersion: "1.2.3",
			Config:       kvisorConfig,
		}, releases[0])
		r.Equal(CoveredStandaloneRelease{
			ReleaseName:  "zulu-exporter",
			ChartName:    components.ComponentNameWorkloadAutoscalerExporter,
			ChartVersion: "0.9.1",
			Config:       nil, // nil release config stays nil, no empty-map substitution
		}, releases[1])
	})

	t.Run("skips unrelated charts", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		hc := mock_helm.NewMockClient(ctrl)

		// castai-agent is operator-managed (probed via Mothership-resolved
		// release names); metrics-server is a kent-profile chart the migration
		// never enables; something-else is not umbrella-covered at all.
		hc.EXPECT().ListReleases(helm.ListReleasesOptions{Namespace: ns}).Return([]*release.Release{
			relWithChart("castai-agent"),
			relWithChart("metrics-server"),
			relWithChart("something-else"),
		}, nil)

		releases, err := InstalledCoveredStandaloneReleases(hc, ns)
		r.NoError(err)
		r.Empty(releases)
	})

	t.Run("tolerates nil Chart and nil Chart.Metadata entries", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		hc := mock_helm.NewMockClient(ctrl)

		hc.EXPECT().ListReleases(helm.ListReleasesOptions{Namespace: ns}).Return([]*release.Release{
			{Name: "no-chart"},
			{Name: "no-metadata", Chart: &chart.Chart{}},
			relWithChartDetails("kvisor-rel", components.ComponentNameKvisor, "1.2.3", nil),
		}, nil)

		releases, err := InstalledCoveredStandaloneReleases(hc, ns)
		r.NoError(err)
		r.Len(releases, 1)
		r.Equal("kvisor-rel", releases[0].ReleaseName)
	})

	t.Run("empty result when no releases exist", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		hc := mock_helm.NewMockClient(ctrl)

		hc.EXPECT().ListReleases(helm.ListReleasesOptions{Namespace: ns}).Return([]*release.Release{}, nil)

		releases, err := InstalledCoveredStandaloneReleases(hc, ns)
		r.NoError(err)
		r.Empty(releases)
	})

	t.Run("helm listing error is returned so callers block (fail-safe)", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		hc := mock_helm.NewMockClient(ctrl)

		hc.EXPECT().ListReleases(helm.ListReleasesOptions{Namespace: ns}).Return(nil, errors.New("helm unreachable"))

		releases, err := InstalledCoveredStandaloneReleases(hc, ns)
		r.Error(err)
		r.Nil(releases)
	})

	t.Run("returns all releases of a chart, sorted by release name", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		hc := mock_helm.NewMockClient(ctrl)

		// One chart with several standalone releases plus another covered chart:
		// every matching release is returned, deterministically ordered by
		// ReleaseName regardless of the helm list order.
		hc.EXPECT().ListReleases(helm.ListReleasesOptions{Namespace: ns}).Return([]*release.Release{
			relWithChartDetails("zebra-kvisor", components.ComponentNameKvisor, "1.1.0", nil),
			relWithChartDetails("alpha-kvisor", components.ComponentNameKvisor, "1.0.0", nil),
			relWithChartDetails("mid-evictor", components.ComponentNameEvictor, "2.0.0", nil),
			relWithChartDetails("beta-kvisor", components.ComponentNameKvisor, "1.2.0", nil),
		}, nil)

		releases, err := InstalledCoveredStandaloneReleases(hc, ns)
		r.NoError(err)
		r.Equal([]string{"alpha-kvisor", "beta-kvisor", "mid-evictor", "zebra-kvisor"},
			[]string{
				releases[0].ReleaseName,
				releases[1].ReleaseName,
				releases[2].ReleaseName,
				releases[3].ReleaseName,
			})
		r.Len(releases, 4)
	})
}

func TestValidateUmbrellaInstallPermissions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("sends the user values as component_params", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mc := mock_castai.NewMockCastAIClient(ctrl)
		mc.EXPECT().ValidateComponentInstall(ctx, &castai.ValidateComponentInstallRequest{
			ClusterID:       "cluster-id",
			ComponentName:   components.ComponentNameUmbrella,
			TargetVersion:   "1.2.3",
			ComponentParams: map[string]any{"tags": map[string]any{"readonly": true}},
		}).Return(&castai.ValidateComponentInstallResponse{Allowed: true}, nil)

		resp, err := ValidateUmbrellaInstallPermissions(ctx, mc, "cluster-id", "1.2.3", "",
			&apiextensionsv1.JSON{Raw: []byte(`{"tags":{"readonly":true}}`)})
		r.NoError(err)
		r.True(resp.Allowed)
		r.Empty(resp.BlockReason)
	})

	t.Run("derived tag is merged under the user values as the effective surface", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mc := mock_castai.NewMockCastAIClient(ctrl)
		// User keeps its own tag; the migration's derived tag adds its key
		// alongside it — the surface UmbrellaValues will actually install.
		mc.EXPECT().ValidateComponentInstall(ctx, &castai.ValidateComponentInstallRequest{
			ClusterID:     "cluster-id",
			ComponentName: components.ComponentNameUmbrella,
			TargetVersion: "1.2.3",
			ComponentParams: map[string]any{
				"tags": map[string]any{"readonly": true, "node-autoscaler": true},
			},
		}).Return(&castai.ValidateComponentInstallResponse{Allowed: true}, nil)

		resp, err := ValidateUmbrellaInstallPermissions(ctx, mc, "cluster-id", "1.2.3", "node-autoscaler",
			&apiextensionsv1.JSON{Raw: []byte(`{"tags":{"readonly":true}}`)})
		r.NoError(err)
		r.True(resp.Allowed)
	})

	t.Run("derived tag with no user values sends the tag alone", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mc := mock_castai.NewMockCastAIClient(ctrl)
		mc.EXPECT().ValidateComponentInstall(ctx, &castai.ValidateComponentInstallRequest{
			ClusterID:       "cluster-id",
			ComponentName:   components.ComponentNameUmbrella,
			TargetVersion:   "1.2.3",
			ComponentParams: map[string]any{"tags": map[string]any{"full": true}},
		}).Return(&castai.ValidateComponentInstallResponse{Allowed: true}, nil)

		resp, err := ValidateUmbrellaInstallPermissions(ctx, mc, "cluster-id", "1.2.3", "full", nil)
		r.NoError(err)
		r.True(resp.Allowed)
	})

	t.Run("nil values are sent as no params", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mc := mock_castai.NewMockCastAIClient(ctrl)
		mc.EXPECT().ValidateComponentInstall(ctx, &castai.ValidateComponentInstallRequest{
			ClusterID:       "cluster-id",
			ComponentName:   components.ComponentNameUmbrella,
			TargetVersion:   "1.2.3",
			ComponentParams: map[string]any{},
		}).Return(&castai.ValidateComponentInstallResponse{Allowed: true}, nil)

		resp, err := ValidateUmbrellaInstallPermissions(ctx, mc, "cluster-id", "1.2.3", "", nil)
		r.NoError(err)
		r.True(resp.Allowed)
	})

	t.Run("empty block reason is normalized", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mc := mock_castai.NewMockCastAIClient(ctrl)
		mc.EXPECT().ValidateComponentInstall(gomock.Any(), gomock.Any()).
			Return(&castai.ValidateComponentInstallResponse{Allowed: false}, nil)

		resp, err := ValidateUmbrellaInstallPermissions(ctx, mc, "cluster-id", "1.2.3", "", nil)
		r.NoError(err)
		r.False(resp.Allowed)
		r.Equal("missing permissions", resp.BlockReason)
	})

	t.Run("server block reason is preserved", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mc := mock_castai.NewMockCastAIClient(ctrl)
		mc.EXPECT().ValidateComponentInstall(gomock.Any(), gomock.Any()).
			Return(&castai.ValidateComponentInstallResponse{
				Allowed:     false,
				BlockReason: "service account lacks permissions for the umbrella chart",
			}, nil)

		resp, err := ValidateUmbrellaInstallPermissions(ctx, mc, "cluster-id", "1.2.3", "", nil)
		r.NoError(err)
		r.False(resp.Allowed)
		r.Equal("service account lacks permissions for the umbrella chart", resp.BlockReason)
	})

	t.Run("invalid values JSON fails before calling Mothership", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mc := mock_castai.NewMockCastAIClient(ctrl)

		resp, err := ValidateUmbrellaInstallPermissions(ctx, mc, "cluster-id", "1.2.3", "",
			&apiextensionsv1.JSON{Raw: []byte(`not-json`)})
		r.Error(err)
		r.ErrorContains(err, "unmarshal umbrella values for permission gate")
		r.Nil(resp)
	})

	t.Run("transport error is returned as-is for caller wrapping", func(t *testing.T) {
		r := require.New(t)
		ctrl := gomock.NewController(t)
		mc := mock_castai.NewMockCastAIClient(ctrl)
		mc.EXPECT().ValidateComponentInstall(gomock.Any(), gomock.Any()).
			Return(nil, errors.New("connection refused"))

		resp, err := ValidateUmbrellaInstallPermissions(ctx, mc, "cluster-id", "1.2.3", "", nil)
		r.EqualError(err, "connection refused")
		r.Nil(resp)
	})
}
