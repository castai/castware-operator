package selfupgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/castai"
	"github.com/castai/castware-operator/internal/config"
	"github.com/castai/castware-operator/internal/helm"
	mock_helm "github.com/castai/castware-operator/internal/helm/mock"
	"github.com/golang/mock/gomock"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSelfUpgrade(t *testing.T) {
	const getByNamePath = "/cluster-management/v1/components:getByName"
	actionResultUrlRegex := regexp.MustCompile("/cluster-management/v1/clusters/(.*?)/components:recordActionResult")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Mock the GetComponentByName API endpoint
		if r.URL.Path == getByNamePath {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			// Return a mock component response
			_, _ = w.Write([]byte(`{
				"id": "test-id",
				"name": "castware-operator",
				"helmChart": "castware-operator",
				"dependencies": [],
				"latestVersion": "v0.2.0"
			}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	t.Run("should upgrade the helm chart successfully", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		testCluster := newTestCluster(t, server)
		testOps := newTestOps(t, testCluster)

		// Create a fake secret for API key
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-api-secret",
				Namespace: "test-namespace",
			},
			Data: map[string][]byte{
				"API_KEY": []byte("test-api-key"),
			},
		}
		r.NoError(testOps.sut.Create(ctx, secret))

		// Mock the initial GetRelease call
		initialRelease := createMockRelease("castware-operator", "0.1.0", "test-namespace")
		testOps.mockHelm.EXPECT().
			GetRelease(gomock.Any()).
			Return(initialRelease, nil).
			Times(1)

		// Mock the dry run upgrade
		testOps.mockHelm.EXPECT().
			Upgrade(gomock.Any(), gomock.Any()).
			DoAndReturn(func(ctx context.Context, opts helm.UpgradeOptions) (*release.Release, error) {
				r.True(opts.DryRun, "First upgrade call should be a dry run")
				r.Equal("v0.1.1", opts.ChartSource.Version)
				return createMockRelease("castware-operator", "v0.1.1", "test-namespace"), nil
			}).
			Times(1)

		// Mock the actual upgrade
		upgradedRelease := createMockRelease("castware-operator", "v0.1.1", "test-namespace")
		upgradedRelease.Info.Status = release.StatusPendingUpgrade
		testOps.mockHelm.EXPECT().
			Upgrade(gomock.Any(), gomock.Any()).
			DoAndReturn(func(ctx context.Context, opts helm.UpgradeOptions) (*release.Release, error) {
				r.False(opts.DryRun, "Second upgrade call should not be a dry run")
				return upgradedRelease, nil
			}).
			Times(1)

		// Mock GetRelease to check status (returns deployed)
		deployedRelease := createMockRelease("castware-operator", "v0.1.1", "test-namespace")
		deployedRelease.Info.Status = release.StatusDeployed
		testOps.mockHelm.EXPECT().
			GetRelease(gomock.Any()).
			Return(deployedRelease, nil).
			MinTimes(1)

		err := testOps.sut.Run(ctx, "v0.1.1")
		r.NoError(err)
	})

	t.Run("should fail when dry run upgrade fails", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()

		testCluster := newTestCluster(t, server)
		testOps := newTestOps(t, testCluster)

		// Create a fake secret for API key
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-api-secret",
				Namespace: "test-namespace",
			},
			Data: map[string][]byte{
				"API_KEY": []byte("test-api-key"),
			},
		}
		r.NoError(testOps.sut.Create(ctx, secret))

		// Mock the initial GetRelease call
		initialRelease := createMockRelease("castware-operator", "0.1.0", "test-namespace")
		testOps.mockHelm.EXPECT().
			GetRelease(gomock.Any()).
			Return(initialRelease, nil).
			Times(1)

		// Mock the dry run upgrade to fail
		expectedErr := fmt.Errorf("dry run validation failed")
		testOps.mockHelm.EXPECT().
			Upgrade(gomock.Any(), gomock.Any()).
			DoAndReturn(func(ctx context.Context, opts helm.UpgradeOptions) (*release.Release, error) {
				r.True(opts.DryRun, "Should be a dry run")
				return nil, expectedErr
			}).
			Times(1)

		err := testOps.sut.Run(ctx, "v0.1.1")
		r.Error(err)
		r.Contains(err.Error(), "upgrade dry run failed")
	})

	t.Run("should rollback when upgrade fails", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()

		testCluster := newTestCluster(t, server)
		testOps := newTestOps(t, testCluster)

		// Create a fake secret for API key
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-api-secret",
				Namespace: "test-namespace",
			},
			Data: map[string][]byte{
				"API_KEY": []byte("test-api-key"),
			},
		}
		r.NoError(testOps.sut.Create(ctx, secret))

		// Mock the initial GetRelease call
		initialRelease := createMockRelease("castware-operator", "0.1.0", "test-namespace")
		testOps.mockHelm.EXPECT().
			GetRelease(gomock.Any()).
			Return(initialRelease, nil).
			Times(1)

		// Mock the dry run upgrade (success)
		testOps.mockHelm.EXPECT().
			Upgrade(gomock.Any(), gomock.Any()).
			DoAndReturn(func(ctx context.Context, opts helm.UpgradeOptions) (*release.Release, error) {
				r.True(opts.DryRun)
				return createMockRelease("castware-operator", "v0.1.1", "test-namespace"), nil
			}).
			Times(1)

		// Mock the actual upgrade (success but will fail during status check)
		upgradedRelease := createMockRelease("castware-operator", "v0.1.1", "test-namespace")
		upgradedRelease.Info.Status = release.StatusPendingUpgrade
		testOps.mockHelm.EXPECT().
			Upgrade(gomock.Any(), gomock.Any()).
			DoAndReturn(func(ctx context.Context, opts helm.UpgradeOptions) (*release.Release, error) {
				r.False(opts.DryRun)
				return upgradedRelease, nil
			}).
			Times(1)

		// Mock GetRelease to return failed status
		failedRelease := createMockRelease("castware-operator", "v0.1.1", "test-namespace")
		failedRelease.Info.Status = release.StatusFailed
		failedRelease.Info.Description = "upgrade failed due to pod crash"
		testOps.mockHelm.EXPECT().
			GetRelease(gomock.Any()).
			Return(failedRelease, nil).
			MinTimes(1)

		// Mock the rollback
		testOps.mockHelm.EXPECT().
			Rollback(gomock.Any()).
			DoAndReturn(func(opts helm.RollbackOptions) error {
				r.Equal("test-namespace", opts.Namespace)
				r.Equal("castware-operator", opts.ReleaseName)
				return nil
			}).
			Times(1)

		ctxWithTimeout, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		err := testOps.sut.Run(ctxWithTimeout, "v0.1.1")
		r.Error(err)
		r.Contains(err.Error(), "helm is in failed status")
	})

	t.Run("should not rollback when release is uninstalled", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()

		testCluster := newTestCluster(t, server)
		testOps := newTestOps(t, testCluster)

		// Create a fake secret for API key
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-api-secret",
				Namespace: "test-namespace",
			},
			Data: map[string][]byte{
				"API_KEY": []byte("test-api-key"),
			},
		}
		r.NoError(testOps.sut.Create(ctx, secret))

		// Mock the initial GetRelease call
		initialRelease := createMockRelease("castware-operator", "0.1.0", "test-namespace")
		testOps.mockHelm.EXPECT().
			GetRelease(gomock.Any()).
			Return(initialRelease, nil).
			Times(1)

		// Mock the dry run upgrade
		testOps.mockHelm.EXPECT().
			Upgrade(gomock.Any(), gomock.Any()).
			Return(createMockRelease("castware-operator", "v0.1.1", "test-namespace"), nil).
			Times(1)

		// Mock the actual upgrade
		testOps.mockHelm.EXPECT().
			Upgrade(gomock.Any(), gomock.Any()).
			Return(createMockRelease("castware-operator", "v0.1.1", "test-namespace"), nil).
			Times(1)

		// Mock GetRelease to return uninstalled status
		uninstalledRelease := createMockRelease("castware-operator", "v0.1.1", "test-namespace")
		uninstalledRelease.Info.Status = release.StatusUninstalled
		testOps.mockHelm.EXPECT().
			GetRelease(gomock.Any()).
			Return(uninstalledRelease, nil).
			MinTimes(1)

		// Rollback should NOT be called
		testOps.mockHelm.EXPECT().
			Rollback(gomock.Any()).
			Times(0)

		ctxWithTimeout, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		err := testOps.sut.Run(ctxWithTimeout, "v0.1.1")
		r.Error(err)
		r.ErrorIs(err, errUninstalled)
	})

	t.Run("should handle context timeout during upgrade", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()

		testCluster := newTestCluster(t, server)
		testOps := newTestOps(t, testCluster)

		// Create a fake secret for API key
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-api-secret",
				Namespace: "test-namespace",
			},
			Data: map[string][]byte{
				"API_KEY": []byte("test-api-key"),
			},
		}
		r.NoError(testOps.sut.Create(ctx, secret))

		// Mock the initial GetRelease call
		initialRelease := createMockRelease("castware-operator", "0.1.0", "test-namespace")
		testOps.mockHelm.EXPECT().
			GetRelease(gomock.Any()).
			Return(initialRelease, nil).
			Times(1)

		// Mock the dry run upgrade
		testOps.mockHelm.EXPECT().
			Upgrade(gomock.Any(), gomock.Any()).
			Return(createMockRelease("castware-operator", "v0.1.1", "test-namespace"), nil).
			Times(1)

		// Mock the actual upgrade
		pendingRelease := createMockRelease("castware-operator", "v0.1.1", "test-namespace")
		pendingRelease.Info.Status = release.StatusPendingUpgrade
		testOps.mockHelm.EXPECT().
			Upgrade(gomock.Any(), gomock.Any()).
			Return(pendingRelease, nil).
			Times(1)

		// Mock GetRelease to always return pending status
		testOps.mockHelm.EXPECT().
			GetRelease(gomock.Any()).
			Return(pendingRelease, nil).
			AnyTimes()

		// Rollback should NOT be called on timeout
		testOps.mockHelm.EXPECT().
			Rollback(gomock.Any()).
			Times(0)

		// Create a context with a very short timeout
		ctxWithTimeout, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()

		err := testOps.sut.Run(ctxWithTimeout, "v0.1.1")
		r.Error(err)
		r.ErrorIs(err, context.DeadlineExceeded)
	})

	t.Run("should fail when GetRelease returns error", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()

		testCluster := newTestCluster(t, server)
		testOps := newTestOps(t, testCluster)

		// Create a fake secret for API key
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-api-secret",
				Namespace: "test-namespace",
			},
			Data: map[string][]byte{
				"API_KEY": []byte("test-api-key"),
			},
		}
		r.NoError(testOps.sut.Create(ctx, secret))

		// Mock GetRelease to fail
		expectedErr := fmt.Errorf("release not found")
		testOps.mockHelm.EXPECT().
			GetRelease(gomock.Any()).
			Return(nil, expectedErr).
			Times(1)

		err := testOps.sut.Run(ctx, "v0.1.1")
		r.Error(err)
		r.Contains(err.Error(), "failed to get helm release")
	})

	t.Run("should record action result after successful upgrade", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		actionResultRecorded := false
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == getByNamePath {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				// Return a mock component response
				_, _ = w.Write([]byte(`{
				"id": "test-id",
				"name": "castware-operator",
				"helmChart": "castware-operator",
				"dependencies": [],
				"latestVersion": "v0.2.0"
			}`))
				return
			} else if actionResultUrlRegex.MatchString(r.URL.Path) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				actionResultRecorded = true
				// Return a mock component response
				_, _ = w.Write([]byte(`{}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(server.Close)

		testCluster := newTestCluster(t, server)
		testOps := newTestOps(t, testCluster)

		// Create a fake secret for API key
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-api-secret",
				Namespace: "test-namespace",
			},
			Data: map[string][]byte{
				"API_KEY": []byte("test-api-key"),
			},
		}
		r.NoError(testOps.sut.Create(ctx, secret))

		// Mock the initial GetRelease call
		initialRelease := createMockRelease("castware-operator", "0.1.0", "test-namespace")
		testOps.mockHelm.EXPECT().
			GetRelease(gomock.Any()).
			Return(initialRelease, nil).
			Times(1)

		// Mock the dry run upgrade
		testOps.mockHelm.EXPECT().
			Upgrade(gomock.Any(), gomock.Any()).
			DoAndReturn(func(ctx context.Context, opts helm.UpgradeOptions) (*release.Release, error) {
				r.True(opts.DryRun, "First upgrade call should be a dry run")
				r.Equal("v0.1.1", opts.ChartSource.Version)
				return createMockRelease("castware-operator", "v0.1.1", "test-namespace"), nil
			}).
			Times(1)

		// Mock the actual upgrade
		upgradedRelease := createMockRelease("castware-operator", "v0.1.1", "test-namespace")
		upgradedRelease.Info.Status = release.StatusPendingUpgrade
		testOps.mockHelm.EXPECT().
			Upgrade(gomock.Any(), gomock.Any()).
			DoAndReturn(func(ctx context.Context, opts helm.UpgradeOptions) (*release.Release, error) {
				r.False(opts.DryRun, "Second upgrade call should not be a dry run")
				return upgradedRelease, nil
			}).
			Times(1)

		// Mock GetRelease to check status (returns deployed)
		deployedRelease := createMockRelease("castware-operator", "v0.1.1", "test-namespace")
		deployedRelease.Info.Status = release.StatusDeployed
		testOps.mockHelm.EXPECT().
			GetRelease(gomock.Any()).
			Return(deployedRelease, nil).
			MinTimes(1)

		err := testOps.sut.Run(ctx, "v0.1.1")
		r.NoError(err)
		r.True(actionResultRecorded)
	})

	t.Run("should record action result after failed upgrade", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()

		actionResult := castai.ComponentActionResult{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			if request.URL.Path == getByNamePath {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				// Return a mock component response
				_, _ = w.Write([]byte(`{
				"id": "test-id",
				"name": "castware-operator",
				"helmChart": "castware-operator",
				"dependencies": [],
				"latestVersion": "v0.2.0"
			}`))
				return
			} else if actionResultUrlRegex.MatchString(request.URL.Path) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				body, err := io.ReadAll(request.Body)
				r.NoError(err)
				r.NoError(json.Unmarshal(body, &actionResult))
				// Return a mock component response
				_, _ = w.Write([]byte(`{}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(server.Close)

		testCluster := newTestCluster(t, server)
		testOps := newTestOps(t, testCluster)

		// Create a fake secret for API key
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-api-secret",
				Namespace: "test-namespace",
			},
			Data: map[string][]byte{
				"API_KEY": []byte("test-api-key"),
			},
		}
		r.NoError(testOps.sut.Create(ctx, secret))

		// Mock the initial GetRelease call
		initialRelease := createMockRelease("castware-operator", "0.1.0", "test-namespace")
		testOps.mockHelm.EXPECT().
			GetRelease(gomock.Any()).
			Return(initialRelease, nil).
			Times(1)

		// Mock the dry run upgrade (success)
		testOps.mockHelm.EXPECT().
			Upgrade(gomock.Any(), gomock.Any()).
			DoAndReturn(func(ctx context.Context, opts helm.UpgradeOptions) (*release.Release, error) {
				r.True(opts.DryRun)
				return createMockRelease("castware-operator", "v0.1.1", "test-namespace"), nil
			}).
			Times(1)

		// Mock the actual upgrade (success but will fail during status check)
		upgradedRelease := createMockRelease("castware-operator", "v0.1.1", "test-namespace")
		upgradedRelease.Info.Status = release.StatusPendingUpgrade
		testOps.mockHelm.EXPECT().
			Upgrade(gomock.Any(), gomock.Any()).
			DoAndReturn(func(ctx context.Context, opts helm.UpgradeOptions) (*release.Release, error) {
				r.False(opts.DryRun)
				return upgradedRelease, nil
			}).
			Times(1)

		// Mock GetRelease to return failed status
		failedRelease := createMockRelease("castware-operator", "v0.1.1", "test-namespace")
		failedRelease.Info.Status = release.StatusFailed
		failedRelease.Info.Description = "upgrade failed due to pod crash"
		testOps.mockHelm.EXPECT().
			GetRelease(gomock.Any()).
			Return(failedRelease, nil).
			MinTimes(1)

		// Mock the rollback
		testOps.mockHelm.EXPECT().
			Rollback(gomock.Any()).
			DoAndReturn(func(opts helm.RollbackOptions) error {
				r.Equal("test-namespace", opts.Namespace)
				r.Equal("castware-operator", opts.ReleaseName)
				return nil
			}).
			Times(1)

		ctxWithTimeout, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		err := testOps.sut.Run(ctxWithTimeout, "v0.1.1")
		r.Error(err)
		r.Contains(err.Error(), "helm is in failed status")

		r.Equal(castai.Action_UPGRADE, actionResult.Action)
		r.Equal(castai.Status_ERROR, actionResult.Status)
		r.Equal("helm is in failed status: upgrade failed due to pod crash", actionResult.Message)
	})
}

type testOps struct {
	sut      *Service
	mockHelm *mock_helm.MockClient
}

func newTestOps(t *testing.T, objs ...client.Object) *testOps {
	t.Helper()
	r := require.New(t)
	scheme := runtime.NewScheme()

	err := castwarev1alpha1.AddToScheme(scheme)
	r.NoError(err)

	err = corev1.AddToScheme(scheme)
	r.NoError(err)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(objs...).Build()

	ctrl := gomock.NewController(t)
	mockHelm := mock_helm.NewMockClient(ctrl)

	opts := &testOps{
		mockHelm: mockHelm,
		sut: &Service{
			Client:             c,
			helmClient:         mockHelm,
			config:             &config.Config{RequestTimeout: time.Second},
			log:                logrus.New(),
			clusterCrName:      "test-cluster",
			clusterCrNamespace: "test-namespace",
		},
	}

	return opts
}

func newTestCluster(t *testing.T, server *httptest.Server) *castwarev1alpha1.Cluster {
	t.Helper()
	// Create a mock HTTP server

	return &castwarev1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "test-namespace",
		},
		Spec: castwarev1alpha1.ClusterSpec{
			Cluster: &castwarev1alpha1.ClusterMetadataSpec{
				ClusterID: uuid.NewString(),
			},
			API: castwarev1alpha1.APISpec{
				APIURL: server.URL,
			},
			APIKeySecret: "test-api-secret",
			HelmRepoURL:  "https://castai.github.io/helm-charts",
		},
	}
}

//nolint:unparam
func createMockRelease(name, version, namespace string) *release.Release {
	return &release.Release{
		Name:      name,
		Namespace: namespace,
		Info: &release.Info{
			Status: release.StatusDeployed,
		},
		Chart: &chart.Chart{
			Metadata: &chart.Metadata{
				Name:    name,
				Version: version,
			},
		},
		Config: map[string]interface{}{},
	}
}
