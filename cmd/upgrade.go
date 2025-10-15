package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bombsimon/logrusr/v4"
	"github.com/castai/castware-operator/internal/config"
	"github.com/castai/castware-operator/internal/helm"
	"github.com/castai/castware-operator/internal/selfupgrade"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
)

func newUpgradeCmd() *cobra.Command {
	var (
		targetVersion      string
		clusterCrName      string
		clusterCrNamespace string
	)

	upgradeCmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade Cast AI components",
		Long:  "Upgrade command handles the upgrade process for Cast AI components.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if clusterCrName == "" {
				return errors.New("cluster-cr-name is required")
			}
			cfg, err := config.GetFromEnvironment()
			if err != nil {
				logrus.StandardLogger().Fatalf("failed to get config from environment: %v", err)
			}
			logrus.StandardLogger().SetLevel(cfg.LogLevel.Level())
			log := logrus.StandardLogger().WithField("gitCommit", version.GitCommit).WithField("version", version.Version)
			controllerruntime.SetLogger(logrusr.New(log))

			if clusterCrNamespace == "" {
				log.Infof("Cluster CR namespace not provided, defaulting to: %s", cfg.PodNamespace)
				clusterCrNamespace = cfg.PodNamespace
			}

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

			ctx, cancel := context.WithTimeout(controllerruntime.SetupSignalHandler(), time.Minute*10)
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

			svc := selfupgrade.NewService(client.GetClient(), helmClient, cfg, log, clusterCrName, clusterCrNamespace)

			return svc.Run(ctx, targetVersion)
		},
	}
	upgradeCmd.Flags().StringVar(&targetVersion, "version", "",
		"The new version to upgrade to, if empty the latest version will be used")
	upgradeCmd.Flags().StringVar(&clusterCrName, "cluster-cr-name", "",
		"The name of cluster custom resource that initiated the upgrade")
	upgradeCmd.Flags().StringVar(&clusterCrNamespace, "cluster-cr-namespace", "",
		"The namespace of cluster custom resource that initiated the upgrade")

	return upgradeCmd
}
