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
// Deletion guard: the umbrella CR carries MigrationFinalizer while the migration
// is in flight (armed in phaseMarkReadonly, before readonly is set). A deletion
// arriving mid-flight is converted into a rollback that restores the individual
// regime before the CR — and its status.migrationPhase — is removed, instead of
// leaving a half-migrated cluster with no controller to drive recovery.
//
// Permission gate: the umbrella chart renders a broader RBAC surface than the
// phase1/phase2 individual charts, so an under-permissioned operator service
// account would only fail mid-swap — after the non-agent individuals are already
// uninstalled — leaving the cluster half-migrated. Before any release is touched
// (top of phaseMarkReadonly, before the finalizer is armed), the controller asks
// Mothership (components:validateInstall) whether the umbrella install is
// permitted. A refusal sets Migrating=False / MigrationBlocked with the Mothership
// block reason and leaves the CR and the cluster untouched; a Mothership failure
// degrades the migration like any other dependency outage.
//
// Standalone conflict guard: the umbrella chart also renders charts the
// operator does not manage as individual components (castai-kvisor,
// castai-evictor, ...). A standalone release of one of those in the namespace
// is invisible to the mutual-exclusivity gate yet collides with the umbrella
// install (TakeOwnership silently absorbs it, or a duplicate workload is
// rendered). phaseMarkReadonly and phaseInstallUmbrella block on such releases
// (Migrating=False / MigrationBlocked) until they are removed; the migration
// then proceeds from the recorded phase.
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
	"helm.sh/helm/v3/pkg/storage/driver"
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

