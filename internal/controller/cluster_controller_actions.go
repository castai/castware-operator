package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/castai"
	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/helm"
	"github.com/castai/castware-operator/internal/migrationgate"
	"github.com/castai/castware-operator/internal/utils"
)

// This file contains Mothership action handling: the reconcile-secret gating
// step, action polling, and the install / upgrade / rollback / uninstall
// handlers (including the install-side mutual-exclusivity gate).

func (r *ClusterReconciler) reconcileSecret(ctx context.Context, cluster *castwarev1alpha1.Cluster) (bool, error) {
	log := r.Log

	// Can't reconcile api key if the cluster is not there.
	if cluster.Spec.Cluster == nil {
		return false, nil
	}

	// Check if api key secret changed.
	secret := &corev1.Secret{}
	secKey := types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Spec.APIKeySecret}
	if err := r.Get(ctx, secKey, secret); err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}

	// If api key changed validate the new one.
	if secret.ResourceVersion != cluster.Status.LastSecretVersion {
		castAiClient, _, err := r.getCastaiClient(ctx, cluster)
		if err != nil {
			log.WithError(err).Error("Failed to get api client")
			return false, err
		}
		if _, err := castAiClient.GetCluster(ctx, cluster.Spec.Cluster.ClusterID); err != nil {
			log.WithError(err).WithField("clusterId", cluster.Spec.Cluster.ClusterID).Error("Failed to get cluster")

			// Set cluster to unavailable if GetCluster fails.
			meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
				Type:    typeAvailableCluster,
				Status:  metav1.ConditionFalse,
				Reason:  "GetClusterFailed",
				Message: fmt.Sprintf("Failed to get cluster by ID: %v", err),
			})
			err = r.Status().Update(ctx, cluster)
			if err != nil {
				log.WithError(err).Error("Failed to set available status to false")
				return false, err
			}

			return false, err
		}
		log.Info("Api key updated")

		cluster.Status.LastSecretVersion = secret.ResourceVersion
		if err := r.Status().Update(ctx, cluster); err != nil {
			return true, err
		}
	}
	return false, nil
}

func (r *ClusterReconciler) pollActions(ctx context.Context, castAiClient castai.CastAIClient, cluster *castwarev1alpha1.Cluster) (ctrl.Result, error) {
	log := r.Log

	log.Debug("Polling actions")

	actions, err := castAiClient.PollActions(ctx, cluster.Spec.Cluster.ClusterID)
	if err != nil {
		log.WithError(err).Error("Failed to poll actions")
		return ctrl.Result{RequeueAfter: time.Minute * 5}, nil
	}
	for _, action := range actions.Actions {
		var actionErr error
		switch a := action.Action().(type) {
		case *castai.ActionInstall:
			log.Infof("install action: %v", a.Component)
			actionErr = r.handleInstall(ctx, castAiClient, cluster, a)
		case *castai.ActionUpgrade:
			log.Infof("upgrade action: %v", a.Component)
			actionErr = r.handleUpgrade(ctx, cluster, a)
		case *castai.ActionUninstall:
			log.Infof("uninstall action: %v", a.Component)
			actionErr = r.handleUninstall(ctx, cluster, a)
		case *castai.ActionRollback:
			log.Infof("rollback action: %v", a.Component)
			actionErr = r.handleRollback(ctx, cluster, a)
		default:
			actionErr = errUnknownAction
			log.Warnf("unknown action: %v", action)
		}
		if actionErr != nil {
			log.WithError(actionErr).Errorf("Failed to handle action: %v", action)
		}

		err := castAiClient.AckAction(ctx, cluster.Spec.Cluster.ClusterID, action.Id, actionErr)
		if err != nil {
			// If action ack fails, we can't do anything about it, just process the next one.
			log.WithError(err).Error("Failed to ack action")
		}
	}

	return ctrl.Result{RequeueAfter: time.Second * 30}, nil
}

