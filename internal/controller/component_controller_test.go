//nolint:goconst
package controller

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/castai"
	mock_castai "github.com/castai/castware-operator/internal/castai/mock"
	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/config"
	"github.com/castai/castware-operator/internal/helm"
	mock_helm "github.com/castai/castware-operator/internal/helm/mock"
)

func TestReconcile(t *testing.T) {
	t.Run("when migrating from helm", func(t *testing.T) {
		t.Run("should set status condition to progressing and finalizer on the first reconcile loop", func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			r := require.New(t)

			testCluster := newTestCluster(t, uuid.NewString(), true)
			testComponent := newTestComponent(t, testCluster.Name, "test-component")
			testComponent.Spec.Migration = castwarev1alpha1.ComponentMigrationHelm

			testOps := newComponentTestOps(t, testCluster, testComponent)

			req := reconcile.Request{NamespacedName: client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}}

			_, err := testOps.sut.Reconcile(ctx, req)
			r.NoError(err)

			var actualComponent castwarev1alpha1.Component
			err = testOps.sut.Get(ctx, client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}, &actualComponent)
			r.NoError(err)

			r.Len(actualComponent.Finalizers, 1)
			r.Equal(ComponentFinalizer, actualComponent.Finalizers[0])

			r.Len(actualComponent.Status.Conditions, 1)
			actualCondition := actualComponent.Status.Conditions[0]

			r.Equal(typeProgressingComponent, actualCondition.Type)
			r.Equal(metav1.ConditionTrue, actualCondition.Status)
			r.Equal(progressingReasonMigrating, actualCondition.Reason)
		})

		t.Run("should available condition and component current version on the second reconcile loop", func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			r := require.New(t)

			testCluster := newTestCluster(t, uuid.NewString(), true)
			testComponent := newTestComponent(t, testCluster.Name, "test-component")
			testComponent.Spec.Migration = castwarev1alpha1.ComponentMigrationHelm
			testComponent.Spec.ReleaseName = "release-name"

			testOps := newComponentTestOps(t, testCluster, testComponent)

			req := reconcile.Request{NamespacedName: client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}}

			_, err := testOps.sut.Reconcile(ctx, req)
			r.NoError(err)

			testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
				Namespace:   testComponent.Namespace,
				ReleaseName: testComponent.Spec.ReleaseName,
			}).Return(&release.Release{
				Name: testComponent.Spec.Component,
				Info: &release.Info{Status: release.StatusDeployed},
				Chart: &chart.Chart{
					Metadata: &chart.Metadata{
						Version: "0.1.2",
					},
				},
			}, nil)

			_, err = testOps.sut.Reconcile(ctx, req)
			r.NoError(err)

			var actualComponent castwarev1alpha1.Component
			err = testOps.sut.Get(ctx, client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}, &actualComponent)
			r.NoError(err)

			r.Equal("0.1.2", actualComponent.Status.CurrentVersion)
			r.Len(actualComponent.Status.Conditions, 2)

			progressingCondition := meta.FindStatusCondition(actualComponent.Status.Conditions, typeProgressingComponent)
			r.NotNil(progressingCondition)
			r.Equal(metav1.ConditionFalse, progressingCondition.Status)
			r.Equal("Completed", progressingCondition.Reason)
			r.Equal("Component migration successful", progressingCondition.Message)

			availableCondition := meta.FindStatusCondition(actualComponent.Status.Conditions, typeAvailableComponent)
			r.NotNil(availableCondition)
			r.Equal(metav1.ConditionTrue, availableCondition.Status)
			r.Equal(reasonInstalled, availableCondition.Reason)
		})

		t.Run("should handle migration from different helm chart version", func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			r := require.New(t)

			testCluster := newTestCluster(t, uuid.NewString(), true)
			testComponent := newTestComponent(t, testCluster.Name, "test-component")
			testComponent.Spec.Migration = castwarev1alpha1.ComponentMigrationHelm
			testComponent.Spec.Version = "0.2.5" // CRD specifies v0.1.1

			testOps := newComponentTestOpsWithCastAIClient(t, testCluster, testComponent)

			req := reconcile.Request{NamespacedName: client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}}

			// First reconcile should set progressing condition to true
			_, err := testOps.sut.Reconcile(ctx, req)
			r.NoError(err)

			testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
				Namespace:   testComponent.Namespace,
				ReleaseName: testComponent.Spec.Component,
			}).Return(&release.Release{
				Name:      testComponent.Spec.Component,
				Namespace: testComponent.Namespace,
				Info:      &release.Info{Status: release.StatusDeployed},
				Chart: &chart.Chart{
					Metadata: &chart.Metadata{
						Name:    testComponent.Spec.Component,
						Version: "0.1.1",
					},
				},
				Config: map[string]interface{}{},
			}, nil).Times(3)

			testOps.mockCastAI.EXPECT().RecordActionResult(gomock.Any(), testCluster.Spec.Cluster.ClusterID, gomock.Any()).Return(nil).AnyTimes()

			// Second reconcile detects version mismatch
			_, err = testOps.sut.Reconcile(ctx, req)
			r.NoError(err)

			var actualComponent castwarev1alpha1.Component
			err = testOps.sut.Get(ctx, client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}, &actualComponent)
			r.NoError(err)

			r.Equal("0.1.1", actualComponent.Status.CurrentVersion)
			r.Len(actualComponent.Status.Conditions, 2)

			progressingCondition := meta.FindStatusCondition(actualComponent.Status.Conditions, typeProgressingComponent)
			r.NotNil(progressingCondition)
			r.Equal(metav1.ConditionFalse, progressingCondition.Status)
			r.Equal("Completed", progressingCondition.Reason)
			r.Equal("Component migration successful", progressingCondition.Message)

			availableCondition := meta.FindStatusCondition(actualComponent.Status.Conditions, typeAvailableComponent)
			r.NotNil(availableCondition)
			r.Equal(metav1.ConditionTrue, availableCondition.Status)
			r.Equal(reasonInstalled, availableCondition.Reason)

			testOps.mockHelm.EXPECT().Upgrade(gomock.Any(), gomock.Any()).Return(&release.Release{
				Name: testComponent.Spec.Component,
				Info: &release.Info{Status: release.StatusDeployed},
				Chart: &chart.Chart{
					Metadata: &chart.Metadata{
						Version: "0.2.5", // Different version than CRD (0.2.5)
					},
				},
			}, nil)

			_, err = testOps.sut.Reconcile(ctx, req)
			r.NoError(err)
		})
	})

	t.Run("when migrating from yaml", func(t *testing.T) {
		t.Run("should set status condition to progressing and finalizer on the first reconcile loop", func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			r := require.New(t)

			testCluster := newTestCluster(t, uuid.NewString(), true)
			testComponent := newTestComponent(t, testCluster.Name, "test-component")
			testComponent.Spec.Migration = castwarev1alpha1.ComponentMigrationYaml

			testOps := newComponentTestOps(t, testCluster, testComponent)

			req := reconcile.Request{NamespacedName: client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}}

			overrides := map[string]interface{}{}
			overrides["apiURL"] = testCluster.Spec.API.APIURL
			overrides["apiKeySecretRef"] = testCluster.Spec.APIKeySecret
			overrides["provider"] = testCluster.Spec.Provider
			overrides["createNamespace"] = false

			helmRelease := &release.Release{
				Name: testComponent.Spec.Component,
				Info: &release.Info{Status: release.StatusDeployed},
				Chart: &chart.Chart{
					Metadata: &chart.Metadata{
						Version: "0.1.2",
					},
				},
			}

			testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
				Namespace:   testComponent.Namespace,
				ReleaseName: testComponent.Spec.Component,
			}).Return(nil, driver.ErrReleaseNotFound).Times(2)

			testOps.mockHelm.EXPECT().Install(gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, options helm.InstallOptions) (*release.Release, error) {
				r.True(options.DryRun)
				return helmRelease, nil
			})

			testOps.mockHelm.EXPECT().Install(gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, options helm.InstallOptions) (*release.Release, error) {
				r.False(options.DryRun)
				return helmRelease, nil
			})

			_, err := testOps.sut.Reconcile(ctx, req)
			r.NoError(err)

			var actualComponent castwarev1alpha1.Component
			err = testOps.sut.Get(ctx, client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}, &actualComponent)
			r.NoError(err)

			r.Len(actualComponent.Finalizers, 1)
			r.Equal(ComponentFinalizer, actualComponent.Finalizers[0])

			r.Len(actualComponent.Status.Conditions, 1)
			actualCondition := actualComponent.Status.Conditions[0]

			r.Equal(typeProgressingComponent, actualCondition.Type)
			r.Equal(metav1.ConditionTrue, actualCondition.Status)
			r.Equal(progressingReasonInstalling, actualCondition.Reason)
		})

		t.Run("should set available condition to false and not install the component if dry run fails", func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			r := require.New(t)

			testCluster := newTestCluster(t, uuid.NewString(), true)
			testComponent := newTestComponent(t, testCluster.Name, "test-component")
			testComponent.Spec.Migration = castwarev1alpha1.ComponentMigrationYaml

			testOps := newComponentTestOps(t, testCluster, testComponent)

			req := reconcile.Request{NamespacedName: client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}}

			overrides := map[string]interface{}{}
			overrides["apiURL"] = testCluster.Spec.API.APIURL
			overrides["apiKeySecretRef"] = testCluster.Spec.APIKeySecret
			overrides["provider"] = testCluster.Spec.Provider
			overrides["createNamespace"] = false

			testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
				Namespace:   testComponent.Namespace,
				ReleaseName: testComponent.Spec.Component,
			}).Return(nil, driver.ErrReleaseNotFound)

			testOps.mockHelm.EXPECT().Install(gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, options helm.InstallOptions) {
				r.True(options.DryRun)
			}).Return(nil, errors.New("dry run failed"))

			_, err := testOps.sut.Reconcile(ctx, req)
			r.NoError(err)

			var actualComponent castwarev1alpha1.Component
			err = testOps.sut.Get(ctx, client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}, &actualComponent)
			r.NoError(err)

			r.Len(actualComponent.Status.Conditions, 1)
			actualCondition := actualComponent.Status.Conditions[0]

			r.Equal(typeAvailableComponent, actualCondition.Type)
			r.Equal(metav1.ConditionFalse, actualCondition.Status)
		})

		t.Run("should set available status condition and component current version on the second reconcile loop", func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			r := require.New(t)

			testCluster := newTestCluster(t, uuid.NewString(), true)
			testComponent := newTestComponent(t, testCluster.Name, "test-component")
			testComponent.Spec.Migration = castwarev1alpha1.ComponentMigrationYaml

			testOps := newComponentTestOps(t, testCluster, testComponent)

			req := reconcile.Request{NamespacedName: client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}}

			overrides := map[string]interface{}{}
			overrides["apiURL"] = testCluster.Spec.API.APIURL
			overrides["apiKeySecretRef"] = testCluster.Spec.APIKeySecret
			overrides["provider"] = testCluster.Spec.Provider
			overrides["createNamespace"] = false

			helmRelease := &release.Release{
				Name: testComponent.Spec.Component,
				Info: &release.Info{Status: release.StatusDeployed},
				Chart: &chart.Chart{
					Metadata: &chart.Metadata{
						Version: "0.1.2",
					},
				},
			}

			testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
				Namespace:   testComponent.Namespace,
				ReleaseName: testComponent.Spec.Component,
			}).Return(nil, driver.ErrReleaseNotFound).Times(2)

			testOps.mockHelm.EXPECT().Install(gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, options helm.InstallOptions) (*release.Release, error) {
				r.True(options.DryRun)
				return helmRelease, nil
			})

			testOps.mockHelm.EXPECT().Install(gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, options helm.InstallOptions) (*release.Release, error) {
				r.False(options.DryRun)
				return helmRelease, nil
			})

			_, err := testOps.sut.Reconcile(ctx, req)
			r.NoError(err)

			testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
				Namespace:   testComponent.Namespace,
				ReleaseName: testComponent.Spec.Component,
			}).Return(helmRelease, nil)

			_, err = testOps.sut.Reconcile(ctx, req)
			r.NoError(err)

			var actualComponent castwarev1alpha1.Component
			err = testOps.sut.Get(ctx, client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}, &actualComponent)
			r.NoError(err)

			r.Equal("0.1.2", actualComponent.Status.CurrentVersion)

			r.Len(actualComponent.Status.Conditions, 2)

			progressingCondition := meta.FindStatusCondition(actualComponent.Status.Conditions, typeProgressingComponent)
			r.Equal(metav1.ConditionFalse, progressingCondition.Status)

			availableCondition := meta.FindStatusCondition(actualComponent.Status.Conditions, typeAvailableComponent)
			r.Equal(metav1.ConditionTrue, availableCondition.Status)
		})
	})

	t.Run("when component is readonly", func(t *testing.T) {
		t.Run("should update currentVersion if it's different from helm version", func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			r := require.New(t)

			testCluster := newTestCluster(t, uuid.NewString(), true)
			testComponent := newTestComponent(t, testCluster.Name, "test-component")
			testComponent.Spec.Readonly = true

			testOps := newComponentTestOps(t, testCluster, testComponent)
			testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
				Namespace:   testComponent.Namespace,
				ReleaseName: testComponent.Spec.Component,
			}).Return(&release.Release{
				Chart: &chart.Chart{
					Metadata: &chart.Metadata{
						Version: "0.2.1",
					},
				},
			}, nil)

			req := reconcile.Request{NamespacedName: client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}}
			_, err := testOps.sut.Reconcile(ctx, req)
			r.NoError(err)

			var actualComponent castwarev1alpha1.Component
			err = testOps.sut.Get(ctx, client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}, &actualComponent)
			r.NoError(err)

			r.Equal("0.2.1", actualComponent.Status.CurrentVersion)
		})
	})

	t.Run("when component has terraform migration", func(t *testing.T) {
		t.Run("should requeue without processing to give cluster controller time to handle migration", func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			r := require.New(t)

			testCluster := newTestCluster(t, uuid.NewString(), true)
			testComponent := newTestComponent(t, testCluster.Name, "test-component")
			testComponent.Spec.Migration = castwarev1alpha1.ComponentMigrationTerraform
			testComponent.Spec.Version = ""

			testOps := newComponentTestOps(t, testCluster, testComponent)

			req := reconcile.Request{NamespacedName: client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}}

			result, err := testOps.sut.Reconcile(ctx, req)
			r.NoError(err)
			r.Equal(time.Second*30, result.RequeueAfter)

			var actualComponent castwarev1alpha1.Component
			err = testOps.sut.Get(ctx, client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}, &actualComponent)
			r.NoError(err)

			r.Empty(actualComponent.Finalizers)
			r.Empty(actualComponent.Status.Conditions)
			r.Empty(actualComponent.Status.CurrentVersion)
		})
	})
}