// MigrationFinalizer blocks deletion of the umbrella CR while a migration is in
// flight. It is armed in phaseMarkReadonly (before readonly is set — the
// validating webhook rejects updates to a CR that is readonly in both old and
// new state, so the finalizer can only be added while the CR is writable) and
// removed in finalizeUmbrellaSuccess/finalizeUmbrellaFailure, atomically with
// clearing spec.migrate. If the CR is deleted mid-flight anyway, Reconcile
// converts the deletion into a rollback that restores the individual regime
// before the CR (and its status.migrationPhase) disappears.
//
// Distinct from the component reconciler's ComponentFinalizer
// ("castware.cast.ai/cleanup-helm"), which uninstalls the helm release on CR
// deletion: the migration owns cleanup here because it knows the migration
// phase and can roll back.
const MigrationFinalizer = "castware.cast.ai/umbrella-migration"

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

	// Only umbrella CRs are of interest here. Every other CR is left to the
	// component reconciler — including the UmbrellaConflict path that implements
	// acceptance criterion 4 (migrate not set → no migration + conflict status).
	if component.Spec.Component != components.ComponentNameUmbrella {
		return ctrl.Result{}, nil
	}

	// Deletion guard: a deletion arriving mid-migration must not let the CR (and
	// its status.migrationPhase) vanish while the cluster is half-migrated.
	// While MigrationFinalizer is present the object stays in a deleting state
	// and every reconcile lands here, driving an abort-rollback instead of the
	// normal phase machine. Runs before the migrate gate and the cluster
	// availability gate so neither a cleared migrate (crash window between the
	// terminal spec write and a hypothetical later finalizer release) nor
	// cluster state can strand a deleting CR holding the finalizer.
	if !component.DeletionTimestamp.IsZero() {
		return r.handleUmbrellaDeletion(ctx, log, component)
	}

	if !component.Spec.Migrate {
		// Terminal repair hook: a migration whose finalize cleared spec.migrate
		// but whose status write failed would otherwise be invisible forever —
		// this gate returns early and nothing re-runs the finalize. The stale
		// phase identifies the outcome (see repairStaleMigrationStatus); clean
		// states (phase "" after success, RolledBack after failure) no-op.
		return r.repairStaleMigrationStatus(ctx, log, component)
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

	// One CAST AI client per reconcile: the permission gate, release-name
	// resolution, present-individuals probing, and the Mothership result report
	// all share it, so the API-key secret is read once and the resty client's
	// connection pool is reused instead of each helper building (and discarding)
	// its own client.
	castAiClient, err := r.getCastaiClient(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, r.degradeMigration(ctx, log, component, fmt.Errorf("get castai client: %w", err))
	}

	switch component.Status.MigrationPhase {
	case "", castwarev1alpha1.MigrationPhaseMarkReadonly:
		return r.phaseMarkReadonly(ctx, log, component, cluster, castAiClient)
	case castwarev1alpha1.MigrationPhaseUninstallIndividuals:
		return r.phaseUninstallIndividuals(ctx, log, component, cluster, castAiClient)
	case castwarev1alpha1.MigrationPhaseInstallUmbrella:
		return r.phaseInstallUmbrella(ctx, log, component, cluster, castAiClient)
	case castwarev1alpha1.MigrationPhaseVerify:
		return r.phaseVerify(ctx, log, component, cluster, castAiClient)
	case castwarev1alpha1.MigrationPhaseFinalize:
		return r.phaseFinalize(ctx, log, component, cluster, castAiClient)
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

// phaseMarkReadonly arms the migration finalizer on the umbrella CR, sets
// spec.readonly=true on it (sidelining the component reconciler for the whole
// migration) and on each present individual sub-component CR, then advances to
// UninstallIndividuals. Idempotent: patching an already-readonly CR is a no-op.
//
// Two pre-flight guards run before the finalizer is armed and before any spec
// is modified, so a refused migration leaves the CR and the cluster
// byte-for-byte unchanged (only a status condition is written): the Mothership
// permission gate (validateMigrationPermissions) and the standalone-release
// conflict guard (checkUmbrellaOnlyConflicts).
func (r *MigrationReconciler) phaseMarkReadonly(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component, cluster *castwarev1alpha1.Cluster, castAiClient castai.CastAIClient) (ctrl.Result, error) {
	// Permission gate: the umbrella chart renders a broader RBAC surface than the
	// individual charts. An under-permissioned service account must fail here —
	// before any release is touched — rather than mid-swap after the non-agent
	// individuals were uninstalled.
	blocked, err := r.validateMigrationPermissions(ctx, log, component, cluster, castAiClient)
	if err != nil {
		// Mothership/auth unreachable: transient. Degrade (retry with backoff)
		// rather than treat it as a refusal — the gate must not block a migration
		// only because it cannot ask the question.
		return ctrl.Result{}, r.degradeMigration(ctx, log, component, err)
	}
	if blocked {
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	// Standalone-release conflict guard: block while a standalone release of
	// an umbrella-managed chart the operator does not support is present — the
	// umbrella install would silently absorb or duplicate it (see
	// checkUmbrellaOnlyConflicts). A blocked migration leaves the cluster
	// untouched and resumes once the conflict is removed.
	if err := r.checkUmbrellaOnlyConflicts(ctx, log, component); err != nil {
		return ctrl.Result{}, err
	}

	// Arm the deletion guard before anything else. Order is load-bearing: the
	// validating webhook (ValidateUpdate) rejects updates to a CR that is
	// readonly in both old and new state, so the finalizer can only be added
	// while the CR is still writable — i.e. before the readonly patch below.
	// On resume the finalizer is already present and this is a no-op.
	if !controllerutil.ContainsFinalizer(component, MigrationFinalizer) {
		if component.Spec.Readonly {
			// Legacy resume: readonly was set by an operator version without the
			// finalizer (interrupted mid-phase). The webhook rejects updates to a
			// readonly CR, so it cannot be armed now — attempting it would wedge
			// the migration on a permanent rejection. Continue without the guard,
			// as this migration ran before.
			log.Warn("Umbrella CR is readonly without the migration finalizer (legacy mid-phase resume); continuing without the deletion guard")
		} else {
			base := component.DeepCopy()
			controllerutil.AddFinalizer(component, MigrationFinalizer)
			if err := r.Patch(ctx, component, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, fmt.Errorf("arm migration finalizer: %w", err)
			}
		}
	}

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
	present, err := r.presentIndividuals(ctx, castAiClient, cluster)
	if err != nil {
		// Returning the error (not Warn + fixed requeue) records a reconcile
		// error and applies exponential backoff, and degradeMigration surfaces
		// the stall on the CR's Migrating condition.
		return ctrl.Result{}, r.degradeMigration(ctx, log, component, fmt.Errorf("resolve present individuals: %w", err))
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

	return r.setMigrationPhase(ctx, component, castwarev1alpha1.MigrationPhaseUninstallIndividuals)
}

// validateMigrationPermissions asks Mothership whether the umbrella install is
// permitted for this cluster (components:validateInstall). The umbrella renders a
// broader RBAC surface than the individual charts, so an under-permissioned
// operator service account is refused here — before any release is touched —
// instead of failing mid-swap and leaving the cluster half-migrated.
//
// A refusal is recorded via setMigratingFalse (ReasonMigrationBlocked) with the
// Mothership block reason; a transport/API error is returned so the caller
// degrades and retries (the gate must not block only because it cannot ask the
// question). A migration resuming past MarkReadonly never re-runs the gate: it
// is already beyond the point the gate protects.
func (r *MigrationReconciler) validateMigrationPermissions(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component, cluster *castwarev1alpha1.Cluster, castAiClient castai.CastAIClient) (bool, error) {
	// The server compares the component's required RBAC surface (selected from
	// component_params) against the operator's installed conditions, so send
	// the umbrella's user-supplied install values.
	componentParams, err := utils.UnmarshalJSON(component.Spec.Values)
	if err != nil {
		return false, fmt.Errorf("unmarshal umbrella values for permission gate: %w", err)
	}

	validation, err := migrationgate.ValidateInstallPermissions(ctx, castAiClient, cluster.Spec.Cluster.ClusterID, component.Spec.Component, component.Spec.Version, componentParams)
	if err != nil {
		return false, fmt.Errorf("validate umbrella install permissions: %w", err)
	}

	if !validation.Allowed {
		blockReason := "umbrella install not permitted by CAST.AI"
		if validation.BlockReason != "" {
			blockReason = validation.BlockReason
		}
		log.Warnf("Migration blocked by Mothership permission validation: %s", blockReason)
		// Same condition the standalone-release conflict guard sets, so a
		// blocked migration reads identically regardless of which guard refused
		// it. The migration is not failed — no rollback is driven — because
		// nothing has been touched yet; the caller requeues and the gate
		// re-checks periodically, so the migration proceeds automatically once
		// the block reason is lifted (e.g. the operator is reinstalled with
		// extendedPermissions="true").
		r.setMigratingFalse(ctx, log, component, castwarev1alpha1.ReasonMigrationBlocked, fmt.Sprintf("Migration blocked: %s", blockReason))
		return true, nil
	}
	log.Info("Permission gate passed; proceeding with migration")
	return false, nil
}

// phaseUninstallIndividuals uninstalls the non-agent individual releases in
// reverse phase order (cluster-controller first, then spot-handler). The agent
// is excluded: its resources are adopted by the umbrella install (TakeOwnership)
// and its release is forgotten in Finalize, never uninstalled. Individual CRs
// are kept (readonly) for rollback. Idempotent via IgnoreNotFound.
func (r *MigrationReconciler) phaseUninstallIndividuals(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component, cluster *castwarev1alpha1.Cluster, castAiClient castai.CastAIClient) (ctrl.Result, error) {
	present, err := r.presentIndividuals(ctx, castAiClient, cluster)
	if err != nil {
		// See phaseMarkReadonly: the error is returned so controller-runtime
		// records it and backs off, and the stall is surfaced on the CR.
		return ctrl.Result{}, r.degradeMigration(ctx, log, component, fmt.Errorf("resolve present individuals: %w", err))
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
		releaseName, err := r.releaseNameFor(ctx, castAiClient, sub)
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
			return r.rollback(ctx, log, component, cluster, castAiClient, fmt.Errorf("uninstall %s: %w", sub, err))
		}
	}

	return r.setMigrationPhase(ctx, component, castwarev1alpha1.MigrationPhaseInstallUmbrella)
}

// phaseInstallUmbrella installs the umbrella chart, deriving its tag mode from
// which individuals were present and using the shared umbrellaValues builder.
// TakeOwnership (set in helm.Client.Install) lets it adopt the still-running
// agent Deployment without a pod restart, preserving the Mothership heartbeat.
// Idempotent: if the umbrella release is already present (a partial prior run),
// it skips straight to Verify.
func (r *MigrationReconciler) phaseInstallUmbrella(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component, cluster *castwarev1alpha1.Cluster, castAiClient castai.CastAIClient) (ctrl.Result, error) {
	umbrellaReleaseName, err := r.releaseNameFor(ctx, castAiClient, components.ComponentNameUmbrella)
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
		return r.setMigrationPhase(ctx, component, castwarev1alpha1.MigrationPhaseVerify)
	} else if !isReleaseNotFound(getErr) {
		// A non-not-found error means helm is unreachable; requeue rather than
		// risk a partial install.
		return ctrl.Result{}, fmt.Errorf("check umbrella release presence: %w", getErr)
	}

	// Pre-install guard: same block as phaseMarkReadonly's pre-flight, re-run
	// here so a migration resumed at this phase (or a standalone release that
	// appeared mid-migration) is caught before the umbrella install absorbs
	// or duplicates it. The already-present fast path above is skipped on
	// purpose: once the umbrella is installed the adoption (if any) already
	// happened, and blocking Finalize would not undo it.
	if err := r.checkUmbrellaOnlyConflicts(ctx, log, component); err != nil {
		return ctrl.Result{}, err
	}

	present, err := r.presentIndividuals(ctx, castAiClient, cluster)
	if err != nil {
		// See phaseMarkReadonly: returned (not swallowed) so the failure is
		// observable in reconcile-error metrics and the Migrating condition.
		return ctrl.Result{}, r.degradeMigration(ctx, log, component, fmt.Errorf("resolve present individuals for tag derivation: %w", err))
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
		return r.rollback(ctx, log, component, cluster, castAiClient, fmt.Errorf("install umbrella: %w", err))
	}

	return r.setMigrationPhase(ctx, component, castwarev1alpha1.MigrationPhaseVerify)
}

// phaseVerify waits for the umbrella release to reach deployed status and the
// agent to be healthy (first verified), then advances to Finalize. A verify
// failure or timeout rolls back to the individual regime.
func (r *MigrationReconciler) phaseVerify(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component, cluster *castwarev1alpha1.Cluster, castAiClient castai.CastAIClient) (ctrl.Result, error) {
	umbrellaReleaseName, err := r.releaseNameFor(ctx, castAiClient, components.ComponentNameUmbrella)
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
			return r.rollback(ctx, log, component, cluster, castAiClient, errors.New("umbrella release not found during verify"))
		}
		return ctrl.Result{}, fmt.Errorf("get umbrella release for verify: %w", err)
	}

	if rel.Info.Status != release.StatusDeployed {
		// Check the verify deadline. The phase's start time is
		// status.migrationPhaseStartedAt (stamped when the phase was entered);
		// if it predates the timeout, roll back rather than wait forever.
		if r.verifyDeadlineExceeded(component) {
			return r.rollback(ctx, log, component, cluster, castAiClient, fmt.Errorf("umbrella verify timeout: release status %s", rel.Info.Status))
		}
		log.Infof("Umbrella release not yet deployed (status=%s); requeueing", rel.Info.Status)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// Umbrella is deployed. Verify the agent (adopted by the umbrella) is
	// healthy — it is the heartbeat signal, so "agent first verified" is the
	// gate before finalizing.
	if err := r.verifyAgentHealthy(ctx, component); err != nil {
		if r.verifyDeadlineExceeded(component) {
			return r.rollback(ctx, log, component, cluster, castAiClient, fmt.Errorf("agent not healthy after verify timeout: %w", err))
		}
		log.WithError(err).Warn("Agent not yet healthy; requeueing")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	return r.setMigrationPhase(ctx, component, castwarev1alpha1.MigrationPhaseFinalize)
}

// phaseFinalize retires the individual regime: forgets the agent's helm release
// (storage secret deleted — no pod deletion, heartbeat preserved), deletes the
// individual Component CRs, then clears the umbrella's migrate/readonly flags
// and reports success to Mothership. After Finalize the umbrella is the sole
// owner and ComponentReconciler resumes normal reconcile.
func (r *MigrationReconciler) phaseFinalize(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component, cluster *castwarev1alpha1.Cluster, castAiClient castai.CastAIClient) (ctrl.Result, error) {
	// Forget the individual agent release: its resources are now owned by the
	// umbrella (adopted via TakeOwnership). Deleting only the storage record
	// leaves the running agent pods untouched. Idempotent.
	agentReleaseName, err := r.releaseNameFor(ctx, castAiClient, components.ComponentNameAgent)
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
		// Clear readonly and remove the finalizer, then delete the CR, all within a
		// single RetryOnConflict loop that re-gets the latest object each attempt.
		//
		// The validating webhook (ValidateUpdate) rejects updates to a CR that is
		// readonly in both old and new state, so readonly must be cleared in the
		// same patch as the finalizer removal. Meanwhile the ComponentReconciler's
		// forceReadonlyIfUmbrellaInstalled races to re-set readonly=true (the
		// umbrella is installed at this point), so a plain Update on a stale object
		// conflicts ("the object has been modified"). Delete must also run on the
		// freshly-patched object: deleting with a stale ResourceVersion captured
		// before the patch would 409 once the patch bumps it, causing an
		// unnecessary requeue of the whole Finalize phase. Running both the patch
		// and the delete inside the retry closure on the same latest object keeps
		// them consistent. The delete is not webhook-gated, so it is not subject
		// to the readonly rejection the patch guards against.
		nn := types.NamespacedName{Namespace: component.Namespace, Name: sub}
		if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			latest := &castwarev1alpha1.Component{}
			if err := r.Get(ctx, nn, latest); err != nil {
				return err
			}
			if controllerutil.ContainsFinalizer(latest, ComponentFinalizer) || latest.Spec.Readonly {
				base := latest.DeepCopy()
				latest.Spec.Readonly = false
				controllerutil.RemoveFinalizer(latest, ComponentFinalizer)
				if err := r.Patch(ctx, latest, client.MergeFrom(base)); err != nil {
					return err
				}
			}
			// Delete on the freshly-read object (its ResourceVersion is current
			// as of this attempt; a concurrent patch surfaces as a conflict the
			// outer RetryOnConflict absorbs).
			return r.Delete(ctx, latest)
		}); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return ctrl.Result{}, fmt.Errorf("delete individual component %s: %w", sub, err)
		}
	}

	// Migration complete: clear migrate + readonly on the umbrella so
	// ComponentReconciler resumes normal reconcile, and mark it Available.
	if err := r.finalizeUmbrellaSuccess(ctx, component); err != nil {
		return ctrl.Result{}, err
	}

	// Report success to Mothership.
	r.recordMigrationResult(ctx, log, castAiClient, cluster, castai.Status_OK, "migration succeeded: cluster now managed by the umbrella chart", "")

	log.Info("Migration finalized: umbrella is the sole owner")
	return ctrl.Result{}, nil
}

