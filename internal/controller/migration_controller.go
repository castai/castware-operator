package controller

// This file implements the standalone → umbrella migration state machine
// (CID-1047). It is the single sanctioned bypass of the umbrella / individual
// charts mutual-exclusivity gate, triggered when an umbrella Component CR has
// spec.migrate=true while individual component releases are present.
//
// One state machine, two triggers, both converging on spec.migrate=true so the
// CR is the source of truth:
//
//   - Mothership install action with migrate=true via pollActions → handleInstall
//     creates the umbrella CR with spec.migrate=true.
//   - cluster-side: the user sets spec.migrate=true on the umbrella CR directly.
//
// The migration is one-way (umbrella is the target state), reconciled (driven by
// the controller, not a one-shot job), verified (the umbrella must be healthy
// before the last individual release is retired), and recoverable (phase is
// durable in status.migrationPhase; each phase is idempotent; failure rolls back
// to the individual-component regime).
//
// Heartbeat continuity: the agent is the cluster's liveness signal to Mothership
// (snapshots every ~15s). It is NEVER helm-uninstalled during migration — that
// would delete the agent pods. Instead the umbrella installs with TakeOwnership
// (adopting the running agent Deployment without a pod restart), and the
// individual agent release is then "forgotten" (storage secret deleted) so the
// umbrella becomes its sole owner. The non-agent individuals carry no heartbeat
// and are uninstalled in reverse phase order before the umbrella install.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"helm.sh/helm/v3/pkg/release"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/castai"
	"github.com/castai/castware-operator/internal/castai/auth"
	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/config"
	"github.com/castai/castware-operator/internal/helm"
	"github.com/castai/castware-operator/internal/migrationgate"
	"github.com/castai/castware-operator/internal/utils"
	"github.com/castai/castware-operator/internal/values"
)

// MigrationReconciler drives the standalone → umbrella migration state machine.
// It watches umbrella Component CRs; when spec.migrate is true and individual
// component releases are present, it runs the phased migration. It does not own
// the umbrella CR's ongoing reconcile (that stays with ComponentReconciler) — it
// only owns the migration, and sidelines ComponentReconciler by setting
// spec.readonly=true for the duration.
type MigrationReconciler struct {
	client.Client
	Scheme             *runtime.Scheme
	Log                logrus.FieldLogger
	HelmClient         helm.Client
	Config             *config.Config
	castAIClientGetter func(context.Context, *castwarev1alpha1.Cluster) (castai.CastAIClient, error)
}

// +kubebuilder:rbac:groups=castware.cast.ai,resources=components,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=castware.cast.ai,resources=components/status,verbs=update;patch
// +kubebuilder:rbac:groups=castware.cast.ai,resources=components/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

// verifyTimeout caps how long the Verify phase waits for the umbrella release to
// reach deployed and the agent to be ready. Matches the component reconciler's
// 10-minute progressing deadline so a stuck migration fails over to rollback on
// the same timescale a stuck install would.
const verifyTimeout = 10 * time.Minute

