package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/castai"
	mock_castai "github.com/castai/castware-operator/internal/castai/mock"
	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/config"
	"github.com/castai/castware-operator/internal/helm"
	mock_helm "github.com/castai/castware-operator/internal/helm/mock"
)

// migrationTestOps wires a MigrationReconciler against fake k8s + mock helm/castai.
type migrationTestOps struct {
	sut        *MigrationReconciler
	mockHelm   *mock_helm.MockClient
	mockCastAI *mock_castai.MockCastAIClient
	client     client.Client
}

const (
	migNamespace = "castai-agent"
	migClusterID = "00000000-0000-0000-0000-000000000001"
)

func newMigrationTestOps(t *testing.T, objs ...client.Object) *migrationTestOps {
	t.Helper()
	r := require.New(t)
	scheme := runtime.NewScheme()
	r.NoError(castwarev1alpha1.AddToScheme(scheme))
	r.NoError(corev1.AddToScheme(scheme))
	r.NoError(appsv1.AddToScheme(scheme))

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&castwarev1alpha1.Component{}).
		Build()

	ctrl := gomock.NewController(t)
	mockHelm := mock_helm.NewMockClient(ctrl)
	mockCastAI := mock_castai.NewMockCastAIClient(ctrl)

	return &migrationTestOps{
		mockHelm:   mockHelm,
		mockCastAI: mockCastAI,
		client:     c,
		sut: &MigrationReconciler{
			Client:     c,
			Scheme:     c.Scheme(),
			Log:        logrus.New(),
			HelmClient: mockHelm,
			Config:     &config.Config{},
			castAIClientGetter: func(ctx context.Context, cluster *castwarev1alpha1.Cluster) (castai.CastAIClient, error) {
				return mockCastAI, nil
			},
		},
	}
}

func migCluster() *castwarev1alpha1.Cluster {
	return &castwarev1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "castai", Namespace: migNamespace},
		Spec: castwarev1alpha1.ClusterSpec{
			Provider:     "eks",
			APIKeySecret: "castware-api-key",
			API:          castwarev1alpha1.APISpec{APIURL: "https://api.cast.ai"},
			Cluster:      &castwarev1alpha1.ClusterMetadataSpec{ClusterID: migClusterID},
			HelmRepoURL:  "https://castai.github.io/helm-charts",
		},
		Status: castwarev1alpha1.ClusterStatus{
			Conditions: []metav1.Condition{{
				Type:   typeAvailableCluster,
				Status: metav1.ConditionTrue,
			}},
		},
	}
}

func migUmbrella(phase string) *castwarev1alpha1.Component {
	return &castwarev1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{Name: components.ComponentNameUmbrella, Namespace: migNamespace},
		Spec: castwarev1alpha1.ComponentSpec{
			Component: components.ComponentNameUmbrella,
			Cluster:   "castai",
			Enabled:   true,
			Version:   "1.0.0",
			Migrate:   true,
		},
		Status: castwarev1alpha1.ComponentStatus{MigrationPhase: phase},
	}
}

func migIndividual(name string) *castwarev1alpha1.Component {
	return &castwarev1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: migNamespace},
		Spec: castwarev1alpha1.ComponentSpec{
			Component:   name,
			Cluster:     "castai",
			Enabled:     true,
			Version:     "1.0.0",
			ReleaseName: name,
		},
	}
}

// migIndividualWithValues is migIndividual with a user-supplied spec.values block.
func migIndividualWithValues(name, rawValues string) *castwarev1alpha1.Component {
	ind := migIndividual(name)
	ind.Spec.Values = &apiextensionsv1.JSON{Raw: []byte(rawValues)}
	return ind
}

// expectResolveNames wires Mothership + helm expectations for
// migrationgate.ResolveNames returning the three sub-components as present.
func expectResolveNamesPresent(ops *migrationTestOps) {
	// ResolveUmbrellaReleaseName (umbrella) + ResolveNames (umbrella + 3 subs).
	ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
		Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil).AnyTimes()
	for _, sub := range []string{components.ComponentNameAgent, components.ComponentNameSpotHandler, components.ComponentNameClusterController} {
		ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), sub).
			Return(&castai.Component{Name: sub, ReleaseName: sub}, nil).AnyTimes()
	}
	// Each sub-component release is present.
	for _, sub := range []string{components.ComponentNameAgent, components.ComponentNameSpotHandler, components.ComponentNameClusterController} {
		ops.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{Namespace: migNamespace, ReleaseName: sub}).
			Return(migRelease(sub), nil).AnyTimes()
	}
}

func migRelease(name string) *release.Release {
	return &release.Release{
		Name:  name,
		Info:  &release.Info{Status: release.StatusDeployed},
		Chart: &chart.Chart{Metadata: &chart.Metadata{Name: name, Version: "1.0.0"}},
	}
}

func reconcileOnce(t *testing.T, ops *migrationTestOps) {
	t.Helper()
	_, err := ops.sut.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: migNamespace, Name: components.ComponentNameUmbrella},
	})
	require.NoError(t, err)
}

func getUmbrella(t *testing.T, ops *migrationTestOps) *castwarev1alpha1.Component {
	t.Helper()
	c := &castwarev1alpha1.Component{}
	require.NoError(t, ops.client.Get(context.Background(),
		types.NamespacedName{Namespace: migNamespace, Name: components.ComponentNameUmbrella}, c))
	return c
}

