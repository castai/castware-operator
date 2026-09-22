package cleanup

import (
	"context"
	"strings"
	"testing"
	"time"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/controller"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestCleanup(t *testing.T) {

	t.Run("should delete operator CRs and CRDs", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Create test components with finalizers
		componentWithFinalizer := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "component-with-finalizer",
				Namespace: "test-namespace",
				Finalizers: []string{
					"castware.cast.ai/cleanup-helm",
				},
			},
			Spec: castwarev1alpha1.ComponentSpec{
				Component: "test-component-1",
				Cluster:   "test-cluster",
				Enabled:   true,
			},
		}

		// Component without finalizer
		componentWithoutFinalizer := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "component-without-finalizer",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ComponentSpec{
				Component: "test-component-2",
				Cluster:   "test-cluster",
				Enabled:   true,
			},
		}

		// Create test cluster
		cluster := &castwarev1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ClusterSpec{
				Cluster: &castwarev1alpha1.ClusterMetadataSpec{
					ClusterID: "test-cluster-id",
				},
			},
		}

		// Create test CRDs
		componentCRD := &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name: "components.castware.cast.ai",
			},
		}

		clusterCRD := &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name: "clusters.castware.cast.ai",
			},
		}

		ops := newTestOps(t, componentWithFinalizer, componentWithoutFinalizer, cluster, componentCRD, clusterCRD)

		// Run cleanup
		err := ops.sut.Run(ctx)
		r.NoError(err)

		// Verify all components are deleted
		componentList := &castwarev1alpha1.ComponentList{}
		err = ops.sut.List(ctx, componentList)
		r.NoError(err)
		r.Empty(componentList.Items, "all components should be deleted")

		// Verify all clusters are deleted
		clusterList := &castwarev1alpha1.ClusterList{}
		err = ops.sut.List(ctx, clusterList)
		r.NoError(err)
		r.Empty(clusterList.Items, "all clusters should be deleted")

		// Verify CRDs are deleted
		crdList := &apiextensionsv1.CustomResourceDefinitionList{}
		err = ops.sut.List(ctx, crdList)
		r.NoError(err)
		r.Empty(crdList.Items, "all CRDs should be deleted")
	})

	t.Run("waits for the operator to resolve the umbrella finalizer before deleting CRDs", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Umbrella component CR with the helm cleanup finalizer.
		umbrella := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "castai-umbrella",
				Namespace: "test-namespace",
				Finalizers: []string{
					controller.ComponentFinalizer,
				},
			},
			Spec: castwarev1alpha1.ComponentSpec{
				Component: components.ComponentNameUmbrella,
				Cluster:   "test-cluster",
				Enabled:   true,
			},
		}

		cluster := &castwarev1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ClusterSpec{
				Cluster: &castwarev1alpha1.ClusterMetadataSpec{
					ClusterID: "test-cluster-id",
				},
			},
		}

		componentCRD := &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name: "components.castware.cast.ai",
			},
		}
		clusterCRD := &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name: "clusters.castware.cast.ai",
			},
		}

		// Give the (simulated) operator ample time before the fallback fires.
		ops := newTestOpsWithWait(t, 10*time.Second, umbrella, cluster, componentCRD, clusterCRD)
		key := client.ObjectKey{Namespace: umbrella.Namespace, Name: umbrella.Name}

		// Simulate the still-running operator: once cleanup has marked and
		// deleted the umbrella CR, remove the finalizer (no helm uninstall).
		type observation struct {
			labelSet     bool
			finalizerSet bool
		}
		obsCh := make(chan observation, 1)
		resolved := make(chan struct{})
		go func() {
			defer close(resolved)
			for {
				var got castwarev1alpha1.Component
				err := ops.sut.Get(ctx, key, &got)
				if apierrors.IsNotFound(err) {
					return
				}
				if err == nil && got.DeletionTimestamp != nil {
					select {
					case obsCh <- observation{
						labelSet:     got.Labels[controller.LabelDeleteCandidate] == "true",
						finalizerSet: controllerutil.ContainsFinalizer(&got, controller.ComponentFinalizer),
					}:
					default:
					}
					controllerutil.RemoveFinalizer(&got, controller.ComponentFinalizer)
					if err := ops.sut.Update(ctx, &got); err != nil {
						return
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
		}()

		err := ops.sut.Run(ctx)
		r.NoError(err)

		select {
		case <-resolved:
		case <-time.After(10 * time.Second):
			t.Error("simulated operator did not finish")
		}

		err = ops.sut.Get(ctx, key, &castwarev1alpha1.Component{})
		r.True(apierrors.IsNotFound(err), "umbrella CR should be fully deleted, got: %v", err)

		// The handoff contract held when the operator resolved it: cleanup
		// had marked the CR and left the finalizer in place.
		select {
		case obs := <-obsCh:
			r.True(obs.labelSet, "cleanup must mark the umbrella CR as delete candidate")
			r.True(obs.finalizerSet, "cleanup must not strip the umbrella finalizer itself")
		default:
			t.Error("simulated operator never observed the terminating umbrella CR")
		}

		// The operator CRDs were deleted only after the CR was resolved.
		for _, name := range []string{"components.castware.cast.ai", "clusters.castware.cast.ai"} {
			err := ops.sut.Get(ctx, client.ObjectKey{Name: name}, &apiextensionsv1.CustomResourceDefinition{})
			r.True(apierrors.IsNotFound(err), "operator CRD %q should be deleted, got: %v", name, err)
		}
	})

	t.Run("falls back to removing the umbrella finalizer when the operator does not", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Umbrella component CR with the helm cleanup finalizer.
		umbrella := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "castai-umbrella",
				Namespace: "test-namespace",
				Finalizers: []string{
					controller.ComponentFinalizer,
				},
			},
			Spec: castwarev1alpha1.ComponentSpec{
				Component: components.ComponentNameUmbrella,
				Cluster:   "test-cluster",
				Enabled:   true,
			},
		}

		// Non-umbrella component CR with the same finalizer.
		agent := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "castai-agent",
				Namespace: "test-namespace",
				Finalizers: []string{
					controller.ComponentFinalizer,
				},
			},
			Spec: castwarev1alpha1.ComponentSpec{
				Component: "castai-agent",
				Cluster:   "test-cluster",
				Enabled:   true,
			},
		}

		cluster := &castwarev1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ClusterSpec{
				Cluster: &castwarev1alpha1.ClusterMetadataSpec{
					ClusterID: "test-cluster-id",
				},
			},
		}

		componentCRD := &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name: "components.castware.cast.ai",
			},
		}
		clusterCRD := &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name: "clusters.castware.cast.ai",
			},
		}

		// No operator runs in this test: the wait times out and cleanup must
		// remove the finalizer itself so nothing is left stuck Terminating.
		ops := newTestOps(t, umbrella, agent, cluster, componentCRD, clusterCRD)

		err := ops.sut.Run(ctx)
		r.NoError(err)

		// The umbrella CR is fully deleted despite no operator resolving it.
		err = ops.sut.Get(ctx, client.ObjectKey{Namespace: umbrella.Namespace, Name: umbrella.Name}, &castwarev1alpha1.Component{})
		r.True(apierrors.IsNotFound(err), "umbrella CR should be fully deleted, got: %v", err)

		// The non-umbrella CR had its finalizer stripped, so the Delete
		// removed it entirely.
		err = ops.sut.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: agent.Name}, &castwarev1alpha1.Component{})
		r.True(apierrors.IsNotFound(err), "non-umbrella component CR should be fully deleted, got: %v", err)

		// The cluster CR and the operator CRDs are deleted.
		err = ops.sut.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, &castwarev1alpha1.Cluster{})
		r.True(apierrors.IsNotFound(err), "cluster CR should be deleted, got: %v", err)
		for _, name := range []string{"components.castware.cast.ai", "clusters.castware.cast.ai"} {
			err := ops.sut.Get(ctx, client.ObjectKey{Name: name}, &apiextensionsv1.CustomResourceDefinition{})
			r.True(apierrors.IsNotFound(err), "operator CRD %q should be deleted, got: %v", name, err)
		}
	})

	t.Run("warns and deletes directly an umbrella CR without finalizer", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Umbrella component CR without the helm cleanup finalizer.
		umbrella := &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "castai-umbrella",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ComponentSpec{
				Component: components.ComponentNameUmbrella,
				Cluster:   "test-cluster",
				Enabled:   true,
			},
		}

		cluster := &castwarev1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ClusterSpec{
				Cluster: &castwarev1alpha1.ClusterMetadataSpec{
					ClusterID: "test-cluster-id",
				},
			},
		}

		componentCRD := &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name: "components.castware.cast.ai",
			},
		}
		clusterCRD := &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name: "clusters.castware.cast.ai",
			},
		}

		ops := newTestOps(t, umbrella, cluster, componentCRD, clusterCRD)

		// Capture cleanup's logs to assert the missing finalizer is visible.
		log := logrus.New()
		hook := &logHook{entries: &[]logrus.Entry{}}
		log.AddHook(hook)
		ops.sut.log = log

		err := ops.sut.Run(ctx)
		r.NoError(err)

		// The CR is deleted directly, without the label-gated handoff.
		err = ops.sut.Get(ctx, client.ObjectKey{Namespace: umbrella.Namespace, Name: umbrella.Name}, &castwarev1alpha1.Component{})
		r.True(apierrors.IsNotFound(err), "umbrella CR should be deleted, got: %v", err)

		// The bypassed handoff is visible in the logs.
		warned := false
		for _, e := range *hook.entries {
			if e.Level == logrus.WarnLevel &&
				e.Data["component"] == "test-namespace/castai-umbrella" &&
				strings.Contains(e.Message, "finalizer") {
				warned = true
			}
		}
		r.True(warned, "expected a warning about the missing umbrella finalizer, got: %+v", *hook.entries)

		// The operator CRDs are still deleted.
		for _, name := range []string{"components.castware.cast.ai", "clusters.castware.cast.ai"} {
			err := ops.sut.Get(ctx, client.ObjectKey{Name: name}, &apiextensionsv1.CustomResourceDefinition{})
			r.True(apierrors.IsNotFound(err), "operator CRD %q should be deleted, got: %v", name, err)
		}
	})

	t.Run("cleanup deletes only the operator CRDs; umbrella subcomponent CRDs survive", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// The last three names are representative: these CRDs are owned by
		// external umbrella subcomponent charts (not this repo); the invariant
		// under test is that exactly the two operator CRDs are deleted.
		crdNames := []string{
			"components.castware.cast.ai",
			"clusters.castware.cast.ai",
			"migrations.live.cast.ai",
			"podmutations.pod-mutations.cast.ai",
			"recommendations.cast.ai",
			"gpurecommendations.cast.ai",
			"custommetricsexporterconfigs.cast.ai",
		}
		objs := make([]client.Object, 0, len(crdNames))
		for _, name := range crdNames {
			objs = append(objs, &apiextensionsv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name: name,
				},
			})
		}

		ops := newTestOps(t, objs...)

		// Run cleanup
		err := ops.sut.Run(ctx)
		r.NoError(err)

		operatorCRDs := []string{
			"components.castware.cast.ai",
			"clusters.castware.cast.ai",
		}
		for _, name := range operatorCRDs {
			crd := &apiextensionsv1.CustomResourceDefinition{}
			err := ops.sut.Get(ctx, client.ObjectKey{Name: name}, crd)
			r.True(apierrors.IsNotFound(err), "operator CRD %q should be deleted, got: %v", name, err)
		}

		umbrellaSubcomponentCRDs := []string{
			"migrations.live.cast.ai",
			"podmutations.pod-mutations.cast.ai",
			"recommendations.cast.ai",
			"gpurecommendations.cast.ai",
			"custommetricsexporterconfigs.cast.ai",
		}
		for _, name := range umbrellaSubcomponentCRDs {
			crd := &apiextensionsv1.CustomResourceDefinition{}
			err := ops.sut.Get(ctx, client.ObjectKey{Name: name}, crd)
			r.NoError(err, "umbrella subcomponent CRD %q must survive the operator cleanup", name)
		}
	})
}

type testOps struct {
	sut *Service
}

type logHook struct {
	entries *[]logrus.Entry
}

func (h *logHook) Levels() []logrus.Level { return logrus.AllLevels }

func (h *logHook) Fire(entry *logrus.Entry) error {
	*h.entries = append(*h.entries, *entry)
	return nil
}

func newTestOps(t *testing.T, objs ...client.Object) *testOps {
	return newTestOpsWithWait(t, 100*time.Millisecond, objs...)
}

func newTestOpsWithWait(t *testing.T, waitTimeout time.Duration, objs ...client.Object) *testOps {
	t.Helper()
	r := require.New(t)
	scheme := runtime.NewScheme()

	err := castwarev1alpha1.AddToScheme(scheme)
	r.NoError(err)

	err = corev1.AddToScheme(scheme)
	r.NoError(err)

	err = apiextensionsv1.AddToScheme(scheme)
	r.NoError(err)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(objs...).Build()

	opts := &testOps{
		sut: &Service{
			Client:                c,
			log:                   logrus.New(),
			umbrellaFinalizerWait: waitTimeout,
		},
	}

	return opts
}