func TestComponentReconciler_ValueOverrides(t *testing.T) {
	t.Parallel()
	log := logrus.New()
	log.SetOutput(io.Discard)

	t.Run("when component.Spec.Values not nil then add overrides", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testComponent := newTestComponent(t, testCluster.Name, "test-component")
		testComponent.Spec.Values = &v1.JSON{Raw: []byte(`{"value1": "value1-value", "value2": true}`)}
		testOps := newComponentTestOps(t, testCluster, testComponent)

		overrides, err := testOps.sut.valueOverrides(ctx, log, testComponent, testCluster)

		r.NoError(err)
		r.Equal("value1-value", overrides["value1"])
		r.Equal(true, overrides["value2"])
	})

	t.Run("when component.Spec.Component is cluster controller then add overrides", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testComponent := newTestComponent(t, testCluster.Name, "cluster-controller")
		apiKeySecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testCluster.Spec.APIKeySecret,
				Namespace: testCluster.Namespace,
			},
			Data: map[string][]byte{
				"API_KEY": []byte("test-api-key"),
			},
		}
		testOps := newComponentTestOps(t, testCluster, testComponent, apiKeySecret)

		overrides, err := testOps.sut.valueOverrides(ctx, log, testComponent, testCluster)

		r.NoError(err)
		castaiOverrides, ok := overrides["castai"].(map[string]any)
		r.True(ok)
		r.Equal(testCluster.Spec.Cluster.ClusterID, castaiOverrides["clusterID"])
		r.Equal("test-api-key", castaiOverrides["apiKey"])
		r.Equal("", castaiOverrides["apiURL"])
		r.Equal("value1-value", overrides["value1"])
		r.Equal(true, overrides["value2"])
	})

	t.Run("when component.Spec.Component is spotHandler then add overrides", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testComponent := newTestComponent(t, testCluster.Name, "spot-handler")
		apiKeySecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testCluster.Spec.APIKeySecret,
				Namespace: testCluster.Namespace,
			},
			Data: map[string][]byte{
				"API_KEY": []byte("test-api-key"),
			},
		}
		testOps := newComponentTestOps(t, testCluster, testComponent, apiKeySecret)

		overrides, err := testOps.sut.valueOverrides(ctx, log, testComponent, testCluster)

		r.NoError(err)
		castaiOverrides, ok := overrides["castai"].(map[string]any)
		r.True(ok)
		r.Equal(testCluster.Spec.Cluster.ClusterID, castaiOverrides["clusterID"])
		r.Equal("test-api-key", castaiOverrides["apiKey"])
		r.Equal("", castaiOverrides["apiURL"])
		r.Equal("aws", castaiOverrides["provider"])
		r.Equal(false, overrides["phase2Permissions"])
		r.Equal("value1-value", overrides["value1"])
	})

	t.Run("when component.Spec.Component is agent then add overrides", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testComponent := newTestComponent(t, testCluster.Name, "castai-agent")
		testOps := newComponentTestOps(t, testCluster, testComponent)

		overrides, err := testOps.sut.valueOverrides(ctx, log, testComponent, testCluster)

		r.NoError(err)
		r.Equal("", overrides["apiURL"])
		r.Equal("test-cluster", overrides["apiKeySecretRef"])
		r.Equal("eks", overrides["provider"])
		r.Equal(true, overrides["createNamespace"])
		r.Equal("value1-value", overrides["value1"])
	})

	t.Run("when component.Spec.Component is umbrella then build global.castai from cluster spec", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		// The umbrella builder maps the cluster spec into global.castai.* and
		// deep-merges the user's Spec.Values on top (user wins). Use a custom
		// values block rather than newTestComponent's default {value1,value2}.
		testComponent := newTestComponent(t, testCluster.Name, "castai-umbrella")
		testComponent.Spec.Values = &v1.JSON{Raw: []byte(`{"tags":{"readonly":true}}`)}
		testOps := newComponentTestOps(t, testCluster, testComponent)

		overrides, err := testOps.sut.valueOverrides(ctx, log, testComponent, testCluster)

		r.NoError(err)
		// The flat keys used by the other components must never leak into the
		// umbrella overrides — the builder is gated strictly on component name.
		r.NotContains(overrides, "apiURL")
		r.NotContains(overrides, "apiKeySecretRef")
		r.NotContains(overrides, "provider")
		r.NotContains(overrides, "createNamespace")

		global, ok := overrides["global"].(map[string]any)
		r.True(ok)
		castai, ok := global["castai"].(map[string]any)
		r.True(ok)
		r.Equal(testCluster.Spec.API.APIURL, castai["apiURL"])
		r.Equal(testCluster.Spec.Provider, castai["provider"])
		r.Equal(testCluster.Spec.Cluster.ClusterID, castai["clusterID"])
		r.Equal(testCluster.Spec.APIKeySecret, castai["apiKeySecretRef"])
		// grpcURL is omitted when the cluster spec does not set it.
		r.NotContains(castai, "grpcURL")

		// User-supplied values are preserved through the merge.
		tags, ok := overrides["tags"].(map[string]any)
		r.True(ok)
		r.Equal(true, tags["readonly"])
	})

	t.Run("when umbrella cluster has grpcURL then it is mapped into global.castai", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testCluster.Spec.API.GrpcURL = "grpc.cast.ai:443"
		testComponent := newTestComponent(t, testCluster.Name, "castai-umbrella")
		testOps := newComponentTestOps(t, testCluster, testComponent)

		overrides, err := testOps.sut.valueOverrides(ctx, log, testComponent, testCluster)

		r.NoError(err)
		castai := overrides["global"].(map[string]any)["castai"].(map[string]any)
		r.Equal("grpc.cast.ai:443", castai["grpcURL"])
	})

	t.Run("when umbrella user values override global.castai then user wins", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		testCluster := newTestCluster(t, uuid.NewString(), true) // provider=eks
		testComponent := newTestComponent(t, testCluster.Name, "castai-umbrella")
		// Override the builder-provided provider and disable a sub-component.
		testComponent.Spec.Values = &v1.JSON{Raw: []byte(`{"global":{"castai":{"provider":"gke"}},"autoscaler":{"castai-evictor":{"enabled":false}}}`)}
		testOps := newComponentTestOps(t, testCluster, testComponent)

		overrides, err := testOps.sut.valueOverrides(ctx, log, testComponent, testCluster)

		r.NoError(err)
		castai := overrides["global"].(map[string]any)["castai"].(map[string]any)
		// User value wins over the builder default (eks -> gke).
		r.Equal("gke", castai["provider"])
		// Builder-provided defaults that the user did not touch survive.
		r.Equal(testCluster.Spec.API.APIURL, castai["apiURL"])
		// User-supplied sub-component tuning survives.
		autoscaler := overrides["autoscaler"].(map[string]any)
		evictor := autoscaler["castai-evictor"].(map[string]any)
		r.Equal(false, evictor["enabled"])
	})

	t.Run("when umbrella cluster spec has nil cluster metadata then clusterID omitted", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testCluster.Spec.Cluster = nil
		testComponent := newTestComponent(t, testCluster.Name, "castai-umbrella")
		testOps := newComponentTestOps(t, testCluster, testComponent)

		overrides, err := testOps.sut.valueOverrides(ctx, log, testComponent, testCluster)

		r.NoError(err)
		castai := overrides["global"].(map[string]any)["castai"].(map[string]any)
		r.NotContains(castai, "clusterID")
	})

	t.Run("when component.Spec.Component is unknown then add default overrides", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testComponent := newTestComponent(t, testCluster.Name, "unknown-component")
		testOps := newComponentTestOps(t, testCluster, testComponent)

		overrides, err := testOps.sut.valueOverrides(ctx, log, testComponent, testCluster)

		r.NoError(err)
		r.Equal("", overrides["apiURL"])
		r.Equal("test-cluster", overrides["apiKeySecretRef"])
		r.Equal("eks", overrides["provider"])
		r.Equal(false, overrides["createNamespace"])
		r.Equal("value1-value", overrides["value1"])
	})
}