func (r *ClusterReconciler) handleInstall(ctx context.Context, castAiClient castai.CastAIClient, cluster *castwarev1alpha1.Cluster, action *castai.ActionInstall) error {
	log := r.Log
	namespacedName := types.NamespacedName{Namespace: cluster.Namespace, Name: action.Component}

	component := &castwarev1alpha1.Component{}
	err := r.Get(ctx, namespacedName, component)
	if err == nil {
		if action.Upsert {
			upgradeAction := &castai.ActionUpgrade{
				Version:              action.Version,
				Component:            action.Component,
				ValuesOverrides:      action.ValuesOverrides,
				ResetThenReuseValues: action.ResetThenReuseValues,
				ReleaseName:          getReleaseName(component),
			}
			return r.handleUpgrade(ctx, cluster, upgradeAction)
		}
		return errors.New("component already exists")
	} else if !apierrors.IsNotFound(err) {
		log.WithError(err).Error("Failed to get component")
		return err
	}

	// Umbrella / individual charts mutual-exclusivity gate. A Mothership install
	// action must not create the conflicting installation side. The returned
	// error is acked back to Mothership by pollActions so the conflict is
	// surfaced there rather than silently double-installing.
	if err := r.installBlockedByMutualExclusivity(ctx, castAiClient, cluster, action.Component); err != nil {
		return err
	}

	releaseName := action.ReleaseName
	if releaseName == "" {
		releaseName = action.Component
	}

	component = &castwarev1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: cluster.Namespace,
			Name:      action.Component,
		},
		Spec: castwarev1alpha1.ComponentSpec{
			Component:   action.Component,
			Cluster:     cluster.Name,
			Enabled:     true,
			Version:     action.Version,
			ReleaseName: releaseName,
		},
	}

	if action.ValuesOverrides != nil {
		values, err := utils.UnflattenMap(action.ValuesOverrides)
		if err != nil {
			return err
		}
		b, err := json.Marshal(values)
		if err != nil {
			return err
		}
		component.Spec.Values = &v1.JSON{Raw: b}
	}

	log.Debugf("creating new component: %v", component)
	return r.Create(ctx, component)
}

// installBlockedByMutualExclusivity returns a non-nil error when a Mothership
// install action for componentName would violate the umbrella / individual
// charts mutual-exclusivity gate:
//   - installing an individual (non-umbrella) component while the umbrella
//     release is installed;
//   - installing the umbrella component while individual component releases
//     are installed.
//
// A scan-created umbrella CR never carries spec.migrate, and Mothership
// install actions do not either, so the umbrella side always blocks when
// individuals are present (the user must set migrate on the CR directly).
// The umbrella-present side always blocks individual installs.
func (r *ClusterReconciler) installBlockedByMutualExclusivity(ctx context.Context, castAiClient castai.CastAIClient, cluster *castwarev1alpha1.Cluster, componentName string) error {
	// The gate only applies to the umbrella and its overlapping sub-components;
	// other components render disjoint workloads and cannot conflict.
	if !migrationgate.IsUmbrellaOrSubcomponent(componentName) {
		return nil
	}

	// Umbrella side: refuse if any individual sub-component release is present.
	if componentName == components.ComponentNameUmbrella {
		names, err := migrationgate.ResolveNames(ctx, castAiClient)
		if err != nil {
			return fmt.Errorf("evaluate umbrella mutual-exclusivity: resolve release names: %w", err)
		}
		present := migrationgate.InstalledSubcomponents(r.HelmClient, cluster.Namespace, names.SubcomponentReleases)
		if len(present) > 0 {
			return fmt.Errorf("cannot install umbrella component: individual component releases present (%s); set spec.migrate: true on the umbrella CR to take them over", strings.Join(present, ", "))
		}
		return nil
	}

	// Sub-component side: only the umbrella release name is needed.
	umbrellaReleaseName, err := migrationgate.ResolveUmbrellaReleaseName(ctx, castAiClient)
	if err != nil {
		return fmt.Errorf("evaluate umbrella mutual-exclusivity: resolve umbrella release name: %w", err)
	}
	if migrationgate.UmbrellaInstalled(r.HelmClient, cluster.Namespace, umbrellaReleaseName) {
		return fmt.Errorf("cannot install individual component %q: umbrella release %q is installed; use the castai-umbrella chart instead", componentName, umbrellaReleaseName)
	}
	return nil
}

