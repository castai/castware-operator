package controller

import (
	"context"
	"testing"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
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
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestReconcile(t *testing.T) {
	t.Run("when migrating from helm", func(t *testing.T) {
		t.Run("should set status condition to progressing on the first reconcile loop", func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			r := require.New(t)

			testCluster := newTestCluster(t, uuid.NewString())
			testComponent := newTestComponent(t, testCluster.Name, "test-component")
			testComponent.Spec.Migration = "helm"

			testOps := newComponentTestOps(t, testCluster, testComponent)

			req := reconcile.Request{NamespacedName: client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}}

			_, err := testOps.sut.Reconcile(ctx, req)
			r.NoError(err)

			var actualComponent castwarev1alpha1.Component
			err = testOps.sut.Client.Get(ctx, client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}, &actualComponent)
			r.NoError(err)

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

			testCluster := newTestCluster(t, uuid.NewString())
			testComponent := newTestComponent(t, testCluster.Name, "test-component")
			testComponent.Spec.Migration = "helm"

			testOps := newComponentTestOps(t, testCluster, testComponent)

			req := reconcile.Request{NamespacedName: client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}}

			_, err := testOps.sut.Reconcile(ctx, req)
			r.NoError(err)

			testOps.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{
				Namespace:   testComponent.Namespace,
				ReleaseName: testComponent.Spec.Component,
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
			err = testOps.sut.Client.Get(ctx, client.ObjectKey{Name: testComponent.Name, Namespace: testComponent.Namespace}, &actualComponent)
			r.NoError(err)

			r.Equal("0.1.2", actualComponent.Status.CurrentVersion)
			r.Len(actualComponent.Status.Conditions, 2)

			progressingCondition := meta.FindStatusCondition(actualComponent.Status.Conditions, typeProgressingComponent)
			r.NotNil(progressingCondition)
			r.Equal(metav1.ConditionFalse, progressingCondition.Status)
			r.Equal("Completed", progressingCondition.Reason)

			availableCondition := meta.FindStatusCondition(actualComponent.Status.Conditions, typeAvailableComponent)
			r.Equal(metav1.ConditionTrue, availableCondition.Status)
			r.Equal(reasonInstalled, availableCondition.Reason)
		})
	})

}

func newTestComponent(t *testing.T, clusterName, name string) *castwarev1alpha1.Component {
	t.Helper()
	return &castwarev1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "test-namespace",
		},
		Spec: castwarev1alpha1.ComponentSpec{
			Component: name,
			Cluster:   clusterName,
			Enabled:   true,
			Version:   "v0.1.1",
			Values:    &v1.JSON{Raw: []byte(`{"value1": "value1-value", "value2": true}`)},
			Migration: "",
			Readonly:  false,
		},
		Status: castwarev1alpha1.ComponentStatus{},
	}
}

type componentTestOps struct {
	sut      *ComponentReconciler
	mockHelm *mock_helm.MockClient
}

func newComponentTestOps(t *testing.T, objs ...client.Object) *componentTestOps {
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