// nolint: unparam
func newTestComponent(t *testing.T, clusterName, name string) *castwarev1alpha1.Component {
	t.Helper()
	return &castwarev1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "test-namespace",
		},
		Spec: castwarev1alpha1.ComponentSpec{
			Component:   name,
			Cluster:     clusterName,
			Enabled:     true,
			Version:     "v0.1.1",
			Values:      &v1.JSON{Raw: []byte(`{"value1": "value1-value", "value2": true}`)},
			Migration:   "",
			Readonly:    false,
			ReleaseName: name,
		},
		Status: castwarev1alpha1.ComponentStatus{},
	}
}

type componentTestOps struct {
	sut        *ComponentReconciler
	mockHelm   *mock_helm.MockClient
	mockCastAI *mock_castai.MockCastAIClient
}

func newComponentTestOps(t *testing.T, objs ...client.Object) *componentTestOps {
	t.Helper()
	r := require.New(t)
	scheme := runtime.NewScheme()

	err := castwarev1alpha1.AddToScheme(scheme)
	r.NoError(err)

	err = corev1.AddToScheme(scheme)
	r.NoError(err)

	err = rbacv1.AddToScheme(scheme)
	r.NoError(err)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(objs...).Build()

	ctrl := gomock.NewController(t)
	mockHelm := mock_helm.NewMockClient(ctrl)

	fakeRecorder := record.NewFakeRecorder(10)

	opts := &componentTestOps{
		mockHelm: mockHelm,
		sut: &ComponentReconciler{
			Client:     c,
			Scheme:     c.Scheme(),
			Log:        logrus.New(),
			HelmClient: mockHelm,
			Recorder:   fakeRecorder,
			Config:     &config.Config{},
		},
	}

	return opts
}

func newComponentTestOpsWithCastAIClient(t *testing.T, objs ...client.Object) *componentTestOps {
	t.Helper()
	r := require.New(t)
	scheme := runtime.NewScheme()

	err := castwarev1alpha1.AddToScheme(scheme)
	r.NoError(err)

	err = corev1.AddToScheme(scheme)
	r.NoError(err)

	err = rbacv1.AddToScheme(scheme)
	r.NoError(err)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(objs...).Build()

	ctrl := gomock.NewController(t)
	mockHelm := mock_helm.NewMockClient(ctrl)
	mockCastAI := mock_castai.NewMockCastAIClient(ctrl)

	fakeRecorder := record.NewFakeRecorder(10)

	opts := &componentTestOps{
		mockHelm:   mockHelm,
		mockCastAI: mockCastAI,
		sut: &ComponentReconciler{
			Client:     c,
			Scheme:     c.Scheme(),
			Log:        logrus.New(),
			HelmClient: mockHelm,
			Recorder:   fakeRecorder,
			Config:     &config.Config{},
			castAIClientGetter: func(ctx context.Context, cluster *castwarev1alpha1.Cluster) (castai.CastAIClient, error) {
				return mockCastAI, nil
			},
		},
	}

	return opts
}

