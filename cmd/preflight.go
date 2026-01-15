package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/bombsimon/logrusr/v4"
	"github.com/castai/castware-operator/internal/config"
	"github.com/castai/castware-operator/internal/helm"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
)

func newPreflightCheckCmd() *cobra.Command {

	preflightCheckCmd := &cobra.Command{
		Use:   "preflight-check",
		Short: "Checks that the operator can be upgraded",
		Long:  "Preflight check command checks that the operator can be upgraded to a newer version.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.GetFromEnvironment()
			if err != nil {
				logrus.StandardLogger().Fatalf("failed to get config from environment: %v", err)
			}
			logrus.StandardLogger().SetLevel(cfg.LogLevel.Level())
			log := logrus.StandardLogger().WithField("gitCommit", version.GitCommit).WithField("version", version.Version)
			controllerruntime.SetLogger(logrusr.New(log))

			restConfig := controllerruntime.GetConfigOrDie()
			client, err := cluster.New(restConfig, func(options *cluster.Options) {
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

			ctx, cancel := context.WithTimeout(controllerruntime.SetupSignalHandler(), time.Minute*5)
			defer cancel()

			go func() {
				err = client.Start(ctx)
				if err != nil {
					log.WithError(err).Error("failed to start cluster client")
					cancel()
				}
			}()

			cacheSynced := client.GetCache().WaitForCacheSync(ctx)
			if !cacheSynced {
				return errors.New("failed to sync cache")
			}

			chartLoader := helm.NewChartLoader(log)
			helmClient := helm.NewClient(log, chartLoader, restConfig)

			helmRelease, err := helmClient.GetRelease(helm.GetReleaseOptions{
				Namespace:   cfg.PodNamespace,
				ReleaseName: cfg.HelmReleaseName,
			})

			if err != nil {
				return fmt.Errorf("failed to get helm release: %w", err)
			}

			extendedPermissionsVal, ok := helmRelease.Chart.Values["extendedPermissions"]
			if !ok {
				//nolint:lll
				log.Error("extendedPermissions value not found in helm release values, assuming extended permissions are not enabled")
				return errors.New("extendedPermissions value not found in helm release values")
			}
			currentExtendedPermissions, ok := extendedPermissionsVal.(bool)
			if !ok {
				return errors.New("failed to parse extendedPermissions from helm release values")
			}

			newExtendedPermissions := os.Getenv("EXTENDED_PERMISSIONS") == "true"

			log.Infof("current extendedPermissions value: %v", currentExtendedPermissions)
			log.Infof("new extendedPermissions value: %v", newExtendedPermissions)

			if !newExtendedPermissions && currentExtendedPermissions {
				return errors.New("cannot downgrade extended permissions once they are already set")
			}

			return nil
		},
	}

	return preflightCheckCmd
}