// rollback restores the individual-component regime after a migration failure.
// It uninstalls the umbrella (if it was installed), clears readonly on the
// surviving individual CRs so ComponentReconciler reinstalls them from their
// stored spec.values, and marks the umbrella as failed. The 1-minute heartbeat
// bound applies to the success path only; rollback may take longer to fully
// reinstall individuals, and the agent specifically incurs a pod restart (see
// the uninstall comment below).
func (r *MigrationReconciler) rollback(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component, cluster *castwarev1alpha1.Cluster, castAiClient castai.CastAIClient, cause error) (ctrl.Result, error) {
	log.WithError(cause).Warn("Migration failed; rolling back to individual regime")

	// Uninstall the umbrella if it was installed. IgnoreNotFound: a pre-install
	// failure has no umbrella.
	//
	// Helm's Uninstall deletes every resource recorded in the release manifest.
	// Because the umbrella install used TakeOwnership, that manifest includes
	// the agent Deployment it adopted from the individual agent release — so
	// this uninstall deletes the agent pods, not just the umbrella's own
	// resources. This is a deliberate trade-off: the heartbeat invariant (no pod
	// restart) is scoped to the success path; the rollback path is permitted a
	// restart. The agent is restored below by re-enabling its CR, which makes
	// ComponentReconciler helm-upgrade--install the individual agent release,
	// re-creating the Deployment. (Forgetting the umbrella instead of
	// uninstalling would preserve the pods but orphan its non-agent resources
	// — cluster-controller/spot-handler — leaving a hybrid state, which is
	// worse.)
	umbrellaReleaseName, nameErr := r.releaseNameFor(ctx, castAiClient, components.ComponentNameUmbrella)
	if nameErr != nil {
		// Fall back to the default umbrella release name rather than skip the
		// uninstall: skipping re-enables the individuals below while the
		// umbrella stays installed — the hybrid state this rollback exists to
		// prevent. The default matches the common case (the release name is the
		// component name unless Mothership overrides it), and Uninstall's
		// IgnoreNotFound makes a wrong guess harmless. Mirrors the fallback in
		// recordMigrationResult.
		log.WithError(nameErr).Warn("Failed to resolve umbrella release name for rollback; falling back to the default release name")
		umbrellaReleaseName = components.ComponentNameUmbrella
	}
	if _, err := r.HelmClient.Uninstall(helm.UninstallOptions{
		Namespace:      component.Namespace,
		ReleaseName:    umbrellaReleaseName,
		IgnoreNotFound: true,
		Wait:           true,
	}); err != nil {
		// A failed umbrella uninstall leaves the cluster in a hybrid state.
		// Surface it on the CR's failure condition (via the wrapped cause below)
		// as well as the operator log; the next reconcile of the scan path will
		// also warn Mothership about the hybrid config.
		log.WithError(err).Error("Failed to uninstall umbrella during rollback; cluster may be in hybrid state")
		cause = fmt.Errorf("%w; additionally, the umbrella uninstall failed: %v — cluster may be in a hybrid state until it is removed", cause, err)
	}

	// Re-enable the individual CRs so ComponentReconciler reinstalls them. The
	// agent's individual helm release was never uninstalled (only adopted by the
	// umbrella), so its release record still exists in storage; but the umbrella
	// uninstall above deleted the agent Deployment it had adopted, so the agent
	// CR's upgrade path re-creates the Deployment — a pod restart and a
	// heartbeat gap within the rollback path's documented allowance. The
	// non-agent individuals were uninstalled outright; their CRs reinstall from
	// spec.values.
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

	r.recordMigrationResult(ctx, log, castAiClient, cluster, castai.Status_ERROR, "migration failed and rolled back; individual components restored", cause.Error())

	return ctrl.Result{}, nil
}