// expectUmbrellaNotInstalledComponent wires up the mock expectations for the
// component reconciler's umbrella mutual-exclusivity probe when a
// sub-component CR is reconciled and the umbrella release is NOT installed:
// the reconciler resolves the umbrella release name from Mothership, then
// probes helm and finds no release. Tests reconciling a sub-component with a
// castai mock must call this so the gate's extra mock calls are expected.
func expectUmbrellaNotInstalledComponent(ops *componentTestOps, namespace string) {
	ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
		Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil)
	ops.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
		Namespace:   namespace,
		ReleaseName: components.ComponentNameUmbrella,
	}).Return(nil, driver.ErrReleaseNotFound)
}

func TestGenerationBasedUpgrade(t *testing.T) {
	t.Run("should trigger upgrade when spec.values change but version stays the same", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testComponent := newTestComponent(t, testCluster.Name, "test-component")
		testComponent.Spec.Version = "v0.1.0"
		testComponent.Status.CurrentVersion = "v0.1.0"
		// Simulate that ObservedGeneration was previously set (e.g. after a prior successful deploy)
		testComponent.Status.ObservedGeneration = 1
		// Simulate a spec change (values change) that incremented the generation
		testComponent.Generation = 2
		meta.SetStatusCondition(&testComponent.Status.Conditions, metav1.Condition{
			Type:   typeAvailableComponent,
			Status: metav1.ConditionTrue,
			Reason: reasonInstalled,
		})

		testOps := newComponentTestOpsWithCastAIClient(t, testCluster, testComponent)

		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   testComponent.Namespace,
			ReleaseName: testComponent.Spec.Component,
		}).Return(&release.Release{
			Name: testComponent.Spec.Component,
			Info: &release.Info{Status: release.StatusDeployed},
			Chart: &chart.Chart{
				Metadata: &chart.Metadata{
					Name:    testComponent.Spec.Component,
					Version: "v0.1.0",
				},
			},
		}, nil)

		testOps.mockCastAI.EXPECT().RecordActionResult(gomock.Any(), testCluster.Spec.Cluster.ClusterID, gomock.Any()).Return(nil).AnyTimes()

		testOps.mockHelm.EXPECT().Upgrade(gomock.Any(), gomock.Any()).DoAndReturn(
			func(ctx context.Context, opts helm.UpgradeOptions) (*release.Release, error) {
				// Verify the upgrade is using the same version (config-only upgrade)
				r.Equal("v0.1.0", opts.ChartSource.Version)
				return &release.Release{
					Name: testComponent.Spec.Component,
					Info: &release.Info{Status: release.StatusDeployed},
					Chart: &chart.Chart{
						Metadata: &chart.Metadata{
							Version: "v0.1.0",
						},
					},
				}, nil
			},
		)

		req := reconcile.Request{NamespacedName: client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}}
		_, err := testOps.sut.Reconcile(ctx, req)
		r.NoError(err)

		var actualComponent castwarev1alpha1.Component
		err = testOps.sut.Get(ctx, client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}, &actualComponent)
		r.NoError(err)

		// Verify progressing condition was set with configuration change message
		progressingCondition := meta.FindStatusCondition(actualComponent.Status.Conditions, typeProgressingComponent)
		r.NotNil(progressingCondition)
		r.Equal(metav1.ConditionTrue, progressingCondition.Status)
		r.Equal(progressingReasonUpgrading, progressingCondition.Reason)
		r.Equal("Upgrading component v0.1.0 (configuration change)", progressingCondition.Message)
	})

	t.Run("should not trigger upgrade when generation matches observed generation", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testComponent := newTestComponent(t, testCluster.Name, "test-component")
		testComponent.Spec.Version = "v0.1.0"
		testComponent.Status.CurrentVersion = "v0.1.0"
		testComponent.Status.ObservedGeneration = 1
		testComponent.Generation = 1
		meta.SetStatusCondition(&testComponent.Status.Conditions, metav1.Condition{
			Type:   typeAvailableComponent,
			Status: metav1.ConditionTrue,
			Reason: reasonInstalled,
		})

		testOps := newComponentTestOps(t, testCluster, testComponent)

		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   testComponent.Namespace,
			ReleaseName: testComponent.Spec.Component,
		}).Return(&release.Release{
			Name: testComponent.Spec.Component,
			Info: &release.Info{Status: release.StatusDeployed},
			Chart: &chart.Chart{
				Metadata: &chart.Metadata{
					Version: "v0.1.0",
				},
			},
		}, nil)

		req := reconcile.Request{NamespacedName: client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}}
		result, err := testOps.sut.Reconcile(ctx, req)
		r.NoError(err)
		r.Equal(time.Minute*15, result.RequeueAfter)
	})

	t.Run("should backfill ObservedGeneration for existing components without triggering upgrade", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testComponent := newTestComponent(t, testCluster.Name, "test-component")
		testComponent.Spec.Version = "v0.1.0"
		testComponent.Status.CurrentVersion = "v0.1.0"
		// ObservedGeneration is 0 (not set - pre-existing component)
		testComponent.Status.ObservedGeneration = 0
		testComponent.Generation = 3
		meta.SetStatusCondition(&testComponent.Status.Conditions, metav1.Condition{
			Type:   typeAvailableComponent,
			Status: metav1.ConditionTrue,
			Reason: reasonInstalled,
		})

		testOps := newComponentTestOps(t, testCluster, testComponent)

		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   testComponent.Namespace,
			ReleaseName: testComponent.Spec.Component,
		}).Return(&release.Release{
			Name: testComponent.Spec.Component,
			Info: &release.Info{Status: release.StatusDeployed},
			Chart: &chart.Chart{
				Metadata: &chart.Metadata{
					Version: "v0.1.0",
				},
			},
		}, nil)

		req := reconcile.Request{NamespacedName: client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}}
		result, err := testOps.sut.Reconcile(ctx, req)
		r.NoError(err)
		// After backfill, reconciler requeues normally
		r.True(result.RequeueAfter > 0)

		var actualComponent castwarev1alpha1.Component
		err = testOps.sut.Get(ctx, client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}, &actualComponent)
		r.NoError(err)

		// ObservedGeneration should be backfilled to the current generation
		r.Equal(int64(3), actualComponent.Status.ObservedGeneration)
	})

	t.Run("should set ObservedGeneration after successful deploy", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testComponent := newTestComponent(t, testCluster.Name, "test-component")
		testComponent.Spec.Migration = castwarev1alpha1.ComponentMigrationHelm
		testComponent.Spec.ReleaseName = "release-name"
		testComponent.Generation = 5

		testOps := newComponentTestOps(t, testCluster, testComponent)

		req := reconcile.Request{NamespacedName: client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}}

		// First reconcile: sets progressing condition
		_, err := testOps.sut.Reconcile(ctx, req)
		r.NoError(err)

		// Second reconcile: checkHelmProgress sees deployed status
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   testComponent.Namespace,
			ReleaseName: testComponent.Spec.ReleaseName,
		}).Return(&release.Release{
			Name: testComponent.Spec.Component,
			Info: &release.Info{Status: release.StatusDeployed},
			Chart: &chart.Chart{
				Metadata: &chart.Metadata{
					Version: "0.1.2",
				},
			},
		}, nil)

		_, err = testOps.sut.Reconcile(ctx, req)
		r.NoError(err)

		var actualComponent castwarev1alpha1.Component
		err = testOps.sut.Get(ctx, client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}, &actualComponent)
		r.NoError(err)

		r.Equal("0.1.2", actualComponent.Status.CurrentVersion)
		// ObservedGeneration should be set to the component's generation
		r.Equal(int64(5), actualComponent.Status.ObservedGeneration)
	})
}