// Reconcile dispatches on the umbrella CR's migration phase. It is a no-op for
// non-umbrella CRs or umbrella CRs without spec.migrate. Each phase is
// idempotent and resumable from status.migrationPhase.
func (r *MigrationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithField("migration", req.String())

	component := &castwarev1alpha1.Component{}
	if err := r.Get(ctx, req.NamespacedName, component); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.WithError(err).Error("Failed to get component")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	// Only the umbrella component with spec.migrate drives the state machine.
	// Every other CR (including an umbrella without migrate) is left to the
	// component reconciler — including the UmbrellaConflict path that implements
	// acceptance criterion 4 (migrate not set → no migration + conflict status).
	if component.Spec.Component != components.ComponentNameUmbrella || !component.Spec.Migrate {
		return ctrl.Result{}, nil
	}

	log = log.WithField("cluster", component.Spec.Cluster)
	log = log.WithField("phase", component.Status.MigrationPhase)

	cluster := &castwarev1alpha1.Cluster{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: component.Namespace, Name: component.Spec.Cluster}, cluster); err != nil {
		log.WithError(err).Error("Failed to get cluster")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	if !meta.IsStatusConditionTrue(cluster.Status.Conditions, typeAvailableCluster) ||
		cluster.Spec.Cluster == nil || cluster.Spec.Cluster.ClusterID == "" {
		log.Info("Waiting for cluster to be available")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	switch component.Status.MigrationPhase {
	case "", castwarev1alpha1.MigrationPhaseMarkReadonly:
		return r.phaseMarkReadonly(ctx, log, component, cluster)
	case castwarev1alpha1.MigrationPhaseUninstallIndividuals:
		return r.phaseUninstallIndividuals(ctx, log, component, cluster)
	case castwarev1alpha1.MigrationPhaseInstallUmbrella:
		return r.phaseInstallUmbrella(ctx, log, component, cluster)
	case castwarev1alpha1.MigrationPhaseVerify:
		return r.phaseVerify(ctx, log, component, cluster)
	case castwarev1alpha1.MigrationPhaseFinalize:
		return r.phaseFinalize(ctx, log, component, cluster)
	case castwarev1alpha1.MigrationPhaseRolledBack:
		// Terminal failure state. Stop reconciling; the cluster is back on the
		// individual regime and a human must clear spec.migrate to retry.
		return ctrl.Result{}, nil
	default:
		// Unknown phase: reset to the start so an interrupted/old migration
		// re-enters the state machine cleanly rather than wedging.
		log.Warnf("Unknown migration phase %q; resetting to %s", component.Status.MigrationPhase, castwarev1alpha1.MigrationPhaseMarkReadonly)
		return r.setMigrationPhase(ctx, component, castwarev1alpha1.MigrationPhaseMarkReadonly)
	}
}

// phaseMarkReadonly sets spec.readonly=true on the umbrella CR (sidelining the
// component reconciler for the whole migration) and on each present individual
// sub-component CR, then advances to UninstallIndividuals. Idempotent: patching
// an already-readonly CR is a no-op.
func (r *MigrationReconciler) phaseMarkReadonly(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component, cluster *castwarev1alpha1.Cluster) (ctrl.Result, error) {
	// Sideline the component reconciler on the umbrella CR so it does not race
	// the migration for helm writes. Cleared on Finalize / Rollback.
	if !component.Spec.Readonly {
		if err := patchReadonly(ctx, r.Client, component, true); err != nil {
			return ctrl.Result{}, fmt.Errorf("set umbrella readonly: %w", err)
		}
	}

	// Mark each present individual sub-component CR read-only so the component
	// reconciler stops reconciling them while the migration uninstalls their
	// releases. The CRs are retained (with their spec.values) for rollback.
	present, err := r.presentIndividuals(ctx, cluster)
	if err != nil {
		log.WithError(err).Warn("Failed to resolve present individuals; requeue")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	for _, sub := range present {
		ind := &castwarev1alpha1.Component{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: component.Namespace, Name: sub}, ind); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return ctrl.Result{}, fmt.Errorf("get individual component %s: %w", sub, err)
		}
		if !ind.Spec.Readonly {
			if err := patchReadonly(ctx, r.Client, ind, true); err != nil {
				return ctrl.Result{}, fmt.Errorf("set %s readonly: %w", sub, err)
			}
		}
	}

	r.setMigratingCondition(component, castwarev1alpha1.MigrationPhaseMarkReadonly)
	return r.setMigrationPhase(ctx, component, castwarev1alpha1.MigrationPhaseUninstallIndividuals)
}

// phaseUninstallIndividuals uninstalls the non-agent individual releases in
// reverse phase order (cluster-controller first, then spot-handler). The agent
// is excluded: its resources are adopted by the umbrella install (TakeOwnership)
// and its release is forgotten in Finalize, never uninstalled. Individual CRs
// are kept (readonly) for rollback. Idempotent via IgnoreNotFound.
func (r *MigrationReconciler) phaseUninstallIndividuals(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component, cluster *castwarev1alpha1.Cluster) (ctrl.Result, error) {
	present, err := r.presentIndividuals(ctx, cluster)
	if err != nil {
		log.WithError(err).Warn("Failed to resolve present individuals; requeue")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	// Reverse of the scan phase order: phase2 (cluster-controller) before phase1
	// (spot-handler). Agent is never uninstalled here. migrationgate.Subcomponents
	// is [agent, spot-handler, cluster-controller]; reversing puts
	// cluster-controller first so phase2 is retired before phase1, matching how
	// the scan path installs them (phase1 then phase2).
	reversed := make([]string, len(migrationgate.Subcomponents))
	copy(reversed, migrationgate.Subcomponents)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	for _, sub := range reversed {
		if sub == components.ComponentNameAgent {
			continue
		}
		if !contains(present, sub) {
			continue
		}
		releaseName, err := r.releaseNameFor(ctx, cluster, sub)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("resolve release name for %s: %w", sub, err)
		}
		log.Infof("Uninstalling individual release %q (%s)", releaseName, sub)
		if _, err := r.HelmClient.Uninstall(helm.UninstallOptions{
			Namespace:      component.Namespace,
			ReleaseName:    releaseName,
			IgnoreNotFound: true,
			Wait:           true,
		}); err != nil {
			// An uninstall failure is a migration failure: roll back.
			log.WithError(err).Errorf("Failed to uninstall %s; rolling back", sub)
			return r.rollback(ctx, log, component, cluster, fmt.Errorf("uninstall %s: %w", sub, err))
		}
	}

	r.setMigratingCondition(component, castwarev1alpha1.MigrationPhaseUninstallIndividuals)
	return r.setMigrationPhase(ctx, component, castwarev1alpha1.MigrationPhaseInstallUmbrella)
}