// handleUmbrellaDeletion handles a deletion of the umbrella CR while
// spec.migrate is still set (MigrationReconciler is only here for such CRs, and
// the deletion pre-check in Reconcile routes every reconcile of a deleting CR
// here while the finalizer is present). With the finalizer armed, apiserver has
// set deletionTimestamp but holds the object; the migration drives an
// abort-rollback restoring the individual regime, and the finalizer removal in
// finalizeUmbrellaFailure then completes the deletion.
func (r *MigrationReconciler) handleUmbrellaDeletion(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component) (ctrl.Result, error) {
	// Without the finalizer the object is not held by us: nothing is in flight
	// from the migration's perspective — either the migration never armed it
	// (a legacy mid-phase resume of a readonly CR cannot be armed; see
	// phaseMarkReadonly) — and the component reconciler's own deletion path owns
	// the CR.
	if !controllerutil.ContainsFinalizer(component, MigrationFinalizer) {
		return ctrl.Result{}, nil
	}

	// The finalizer is armed but migrate is already cleared. Normally
	// unreachable — the finalizer is removed atomically with the migrate clear
	// in finalizeUmbrellaSuccess/Failure — but a crash between those two writes
	// could leave this state. There is nothing to roll back (no migration in
	// flight), so just release the CR.
	if !component.Spec.Migrate {
		log.Info("Umbrella CR deleting with migration finalizer but no migration in flight; releasing finalizer")
		return ctrl.Result{}, r.removeMigrationFinalizer(ctx, component)
	}

	// Migration in flight + CR deleted: abort. Cluster lookup first — an absent
	// cluster means Mothership connectivity and helm release-name resolution are
	// gone, so rollback is not drivable; release the finalizer rather than block
	// the user's deletion forever (the cluster is being decommissioned).
	cluster := &castwarev1alpha1.Cluster{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: component.Namespace, Name: component.Spec.Cluster}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			log.Warn("Umbrella CR deleted mid-migration and cluster CR is gone; releasing finalizer without rollback")
			return ctrl.Result{}, r.removeMigrationFinalizer(ctx, component)
		}
		log.WithError(err).Error("Failed to get cluster for mid-migration deletion")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	if !meta.IsStatusConditionTrue(cluster.Status.Conditions, typeAvailableCluster) ||
		cluster.Spec.Cluster == nil || cluster.Spec.Cluster.ClusterID == "" {
		// Same availability gate as the main path; requeue until it lifts.
		log.Info("Waiting for cluster to be available before aborting migration")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// One client for the abort-rollback (see Reconcile): built once, shared by
	// every Mothership call in the path.
	castAiClient, err := r.getCastaiClient(ctx, cluster)
	if err != nil {
		log.WithError(err).Error("Failed to get castai client for mid-migration deletion abort")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	log.Warn("Umbrella CR deleted mid-migration; aborting and rolling back to individual regime")
	return r.rollback(ctx, log, component, cluster, castAiClient, errors.New("umbrella component CR deleted mid-migration"))
}

// removeMigrationFinalizer releases the deletion guard via a merge patch,
// tolerating the CR having already gone. Used when the migration is not in
// flight (see handleUmbrellaDeletion) — the terminal paths remove it inside
// finalizeUmbrellaSuccess/Failure instead.
func (r *MigrationReconciler) removeMigrationFinalizer(ctx context.Context, component *castwarev1alpha1.Component) error {
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &castwarev1alpha1.Component{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: component.Namespace, Name: component.Name}, latest); err != nil {
			return err
		}
		if !controllerutil.ContainsFinalizer(latest, MigrationFinalizer) {
			return nil
		}
		base := latest.DeepCopy()
		controllerutil.RemoveFinalizer(latest, MigrationFinalizer)
		return r.Patch(ctx, latest, client.MergeFrom(base))
	}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("remove migration finalizer: %w", err)
	}
	return nil
}