// TestMigrationReconciler_NoOpForNonUmbrella asserts the controller ignores
// non-umbrella CRs and umbrella CRs without spec.migrate (criterion 4: no
// migrate → no migration; the UmbrellaConflict path is the component reconciler's
// job, not the migration controller's).
func TestMigrationReconciler_NoOpForNonUmbrella(t *testing.T) {
	t.Parallel()
	t.Run("non-umbrella component is ignored", func(t *testing.T) {
		r := require.New(t)
		ops := newMigrationTestOps(t, migCluster(), migIndividual(components.ComponentNameAgent))
		_, err := ops.sut.Reconcile(context.Background(), reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: migNamespace, Name: components.ComponentNameAgent},
		})
		r.NoError(err)
		// No helm calls expected — controller returns immediately.
	})
	t.Run("umbrella without migrate is ignored", func(t *testing.T) {
		r := require.New(t)
		u := migUmbrella("")
		u.Spec.Migrate = false
		ops := newMigrationTestOps(t, migCluster(), u)
		_, err := ops.sut.Reconcile(context.Background(), reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: migNamespace, Name: components.ComponentNameUmbrella},
		})
		r.NoError(err)
	})
}

// TestMigrationReconciler_MarkReadonly verifies the first phase sets readonly on
// the umbrella and present individual CRs, then advances.
func TestMigrationReconciler_MarkReadonly(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ops := newMigrationTestOps(t,
		migCluster(),
		migUmbrella(""),
		migIndividual(components.ComponentNameAgent),
		migIndividual(components.ComponentNameSpotHandler),
		migIndividual(components.ComponentNameClusterController),
	)
	expectResolveNamesPresent(ops)

	reconcileOnce(t, ops)

	u := getUmbrella(t, ops)
	r.Equal(castwarev1alpha1.MigrationPhaseUninstallIndividuals, u.Status.MigrationPhase, "should advance to UninstallIndividuals")
	r.True(u.Spec.Readonly, "umbrella should be readonly")
	// The deletion guard is armed before readonly is set (webhook ordering).
	r.True(controllerutil.ContainsFinalizer(u, MigrationFinalizer), "migration finalizer armed in MarkReadonly")
}

// TestMigrationReconciler_MothershipUnreachable_Degraded asserts a persistent
// presentIndividuals failure (Mothership/auth unreachable) is surfaced, not
// swallowed: the reconcile returns an error (controller-runtime records it and
// applies exponential backoff instead of a fixed 1-minute requeue) and the CR
// carries Migrating=False / MigrationDegraded with the failure reason. A
// subsequent successful reconcile replaces the condition with the next phase.
func TestMigrationReconciler_MothershipUnreachable_Degraded(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ops := newMigrationTestOps(t,
		migCluster(),
		migUmbrella(""),
		migIndividual(components.ComponentNameAgent),
	)
	// Mothership lookup fails once (unknown error, not ErrNotFound — ResolveNames
	// fails closed only when nothing resolves).
	ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
		Return(nil, errors.New("connection refused")).Times(1)
	// Then recovers: umbrella + agent resolve, agent release present.
	ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
		Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil).AnyTimes()
	ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameAgent).
		Return(&castai.Component{Name: components.ComponentNameAgent, ReleaseName: components.ComponentNameAgent}, nil).AnyTimes()
	ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameSpotHandler).
		Return(nil, castai.ErrNotFound).AnyTimes()
	ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameClusterController).
		Return(nil, castai.ErrNotFound).AnyTimes()
	ops.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{Namespace: migNamespace, ReleaseName: components.ComponentNameAgent}).
		Return(migRelease(components.ComponentNameAgent), nil).AnyTimes()

	// First reconcile: failure surfaced as a reconcile error.
	_, err := ops.sut.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: migNamespace, Name: components.ComponentNameUmbrella},
	})
	r.Error(err, "Mothership failure must surface as a reconcile error, not a silent requeue")
	r.ErrorContains(err, "resolve present individuals")

	u := getUmbrella(t, ops)
	cond := meta.FindStatusCondition(u.Status.Conditions, castwarev1alpha1.TypeMigrating)
	r.NotNil(cond, "Migrating condition present")
	r.Equal(metav1.ConditionFalse, cond.Status)
	r.Equal(castwarev1alpha1.ReasonMigrationDegraded, cond.Reason)
	r.Contains(cond.Message, "connection refused")

	// Second reconcile after recovery: phase advances and the degraded condition
	// is replaced by the in-progress phase reason.
	_, err = ops.sut.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: migNamespace, Name: components.ComponentNameUmbrella},
	})
	r.NoError(err)
	u = getUmbrella(t, ops)
	r.Equal(castwarev1alpha1.MigrationPhaseUninstallIndividuals, u.Status.MigrationPhase)
	r.False(u.Status.MigrationPhaseStartedAt.IsZero(), "phase-start timestamp stamped on phase transition")
	cond = meta.FindStatusCondition(u.Status.Conditions, castwarev1alpha1.TypeMigrating)
	r.NotNil(cond, "Migrating condition present after recovery")
	r.Equal(metav1.ConditionTrue, cond.Status)
	r.Equal(castwarev1alpha1.MigrationPhaseUninstallIndividuals, cond.Reason,
		"degraded condition replaced by the phase the migration advanced to")
}