func (r *ClusterReconciler) handleUpgrade(ctx context.Context, cluster *castwarev1alpha1.Cluster, action *castai.ActionUpgrade) error {
	log := r.Log

	if action.Component == components.ComponentNameOperator {
		log.Infof("operator upgrade action: version %s", action.Version)
		return r.handleOperatorUpgrade(ctx, cluster, action)
	}

	if action.ReleaseName == "" {
		return errors.New("release name is required for component upgrade")
	}

	namespacedName := types.NamespacedName{Namespace: cluster.Namespace, Name: action.Component}

	component := &castwarev1alpha1.Component{}
	err := r.Get(ctx, namespacedName, component)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.WithError(err).Error("Failed to get component")
			return errComponentNotFount
		}
		log.WithError(err).Error("Failed to get component")
		return fmt.Errorf("failed to get component: %w", err)
	}

	if component.Spec.Version == action.Version {
		return errors.New("component already up to date")
	}

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := r.Get(ctx, types.NamespacedName{
			Name:      component.Name,
			Namespace: component.Namespace,
		}, component); err != nil {
			return err
		}

		updatedComponent := component.DeepCopy()
		updatedComponent.Spec.Version = action.Version
		updatedComponent.Spec.ReleaseName = action.ReleaseName

		if action.ValuesOverrides != nil {
			values, err := utils.UnflattenMap(action.ValuesOverrides)
			if err != nil {
				return err
			}
			if component.Spec.Values != nil {
				currentValues := map[string]interface{}{}
				if err = json.Unmarshal(component.Spec.Values.Raw, &currentValues); err != nil {
					return err
				}
				err = utils.MergeMaps(currentValues, values)
				if err != nil {
					return err
				}
				// MergeMaps merges the second map into the first one.
				values = currentValues
			}

			b, err := json.Marshal(values)
			if err != nil {
				return err
			}
			updatedComponent.Spec.Values = &v1.JSON{Raw: b}
		}

		return r.Patch(ctx, updatedComponent, client.MergeFrom(component))
	})
}

func (r *ClusterReconciler) handleRollback(ctx context.Context, cluster *castwarev1alpha1.Cluster, action *castai.ActionRollback) error {
	log := r.Log
	namespacedName := types.NamespacedName{Namespace: cluster.Namespace, Name: action.Component}

	component := &castwarev1alpha1.Component{}
	err := r.Get(ctx, namespacedName, component)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.WithError(err).Error("Failed to get component")
			return errComponentNotFount
		}
		log.WithError(err).Error("Failed to get component")
		return fmt.Errorf("failed to get component: %w", err)
	}

	helmRelease, err := r.HelmClient.GetRelease(helm.GetReleaseOptions{
		Namespace:   component.Namespace,
		ReleaseName: getReleaseName(component),
	})
	if err != nil {
		log.WithError(err).Error("Failed to get helm release")
		return err
	}
	// Helm release version start from 1 for the first install, if version is lower than 2
	// the component has never been upgrade, hence nothing to rollback
	if helmRelease.Version < 2 {
		return ErrNothingToRollback
	}

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var latestComponent castwarev1alpha1.Component
		if err := r.Get(ctx, types.NamespacedName{
			Name:      component.Name,
			Namespace: component.Namespace,
		}, &latestComponent); err != nil {
			return err
		}

		latestComponent.Status.Rollback = true

		return r.Status().Update(ctx, &latestComponent)
	})
}

func (r *ClusterReconciler) handleUninstall(ctx context.Context, cluster *castwarev1alpha1.Cluster, action *castai.ActionUninstall) error {
	log := r.Log
	namespacedName := types.NamespacedName{Namespace: cluster.Namespace, Name: action.Component}

	component := &castwarev1alpha1.Component{}
	err := r.Get(ctx, namespacedName, component)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.WithError(err).Error("Failed to get component")
			return errComponentNotFount
		}
		log.WithError(err).Error("Failed to get component")
		return fmt.Errorf("failed to get component: %w", err)
	}

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := r.Get(ctx, types.NamespacedName{
			Name:      component.Name,
			Namespace: component.Namespace,
		}, component); err != nil {
			return err
		}

		return r.Delete(ctx, component)
	})
}