// finalizeUmbrellaSuccess clears migrate/readonly, removes the migration
// finalizer, and sets Available=True on the umbrella, and records the migration
// phase as empty (terminal success). Spec and status are separate subresources,
// so they are updated in two fresh-read steps: spec via Update, then status via
// Status().Update on a re-read object (matching
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
		// Release the deletion guard atomically with the migrate clear, so there
		// is no window where the migration is terminal but a deletion would
		// still be blocked.
		controllerutil.RemoveFinalizer(latest, MigrationFinalizer)
		return r.Update(ctx, latest)
	}); err != nil {
		// On the abort path (handleUmbrellaDeletion) this Update completes the
		// CR's deletion: the CR is in a deleting state and this may be its last
		// finalizer — apiserver removes it once no finalizers remain. The
		// status write below then 404s, which is expected, not a failure.
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("clear umbrella migrate/readonly: %w", err)
	}
	return r.writeUmbrellaSuccessStatus(ctx, component)
}

// writeUmbrellaSuccessStatus records the terminal success status: empty
// migration phase, Migrating=False (ReasonMigrationSucceeded), Available=True.
// Split out of finalizeUmbrellaSuccess so repairStaleMigrationStatus can
// re-record it when the original write failed after spec.migrate was cleared.
// The CR having already been deleted (abort path) is tolerated.
func (r *MigrationReconciler) writeUmbrellaSuccessStatus(ctx context.Context, component *castwarev1alpha1.Component) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &castwarev1alpha1.Component{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: component.Namespace, Name: component.Name}, latest); err != nil {
			if apierrors.IsNotFound(err) {
				// The spec write above completed the deletion (abort path: last
				// finalizer removed on a deleting object). Nothing left to report.
				return nil
			}
			return err
		}
		latest.Status.MigrationPhase = ""
		latest.Status.MigrationPhaseStartedAt = metav1.Time{}
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