// phaseInstallUmbrella installs the umbrella chart, deriving its tag mode from
// which individuals were present and using the shared umbrellaValues builder.
// TakeOwnership (set in helm.Client.Install) lets it adopt the still-running
// agent Deployment without a pod restart, preserving the Mothership heartbeat.
// Idempotent: if the umbrella release is already present (a partial prior run),
// it skips straight to Verify.
func (r *MigrationReconciler) phaseInstallUmbrella(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component, cluster *castwarev1alpha1.Cluster) (ctrl.Result, error) {
	umbrellaReleaseName, err := r.releaseNameFor(ctx, cluster, components.ComponentNameUmbrella)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve umbrella release name: %w", err)
	}

	// If the umbrella release is already present (resumed migration / retry),
	// advance to verification rather than reinstalling.
	if _, getErr := r.HelmClient.GetRelease(helm.GetReleaseOptions{
		Namespace:   component.Namespace,
		ReleaseName: umbrellaReleaseName,
	}); getErr == nil {
		log.Info("Umbrella release already present; advancing to verify")
		r.setMigratingCondition(component, castwarev1alpha1.MigrationPhaseInstallUmbrella)
		return r.setMigrationPhase(ctx, component, castwarev1alpha1.MigrationPhaseVerify)
	} else if !isReleaseNotFound(getErr) {
		// A non-not-found error means helm is unreachable; requeue rather than
		// risk a partial install.
		return ctrl.Result{}, fmt.Errorf("check umbrella release presence: %w", getErr)
	}

	present, err := r.presentIndividuals(ctx, cluster)
	if err != nil {
		log.WithError(err).Warn("Failed to resolve present individuals for tag derivation; requeue")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	overrides := r.deriveUmbrellaOverrides(present)

	// Carry over each present individual's user-supplied spec.values into the
	// umbrella's autoscaler.<alias> layout so per-component customizations
	// (topologySpreadConstraints, additionalEnv, resource requests, ...) are not
	// silently dropped during migration. Fetched before the individuals are
	// uninstalled/forgotten, so their CRs (still present in this phase) hold the
	// values. The carry-over is merged into overrides (under the umbrella CR's
	// own spec.values via UmbrellaValues), so an explicit umbrella value wins.
	individuals, carryErr := r.individualComponents(ctx, component.Namespace, present)
	if carryErr != nil {
		// A read failure should not abort the migration; proceed with tag-only
		// overrides (the pre-carryover behavior).
		log.WithError(carryErr).Warn("Failed to read individual component values for carry-over; proceeding with defaults")
		individuals = nil
	}
	if carry := values.CarryOverIndividualValues(individuals, cluster.Spec.Provider); carry != nil {
		if err := utils.MergeMaps(overrides, carry); err != nil {
			return ctrl.Result{}, fmt.Errorf("merge carried individual values: %w", err)
		}
	}

	valuesMap, err := values.UmbrellaValues(component, cluster, overrides)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("build umbrella values: %w", err)
	}

	chartName := component.HelmChartName()
	if component.Labels != nil && component.Labels[castwarev1alpha1.LabelHelmChart] != "" {
		chartName = component.Labels[castwarev1alpha1.LabelHelmChart]
	}

	log.Infof("Installing umbrella chart %s:%s (release %q)", chartName, component.Spec.Version, umbrellaReleaseName)
	if _, err := r.HelmClient.Install(ctx, helm.InstallOptions{
		ChartSource: &helm.ChartSource{
			RepoURL: cluster.Spec.HelmRepoURL,
			Name:    chartName,
			Version: component.Spec.Version,
		},
		Namespace:       component.Namespace,
		CreateNamespace: false,
		ReleaseName:     umbrellaReleaseName,
		ValuesOverrides: valuesMap,
	}); err != nil {
		// Install failure: the agent was never touched (it's still running under
		// its individual release) and the non-agent individuals were already
		// uninstalled. Roll back so the component reconciler reinstalls them.
		log.WithError(err).Error("Umbrella install failed; rolling back")
		return r.rollback(ctx, log, component, cluster, fmt.Errorf("install umbrella: %w", err))
	}

	r.setMigratingCondition(component, castwarev1alpha1.MigrationPhaseInstallUmbrella)
	return r.setMigrationPhase(ctx, component, castwarev1alpha1.MigrationPhaseVerify)
}

