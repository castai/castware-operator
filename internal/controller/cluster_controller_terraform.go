package controller

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sirupsen/logrus"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/castai"
	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/rolebindings"
)

// This file contains the terraform onboarding sync loop: detecting Component
// CRs created by Terraform and resolving their version/values from existing
// installations.

func (r *ClusterReconciler) syncTerraformComponents(ctx context.Context, castaiClient castai.CastAIClient, cluster *castwarev1alpha1.Cluster) (bool, error) {
	// Only process if terraform flag is set
	if !cluster.Spec.Terraform {
		return false, nil
	}

	log := r.Log.WithField("action", "sync-terraform-components")

	extendedPermsExist, err := rolebindings.CheckExtendedPermissionsExist(ctx, r.Client, cluster.Namespace)
	if err != nil {
		return false, fmt.Errorf("failed to check extended permissions: %w", err)
	}

	reconcileNeeded := false
	for _, componentName := range components.SupportedComponents {
		component := &castwarev1alpha1.Component{}
		err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: componentName}, component)
		if err != nil {
			if apierrors.IsNotFound(err) {
				// CR doesn't exist yet, continue checking other components
				log.Debugf("Component CR %s not found, may be created by Terraform later", componentName)
				continue
			}
			log.WithError(err).Errorf("Failed to get component %s", componentName)
			return reconcileNeeded, err
		}

		// Component CR exists, check if it needs terraform migration handling
		if component.IsInitiliazedByTerraform() {
			log.Infof("Processing terraform migration for component %s", componentName)
			if requiresExtendedPermissions(component) && !extendedPermsExist {
				log.Warnf("Component %s requires extended permissions, but extendedPermissions flag is not enabled, skipping", componentName)
				continue
			}
			needsReconcile, err := r.handleComponentTerraformMigration(ctx, castaiClient, cluster, component)
			if err != nil {
				return reconcileNeeded, err
			}
			if needsReconcile {
				reconcileNeeded = true
			}
		}
	}

	return reconcileNeeded, nil
}

// handleComponentTerraformMigration processes a Component CR with terraform migration mode
func (r *ClusterReconciler) handleComponentTerraformMigration(
	ctx context.Context,
	castaiClient castai.CastAIClient,
	cluster *castwarev1alpha1.Cluster,
	component *castwarev1alpha1.Component,
) (bool, error) {
	log := r.Log.WithFields(logrus.Fields{
		"component": component.Name,
		"migration": component.Spec.Migration,
		"version":   component.Spec.Version,
	})

	updatedComponent := component.DeepCopy()

	// Case 1: Version is already set in the CR
	if component.Spec.Version != "" {
		log.Info("Version already set in terraform migration, clearing migration flag")
		updatedComponent.Spec.Migration = ""
	} else {
		// Case 2: No version set, need to detect based on cluster migration mode
		switch cluster.Spec.MigrationMode {
		case castwarev1alpha1.ClusterMigrationModeAutoupgrade:
			// Leave version empty - component controller or mutating webhook will handle it and set latest version
			updatedComponent.Spec.Migration = ""
		default:
			// Write mode (or empty/default) - must detect version from existing installation
			log.Info("Write mode: detecting version from existing installation")
			existingVersion, err := r.detectComponentVersion(ctx, log, castaiClient, cluster, getReleaseName(component), component.Name)
			if err != nil {
				log.WithError(err).Warn("Failed to detect component version")
			}

			if existingVersion != nil && existingVersion.Version != "" {
				log.Infof("Detected version: %s", existingVersion.Version)
				updatedComponent.Spec.Version = existingVersion.Version

				// Also sync values if they exist and were not set by TF
				if existingVersion.ComponentConfig != nil && component.Spec.Values == nil {
					values, err := json.Marshal(existingVersion.ComponentConfig)
					if err != nil {
						log.WithError(err).Warn("Failed to marshal component config")
					} else {
						updatedComponent.Spec.Values = &v1.JSON{Raw: values}
					}
				}

				updatedComponent.Spec.Migration = ""
			} else {
				log.Info("No existing installation found for terraform migration in write mode")
				// In write mode without existing installation, leave version empty for latest
				updatedComponent.Spec.Migration = ""
			}
		}
	}

	err := r.Patch(ctx, updatedComponent, client.MergeFrom(component))
	if err != nil {
		log.WithError(err).Error("Failed to patch component")
		return false, err
	}
	log.Info("Successfully processed terraform migration")
	return true, nil
}