// finalizeUmbrellaFailure clears migrate/readonly, removes the migration
// finalizer, sets the umbrella Available=False with a MigrationFailed reason,
// and records the RolledBack phase (terminal failure). Same two-step
// spec/status update as finalizeUmbrellaSuccess; the same IsNotFound tolerance
// applies on the abort path where the spec write completes the deletion.
func (r *MigrationReconciler) finalizeUmbrellaFailure(ctx context.Context, component *castwarev1alpha1.Component, cause error) error {
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &castwarev1alpha1.Component{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: component.Namespace, Name: component.Name}, latest); err != nil {
			return err
		}
		latest.Spec.Migrate = false
		latest.Spec.Readonly = false
		// Release the deletion guard atomically with the migrate clear (see
		// finalizeUmbrellaSuccess).
		controllerutil.RemoveFinalizer(latest, MigrationFinalizer)
		return r.Update(ctx, latest)
	}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("clear umbrella migrate/readonly on failure: %w", err)
	}
	return r.writeUmbrellaFailureStatus(ctx, component, cause)
}

// writeUmbrellaFailureStatus records the terminal failure status: RolledBack
// phase, Migrating=False (ReasonMigrationFailed), Available=False. Split out of
// finalizeUmbrellaFailure so repairStaleMigrationStatus can re-record it when
// the original write failed after spec.migrate was cleared; the original cause
// is lost in that case, so the repair passes a synthetic one. The CR having
// already been deleted (abort path) is tolerated.
func (r *MigrationReconciler) writeUmbrellaFailureStatus(ctx context.Context, component *castwarev1alpha1.Component, cause error) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &castwarev1alpha1.Component{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: component.Namespace, Name: component.Name}, latest); err != nil {
			if apierrors.IsNotFound(err) {
				// The spec write above completed the deletion (abort path).
				return nil
			}
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

// repairStaleMigrationStatus re-records the terminal migration status on an
// umbrella CR whose finalize status write failed. The finalize paths clear
// spec.migrate (and readonly/finalizer) in one write and record the terminal
// status in a second; if that second write fails, the requeue hits the
// spec.migrate gate in Reconcile and nothing ever corrects the status — the CR
// would keep reporting an in-flight migration forever. The stale phase
// identifies the terminal outcome, because the only writers of spec.migrate=false
// are the finalize paths and the phase each leaves behind is deterministic:
//
//   - phase Finalize: the migration reached and completed phaseFinalize (no
//     rollback is reachable from Finalize — its error paths requeue instead),
//     so the success status write failed. Re-record the success status.
//   - any other non-empty phase: the rollback path ran (its setMigrationPhase
//     recorded the phase it was triggered from), so the failure status write
//     failed. Re-record the failure status with a synthetic cause (the original
//     cause was lost with the failed write).
//   - phase "" (clean success) and RolledBack (the failure path's own terminal
//     record) are the normal terminal states and are left alone.
func (r *MigrationReconciler) repairStaleMigrationStatus(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component) (ctrl.Result, error) {
	switch phase := component.Status.MigrationPhase; phase {
	case "", castwarev1alpha1.MigrationPhaseRolledBack:
		return ctrl.Result{}, nil
	case castwarev1alpha1.MigrationPhaseFinalize:
		log.Info("Repairing stale migration status: finalize succeeded but its status write failed")
		if err := r.writeUmbrellaSuccessStatus(ctx, component); err != nil {
			return ctrl.Result{}, fmt.Errorf("repair stale success status: %w", err)
		}
		return ctrl.Result{}, nil
	default:
		log.Warnf("Repairing stale migration status: rollback from phase %q completed but its status write failed", phase)
		err := r.writeUmbrellaFailureStatus(ctx, component, errors.New("original failure cause unavailable (terminal status re-recorded after a failed status write)"))
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("repair stale failure status: %w", err)
		}
		return ctrl.Result{}, nil
	}
}

