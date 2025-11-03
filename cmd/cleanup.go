package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bombsimon/logrusr/v4"
	"github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/config"
	"github.com/castai/castware-operator/internal/controller"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func newCleanupCmd() *cobra.Command {

	cleanupCmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Cleanup custom resources after uninstalling the operator",
		Long:  "Cleanup custom resources after uninstalling the operator",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.GetFromEnvironment()
			if err != nil {
				logrus.StandardLogger().Fatalf("failed to get config from environment: %v", err)
			}
			logrus.StandardLogger().SetLevel(cfg.LogLevel.Level())
			log := logrus.StandardLogger().WithField("gitCommit", version.GitCommit).WithField("version", version.Version)
			controllerruntime.SetLogger(logrusr.New(log))

			restConfig := controllerruntime.GetConfigOrDie()
			Client, err := cluster.New(restConfig, func(options *cluster.Options) {
				options.Scheme = scheme
				options.Client.Cache = &client.CacheOptions{
					DisableFor: []client.Object{
						&v1.Secret{},
					},
				}
			})
			if err != nil {
				return fmt.Errorf("failed to create cluster client: %w", err)
			}

			ctx, cancel := context.WithTimeout(controllerruntime.SetupSignalHandler(), time.Minute*10)
			defer cancel()

			go func() {
				err = Client.Start(ctx)
				if err != nil {
					log.WithError(err).Error("failed to start cluster client")
					cancel()
				}
			}()

			cacheSynced := Client.GetCache().WaitForCacheSync(ctx)
			if !cacheSynced {
				return errors.New("failed to sync cache")
			}

			componentCRs := v1alpha1.ComponentList{}
			if err := Client.GetClient().List(ctx, &componentCRs); err != nil {
				log.WithError(err).Error("failed to list component CRs")
				return err
			}
			for _, component := range componentCRs.Items {
				if controllerutil.ContainsFinalizer(&component, controller.ComponentFinalizer) {
					controllerutil.RemoveFinalizer(&component, controller.ComponentFinalizer)
					if component.Labels == nil {
						component.Labels = map[string]string{}
					}
					component.Labels["castware.cast.ai/delete-candidate"] = "true"
					if err := Client.GetClient().Update(ctx, &component); err != nil {
						log.WithError(err).Error("failed to remove finalizer from component CR")
						return err
					}
				}
				if err := Client.GetClient().Delete(ctx, &component); err != nil {
					log.WithError(err).Error("failed to delete component CR")
					return err
				}
			}
			log.Info("component CRs deleted")

			clusterCRs := v1alpha1.ClusterList{}
			if err := Client.GetClient().List(ctx, &clusterCRs); err != nil {
				log.WithError(err).Error("failed to list cluster CRs")
				return err
			}
			for _, clusterCR := range clusterCRs.Items {
				if err := Client.GetClient().Delete(ctx, &clusterCR); err != nil {
					log.WithError(err).Error("failed to delete cluster CR")
					return err
				}
			}
			log.Info("cluster CRs deleted")

			// Delete CRDs
			crdNames := []string{
				"components.castware.cast.ai",
				"clusters.castware.cast.ai",
			}

			for _, crdName := range crdNames {
				crd := &apiextensionsv1.CustomResourceDefinition{
					ObjectMeta: metav1.ObjectMeta{
						Name: crdName,
					},
				}
				if err := Client.GetClient().Delete(ctx, crd); err != nil {
					log.WithError(err).WithField("crd", crdName).Error("failed to delete CRD")
					return err
				}
				log.WithField("crd", crdName).Info("CRD deleted")
			}

			log.Info("cleanup completed")
			return nil
		},
	}

	return cleanupCmd
}