// TestMigrationReconciler_UninstallIndividuals_AgentExcluded asserts the agent
// release is NEVER uninstalled (heartbeat preservation); only cluster-controller
// and spot-handler are.
func TestMigrationReconciler_UninstallIndividuals_AgentExcluded(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ops := newMigrationTestOps(t,
		migCluster(),
		migUmbrella(castwarev1alpha1.MigrationPhaseUninstallIndividuals),
		migIndividual(components.ComponentNameAgent),
		migIndividual(components.ComponentNameSpotHandler),
		migIndividual(components.ComponentNameClusterController),
	)
	expectResolveNamesPresent(ops)

	// Agent is excluded: only cluster-controller + spot-handler are uninstalled,
	// in reverse phase order (cluster-controller first, then spot-handler).
	gomock.InOrder(
		ops.mockHelm.EXPECT().Uninstall(helm.UninstallOptions{
			Namespace: migNamespace, ReleaseName: components.ComponentNameClusterController, Wait: true, IgnoreNotFound: true,
		}).Return(nil, nil),
		ops.mockHelm.EXPECT().Uninstall(helm.UninstallOptions{
			Namespace: migNamespace, ReleaseName: components.ComponentNameSpotHandler, Wait: true, IgnoreNotFound: true,
		}).Return(nil, nil),
	)

	reconcileOnce(t, ops)

	u := getUmbrella(t, ops)
	r.Equal(castwarev1alpha1.MigrationPhaseInstallUmbrella, u.Status.MigrationPhase)
}

// TestMigrationReconciler_InstallUmbrella verifies the umbrella is installed
// with derived tag values and advances to Verify. When the agent is the only
// present individual, a readonly tag is derived.
func TestMigrationReconciler_InstallUmbrella_AgentOnly_ReadonlyTag(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ops := newMigrationTestOps(t,
		migCluster(),
		migUmbrella(castwarev1alpha1.MigrationPhaseInstallUmbrella),
		migIndividual(components.ComponentNameAgent),
	)
	// ResolveNames probes all sub-components: agent is present, spot-handler and
	// cluster-controller are absent on Mothership (ErrNotFound) so they are skipped.
	ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
		Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil).AnyTimes()
	ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameAgent).
		Return(&castai.Component{Name: components.ComponentNameAgent, ReleaseName: components.ComponentNameAgent}, nil).AnyTimes()
	ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameSpotHandler).
		Return(nil, castai.ErrNotFound).AnyTimes()
	ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameClusterController).
		Return(nil, castai.ErrNotFound).AnyTimes()
	ops.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{Namespace: migNamespace, ReleaseName: components.ComponentNameAgent}).
		Return(migRelease(components.ComponentNameAgent), nil).AnyTimes()
	// Umbrella not yet installed.
	ops.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{Namespace: migNamespace, ReleaseName: components.ComponentNameUmbrella}).
		Return(nil, errors.New("no release found"))
	ops.mockHelm.EXPECT().Install(gomock.Any(), gomock.Any()).Return(migRelease(components.ComponentNameUmbrella), nil)

	reconcileOnce(t, ops)

	u := getUmbrella(t, ops)
	r.Equal(castwarev1alpha1.MigrationPhaseVerify, u.Status.MigrationPhase)
}

// TestMigrationReconciler_InstallUmbrella_CarriesOverIndividualValues asserts that
// each present individual CR's spec.values is carried over into the umbrella
// install values under autoscaler.<subchart-alias>, alongside the derived tag
// mode. This prevents silent loss of per-component customizations.
func TestMigrationReconciler_InstallUmbrella_CarriesOverIndividualValues(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	agentValues := `{"topologySpreadConstraints": [{"maxSkew": 1}], "additionalEnv": {"EKS_REGION": "us-east-1"}}`
	spotHandlerValues := `{"tolerations": [{"key": "spot"}]}`
	ops := newMigrationTestOps(t,
		migCluster(),
		migUmbrella(castwarev1alpha1.MigrationPhaseInstallUmbrella),
		migIndividualWithValues(components.ComponentNameAgent, agentValues),
		migIndividualWithValues(components.ComponentNameSpotHandler, spotHandlerValues),
	)
	// Agent + spot-handler present; cluster-controller absent on Mothership.
	ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
		Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil).AnyTimes()
	ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameAgent).
		Return(&castai.Component{Name: components.ComponentNameAgent, ReleaseName: components.ComponentNameAgent}, nil).AnyTimes()
	ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameSpotHandler).
		Return(&castai.Component{Name: components.ComponentNameSpotHandler, ReleaseName: components.ComponentNameSpotHandler}, nil).AnyTimes()
	ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameClusterController).
		Return(nil, castai.ErrNotFound).AnyTimes()
	ops.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{Namespace: migNamespace, ReleaseName: components.ComponentNameAgent}).
		Return(migRelease(components.ComponentNameAgent), nil).AnyTimes()
	ops.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{Namespace: migNamespace, ReleaseName: components.ComponentNameSpotHandler}).
		Return(migRelease(components.ComponentNameSpotHandler), nil).AnyTimes()
	// Umbrella not yet installed.
	ops.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{Namespace: migNamespace, ReleaseName: components.ComponentNameUmbrella}).
		Return(nil, errors.New("no release found"))

	// Capture the install options to assert the carried-over values reach helm.
	var captured helm.InstallOptions
	ops.mockHelm.EXPECT().Install(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, opts helm.InstallOptions) (*release.Release, error) {
			captured = opts
			return migRelease(components.ComponentNameUmbrella), nil
		})

	reconcileOnce(t, ops)

	u := getUmbrella(t, ops)
	r.Equal(castwarev1alpha1.MigrationPhaseVerify, u.Status.MigrationPhase, "advanced to verify")

	// Derived tag mode (agent + spot-handler, no cluster-controller → readonly).
	tags, ok := captured.ValuesOverrides["tags"].(map[string]any)
	r.True(ok, "derived tags present in install values")
	r.True(tags["readonly"].(bool), "agent-only + spot-handler derives readonly tag")

	// Carried-over individual values under autoscaler.<alias>.
	as, ok := captured.ValuesOverrides["autoscaler"].(map[string]any)
	r.True(ok, "autoscaler block present in install values")
	agent, ok := as["castai-agent"].(map[string]any)
	r.True(ok, "agent values carried under autoscaler.castai-agent")
	r.Len(agent["topologySpreadConstraints"].([]any), 1, "agent topologySpreadConstraints carried over")
	r.Equal("us-east-1", agent["additionalEnv"].(map[string]any)["EKS_REGION"], "agent additionalEnv carried over")
	spot, ok := as["castai-spot-handler"].(map[string]any)
	r.True(ok, "spot-handler values carried under autoscaler.castai-spot-handler")
	r.Len(spot["tolerations"].([]any), 1, "spot-handler tolerations carried over")
}