func TestRollbackTimeoutReopensProgressDeadline(t *testing.T) {
	t.Run("stuck upgrade recovers via checkHelmProgress instead of looping on rollback", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		const (
			stuckVersion      = "v0.92.14" // version whose rollout timed out
			rolledBackVersion = "v0.92.7"  // previous release we roll back to
		)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testComponent := newTestComponent(t, testCluster.Name, "cluster-controller")
		testComponent.Finalizers = []string{ComponentFinalizer}
		testComponent.Spec.Version = stuckVersion
		testComponent.Status.CurrentVersion = stuckVersion
		// Wedged past the 10-minute progress deadline: Progressing has been True since 11 minutes
		// ago, so every reconcile immediately enters the timeout branch.
		testComponent.Status.Conditions = []metav1.Condition{
			{
				Type:               typeProgressingComponent,
				Status:             metav1.ConditionTrue,
				Reason:             progressingReasonUpgrading,
				Message:            "Upgrading component: " + rolledBackVersion + " -> " + stuckVersion,
				LastTransitionTime: metav1.NewTime(time.Now().Add(-11 * time.Minute)),
			},
		}

		testOps := newComponentTestOpsWithCastAIClient(t, testCluster, testComponent)

		// After a rollback the newest revision is the rolled-back content, Deployed. Return that
		// for both the "current" and "previous" GetRelease lookups; version >= 2 so rollback is allowed.
		testOps.mockHelm.EXPECT().GetRelease(gomock.Any()).DoAndReturn(
			func(opts helm.GetReleaseOptions) (*release.Release, error) {
				return &release.Release{
					Name:    testComponent.Spec.Component,
					Version: 2,
					Info:    &release.Info{Status: release.StatusDeployed},
					Chart: &chart.Chart{Metadata: &chart.Metadata{
						Name:    testComponent.Spec.Component,
						Version: rolledBackVersion,
					}},
				}, nil
			}).AnyTimes()

		rollbacks := 0
		testOps.mockHelm.EXPECT().Rollback(gomock.Any()).DoAndReturn(
			func(opts helm.RollbackOptions) error { rollbacks++; return nil }).AnyTimes()

		testOps.mockCastAI.EXPECT().
			RecordActionResult(gomock.Any(), testCluster.Spec.Cluster.ClusterID, gomock.Any()).
			Return(nil).AnyTimes()

		req := reconcile.Request{NamespacedName: client.ObjectKey{
			Namespace: testComponent.Namespace, Name: testComponent.Name,
		}}

		// Reconcile #1: timeout branch -> rollback -> reopen the deadline.
		_, err := testOps.sut.Reconcile(ctx, req)
		r.NoError(err)

		var afterFirst castwarev1alpha1.Component
		r.NoError(testOps.sut.Get(ctx, req.NamespacedName, &afterFirst))
		cond := meta.FindStatusCondition(afterFirst.Status.Conditions, typeProgressingComponent)
		r.NotNil(cond)
		r.Equal(metav1.ConditionTrue, cond.Status)
		r.WithinDuration(time.Now(), cond.LastTransitionTime.Time, time.Minute,
			"timeout branch must refresh the Progressing deadline after rollback")

		// Reconcile #2: within the fresh window -> checkHelmProgress sees Deployed -> finalizes.
		_, err = testOps.sut.Reconcile(ctx, req)
		r.NoError(err)

		var afterSecond castwarev1alpha1.Component
		r.NoError(testOps.sut.Get(ctx, req.NamespacedName, &afterSecond))
		cond = meta.FindStatusCondition(afterSecond.Status.Conditions, typeProgressingComponent)
		r.NotNil(cond)
		r.Equal(metav1.ConditionFalse, cond.Status, "component should stop progressing after recovery")
		r.Equal(rolledBackVersion, afterSecond.Status.CurrentVersion,
			"current version should reflect the rolled-back release")
		r.Equal(1, rollbacks, "rollback must happen once, not loop")
	})
}

func TestProgressingStatusSetBeforeOperation(t *testing.T) {
	t.Run("when installing component", func(t *testing.T) {
		t.Run("should set progressing status before calling helm install", func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			r := require.New(t)

			testCluster := newTestCluster(t, uuid.NewString(), true)
			testComponent := newTestComponent(t, testCluster.Name, "test-component")

			testOps := newComponentTestOps(t, testCluster, testComponent)

			// Track the order of operations
			var operationOrder []string

			// Mock GetRelease to return not found (triggering install)
			testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
				Namespace:   testComponent.Namespace,
				ReleaseName: testComponent.Spec.Component,
			}).DoAndReturn(func(opts helm.GetReleaseOptions) (*release.Release, error) {
				operationOrder = append(operationOrder, "GetRelease")
				return nil, driver.ErrReleaseNotFound
			})

			// Mock Install - this should be called AFTER progressing status is set
			testOps.mockHelm.EXPECT().Install(gomock.Any(), gomock.Any()).DoAndReturn(
				func(ctx context.Context, opts helm.InstallOptions) (*release.Release, error) {
					operationOrder = append(operationOrder, "Install")

					// At this point, the component should have progressing status set
					var component castwarev1alpha1.Component
					err := testOps.sut.Get(ctx, client.ObjectKey{
						Name:      testComponent.Name,
						Namespace: testComponent.Namespace,
					}, &component)
					r.NoError(err)

					// Verify progressing status is true
					progressingCondition := meta.FindStatusCondition(component.Status.Conditions, typeProgressingComponent)
					r.NotNil(progressingCondition, "progressing condition should be set before helm install")
					r.Equal(metav1.ConditionTrue, progressingCondition.Status, "progressing should be true before helm install")
					r.Equal(progressingReasonInstalling, progressingCondition.Reason)

					return &release.Release{
						Name: testComponent.Spec.Component,
						Info: &release.Info{Status: release.StatusDeployed},
						Chart: &chart.Chart{
							Metadata: &chart.Metadata{
								Version: testComponent.Spec.Version,
							},
						},
					}, nil
				},
			)

			req := reconcile.Request{NamespacedName: client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}}
			_, err := testOps.sut.Reconcile(ctx, req)
			r.NoError(err)

			// Verify operations happened in correct order
			r.Equal([]string{"GetRelease", "Install"}, operationOrder, "GetRelease should be called before Install")
		})
	})

	t.Run("when upgrading component", func(t *testing.T) {
		t.Run("should set progressing status before calling helm upgrade", func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			r := require.New(t)

			testCluster := newTestCluster(t, uuid.NewString(), true)
			testComponent := newTestComponent(t, testCluster.Name, "test-component")
			testComponent.Spec.Version = "v0.2.0" // New version

			// Set component as already installed with older version
			testComponent.Status.CurrentVersion = "v0.1.0"
			meta.SetStatusCondition(&testComponent.Status.Conditions, metav1.Condition{
				Type:   typeAvailableComponent,
				Status: metav1.ConditionTrue,
				Reason: reasonInstalled,
			})

			testOps := newComponentTestOps(t, testCluster, testComponent)

			// Track the order of operations
			var operationOrder []string

			// Mock GetRelease to return existing release
			testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
				Namespace:   testComponent.Namespace,
				ReleaseName: testComponent.Spec.Component,
			}).DoAndReturn(func(opts helm.GetReleaseOptions) (*release.Release, error) {
				operationOrder = append(operationOrder, "GetRelease")
				return &release.Release{
					Name: testComponent.Spec.Component,
					Info: &release.Info{Status: release.StatusDeployed},
					Chart: &chart.Chart{
						Metadata: &chart.Metadata{
							Version: "v0.1.0",
						},
					},
				}, nil
			})

			// Mock Upgrade - this should be called AFTER progressing status is set
			testOps.mockHelm.EXPECT().Upgrade(gomock.Any(), gomock.Any()).DoAndReturn(
				func(ctx context.Context, opts helm.UpgradeOptions) (*release.Release, error) {
					operationOrder = append(operationOrder, "Upgrade")

					// At this point, the component should have progressing status set
					var component castwarev1alpha1.Component
					err := testOps.sut.Get(ctx, client.ObjectKey{
						Name:      testComponent.Name,
						Namespace: testComponent.Namespace,
					}, &component)
					r.NoError(err)

					// Verify progressing status is true
					progressingCondition := meta.FindStatusCondition(component.Status.Conditions, typeProgressingComponent)
					r.NotNil(progressingCondition, "progressing condition should be set before helm upgrade")
					r.Equal(metav1.ConditionTrue, progressingCondition.Status, "progressing should be true before helm upgrade")
					r.Equal(progressingReasonUpgrading, progressingCondition.Reason)

					return &release.Release{
						Name: testComponent.Spec.Component,
						Info: &release.Info{Status: release.StatusDeployed},
						Chart: &chart.Chart{
							Metadata: &chart.Metadata{
								Version: testComponent.Spec.Version,
							},
						},
					}, nil
				},
			)

			req := reconcile.Request{NamespacedName: client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}}
			_, err := testOps.sut.Reconcile(ctx, req)
			r.NoError(err)

			// Verify operations happened in correct order
			r.Equal([]string{"GetRelease", "Upgrade"}, operationOrder, "GetRelease should be called before Upgrade")
		})
	})
}