// setMigrationPhase records the phase in status, stamps the per-phase start
// time used by the Verify deadline, and sets the Migrating condition, then
// returns nil so the next reconcile enters the new phase. This is the single
// durable write of the Migrating condition: it re-reads the latest object and
// persists it via the status subresource, so callers must not rely on
// in-memory mutations of their local component copy being persisted.
func (r *MigrationReconciler) setMigrationPhase(ctx context.Context, component *castwarev1alpha1.Component, phase string) (ctrl.Result, error) {
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &castwarev1alpha1.Component{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: component.Namespace, Name: component.Name}, latest); err != nil {
			return err
		}
		latest.Status.MigrationPhase = phase
		latest.Status.MigrationPhaseStartedAt = metav1.Now()
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

// degradeMigration sets Migrating=False (ReasonMigrationDegraded) on the
// umbrella CR with the failure reason, then returns the error so
// controller-runtime records a reconcile error and applies exponential backoff.
// A status-write failure is logged, not propagated — the reconcile error must
// surface the dependency failure, not the observability attempt. The condition
// is cleared by the next successful setMigrationPhase.
func (r *MigrationReconciler) degradeMigration(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component, err error) error {
	r.setMigratingFalse(ctx, log, component, castwarev1alpha1.ReasonMigrationDegraded, fmt.Sprintf("Migration stalled: %s", err))
	return err
}

// blockMigration sets Migrating=False (ReasonMigrationBlocked) on the
// umbrella CR with the blocking reason, then returns the error so
// controller-runtime records a reconcile error and applies exponential
// backoff. Unlike a degradation, a block requires human action (remove the
// standalone release); the migration proceeds from the recorded phase on the
// next reconcile once the blocking condition is gone, and the condition is
// replaced by the next successful setMigrationPhase.
func (r *MigrationReconciler) blockMigration(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component, err error) error {
	r.setMigratingFalse(ctx, log, component, castwarev1alpha1.ReasonMigrationBlocked, fmt.Sprintf("Migration blocked: %s", err))
	return err
}