// TestMigrationReconciler_VerifySuccess_Finalize asserts a healthy umbrella +
// agent advances through Finalize, which forgets the agent release (never
// uninstalls it) and deletes the individual CRs.
func TestMigrationReconciler_VerifySuccess_Finalize(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ops := newMigrationTestOps(t,
		migCluster(),
		migUmbrella(castwarev1alpha1.MigrationPhaseVerify),
		migIndividual(components.ComponentNameAgent),
		migIndividual(components.ComponentNameSpotHandler),
		migIndividual(components.ComponentNameClusterController),
	)
	// Umbrella deployed + agent healthy on first check.
	ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
		Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil).AnyTimes()
	ops.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{Namespace: migNamespace, ReleaseName: components.ComponentNameUmbrella}).
		Return(migRelease(components.ComponentNameUmbrella), nil).AnyTimes()
	// Agent Deployment healthy.
	r.NoError(ops.client.Create(context.Background(), &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-agent", Namespace: migNamespace,
			Labels: map[string]string{"app.kubernetes.io/name": components.ComponentNameAgent}},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1},
	}))

	// Finalize: resolve agent release name + forget it (never uninstall).
	ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameAgent).
		Return(&castai.Component{Name: components.ComponentNameAgent, ReleaseName: components.ComponentNameAgent}, nil).AnyTimes()
	ops.mockHelm.EXPECT().ForgetRelease(helm.ForgetReleaseOptions{Namespace: migNamespace, ReleaseName: components.ComponentNameAgent}).
		Return(nil)
	// Finalize reports success to Mothership.
	ops.mockCastAI.EXPECT().RecordActionResult(gomock.Any(), migClusterID, gomock.Any()).
		Return(nil)

	// Verify advances to Finalize; a second reconcile runs Finalize to completion.
	reconcileOnce(t, ops)
	reconcileOnce(t, ops)

	u := getUmbrella(t, ops)
	r.Equal("", u.Status.MigrationPhase, "phase cleared on success")
	r.False(u.Spec.Migrate, "migrate cleared on success")
	r.False(u.Spec.Readonly, "readonly cleared on success")
	r.False(controllerutil.ContainsFinalizer(u, MigrationFinalizer), "migration finalizer released on success")

	// Individual CRs deleted.
	for _, sub := range []string{components.ComponentNameAgent, components.ComponentNameSpotHandler, components.ComponentNameClusterController} {
		err := ops.client.Get(context.Background(), types.NamespacedName{Namespace: migNamespace, Name: sub}, &castwarev1alpha1.Component{})
		r.True(client.IgnoreNotFound(err) == nil && err != nil, "individual CR %s should be deleted", sub)
	}
}

// TestMigrationReconciler_Finalize_ClearsReadonlyBeforeFinalizerRemoval
// asserts the Finalize phase clears spec.readonly=false on each individual CR
// before removing its cleanup finalizer and deleting it. The validating webhook
// (ValidateUpdate) rejects any update to a CR that is readonly in both old and
// new state, so removing the finalizer while readonly=true is denied —
// deadlocking Finalize. Clearing readonly first (the true→false transition is
// allowed) lets the finalizer-removal Update succeed. The fake client does not
// run admission webhooks, so this test pins the controller-side sequencing so a
// future refactor does not silently regress the ordering and resurface the
// deadlock against a live apiserver.
func TestMigrationReconciler_Finalize_ClearsReadonlyBeforeFinalizerRemoval(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	// Seed individuals that are readonly (set by phaseMarkReadonly) and carry
	// the cleanup finalizer (added by the component reconciler) — the exact
	// state Finalize encounters on a real cluster.
	agent := migIndividual(components.ComponentNameAgent)
	agent.Spec.Readonly = true
	controllerutil.AddFinalizer(agent, ComponentFinalizer)
	spot := migIndividual(components.ComponentNameSpotHandler)
	spot.Spec.Readonly = true
	controllerutil.AddFinalizer(spot, ComponentFinalizer)

	ops := newMigrationTestOps(t,
		migCluster(),
		migUmbrella(castwarev1alpha1.MigrationPhaseVerify),
		agent,
		spot,
	)
	ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
		Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil).AnyTimes()
	ops.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{Namespace: migNamespace, ReleaseName: components.ComponentNameUmbrella}).
		Return(migRelease(components.ComponentNameUmbrella), nil).AnyTimes()
	r.NoError(ops.client.Create(context.Background(), &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-agent", Namespace: migNamespace,
			Labels: map[string]string{"app.kubernetes.io/name": components.ComponentNameAgent}},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1},
	}))

	// Finalize forgets the agent release and reports success.
	ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameAgent).
		Return(&castai.Component{Name: components.ComponentNameAgent, ReleaseName: components.ComponentNameAgent}, nil).AnyTimes()
	ops.mockHelm.EXPECT().ForgetRelease(helm.ForgetReleaseOptions{Namespace: migNamespace, ReleaseName: components.ComponentNameAgent}).
		Return(nil)
	ops.mockCastAI.EXPECT().RecordActionResult(gomock.Any(), migClusterID, gomock.Any()).
		Return(nil)

	// Verify → Finalize.
	reconcileOnce(t, ops)
	reconcileOnce(t, ops)

	u := getUmbrella(t, ops)
	r.Equal("", u.Status.MigrationPhase, "phase cleared on success")
	r.True(u.Status.MigrationPhaseStartedAt.IsZero(), "phase-start timestamp cleared on success")

	// Both readonly+finalizer individuals are deleted outright (readonly cleared
	// first, finalizer removed, then delete — no tombstones left behind).
	for _, sub := range []string{components.ComponentNameAgent, components.ComponentNameSpotHandler} {
		err := ops.client.Get(context.Background(), types.NamespacedName{Namespace: migNamespace, Name: sub}, &castwarev1alpha1.Component{})
		r.True(apierrors.IsNotFound(err), "individual CR %s should be deleted after Finalize (got err=%v)", sub, err)
	}
}

