package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bombsimon/logrusr/v4"
	"github.com/castai/castware-operator/internal/cleanup"
	"github.com/castai/castware-operator/internal/config"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
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

			svc := cleanup.NewService(Client.GetClient(), log)
			return svc.Run(ctx)
		},
	}

	return cleanupCmd
}