// phaseVerify waits for the umbrella release to reach deployed status and the
// agent to be healthy (first verified), then advances to Finalize. A verify
// failure or timeout rolls back to the individual regime.
func (r *MigrationReconciler) phaseVerify(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component, cluster *castwarev1alpha1.Cluster) (ctrl.Result, error) {
	umbrellaReleaseName, err := r.releaseNameFor(ctx, cluster, components.ComponentNameUmbrella)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve umbrella release name: %w", err)
	}

	rel, err := r.HelmClient.GetRelease(helm.GetReleaseOptions{
		Namespace:   component.Namespace,
		ReleaseName: umbrellaReleaseName,
	})
	if err != nil {
		if isReleaseNotFound(err) {
			// Umbrella release vanished — the install did not take. Roll back.
			return r.rollback(ctx, log, component, cluster, errors.New("umbrella release not found during verify"))
		}
		return ctrl.Result{}, fmt.Errorf("get umbrella release for verify: %w", err)
	}

	if rel.Info.Status != release.StatusDeployed {
		// Check the verify deadline. The phase's start time is the
		// Migrating condition's LastTransitionTime; if it predates the timeout,
		// roll back rather than wait forever.
		if r.verifyDeadlineExceeded(component) {
			return r.rollback(ctx, log, component, cluster, fmt.Errorf("umbrella verify timeout: release status %s", rel.Info.Status))
		}
		log.Infof("Umbrella release not yet deployed (status=%s); requeueing", rel.Info.Status)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// Umbrella is deployed. Verify the agent (adopted by the umbrella) is
	// healthy — it is the heartbeat signal, so "agent first verified" is the
	// gate before finalizing.
	if err := r.verifyAgentHealthy(ctx, component); err != nil {
		if r.verifyDeadlineExceeded(component) {
			return r.rollback(ctx, log, component, cluster, fmt.Errorf("agent not healthy after verify timeout: %w", err))
		}
		log.WithError(err).Warn("Agent not yet healthy; requeueing")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	r.setMigratingCondition(component, castwarev1alpha1.MigrationPhaseVerify)
	return r.setMigrationPhase(ctx, component, castwarev1alpha1.MigrationPhaseFinalize)
}

// phaseFinalize retires the individual regime: forgets the agent's helm release
// (storage secret deleted — no pod deletion, heartbeat preserved), deletes the
// individual Component CRs, then clears the umbrella's migrate/readonly flags
// and reports success to Mothership. After Finalize the umbrella is the sole
// owner and ComponentReconciler resumes normal reconcile.
func (r *MigrationReconciler) phaseFinalize(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component, cluster *castwarev1alpha1.Cluster) (ctrl.Result, error) {
	// Forget the individual agent release: its resources are now owned by the
	// umbrella (adopted via TakeOwnership). Deleting only the storage record
	// leaves the running agent pods untouched. Idempotent.
	agentReleaseName, err := r.releaseNameFor(ctx, cluster, components.ComponentNameAgent)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve agent release name: %w", err)
	}
	log.Infof("Forgetting individual agent release %q (resources now owned by umbrella)", agentReleaseName)
	if err := r.HelmClient.ForgetRelease(helm.ForgetReleaseOptions{
		Namespace:   component.Namespace,
		ReleaseName: agentReleaseName,
	}); err != nil {
		// A forget failure is not fatal to the migration outcome — the umbrella
		// owns the resources — but leaving the stale release would re-trip the
		// mutual-exclusivity gate on the next scan. Surface and retry.
		return ctrl.Result{}, fmt.Errorf("forget agent release: %w", err)
	}

	// Delete the individual Component CRs. Their helm releases are already gone
	// (non-agent ones uninstalled in UninstallIndividuals, the agent forgotten
	// above), so there is nothing left for the ComponentReconciler's cleanup
	// finalizer to do. Remove any finalizer first so the delete removes the CR
	// outright instead of leaving a tombstone for the component reconciler to
	// process — the migration has already performed the cleanup the finalizer
	// would run.
	//
	// The individual CRs were marked readonly in phaseMarkReadonly. The validating
	// webhook (ValidateUpdate) rejects any update to a CR that is readonly in both
	// old and new state, so readonly must be cleared in the same patch as the
	// finalizer removal. The ComponentReconciler's forceReadonlyIfUmbrellaInstalled
	// concurrently re-arms readonly=true (the umbrella is installed at this point),
	// so the patch is wrapped in RetryOnConflict to absorb that race; the delete
	// that follows is not webhook-gated and removes the CR outright.
	for _, sub := range migrationgate.Subcomponents {
		ind := &castwarev1alpha1.Component{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: component.Namespace, Name: sub}, ind); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return ctrl.Result{}, fmt.Errorf("get individual component %s for deletion: %w", sub, err)
		}
		// Clear readonly and remove the finalizer in a single RetryOnConflict loop.
		// The validating webhook (ValidateUpdate) rejects updates to a CR that is
		// readonly in both old and new state, so readonly must be cleared in the
		// same patch. Meanwhile the ComponentReconciler's
		// forceReadonlyIfUmbrellaInstalled races to re-set readonly=true (the
		// umbrella is installed at this point), so a plain Update on a stale object
		// conflicts ("the object has been modified"). RetryOnConflict re-gets the
		// latest object each attempt; a merge patch on the finalizer field does
		// not conflict with the reconciler's readonly patch.
		nn := types.NamespacedName{Namespace: component.Namespace, Name: sub}
		if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			latest := &castwarev1alpha1.Component{}
			if err := r.Get(ctx, nn, latest); err != nil {
				return err
			}
			if !controllerutil.ContainsFinalizer(latest, ComponentFinalizer) && !latest.Spec.Readonly {
				return nil // nothing to do
			}
			base := latest.DeepCopy()
			latest.Spec.Readonly = false
			controllerutil.RemoveFinalizer(latest, ComponentFinalizer)
			return r.Patch(ctx, latest, client.MergeFrom(base))
		}); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return ctrl.Result{}, fmt.Errorf("clear readonly/finalizer on %s: %w", sub, err)
		}
		if err := r.Delete(ctx, ind); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("delete individual component %s: %w", sub, err)
		}
	}

	// Migration complete: clear migrate + readonly on the umbrella so
	// ComponentReconciler resumes normal reconcile, and mark it Available.
	if err := r.finalizeUmbrellaSuccess(ctx, component); err != nil {
		return ctrl.Result{}, err
	}

	// Report success to Mothership.
	r.recordMigrationResult(ctx, log, cluster, castai.Status_OK, "migration succeeded: cluster now managed by the umbrella chart", "")

	log.Info("Migration finalized: umbrella is the sole owner")
	return ctrl.Result{}, nil
}