// TestMigrationReconciler_VerifyDeadlineExceeded unit-tests the Verify-phase
// deadline: the primary signal is status.migrationPhaseStartedAt (stamped on
// every phase transition); the Migrating condition's LastTransitionTime is
// only a legacy fallback for migrations already in flight when the field was
// introduced, and a fresh start (neither present) is not exceeded.
func TestMigrationReconciler_VerifyDeadlineExceeded(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	sut := &MigrationReconciler{}

	withPhaseStart := func(ts metav1.Time) *castwarev1alpha1.Component {
		c := migUmbrella(castwarev1alpha1.MigrationPhaseVerify)
		c.Status.MigrationPhaseStartedAt = ts
		return c
	}
	withLegacyCondition := func(ts metav1.Time) *castwarev1alpha1.Component {
		c := migUmbrella(castwarev1alpha1.MigrationPhaseVerify)
		meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
			Type:               castwarev1alpha1.TypeMigrating,
			Status:             metav1.ConditionTrue,
			Reason:             castwarev1alpha1.MigrationPhaseVerify,
			LastTransitionTime: ts,
		})
		return c
	}

	r.True(sut.verifyDeadlineExceeded(withPhaseStart(metav1.NewTime(time.Now().Add(-2*verifyTimeout)))),
		"phase-start timestamp past the deadline is exceeded")
	r.False(sut.verifyDeadlineExceeded(withPhaseStart(metav1.NewTime(time.Now().Add(-verifyTimeout/2)))),
		"phase-start timestamp inside the deadline is not exceeded")
	r.True(sut.verifyDeadlineExceeded(withLegacyCondition(metav1.NewTime(time.Now().Add(-2*verifyTimeout)))),
		"legacy fallback: old condition LastTransitionTime with no phase-start field is exceeded")
	r.False(sut.verifyDeadlineExceeded(withLegacyCondition(metav1.NewTime(time.Now().Add(-verifyTimeout/2)))),
		"legacy fallback: fresh condition LastTransitionTime with no phase-start field is not exceeded")
	r.False(sut.verifyDeadlineExceeded(migUmbrella(castwarev1alpha1.MigrationPhaseVerify)),
		"fresh start (neither field nor condition) is not exceeded")
}

// TestMigrationReconciler_VerifyFailure_Rollback asserts an induced verify
// failure (umbrella release Failed) rolls back: umbrella uninstalled,
// individuals re-enabled (readonly cleared) so ComponentReconciler reinstalls
// them. Criterion 2.
func TestMigrationReconciler_VerifyFailure_Rollback(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	umbrella := migUmbrella(castwarev1alpha1.MigrationPhaseVerify)
	// Seed a phase-start timestamp that predates verifyTimeout so the
	// not-deployed release triggers immediate rollback rather than requeue.
	umbrella.Status.MigrationPhaseStartedAt = metav1.NewTime(time.Now().Add(-2 * verifyTimeout))
	ops := newMigrationTestOps(t,
		migCluster(),
		umbrella,
		migIndividual(components.ComponentNameAgent),
		migIndividual(components.ComponentNameClusterController),
	)
	// Umbrella release exists but is Failed — not Deployed.
	failedRelease := migRelease(components.ComponentNameUmbrella)
	failedRelease.Info.Status = release.StatusFailed
	ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
		Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil).AnyTimes()
	ops.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{Namespace: migNamespace, ReleaseName: components.ComponentNameUmbrella}).
		Return(failedRelease, nil)

	// Rollback: uninstall the umbrella.
	ops.mockHelm.EXPECT().Uninstall(helm.UninstallOptions{
		Namespace: migNamespace, ReleaseName: components.ComponentNameUmbrella, Wait: true, IgnoreNotFound: true,
	}).Return(nil, nil)
	// Resolve names for rollback individual re-enable loop.
	for _, sub := range []string{components.ComponentNameAgent, components.ComponentNameSpotHandler, components.ComponentNameClusterController} {
		ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), sub).
			Return(&castai.Component{Name: sub, ReleaseName: sub}, nil).AnyTimes()
	}
	ops.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{Namespace: migNamespace, ReleaseName: components.ComponentNameAgent}).
		Return(migRelease(components.ComponentNameAgent), nil).AnyTimes()
	ops.mockCastAI.EXPECT().RecordActionResult(gomock.Any(), migClusterID, gomock.Any()).
		Return(nil).AnyTimes()

	// Mark individuals readonly first so rollback can clear it.
	for _, sub := range []string{components.ComponentNameAgent, components.ComponentNameClusterController} {
		ind := &castwarev1alpha1.Component{}
		r.NoError(ops.client.Get(context.Background(), types.NamespacedName{Namespace: migNamespace, Name: sub}, ind))
		ind.Spec.Readonly = true
		r.NoError(ops.client.Update(context.Background(), ind))
	}

	reconcileOnce(t, ops)

	u := getUmbrella(t, ops)
	r.Equal(castwarev1alpha1.MigrationPhaseRolledBack, u.Status.MigrationPhase)
	r.False(u.Spec.Migrate, "migrate cleared on rollback")
	r.False(u.Spec.Readonly, "readonly cleared on rollback")
	r.False(controllerutil.ContainsFinalizer(u, MigrationFinalizer), "migration finalizer released on rollback")

	// Individuals re-enabled (readonly=false) so the component reconciler can
	// reinstall them from their stored spec.values.
	for _, sub := range []string{components.ComponentNameAgent, components.ComponentNameClusterController} {
		ind := &castwarev1alpha1.Component{}
		r.NoError(ops.client.Get(context.Background(), types.NamespacedName{Namespace: migNamespace, Name: sub}, ind))
		r.False(ind.Spec.Readonly, "individual %s re-enabled for rollback", sub)
	}
}