// setMigratingFalse writes Migrating=False with the given reason on the
// umbrella CR. A status-write failure is logged, not propagated — the
// returned reconcile error must surface the blocking/stalling cause, not the
// observability attempt.
func (r *MigrationReconciler) setMigratingFalse(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component, reason, message string) {
	if condErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &castwarev1alpha1.Component{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: component.Namespace, Name: component.Name}, latest); err != nil {
			return err
		}
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:    castwarev1alpha1.TypeMigrating,
			Status:  metav1.ConditionFalse,
			Reason:  reason,
			Message: message,
		})
		return r.Status().Update(ctx, latest)
	}); condErr != nil {
		log.WithError(condErr).Warnf("Failed to record %s condition", reason)
	}
}

// checkUmbrellaOnlyConflicts blocks the migration while a standalone release
// of an umbrella-managed chart the operator does not individually support
// (e.g. castai-kvisor, castai-evictor) is present in the component's
// namespace. The umbrella install takes ownership of name-matching
// resources — silently absorbing the standalone release and leaving a ghost
// that the migration rollback would then delete — and renders duplicates
// when names differ. Blocking forces the standalone release to be removed
// first; the migration then proceeds from the recorded phase. Fail-safe: if
// helm cannot be queried, the migration is degraded rather than allowed to
// proceed on unknown state.
func (r *MigrationReconciler) checkUmbrellaOnlyConflicts(ctx context.Context, log logrus.FieldLogger, component *castwarev1alpha1.Component) error {
	present, err := migrationgate.InstalledUmbrellaOnlyCharts(r.HelmClient, component.Namespace)
	if err != nil {
		return r.degradeMigration(ctx, log, component, fmt.Errorf("check standalone umbrella-managed releases: %w", err))
	}
	if len(present) == 0 {
		return nil
	}
	return r.blockMigration(ctx, log, component, fmt.Errorf(
		"standalone release(s) of umbrella-managed chart(s) present: %s; the umbrella chart installs them and the operator cannot migrate a standalone install — remove the release(s) and the migration will proceed",
		strings.Join(present, ", ")))
}

// verifyDeadlineExceeded reports whether the Verify phase has been running
// longer than verifyTimeout, using status.migrationPhaseStartedAt as the phase
// start. The Migrating condition's LastTransitionTime cannot serve this
// purpose: meta.SetStatusCondition only moves it on a condition status change,
// and the condition stays True across the whole state machine, so it marks
// the migration start, not the Verify start. For migrations already in flight
// under an operator version that predates the field (it is empty), the
// condition's LastTransitionTime is used as a best-effort fallback; when both
// are absent the deadline starts fresh (not exceeded).
func (r *MigrationReconciler) verifyDeadlineExceeded(component *castwarev1alpha1.Component) bool {
	if !component.Status.MigrationPhaseStartedAt.IsZero() {
		return time.Since(component.Status.MigrationPhaseStartedAt.Time) > verifyTimeout
	}
	cond := meta.FindStatusCondition(component.Status.Conditions, castwarev1alpha1.TypeMigrating)
	if cond == nil {
		return false
	}
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
func (r *MigrationReconciler) presentIndividuals(ctx context.Context, castAiClient castai.CastAIClient, cluster *castwarev1alpha1.Cluster) ([]string, error) {
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
func (r *MigrationReconciler) releaseNameFor(ctx context.Context, castAiClient castai.CastAIClient, componentName string) (string, error) {
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
func (r *MigrationReconciler) recordMigrationResult(ctx context.Context, log logrus.FieldLogger, castAiClient castai.CastAIClient, cluster *castwarev1alpha1.Cluster, status castai.Status, message, errMsg string) {
	releaseName, err := r.releaseNameFor(ctx, castAiClient, components.ComponentNameUmbrella)
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

// getCastaiClient builds a CAST AI client for the cluster (loads the API-key
// secret, constructs the resty client). Reconcile resolves it once per
// reconcile and passes it down to every Mothership call in the path, so the
// secret is read once and the HTTP connection pool is shared.
func (r *MigrationReconciler) getCastaiClient(ctx context.Context, cluster *castwarev1alpha1.Cluster) (castai.CastAIClient, error) {
	if r.castAIClientGetter != nil {
		return r.castAIClientGetter(ctx, cluster)
	}
	auth := auth.NewAuth(cluster.Namespace, cluster.Name)
	if err := auth.LoadApiKey(ctx, r.Client); err != nil {
		return nil, err
	}
	rest := castai.NewRestyClient(r.Config, cluster.Spec.API.APIURL, auth)
	return castai.NewClient(r.Log, r.Config, rest), nil
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
// It checks the typed sentinel first (driver.ErrReleaseNotFound, which
// GetRelease preserves through its %w wrapping) and falls back to a substring
// match for older Helm error shapes that don't wrap the sentinel.
func isReleaseNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return true
	}
	return strings.Contains(err.Error(), driver.ErrReleaseNotFound.Error()) ||
		strings.Contains(err.Error(), "no release found")
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
