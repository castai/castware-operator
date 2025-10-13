package main

import (
	"github.com/castai/castware-operator/internal/config"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

func newUpgradeCmd() *cobra.Command {
	upgradeCmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade Cast AI components",
		Long:  "Upgrade command handles the upgrade process for Cast AI components.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.GetFromEnvironment()
			if err != nil {
				logrus.StandardLogger().Fatalf("failed to get config from environment: %v", err)
			}

			logrus.StandardLogger().SetLevel(cfg.LogLevel.Level())
			log := logrus.StandardLogger().WithField("gitCommit", version.GitCommit).WithField("version", version.Version)

			log.Warn("Upgrade command not implemented")
			return nil
		},
	}

	return upgradeCmd
}
