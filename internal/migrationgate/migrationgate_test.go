package migrationgate

import (
	"context"
	"errors"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"

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