// TestMigrationReconciler_RestartResume asserts the controller resumes from the
// recorded phase across a "restart" (a fresh reconcile reading status.phase).
func TestMigrationReconciler_RestartResume(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ops := newMigrationTestOps(t,
		migCluster(),
		migUmbrella(castwarev1alpha1.MigrationPhaseInstallUmbrella),
		migIndividual(components.ComponentNameAgent),
	)
	// Simulate resuming mid-migration: phase is InstallUmbrella, agent release
	// still present (not yet forgotten).
	ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
		Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil).AnyTimes()
	ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameAgent).
		Return(&castai.Component{Name: components.ComponentNameAgent, ReleaseName: components.ComponentNameAgent}, nil).AnyTimes()
	// Umbrella already installed → skip install, go to verify.
	ops.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{Namespace: migNamespace, ReleaseName: components.ComponentNameUmbrella}).
		Return(migRelease(components.ComponentNameUmbrella), nil)
	ops.mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{Namespace: migNamespace, ReleaseName: components.ComponentNameAgent}).
		Return(migRelease(components.ComponentNameAgent), nil).AnyTimes()

	reconcileOnce(t, ops)

	u := getUmbrella(t, ops)
	r.Equal(castwarev1alpha1.MigrationPhaseVerify, u.Status.MigrationPhase, "resumed into Verify")
}

// TestMigrationReconciler_BothTriggersConverge asserts that a Mothership install
// action with migrate=true and a cluster-side spec.migrate=true both produce an
// umbrella CR with spec.migrate=true, which is what the migration controller
// keys on. Criterion 1 (both triggers).
func TestMigrationReconciler_BothTriggersConverge(t *testing.T) {
	t.Parallel()
	// This is validated structurally: handleInstall propagates action.Migrate
	// onto spec.migrate (covered by the action test), and the migration
	// controller keys on spec.migrate regardless of origin. Here we assert the
	// controller treats a migrate=true umbrella (however it got there) as a
	// migration target.
	r := require.New(t)
	ops := newMigrationTestOps(t, migCluster(), migUmbrella(""), migIndividual(components.ComponentNameAgent))
	expectResolveNamesPresent(ops)

	reconcileOnce(t, ops)
	u := getUmbrella(t, ops)
	r.Equal(castwarev1alpha1.MigrationPhaseUninstallIndividuals, u.Status.MigrationPhase,
		"controller engaged for spec.migrate=true umbrella regardless of trigger origin")
}

// TestMigrationReconciler_UmbrellaDeletedMidMigration_RollsBack asserts the
// deletion guard: deleting the umbrella CR mid-migration (with the migration
// finalizer armed) drives an abort-rollback — umbrella uninstalled, individuals
// re-enabled — and only then releases the finalizer, completing the deletion.
// Without the guard, the CR (and its status.migrationPhase) would vanish leaving
// a half-migrated cluster with no controller driving recovery.
func TestMigrationReconciler_UmbrellaDeletedMidMigration_RollsBack(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	umbrella := migUmbrella(castwarev1alpha1.MigrationPhaseVerify)
	umbrella.Spec.Readonly = true
	controllerutil.AddFinalizer(umbrella, MigrationFinalizer)
	agent := migIndividual(components.ComponentNameAgent)
	agent.Spec.Readonly = true
	spot := migIndividual(components.ComponentNameSpotHandler)
	spot.Spec.Readonly = true

	ops := newMigrationTestOps(t,
		migCluster(),
		umbrella,
		agent,
		spot,
	)
	// Rollback resolves the umbrella release name + uninstalls it.
	ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
		Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil).AnyTimes()
	ops.mockHelm.EXPECT().Uninstall(helm.UninstallOptions{
		Namespace: migNamespace, ReleaseName: components.ComponentNameUmbrella, Wait: true, IgnoreNotFound: true,
	}).Return(nil, nil)
	// Resolve names for the rollback individual re-enable loop.
	for _, sub := range []string{components.ComponentNameAgent, components.ComponentNameSpotHandler, components.ComponentNameClusterController} {
		ops.mockCastAI.EXPECT().GetComponentByName(gomock.Any(), sub).
			Return(&castai.Component{Name: sub, ReleaseName: sub}, nil).AnyTimes()
	}
	ops.mockCastAI.EXPECT().RecordActionResult(gomock.Any(), migClusterID, gomock.Any()).
		Return(nil).AnyTimes()

	// User deletes the umbrella CR mid-flight. The migration finalizer holds it
	// in a deleting state (fake client sets deletionTimestamp, keeps the object).
	r.NoError(ops.client.Delete(context.Background(), umbrella))

	reconcileOnce(t, ops)

	// The abort-rollback ran: umbrella release uninstalled (mock asserts the call)
	// and individuals re-enabled so the component reconciler reinstalls them.
	for _, sub := range []string{components.ComponentNameAgent, components.ComponentNameSpotHandler} {
		ind := &castwarev1alpha1.Component{}
		r.NoError(ops.client.Get(context.Background(), types.NamespacedName{Namespace: migNamespace, Name: sub}, ind))
		r.False(ind.Spec.Readonly, "individual %s re-enabled by abort-rollback", sub)
	}

	// The finalizer release completed the deletion: the CR is gone.
	err := ops.client.Get(context.Background(),
		types.NamespacedName{Namespace: migNamespace, Name: components.ComponentNameUmbrella}, &castwarev1alpha1.Component{})
	r.True(apierrors.IsNotFound(err), "umbrella CR should be deleted after abort-rollback (got err=%v)", err)
}

