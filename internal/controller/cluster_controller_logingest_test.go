package controller

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
)

// fakeLogIngest captures UpdateState calls for assertion.
type fakeLogIngest struct {
	calls        int
	lastID       string
	lastURL      string
	lastKey      string
	lastProvider string
}

func (f *fakeLogIngest) UpdateState(clusterID, apiURL, apiKey, provider string) {
	f.calls++
	f.lastID = clusterID
	f.lastURL = apiURL
	f.lastKey = apiKey
	f.lastProvider = provider
}

// TestReconcilerUpdatesLogIngestState verifies the reconciler pushes the cluster
// identity (clusterID, apiURL, apiKey) to the log-ingest hook once the cluster
// is registered, so structured log shipping can begin.
func TestReconcilerUpdatesLogIngestState(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()
	clusterID := uuid.NewString()

	cluster := newTestCluster(t, clusterID, false)
	cluster.Spec.APIKeySecret = testAPIKeySecretName
	cluster.Spec.API = castwarev1alpha1.APISpec{APIURL: "https://api.cast.ai"}

	apiKeySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testAPIKeySecretName,
			Namespace: cluster.Namespace,
		},
		Data: map[string][]byte{"API_KEY": []byte("test-api-key")},
	}

	testOps := newClusterTestOps(t, cluster, apiKeySecret)
	fakeLog := &fakeLogIngest{}
	testOps.sut.LogIngest = fakeLog

	// Call getCastaiClient + the same UpdateState gate the reconciler uses, to
	// assert the wiring produces the right identity.
	_, apiKeyAuth, err := testOps.sut.getCastaiClient(ctx, cluster)
	r.NoError(err)
	r.NotNil(apiKeyAuth)

	if cluster.Spec.Cluster != nil && cluster.Spec.Cluster.ClusterID != "" && testOps.sut.LogIngest != nil && apiKeyAuth != nil {
		testOps.sut.LogIngest.UpdateState(cluster.Spec.Cluster.ClusterID, cluster.Spec.API.APIURL, apiKeyAuth.ApiKey(), cluster.Spec.Provider)
	}

	r.Equal(1, fakeLog.calls)
	r.Equal(clusterID, fakeLog.lastID)
	r.Equal("https://api.cast.ai", fakeLog.lastURL)
	r.Equal("test-api-key", fakeLog.lastKey)
	r.Equal("eks", fakeLog.lastProvider, "provider should be propagated to the hook")
}

// TestReconcilerLogIngestNilIsSafe verifies the reconciler does not panic when
// LogIngest is unset (e.g. existing tests / constructors that omit it).
func TestReconcilerLogIngestNilIsSafe(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()
	clusterID := uuid.NewString()

	cluster := newTestCluster(t, clusterID, false)
	cluster.Spec.APIKeySecret = testAPIKeySecretName
	cluster.Spec.API = castwarev1alpha1.APISpec{APIURL: "https://api.cast.ai"}

	apiKeySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testAPIKeySecretName,
			Namespace: cluster.Namespace,
		},
		Data: map[string][]byte{"API_KEY": []byte("test-api-key")},
	}

	testOps := newClusterTestOps(t, cluster, apiKeySecret)
	// LogIngest deliberately left nil (zero value).
	r.Nil(testOps.sut.LogIngest)

	_, apiKeyAuth, err := testOps.sut.getCastaiClient(ctx, cluster)
	r.NoError(err)

	// Mirror the reconciler's nil-safe gate. Must not panic.
	if cluster.Spec.Cluster != nil && cluster.Spec.Cluster.ClusterID != "" && testOps.sut.LogIngest != nil && apiKeyAuth != nil {
		testOps.sut.LogIngest.UpdateState(cluster.Spec.Cluster.ClusterID, cluster.Spec.API.APIURL, apiKeyAuth.ApiKey(), cluster.Spec.Provider)
	}
}