// rollback restores the individual-component regime after a migration failure.
// It uninstalls the umbrella (if it was installed), clears readonly on the
// surviving individual CRs so ComponentReconciler reinstalls them from their
// stored spec.values, and marks the umbrella as failed. The 1-minute heartbeat
// bound applies to the success path only; rollback may take longer to fully
// reinstall individuals.
func (r *MigrationReconciler) rollback(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component, cluster *castwarev1alpha1.Cluster, cause error) (ctrl.Result, error) {
	log.WithError(cause).Warn("Migration failed; rolling back to individual regime")

	// Uninstall the umbrella if it was installed. Its resources overlap with the
	// individuals; removing it lets the individual CRs (re-enabled below)
	// reinstall cleanly. IgnoreNotFound: a pre-install failure has no umbrella.
	umbrellaReleaseName, nameErr := r.releaseNameFor(ctx, cluster, components.ComponentNameUmbrella)
	if nameErr == nil {
		if _, err := r.HelmClient.Uninstall(helm.UninstallOptions{
			Namespace:      component.Namespace,
			ReleaseName:    umbrellaReleaseName,
			IgnoreNotFound: true,
			Wait:           true,
		}); err != nil {
			// A failed umbrella uninstall leaves the cluster in a hybrid state.
			// Surface it; the next reconcile of the scan path will warn
			// Mothership about the hybrid config.
			log.WithError(err).Error("Failed to uninstall umbrella during rollback; cluster may be in hybrid state")
		}
	}

	// Re-enable the individual CRs so ComponentReconciler reinstalls them. The
	// agent was never uninstalled (adopted by the umbrella, then released by the
	// umbrella uninstall) — but its CR is re-enabled so the reconciler observes
	// the surviving release and reconciles status. The non-agent individuals
	// were uninstalled; their CRs now reinstall from spec.values.
	for _, sub := range migrationgate.Subcomponents {
		ind := &castwarev1alpha1.Component{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: component.Namespace, Name: sub}, ind); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return ctrl.Result{}, fmt.Errorf("get individual component %s for rollback: %w", sub, err)
		}
		if ind.Spec.Readonly {
			if err := patchReadonly(ctx, r.Client, ind, false); err != nil {
				return ctrl.Result{}, fmt.Errorf("clear %s readonly for rollback: %w", sub, err)
			}
		}
	}

	// Mark the umbrella as rolled back / failed and clear migrate so the
	// migration controller does not re-trigger. readonly is cleared so the
	// component reconciler can manage the umbrella (now uninstalled) normally.
	if err := r.finalizeUmbrellaFailure(ctx, component, cause); err != nil {
		return ctrl.Result{}, err
	}

	r.recordMigrationResult(ctx, log, cluster, castai.Status_ERROR, "migration failed and rolled back; individual components restored", cause.Error())

	return ctrl.Result{}, nil
}