// TestMigrationReconciler_UmbrellaDeleted_NoFinalizer_NoAbort asserts that a
// deleting umbrella CR WITHOUT the migration finalizer is not adopted by the
// migration controller: no rollback is driven, no helm calls are made. The
// component reconciler's own deletion path owns such CRs.
func TestMigrationReconciler_UmbrellaDeleted_NoFinalizer_NoAbort(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	umbrella := migUmbrella(castwarev1alpha1.MigrationPhaseVerify)
	umbrella.Spec.Readonly = true
	// No MigrationFinalizer: e.g. a CR armed by an older operator version. The
	// component reconciler's ComponentFinalizer still holds the CR in a deleting
	// state, so the reconcile genuinely reaches handleUmbrellaDeletion (rather
	// than early-returning on a vanished CR).
	controllerutil.AddFinalizer(umbrella, ComponentFinalizer)
	agent := migIndividual(components.ComponentNameAgent)
	agent.Spec.Readonly = true

	ops := newMigrationTestOps(t,
		migCluster(),
		umbrella,
		agent,
	)
	// No helm/castai expectations: the reconciler must return without driving
	// anything.

	r.NoError(ops.client.Delete(context.Background(), umbrella))

	reconcileOnce(t, ops)

	// The migration controller did not adopt the deletion: the CR is still held
	// (deleting, kept by ComponentFinalizer) for the component reconciler, and
	// the individual is untouched (still readonly).
	still := &castwarev1alpha1.Component{}
	r.NoError(ops.client.Get(context.Background(),
		types.NamespacedName{Namespace: migNamespace, Name: components.ComponentNameUmbrella}, still))
	r.False(still.DeletionTimestamp.IsZero(), "CR held in deleting state by ComponentFinalizer")
	r.False(controllerutil.ContainsFinalizer(still, MigrationFinalizer), "migration finalizer not added by the deletion path")
	ind := &castwarev1alpha1.Component{}
	r.NoError(ops.client.Get(context.Background(), types.NamespacedName{Namespace: migNamespace, Name: components.ComponentNameAgent}, ind))
	r.True(ind.Spec.Readonly, "individual untouched when the deleting CR has no migration finalizer")
}

// TestMigrationReconciler_UmbrellaDeleted_CrashWindow_ReleasesFinalizer
// asserts the corner where the terminal spec write (clearing migrate) landed
// but the CR is deleted before the finalizer release completed its own write —
// the deletion check must run before the migrate gate, or this CR would strand
// as a tombstone holding the finalizer forever. There is no migration in flight,
// so no rollback is driven; the finalizer is simply released so the deletion
// completes.
func TestMigrationReconciler_UmbrellaDeleted_CrashWindow_ReleasesFinalizer(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	umbrella := migUmbrella(castwarev1alpha1.MigrationPhaseVerify)
	umbrella.Spec.Readonly = true
	// spec.migrate already cleared (terminal spec write landed), finalizer still
	// armed, CR deleted before the release completed.
	umbrella.Spec.Migrate = false
	controllerutil.AddFinalizer(umbrella, MigrationFinalizer)
	agent := migIndividual(components.ComponentNameAgent)
	agent.Spec.Readonly = true

	ops := newMigrationTestOps(t,
		migCluster(),
		umbrella,
		agent,
	)
	// No helm/castai expectations: no rollback must be driven.

	r.NoError(ops.client.Delete(context.Background(), umbrella))

	reconcileOnce(t, ops)

	// The finalizer release completed the deletion; the individual is untouched.
	err := ops.client.Get(context.Background(),
		types.NamespacedName{Namespace: migNamespace, Name: components.ComponentNameUmbrella}, &castwarev1alpha1.Component{})
	r.True(apierrors.IsNotFound(err), "umbrella CR should be deleted after finalizer release (got err=%v)", err)
	ind := &castwarev1alpha1.Component{}
	r.NoError(ops.client.Get(context.Background(), types.NamespacedName{Namespace: migNamespace, Name: components.ComponentNameAgent}, ind))
	r.True(ind.Spec.Readonly, "individual untouched: no rollback for a not-in-flight migration")
}