func TestCheckAndUpdatePhase2Permissions(t *testing.T) {
	t.Run("should return true when extended permissions exist and phase2Permissions is false", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testComponent := newTestComponent(t, testCluster.Name, "test-component")

		roleBinding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-rolebinding",
				Namespace: testComponent.Namespace,
				Labels: map[string]string{
					"castware.cast.ai/extended-permissions": "true",
				},
			},
		}

		clusterRoleBinding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-clusterrolebinding",
				Labels: map[string]string{
					"castware.cast.ai/extended-permissions": "true",
				},
			},
		}

		testOps := newComponentTestOps(t, testCluster, testComponent, roleBinding, clusterRoleBinding)

		helmRel := &release.Release{
			Name: testComponent.Spec.Component,
			Config: map[string]interface{}{
				"phase2Permissions": false,
			},
		}

		needsUpdate, err := testOps.sut.checkPhase2PermissionsNeedUpdate(ctx, logrus.New(), testComponent, helmRel)
		r.NoError(err)
		r.True(needsUpdate)
	})

	t.Run("should return false when extended permissions exist and phase2Permissions is true", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testComponent := newTestComponent(t, testCluster.Name, "test-component")

		roleBinding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-rolebinding",
				Namespace: testComponent.Namespace,
				Labels: map[string]string{
					"castware.cast.ai/extended-permissions": "true",
				},
			},
		}

		clusterRoleBinding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-clusterrolebinding",
				Labels: map[string]string{
					"castware.cast.ai/extended-permissions": "true",
				},
			},
		}

		testOps := newComponentTestOps(t, testCluster, testComponent, roleBinding, clusterRoleBinding)

		helmRel := &release.Release{
			Name: testComponent.Spec.Component,
			Config: map[string]interface{}{
				"phase2Permissions": true,
			},
		}

		needsUpdate, err := testOps.sut.checkPhase2PermissionsNeedUpdate(ctx, logrus.New(), testComponent, helmRel)
		r.NoError(err)
		r.False(needsUpdate)
	})

	t.Run("should return false when extended permissions do not exist", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testComponent := newTestComponent(t, testCluster.Name, "test-component")

		testOps := newComponentTestOps(t, testCluster, testComponent)

		// No helm release needed since extended permissions don't exist
		helmRel := &release.Release{
			Name: testComponent.Spec.Component,
		}

		needsUpdate, err := testOps.sut.checkPhase2PermissionsNeedUpdate(ctx, logrus.New(), testComponent, helmRel)
		r.NoError(err)
		r.False(needsUpdate)
	})

	t.Run("should return true when phase2Permissions is not set in helm config", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testComponent := newTestComponent(t, testCluster.Name, "test-component")

		roleBinding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-rolebinding",
				Namespace: testComponent.Namespace,
				Labels: map[string]string{
					"castware.cast.ai/extended-permissions": "true",
				},
			},
		}

		clusterRoleBinding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-clusterrolebinding",
				Labels: map[string]string{
					"castware.cast.ai/extended-permissions": "true",
				},
			},
		}

		testOps := newComponentTestOps(t, testCluster, testComponent, roleBinding, clusterRoleBinding)

		helmRel := &release.Release{
			Name:   testComponent.Spec.Component,
			Config: map[string]interface{}{},
		}

		needsUpdate, err := testOps.sut.checkPhase2PermissionsNeedUpdate(ctx, logrus.New(), testComponent, helmRel)
		r.NoError(err)
		r.True(needsUpdate)
	})
}

func TestReconcileSpotHandlerPhase2Permissions(t *testing.T) {
	t.Run("should trigger upgrade when extended permissions are added after spot-handler is installed", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testComponent := newTestComponent(t, testCluster.Name, "spot-handler")
		testComponent.Spec.Component = "spot-handler"
		testComponent.Spec.Cluster = testCluster.Name
		testComponent.Status.CurrentVersion = "v0.1.0"
		testComponent.Spec.Version = "v0.1.0"
		testComponent.Spec.ReleaseName = "castai-spot-handler"
		meta.SetStatusCondition(&testComponent.Status.Conditions, metav1.Condition{
			Type:   typeAvailableComponent,
			Status: metav1.ConditionTrue,
			Reason: reasonInstalled,
		})

		apiKeySecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testCluster.Name,
				Namespace: testComponent.Namespace,
			},
			Data: map[string][]byte{
				"API_KEY": []byte("test-api-key"),
			},
		}

		roleBinding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-rolebinding",
				Namespace: testComponent.Namespace,
				Labels: map[string]string{
					"castware.cast.ai/extended-permissions": "true",
				},
			},
		}

		clusterRoleBinding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-clusterrolebinding",
				Labels: map[string]string{
					"castware.cast.ai/extended-permissions": "true",
				},
			},
		}

		testOps := newComponentTestOpsWithCastAIClient(t, testCluster, testComponent, apiKeySecret, roleBinding, clusterRoleBinding)

		// spot-handler is an umbrella sub-component, so the reconciler probes
		// whether the umbrella release is installed before managing it.
		expectUmbrellaNotInstalledComponent(testOps, testComponent.Namespace)

		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   testComponent.Namespace,
			ReleaseName: testComponent.Spec.ReleaseName,
		}).Return(&release.Release{
			Name: testComponent.Spec.Component,
			Chart: &chart.Chart{
				Metadata: &chart.Metadata{
					Version: "v0.1.0",
				},
			},
			Config: map[string]interface{}{
				"phase2Permissions": false,
			},
		}, nil).Times(2)

		testOps.mockCastAI.EXPECT().RecordActionResult(gomock.Any(), testCluster.Spec.Cluster.ClusterID, gomock.Any()).Return(nil).AnyTimes()

		testOps.mockHelm.EXPECT().Upgrade(gomock.Any(), gomock.Any()).DoAndReturn(
			func(ctx context.Context, opts helm.UpgradeOptions) (*release.Release, error) {
				phase2, ok := opts.ValuesOverrides["phase2Permissions"].(bool)
				r.True(ok, "phase2Permissions should be present in overrides")
				r.True(phase2, "phase2Permissions should be true")

				return &release.Release{
					Name: testComponent.Spec.Component,
					Info: &release.Info{Status: release.StatusDeployed},
					Chart: &chart.Chart{
						Metadata: &chart.Metadata{
							Version: "v0.1.0",
						},
					},
				}, nil
			},
		)

		req := reconcile.Request{NamespacedName: client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}}
		_, err := testOps.sut.Reconcile(ctx, req)
		r.NoError(err)

		var actualComponent castwarev1alpha1.Component
		err = testOps.sut.Get(ctx, client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}, &actualComponent)
		r.NoError(err)

		progressingCondition := meta.FindStatusCondition(actualComponent.Status.Conditions, typeProgressingComponent)
		r.NotNil(progressingCondition)
		r.Equal(metav1.ConditionTrue, progressingCondition.Status)
		r.Equal(progressingReasonUpgrading, progressingCondition.Reason)
	})

	t.Run("should not trigger upgrade when extended permissions do not exist", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testComponent := newTestComponent(t, testCluster.Name, "spot-handler")
		testComponent.Spec.Component = "spot-handler"
		testComponent.Status.CurrentVersion = "v0.1.1"
		testComponent.Status.LastReportedHelmRevision = 1
		meta.SetStatusCondition(&testComponent.Status.Conditions, metav1.Condition{
			Type:   typeAvailableComponent,
			Status: metav1.ConditionTrue,
			Reason: reasonInstalled,
		})

		testOps := newComponentTestOpsWithCastAIClient(t, testCluster, testComponent)

		// spot-handler is an umbrella sub-component, so the reconciler probes
		// whether the umbrella release is installed before managing it.
		expectUmbrellaNotInstalledComponent(testOps, testComponent.Namespace)

		// Expect GetRelease call for detectAndReportHelmRevisionChange
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   testComponent.Namespace,
			ReleaseName: testComponent.Spec.Component,
		}).Return(&release.Release{
			Name:    testComponent.Spec.Component,
			Version: 1, // Same as LastReportedHelmRevision
			Chart: &chart.Chart{
				Metadata: &chart.Metadata{
					Version: "v0.1.1",
				},
			},
		}, nil).Times(1)

		req := reconcile.Request{NamespacedName: client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}}
		result, err := testOps.sut.Reconcile(ctx, req)
		r.NoError(err)
		r.Equal(time.Minute*15, result.RequeueAfter)

		var actualComponent castwarev1alpha1.Component
		err = testOps.sut.Get(ctx, client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}, &actualComponent)
		r.NoError(err)

		progressingCondition := meta.FindStatusCondition(actualComponent.Status.Conditions, typeProgressingComponent)
		if progressingCondition != nil {
			r.Equal(metav1.ConditionFalse, progressingCondition.Status)
		}
	})

	t.Run("should not trigger upgrade when phase2Permissions is already true", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testComponent := newTestComponent(t, testCluster.Name, "spot-handler")
		testComponent.Spec.Component = "spot-handler"
		testComponent.Status.CurrentVersion = "v0.1.1"
		testComponent.Status.LastReportedHelmRevision = 1
		meta.SetStatusCondition(&testComponent.Status.Conditions, metav1.Condition{
			Type:   typeAvailableComponent,
			Status: metav1.ConditionTrue,
			Reason: reasonInstalled,
		})

		roleBinding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-rolebinding",
				Namespace: testComponent.Namespace,
				Labels: map[string]string{
					"castware.cast.ai/extended-permissions": "true",
				},
			},
		}

		clusterRoleBinding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-clusterrolebinding",
				Labels: map[string]string{
					"castware.cast.ai/extended-permissions": "true",
				},
			},
		}

		testOps := newComponentTestOpsWithCastAIClient(t, testCluster, testComponent, roleBinding, clusterRoleBinding)

		// spot-handler is an umbrella sub-component, so the reconciler probes
		// whether the umbrella release is installed before managing it.
		expectUmbrellaNotInstalledComponent(testOps, testComponent.Namespace)

		// Single GetRelease call in reconcile loop (cached for both phase2 check and revision check)
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   testComponent.Namespace,
			ReleaseName: testComponent.Spec.Component,
		}).Return(&release.Release{
			Name:    testComponent.Spec.Component,
			Version: 1, // Same as LastReportedHelmRevision
			Chart: &chart.Chart{
				Metadata: &chart.Metadata{
					Version: "v0.1.1",
				},
			},
			Config: map[string]interface{}{
				"phase2Permissions": true,
			},
		}, nil).Times(1) // Single call, result reused for both checks

		req := reconcile.Request{NamespacedName: client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}}
		result, err := testOps.sut.Reconcile(ctx, req)
		r.NoError(err)
		r.Equal(time.Minute*15, result.RequeueAfter)

		var actualComponent castwarev1alpha1.Component
		err = testOps.sut.Get(ctx, client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}, &actualComponent)
		r.NoError(err)

		progressingCondition := meta.FindStatusCondition(actualComponent.Status.Conditions, typeProgressingComponent)
		if progressingCondition != nil {
			r.Equal(metav1.ConditionFalse, progressingCondition.Status)
		}
	})
}