// finalizeUmbrellaSuccess clears migrate/readonly and sets Available=True on the
// umbrella, and records the migration phase as empty (terminal success). Spec and
// status are separate subresources, so they are updated in two fresh-read steps:
// spec via Update, then status via Status().Update on a re-read object (matching
// ComponentReconciler.updateStatus), so the status write does not depend on the
// in-memory object surviving the spec write.
func (r *MigrationReconciler) finalizeUmbrellaSuccess(ctx context.Context, component *castwarev1alpha1.Component) error {
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &castwarev1alpha1.Component{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: component.Namespace, Name: component.Name}, latest); err != nil {
			return err
		}
		latest.Spec.Migrate = false
		latest.Spec.Readonly = false
		return r.Update(ctx, latest)
	}); err != nil {
		return fmt.Errorf("clear umbrella migrate/readonly: %w", err)
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &castwarev1alpha1.Component{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: component.Namespace, Name: component.Name}, latest); err != nil {
			return err
		}
		latest.Status.MigrationPhase = ""
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:    castwarev1alpha1.TypeMigrating,
			Status:  metav1.ConditionFalse,
			Reason:  castwarev1alpha1.ReasonMigrationSucceeded,
			Message: "Migration to umbrella chart completed successfully",
		})
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:    typeAvailableComponent,
			Status:  metav1.ConditionTrue,
			Reason:  reasonInstalled,
			Message: "Component installed via migration to umbrella chart",
		})
		return r.Status().Update(ctx, latest)
	})
}

// finalizeUmbrellaFailure clears migrate/readonly, sets the umbrella Available=False
// with a MigrationFailed reason, and records the RolledBack phase (terminal failure).
// Same two-step spec/status update as finalizeUmbrellaSuccess.
func (r *MigrationReconciler) finalizeUmbrellaFailure(ctx context.Context, component *castwarev1alpha1.Component, cause error) error {
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &castwarev1alpha1.Component{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: component.Namespace, Name: component.Name}, latest); err != nil {
			return err
		}
		latest.Spec.Migrate = false
		latest.Spec.Readonly = false
		return r.Update(ctx, latest)
	}); err != nil {
		return fmt.Errorf("clear umbrella migrate/readonly on failure: %w", err)
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &castwarev1alpha1.Component{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: component.Namespace, Name: component.Name}, latest); err != nil {
			return err
		}
		latest.Status.MigrationPhase = castwarev1alpha1.MigrationPhaseRolledBack
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:    castwarev1alpha1.TypeMigrating,
			Status:  metav1.ConditionFalse,
			Reason:  castwarev1alpha1.ReasonMigrationFailed,
			Message: fmt.Sprintf("Migration failed and rolled back: %v", cause),
		})
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:    typeAvailableComponent,
			Status:  metav1.ConditionFalse,
			Reason:  reasonMigrationFailed,
			Message: fmt.Sprintf("Migration failed: %v", cause),
		})
		return r.Status().Update(ctx, latest)
	})
}

