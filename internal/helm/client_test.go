package helm

import (
	"errors"
	"fmt"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
)

// newTestClient builds a client whose configurationGetter returns an
// action.Configuration backed by an in-memory Helm storage driver, seeded
// with the provided releases. The memory driver exercises ForgetRelease's
// real storage interaction (ListReleases -> Delete per revision) without a
// cluster, which is more faithful than mocking the interface.
func newTestClient(t *testing.T, releases ...*release.Release) *client {
	t.Helper()
	mem := driver.NewMemory()
	store := storage.Init(mem)
	for _, r := range releases {
		// Seed via the storage wrapper so the key namespace matches what
		// ListReleases/Delete use at runtime.
		require.NoError(t, store.Create(r))
	}
	return &client{
		log: logrus.New(),
		configurationGetter: &fakeConfigurationGetter{cfg: &action.Configuration{
			Releases: store,
		}},
	}
}

type fakeConfigurationGetter struct {
	cfg *action.Configuration
}

func (f *fakeConfigurationGetter) Get(_ string) (*action.Configuration, error) {
	return f.cfg, nil
}

// newRelease is a minimal release fixture used to populate the storage driver.
// nolint: unparam
func newRelease(name, namespace string, version int) *release.Release {
	return &release.Release{
		Name:      name,
		Namespace: namespace,
		Version:   version,
		Info:      &release.Info{Status: release.StatusDeployed},
		Chart:     &chart.Chart{Metadata: &chart.Metadata{Name: name, Version: "1.0.0"}},
	}
}

func TestClient_ForgetRelease_MultipleRevisions(t *testing.T) {
	const ns = "castai-agent"
	// A release that was installed then upgraded twice: revisions 1, 2, 3.
	seeded := []*release.Release{
		newRelease("castai-agent", ns, 1),
		newRelease("castai-agent", ns, 2),
		newRelease("castai-agent", ns, 3),
		// An unrelated release that must survive the forget.
		newRelease("castai-monitor", ns, 1),
	}
	c := newTestClient(t, seeded...)

	err := c.ForgetRelease(ForgetReleaseOptions{Namespace: ns, ReleaseName: "castai-agent"})
	require.NoError(t, err)

	cfg, _ := c.configurationGetter.Get(ns) // fakeConfigurationGetter never errors
	store := cfg.Releases

	// Every revision of the target release is gone.
	for _, v := range []int{1, 2, 3} {
		_, err := store.Get("castai-agent", v)
		require.ErrorIs(t, err, driver.ErrReleaseNotFound, "revision %d should be deleted", v)
	}

	// The unrelated release is untouched.
	got, err := store.Get("castai-monitor", 1)
	require.NoError(t, err)
	require.Equal(t, "castai-monitor", got.Name)

	// No records named castai-agent remain in the full listing.
	remaining, err := store.ListReleases()
	require.NoError(t, err)
	for _, r := range remaining {
		require.NotEqual(t, "castai-agent", r.Name, "no castai-agent records should remain")
	}
}

func TestClient_ForgetRelease_AlreadyForgotten(t *testing.T) {
	const ns = "castai-agent"
	// Storage holds only an unrelated release; the target was never present
	// (or was already forgotten on a previous run).
	c := newTestClient(t, newRelease("castai-monitor", ns, 1))

	err := c.ForgetRelease(ForgetReleaseOptions{Namespace: ns, ReleaseName: "castai-agent"})
	require.NoError(t, err, "forgetting a missing release is a no-op")

	// Re-forgetting is idempotent.
	err = c.ForgetRelease(ForgetReleaseOptions{Namespace: ns, ReleaseName: "castai-agent"})
	require.NoError(t, err)
}

func TestClient_ForgetRelease_EmptyStorage(t *testing.T) {
	// An empty storage driver (no releases at all). ForgetRelease must not
	// error: ListReleases returns an empty slice, the loop body never runs.
	c := newTestClient(t)

	err := c.ForgetRelease(ForgetReleaseOptions{Namespace: "castai-agent", ReleaseName: "castai-agent"})
	require.NoError(t, err)
}

func TestClient_ForgetRelease_ListReleasesError(t *testing.T) {
	// A configuration getter whose storage driver returns an arbitrary error
	// from ListReleases (simulating a secrets-backend API failure). This is
	// neither ErrReleaseNotFound nor a string match, so it must surface.
	c := &client{
		log: logrus.New(),
		configurationGetter: &fakeConfigurationGetter{cfg: &action.Configuration{
			Releases: storage.Init(&errorDriver{listErr: errors.New("storage backend unavailable")}),
		}},
	}

	err := c.ForgetRelease(ForgetReleaseOptions{Namespace: "castai-agent", ReleaseName: "castai-agent"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "listing helm releases for forget")
	require.Contains(t, err.Error(), "storage backend unavailable")
}

func TestClient_ForgetRelease_ListReleasesNotFound(t *testing.T) {
	// A storage driver whose List returns the typed driver.ErrReleaseNotFound
	// sentinel (a hypothetical driver signaling "no records" via the sentinel
	// rather than an empty slice). ForgetRelease treats this as success.
	// Validates the errors.Is path and guards against a regression to string
	// matching: a wrapped sentinel must still be recognized.
	c := &client{
		log: logrus.New(),
		configurationGetter: &fakeConfigurationGetter{cfg: &action.Configuration{
			Releases: storage.Init(&errorDriver{listErr: fmt.Errorf("wrapped: %w", driver.ErrReleaseNotFound)}),
		}},
	}

	err := c.ForgetRelease(ForgetReleaseOptions{Namespace: "castai-agent", ReleaseName: "castai-agent"})
	require.NoError(t, err, "driver.ErrReleaseNotFound from ListReleases is a no-op forget")
}

// errorDriver is a storage.Driver stub whose List returns a configurable
// error, exercising ForgetRelease's ListReleases error path. All other
// methods are unreachable because the error is raised before the per-revision
// Delete loop runs.
type errorDriver struct {
	driver.Driver
	listErr error
}

func (d *errorDriver) Name() string { return "error" }

func (d *errorDriver) List(_ func(*release.Release) bool) ([]*release.Release, error) {
	return nil, d.listErr
}