// TestReconcileMutualExclusivityGate exercises the umbrella / individual charts
// mutual-exclusivity guard in the ComponentReconciler. The umbrella chart
// (castai-umbrella) renders the same workloads as the individual component
// charts; running both produces duplicate Deployments and conflicting Helm
// ownership, so ambiguity resolves to blocking.
func TestReconcileMutualExclusivityGate(t *testing.T) {
	t.Parallel()

	// expectUmbrellaInstalledComponent sets up the Mothership + helm mock
	// expectations used by forceReadonlyIfUmbrellaInstalled when the umbrella
	// release IS installed: resolve the umbrella release name, then find a
	// deployed umbrella release in helm.
	expectUmbrellaInstalledComponent := func(ops *componentTestOps, namespace string) {
		ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
			Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil)
		ops.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   namespace,
			ReleaseName: components.ComponentNameUmbrella,
		}).Return(&release.Release{
			Name: components.ComponentNameUmbrella,
			Info: &release.Info{Status: release.StatusDeployed},
		}, nil)
	}

	t.Run("per-component CR while umbrella installed is forced read-only with UmbrellaConflict and not installed", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testComponent := newTestComponent(t, testCluster.Name, components.ComponentNameAgent)
		testComponent.Spec.Component = components.ComponentNameAgent
		testComponent.Spec.ReleaseName = components.ComponentNameAgent

		testOps := newComponentTestOpsWithCastAIClient(t, testCluster, testComponent)

		// The gate resolves the umbrella release name from Mothership and finds
		// it installed in helm, so the component is forced read-only.
		expectUmbrellaInstalledComponent(testOps, testComponent.Namespace)

		req := reconcile.Request{NamespacedName: client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}}
		result, err := testOps.sut.Reconcile(ctx, req)
		r.NoError(err)
		// Blocked: requeue with a backoff, no install attempted.
		r.NotEqual(time.Duration(0), result.RequeueAfter)

		var actualComponent castwarev1alpha1.Component
		r.NoError(testOps.sut.Get(ctx, client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}, &actualComponent))

		// Component was forced read-only.
		r.True(actualComponent.Spec.Readonly, "component must be forced read-only when the umbrella is installed")

		// UmbrellaConflict condition is set to True.
		conflict := meta.FindStatusCondition(actualComponent.Status.Conditions, typeUmbrellaConflict)
		r.NotNil(conflict, "UmbrellaConflict condition must be set")
		r.Equal(metav1.ConditionTrue, conflict.Status)
		r.Equal(reasonUmbrellaReleasePresent, conflict.Reason)

		// No progressing/available-install state was recorded: the install path
		// never ran.
		progressing := meta.FindStatusCondition(actualComponent.Status.Conditions, typeProgressingComponent)
		r.Nil(progressing, "component must not be marked as progressing when blocked by the gate")
	})

	t.Run("umbrella CR while individual releases present and migrate is not set is refused with a condition", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		umbrella := newTestComponent(t, testCluster.Name, components.ComponentNameUmbrella)
		umbrella.Spec.Component = components.ComponentNameUmbrella
		umbrella.Spec.ReleaseName = components.ComponentNameUmbrella
		umbrella.Spec.Migrate = false
		// Fresh install: no current version, not progressing.
		umbrella.Status.CurrentVersion = ""
		// values are not needed for the gate path (refusal happens before installComponent).

		testOps := newComponentTestOpsWithCastAIClient(t, testCluster, umbrella)

		// refuseUmbrellaIfIndividualsPresent resolves the umbrella + all
		// sub-component release names from Mothership, then probes helm. The
		// castai-agent release is found deployed, so the install is refused.
		testOps.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
			Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil)
		testOps.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameAgent).
			Return(&castai.Component{Name: components.ComponentNameAgent, ReleaseName: components.ComponentNameAgent}, nil)
		testOps.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameSpotHandler).
			Return(&castai.Component{Name: components.ComponentNameSpotHandler, ReleaseName: components.ComponentNameSpotHandler}, nil)
		testOps.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameClusterController).
			Return(&castai.Component{Name: components.ComponentNameClusterController, ReleaseName: components.ComponentNameClusterController}, nil)

		// castai-agent is installed (the conflict); the other two are not.
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace: umbrella.Namespace, ReleaseName: components.ComponentNameAgent,
		}).Return(&release.Release{Name: components.ComponentNameAgent, Info: &release.Info{Status: release.StatusDeployed}}, nil)
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace: umbrella.Namespace, ReleaseName: components.ComponentNameSpotHandler,
		}).Return(nil, driver.ErrReleaseNotFound)
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace: umbrella.Namespace, ReleaseName: components.ComponentNameClusterController,
		}).Return(nil, driver.ErrReleaseNotFound)

		req := reconcile.Request{NamespacedName: client.ObjectKey{Name: umbrella.Name, Namespace: umbrella.Namespace}}
		result, err := testOps.sut.Reconcile(ctx, req)
		r.NoError(err)
		r.NotEqual(time.Duration(0), result.RequeueAfter)

		var actualComponent castwarev1alpha1.Component
		r.NoError(testOps.sut.Get(ctx, client.ObjectKey{Name: umbrella.Name, Namespace: umbrella.Namespace}, &actualComponent))

		// Umbrella install was refused: UmbrellaConflict=True and Available=False.
		conflict := meta.FindStatusCondition(actualComponent.Status.Conditions, typeUmbrellaConflict)
		r.NotNil(conflict, "UmbrellaConflict condition must be set")
		r.Equal(metav1.ConditionTrue, conflict.Status)
		r.Equal(reasonIndividualReleasesPresent, conflict.Reason)

		available := meta.FindStatusCondition(actualComponent.Status.Conditions, typeAvailableComponent)
		r.NotNil(available, "Available condition must be set")
		r.Equal(metav1.ConditionFalse, available.Status)

		// The install path never ran.
		progressing := meta.FindStatusCondition(actualComponent.Status.Conditions, typeProgressingComponent)
		r.Nil(progressing, "umbrella must not be marked as progressing when refused by the gate")
	})

	t.Run("umbrella CR with migrate true proceeds to install despite individual releases present", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		umbrella := newTestComponent(t, testCluster.Name, components.ComponentNameUmbrella)
		umbrella.Spec.Component = components.ComponentNameUmbrella
		umbrella.Spec.ReleaseName = components.ComponentNameUmbrella
		umbrella.Spec.Migrate = true
		umbrella.Spec.Version = "0.1.0"
		umbrella.Spec.Values = &v1.JSON{Raw: []byte(`{"tags":{"readonly":true}}`)}
		umbrella.Status.CurrentVersion = ""

		testOps := newComponentTestOpsWithCastAIClient(t, testCluster, umbrella)

		// refuseUmbrellaIfIndividualsPresent resolves all release names and
		// finds castai-agent installed, but spec.migrate is true so the install
		// is allowed to proceed (blocked=false).
		testOps.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
			Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil)
		testOps.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameAgent).
			Return(&castai.Component{Name: components.ComponentNameAgent, ReleaseName: components.ComponentNameAgent}, nil)
		testOps.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameSpotHandler).
			Return(&castai.Component{Name: components.ComponentNameSpotHandler, ReleaseName: components.ComponentNameSpotHandler}, nil)
		testOps.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameClusterController).
			Return(&castai.Component{Name: components.ComponentNameClusterController, ReleaseName: components.ComponentNameClusterController}, nil)

		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace: umbrella.Namespace, ReleaseName: components.ComponentNameAgent,
		}).Return(&release.Release{Name: components.ComponentNameAgent, Info: &release.Info{Status: release.StatusDeployed}}, nil)
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace: umbrella.Namespace, ReleaseName: components.ComponentNameSpotHandler,
		}).Return(nil, driver.ErrReleaseNotFound)
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace: umbrella.Namespace, ReleaseName: components.ComponentNameClusterController,
		}).Return(nil, driver.ErrReleaseNotFound)

		// installComponent: record progressing, probe the umbrella release
		// (not found -> install), then install the chart. The deferred
		// recordActionResult on success records an OK result.
		testOps.mockCastAI.EXPECT().RecordActionResult(gomock.Any(), testCluster.Spec.Cluster.ClusterID, gomock.Any()).Return(nil).AnyTimes()

		helmRelease := &release.Release{
			Name:  components.ComponentNameUmbrella,
			Info:  &release.Info{Status: release.StatusDeployed},
			Chart: &chart.Chart{Metadata: &chart.Metadata{Version: "0.1.0"}},
		}
		// installComponent GetRelease for the umbrella release (not found -> install).
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace: umbrella.Namespace, ReleaseName: components.ComponentNameUmbrella,
		}).Return(nil, driver.ErrReleaseNotFound)
		testOps.mockHelm.EXPECT().Install(gomock.Any(), gomock.Any()).Return(helmRelease, nil)

		req := reconcile.Request{NamespacedName: client.ObjectKey{Name: umbrella.Name, Namespace: umbrella.Namespace}}
		_, err := testOps.sut.Reconcile(ctx, req)
		r.NoError(err)

		var actualComponent castwarev1alpha1.Component
		r.NoError(testOps.sut.Get(ctx, client.ObjectKey{Name: umbrella.Name, Namespace: umbrella.Namespace}, &actualComponent))

		// Install proceeded: progressing condition is set to installing.
		progressing := meta.FindStatusCondition(actualComponent.Status.Conditions, typeProgressingComponent)
		r.NotNil(progressing, "umbrella with migrate=true must proceed to install")
		r.Equal(metav1.ConditionTrue, progressing.Status)
		r.Equal(progressingReasonInstalling, progressing.Reason)

		// No UmbrellaConflict condition is left blocking: with migrate=true the
		// gate clears any stale refusal.
		conflict := meta.FindStatusCondition(actualComponent.Status.Conditions, typeUmbrellaConflict)
		if conflict != nil {
			r.Equal(metav1.ConditionFalse, conflict.Status, "UmbrellaConflict must not be True when migrate allowed the install")
		}
	})

	t.Run("per-component CR fails closed when Mothership cannot resolve the umbrella release name", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testComponent := newTestComponent(t, testCluster.Name, components.ComponentNameAgent)
		testComponent.Spec.Component = components.ComponentNameAgent
		testComponent.Spec.ReleaseName = components.ComponentNameAgent

		testOps := newComponentTestOpsWithCastAIClient(t, testCluster, testComponent)

		// Mothership is unreachable for the umbrella lookup, so the gate cannot
		// determine whether the umbrella release is installed. It must fail
		// closed: surface an error so the reconcile requeues instead of letting
		// the individual CR install alongside an umbrella release it could not
		// see.
		testOps.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
			Return(nil, errors.New("mothership timeout"))

		req := reconcile.Request{NamespacedName: client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}}
		result, err := testOps.sut.Reconcile(ctx, req)
		// The error path requeues with a backoff and returns nil (no install).
		r.NoError(err)
		r.NotEqual(time.Duration(0), result.RequeueAfter)

		var actualComponent castwarev1alpha1.Component
		r.NoError(testOps.sut.Get(ctx, client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}, &actualComponent))

		// The component was NOT forced read-only (we couldn't confirm a conflict).
		r.False(actualComponent.Spec.Readonly, "component must not be forced read-only when the gate cannot resolve the umbrella")
		// No install proceeded.
		progressing := meta.FindStatusCondition(actualComponent.Status.Conditions, typeProgressingComponent)
		r.Nil(progressing, "component must not be marked as progressing when the gate fails closed")
	})

	t.Run("per-component CR clears UmbrellaConflict with a NotPresent reason when the umbrella is removed", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := require.New(t)

		testCluster := newTestCluster(t, uuid.NewString(), true)
		testComponent := newTestComponent(t, testCluster.Name, components.ComponentNameAgent)
		testComponent.Spec.Component = components.ComponentNameAgent
		testComponent.Spec.ReleaseName = components.ComponentNameAgent

		testOps := newComponentTestOpsWithCastAIClient(t, testCluster, testComponent)
		req := reconcile.Request{NamespacedName: client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}}

		// First reconcile: the umbrella is installed, so the component is
		// forced read-only with an UmbrellaConflict=True condition.
		testOps.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
			Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil)
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace: testComponent.Namespace, ReleaseName: components.ComponentNameUmbrella,
		}).Return(&release.Release{Name: components.ComponentNameUmbrella, Info: &release.Info{Status: release.StatusDeployed}}, nil)
		_, err := testOps.sut.Reconcile(ctx, req)
		r.NoError(err)

		var afterFirst castwarev1alpha1.Component
		r.NoError(testOps.sut.Get(ctx, client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}, &afterFirst))
		r.True(afterFirst.Spec.Readonly)
		conflict := meta.FindStatusCondition(afterFirst.Status.Conditions, typeUmbrellaConflict)
		r.NotNil(conflict)
		r.Equal(metav1.ConditionTrue, conflict.Status)
		r.Equal(reasonUmbrellaReleasePresent, conflict.Reason)

		// Revert the read-only patch so the gate re-runs on the next pass (the
		// gate only fires when !Readonly). This mirrors an operator that was
		// forced read-only, then the umbrella was removed and management resumed.
		updated := afterFirst.DeepCopy()
		updated.Spec.Readonly = false
		r.NoError(testOps.sut.Patch(ctx, updated, client.MergeFrom(&afterFirst)))

		// Second reconcile: the umbrella release is now gone. The stale conflict
		// condition must clear to False with a reason reflecting the umbrella no
		// longer being present (not the contradictory ...Present reason).
		testOps.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
			Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil)
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace: testComponent.Namespace, ReleaseName: components.ComponentNameUmbrella,
		}).Return(nil, driver.ErrReleaseNotFound)
		// The gate clears the condition and lets the reconcile proceed to
		// install the agent (umbrella no longer blocks it).
		testOps.mockCastAI.EXPECT().RecordActionResult(gomock.Any(), testCluster.Spec.Cluster.ClusterID, gomock.Any()).Return(nil).AnyTimes()
		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace: testComponent.Namespace, ReleaseName: components.ComponentNameAgent,
		}).Return(nil, driver.ErrReleaseNotFound)
		testOps.mockHelm.EXPECT().Install(gomock.Any(), gomock.Any()).Return(&release.Release{
			Name:  components.ComponentNameAgent,
			Info:  &release.Info{Status: release.StatusDeployed},
			Chart: &chart.Chart{Metadata: &chart.Metadata{Version: "v0.1.1"}},
		}, nil)
		_, err = testOps.sut.Reconcile(ctx, req)
		r.NoError(err)

		var afterSecond castwarev1alpha1.Component
		r.NoError(testOps.sut.Get(ctx, client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}, &afterSecond))
		cleared := meta.FindStatusCondition(afterSecond.Status.Conditions, typeUmbrellaConflict)
		r.NotNil(cleared, "UmbrellaConflict condition must still be present (now False)")
		r.Equal(metav1.ConditionFalse, cleared.Status)
		r.Equal(reasonUmbrellaReleaseNotPresent, cleared.Reason, "cleared condition must use a NotPresent reason, not a Present one")
	})
}