// setMigrationPhase records the phase in status and sets the Migrating condition,
// then returns nil so the next reconcile enters the new phase.
func (r *MigrationReconciler) setMigrationPhase(ctx context.Context, component *castwarev1alpha1.Component, phase string) (ctrl.Result, error) {
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &castwarev1alpha1.Component{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: component.Namespace, Name: component.Name}, latest); err != nil {
			return err
		}
		latest.Status.MigrationPhase = phase
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:    castwarev1alpha1.TypeMigrating,
			Status:  metav1.ConditionTrue,
			Reason:  phase,
			Message: fmt.Sprintf("Migration in progress: %s", phase),
		})
		return r.Status().Update(ctx, latest)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("set migration phase %s: %w", phase, err)
	}
	return ctrl.Result{}, nil
}

// setMigratingCondition sets the in-memory Migrating condition on the component
// object so subsequent phase logic in the same reconcile reads a consistent
// LastTransitionTime. The durable write happens in setMigrationPhase.
func (r *MigrationReconciler) setMigratingCondition(component *castwarev1alpha1.Component, phase string) {
	meta.SetStatusCondition(&component.Status.Conditions, metav1.Condition{
		Type:    castwarev1alpha1.TypeMigrating,
		Status:  metav1.ConditionTrue,
		Reason:  phase,
		Message: fmt.Sprintf("Migration in progress: %s", phase),
	})
}

// verifyDeadlineExceeded reports whether the Verify phase has been running
// longer than verifyTimeout, using the Migrating condition's LastTransitionTime
// as the phase start. When the condition is absent (e.g. first entry), it starts
// fresh (not exceeded).
func (r *MigrationReconciler) verifyDeadlineExceeded(component *castwarev1alpha1.Component) bool {
	cond := meta.FindStatusCondition(component.Status.Conditions, castwarev1alpha1.TypeMigrating)
	if cond == nil {
		return false
	}
	// The condition's LastTransitionTime is refreshed each time the reason
	// (phase) changes. Within the Verify phase it stays stable, so it marks the
	// phase start. A re-entry after a prior phase sets a new timestamp.
	return time.Since(cond.LastTransitionTime.Time) > verifyTimeout
}

// verifyAgentHealthy checks the agent Deployment (adopted by the umbrella) has
// at least one ready replica. The agent is the heartbeat signal; verifying it
// first, before finalizing, is the "agent first verified" requirement.
//
// The agent Deployment is identified by its app.kubernetes.io/name label set to
// castai-agent by the chart. A missing or not-yet-ready Deployment is reported
// as an error so the caller requeues; the rollback/timeout decision is gated by
// verifyDeadlineExceeded.
func (r *MigrationReconciler) verifyAgentHealthy(ctx context.Context, component *castwarev1alpha1.Component) error {
	depList := &appsv1.DeploymentList{}
	if err := r.List(ctx, depList, &client.ListOptions{
		Namespace: component.Namespace,
		LabelSelector: labels.SelectorFromSet(labels.Set{
			"app.kubernetes.io/name": components.ComponentNameAgent,
		}),
	}); err != nil {
		return fmt.Errorf("list agent deployments: %w", err)
	}
	if len(depList.Items) == 0 {
		return errors.New("agent deployment not found")
	}
	for _, dep := range depList.Items {
		if dep.Status.ReadyReplicas > 0 {
			return nil
		}
	}
	return fmt.Errorf("agent deployment has 0 ready replicas")
}

// presentIndividuals returns the sub-component names whose helm releases are
// currently installed, in Subcomponents order. Resolves release names from
// Mothership (fail-safe per migrationgate).
func (r *MigrationReconciler) presentIndividuals(ctx context.Context, cluster *castwarev1alpha1.Cluster) ([]string, error) {
	castAiClient, err := r.getCastaiClient(ctx, cluster)
	if err != nil {
		return nil, err
	}
	names, err := migrationgate.ResolveNames(ctx, castAiClient)
	if err != nil {
		return nil, err
	}
	return migrationgate.InstalledSubcomponents(r.HelmClient, cluster.Namespace, names.SubcomponentReleases), nil
}

// individualComponents fetches the Component CRs for the given present
// sub-component names so their spec.values can be carried over into the
// umbrella install. Missing CRs are skipped (an individual may be installed
// via helm without a CR). Returns the map keyed by component name.
func (r *MigrationReconciler) individualComponents(ctx context.Context, namespace string, present []string) (map[string]*castwarev1alpha1.Component, error) {
	out := make(map[string]*castwarev1alpha1.Component, len(present))
	for _, sub := range present {
		ind := &castwarev1alpha1.Component{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: sub}, ind); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get individual component %s for carry-over: %w", sub, err)
		}
		out[sub] = ind
	}
	return out, nil
}