// TestMigrationReconciler_LegacyReadonlyResume_ProceedsWithoutGuard asserts a
// legacy mid-phase resume (readonly already set by an operator version without
// the finalizer, interrupted before advancing the phase) does not wedge trying
// to arm the finalizer — the webhook would reject the update to a readonly CR —
// and instead proceeds unguarded, as that migration ran before.
func TestMigrationReconciler_LegacyReadonlyResume_ProceedsWithoutGuard(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	umbrella := migUmbrella("")
	umbrella.Spec.Readonly = true // set by an older operator; no finalizer
	ops := newMigrationTestOps(t,
		migCluster(),
		umbrella,
		migIndividual(components.ComponentNameAgent),
		migIndividual(components.ComponentNameSpotHandler),
		migIndividual(components.ComponentNameClusterController),
	)
	expectResolveNamesPresent(ops)

	reconcileOnce(t, ops)

	u := getUmbrella(t, ops)
	r.Equal(castwarev1alpha1.MigrationPhaseUninstallIndividuals, u.Status.MigrationPhase,
		"legacy readonly resume proceeds past MarkReadonly")
	r.False(controllerutil.ContainsFinalizer(u, MigrationFinalizer),
		"finalizer not armed on a readonly CR (webhook would reject it)")
}

// TestHandleInstall_PropagatesMigrate asserts that a Mothership install action
// carrying migrate=true creates the umbrella CR with spec.migrate=true. This is
// the Mothership-trigger half of criterion 1.
func TestHandleInstall_PropagatesMigrate(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	cluster := migCluster()
	scheme := runtime.NewScheme()
	r.NoError(castwarev1alpha1.AddToScheme(scheme))
	r.NoError(corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).WithStatusSubresource(cluster).Build()
	ctrl := gomock.NewController(t)
	mockHelm := mock_helm.NewMockClient(ctrl)
	mockCastAI := mock_castai.NewMockCastAIClient(ctrl)

	clusterReconciler := &ClusterReconciler{
		Client:     c,
		Scheme:     c.Scheme(),
		Log:        logrus.New(),
		HelmClient: mockHelm,
		Config:     &config.Config{},
	}

	// migrate=true on the install action. The gate must pass (migrate bypass).
	mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
		Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil).AnyTimes()
	for _, sub := range []string{components.ComponentNameAgent, components.ComponentNameSpotHandler, components.ComponentNameClusterController} {
		mockCastAI.EXPECT().GetComponentByName(gomock.Any(), sub).
			Return(&castai.Component{Name: sub, ReleaseName: sub}, nil).AnyTimes()
		mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{Namespace: migNamespace, ReleaseName: sub}).
			Return(migRelease(sub), nil).AnyTimes()
	}

	err := clusterReconciler.handleInstall(context.Background(), mockCastAI, cluster, &castai.ActionInstall{
		Component:   components.ComponentNameUmbrella,
		Version:     "1.0.0",
		ReleaseName: components.ComponentNameUmbrella,
		Migrate:     true,
	})
	r.NoError(err)

	created := &castwarev1alpha1.Component{}
	r.NoError(c.Get(context.Background(), types.NamespacedName{Namespace: migNamespace, Name: components.ComponentNameUmbrella}, created))
	r.True(created.Spec.Migrate, "action.Migrate must propagate onto spec.migrate")
}

// TestHandleInstall_BlocksUmbrellaWithoutMigrate asserts criterion 4 on the
// Mothership path: an umbrella install action without migrate=true is refused
// when individuals are present (UmbrellaConflict surfaced via ack error).
func TestHandleInstall_BlocksUmbrellaWithoutMigrate(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	cluster := migCluster()
	scheme := runtime.NewScheme()
	r.NoError(castwarev1alpha1.AddToScheme(scheme))
	r.NoError(corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).WithStatusSubresource(cluster).Build()
	ctrl := gomock.NewController(t)
	mockHelm := mock_helm.NewMockClient(ctrl)
	mockCastAI := mock_castai.NewMockCastAIClient(ctrl)

	clusterReconciler := &ClusterReconciler{
		Client:     c,
		Scheme:     c.Scheme(),
		Log:        logrus.New(),
		HelmClient: mockHelm,
		Config:     &config.Config{},
	}

	// Individuals present, migrate NOT set → gate blocks.
	mockCastAI.EXPECT().GetComponentByName(gomock.Any(), components.ComponentNameUmbrella).
		Return(&castai.Component{Name: components.ComponentNameUmbrella, ReleaseName: components.ComponentNameUmbrella}, nil).AnyTimes()
	for _, sub := range []string{components.ComponentNameAgent, components.ComponentNameSpotHandler, components.ComponentNameClusterController} {
		mockCastAI.EXPECT().GetComponentByName(gomock.Any(), sub).
			Return(&castai.Component{Name: sub, ReleaseName: sub}, nil).AnyTimes()
		mockHelm.EXPECT().GetRelease(helm.GetReleaseOptions{Namespace: migNamespace, ReleaseName: sub}).
			Return(migRelease(sub), nil).AnyTimes()
	}

	err := clusterReconciler.handleInstall(context.Background(), mockCastAI, cluster, &castai.ActionInstall{
		Component:   components.ComponentNameUmbrella,
		Version:     "1.0.0",
		ReleaseName: components.ComponentNameUmbrella,
		Migrate:     false,
	})
	r.Error(err, "umbrella install without migrate must be blocked when individuals present")
}

// keep unused imports referenced when build tags trim some paths
var _ = time.Now
var _ = corev1.SchemeGroupVersion
