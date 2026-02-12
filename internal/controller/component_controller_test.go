//nolint:goconst
package controller

import (
	"context"
	"errors"
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
			})

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

		// No helm calls expected - the reconciler should just requeue
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

		// No helm calls expected - only status backfill
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
				})

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
				})

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

		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   testComponent.Namespace,
			ReleaseName: testComponent.Spec.Component,
		}).Return(&release.Release{
			Name: testComponent.Spec.Component,
			Config: map[string]interface{}{
				"phase2Permissions": false,
			},
		}, nil)

		needsUpdate, err := testOps.sut.checkAndUpdatePhase2Permissions(ctx, logrus.New(), testComponent)
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

		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   testComponent.Namespace,
			ReleaseName: testComponent.Spec.Component,
		}).Return(&release.Release{
			Name: testComponent.Spec.Component,
			Config: map[string]interface{}{
				"phase2Permissions": true,
			},
		}, nil)

		needsUpdate, err := testOps.sut.checkAndUpdatePhase2Permissions(ctx, logrus.New(), testComponent)
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

		needsUpdate, err := testOps.sut.checkAndUpdatePhase2Permissions(ctx, logrus.New(), testComponent)
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

		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   testComponent.Namespace,
			ReleaseName: testComponent.Spec.Component,
		}).Return(&release.Release{
			Name:   testComponent.Spec.Component,
			Config: map[string]interface{}{},
		}, nil)

		needsUpdate, err := testOps.sut.checkAndUpdatePhase2Permissions(ctx, logrus.New(), testComponent)
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
			})

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
		meta.SetStatusCondition(&testComponent.Status.Conditions, metav1.Condition{
			Type:   typeAvailableComponent,
			Status: metav1.ConditionTrue,
			Reason: reasonInstalled,
		})

		testOps := newComponentTestOps(t, testCluster, testComponent)

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

		testOps := newComponentTestOps(t, testCluster, testComponent, roleBinding, clusterRoleBinding)

		testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
			Namespace:   testComponent.Namespace,
			ReleaseName: testComponent.Spec.Component,
		}).Return(&release.Release{
			Name: testComponent.Spec.Component,
			Config: map[string]interface{}{
				"phase2Permissions": true,
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
}