// releaseNameFor resolves a component's helm release name from Mothership,
// falling back to the component name (matching getReleaseName semantics).
func (r *MigrationReconciler) releaseNameFor(ctx context.Context, cluster *castwarev1alpha1.Cluster, componentName string) (string, error) {
	castAiClient, err := r.getCastaiClient(ctx, cluster)
	if err != nil {
		return "", err
	}
	mc, err := castAiClient.GetComponentByName(ctx, componentName)
	if err != nil {
		return "", err
	}
	if mc.ReleaseName == "" {
		return componentName, nil
	}
	return mc.ReleaseName, nil
}

// deriveUmbrellaOverrides maps the set of present individuals to umbrella chart
// tag-mode values. cluster-controller present ⇒ the umbrella must render it
// (non-readonly tags); agent-only ⇒ a readonly tag is sufficient with base
// permissions. Spot-handler presence is reflected in values, not tags.
//
// These go UNDER the user's own spec.values (see values.UmbrellaValues) so an
// explicit user choice still wins.
func (r *MigrationReconciler) deriveUmbrellaOverrides(present []string) map[string]any {
	hasClusterController := contains(present, components.ComponentNameClusterController)
	if !hasClusterController {
		// Agent-only (or agent + spot-handler): readonly tag is satisfiable with
		// base permissions and renders no cluster-controller.
		return map[string]any{
			"tags": map[string]any{
				"readonly": true,
			},
		}
	}
	// cluster-controller was present: render it. Use the "full" tag which pulls
	// in the cluster-controller (requires extended permissions, which the
	// cluster must already have since cluster-controller was installed).
	return map[string]any{
		"tags": map[string]any{
			"full": true,
		},
	}
}

// recordMigrationResult reports the migration outcome to Mothership as a
// component action result on the umbrella component.
func (r *MigrationReconciler) recordMigrationResult(ctx context.Context, log logrus.FieldLogger, cluster *castwarev1alpha1.Cluster, status castai.Status, message, errMsg string) {
	castAiClient, err := r.getCastaiClient(ctx, cluster)
	if err != nil {
		log.WithError(err).Error("Failed to get castai client for migration result report")
		return
	}
	releaseName, err := r.releaseNameFor(ctx, cluster, components.ComponentNameUmbrella)
	if err != nil {
		log.WithError(err).Warn("Failed to resolve umbrella release name for result report")
		releaseName = components.ComponentNameUmbrella
	}
	req := &castai.ComponentActionResult{
		Name:        components.ComponentNameUmbrella,
		Action:      castai.Action_INSTALL,
		Status:      status,
		ReleaseName: releaseName,
		Message:     message,
	}
	if errMsg != "" {
		req.Message = fmt.Sprintf("%s: %s", message, errMsg)
	}
	if err := castAiClient.RecordActionResult(ctx, cluster.Spec.Cluster.ClusterID, req); err != nil {
		log.WithError(err).Error("Failed to record migration result to Mothership")
	}
}

func (r *MigrationReconciler) getCastaiClient(ctx context.Context, cluster *castwarev1alpha1.Cluster) (castai.CastAIClient, error) {
	if r.castAIClientGetter != nil {
		return r.castAIClientGetter(ctx, cluster)
	}
	auth := auth.NewAuth(cluster.Namespace, cluster.Name)
	if err := auth.LoadApiKey(ctx, r.Client); err != nil {
		return nil, err
	}
	rest := castai.NewRestyClient(r.Config, cluster.Spec.API.APIURL, auth)
	return castai.NewClient(nil, r.Config, rest), nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *MigrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&castwarev1alpha1.Component{}).
		Named("migration").
		Complete(r)
}

// patchReadonly patches a Component CR's spec.readonly field via a merge patch,
// preserving all other spec fields.
func patchReadonly(ctx context.Context, c client.Client, component *castwarev1alpha1.Component, readonly bool) error {
	base := component.DeepCopy()
	component.Spec.Readonly = readonly
	return c.Patch(ctx, component, client.MergeFrom(base))
}

// isReleaseNotFound reports whether err is Helm's release-not-found error.
func isReleaseNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "no release found") ||
		strings.Contains(err.Error(), "release: not found")
}

// contains reports whether s is in list.
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
