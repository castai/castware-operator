package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/bombsimon/logrusr/v4"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"

	"github.com/castai/castware-operator/internal/castai"
	"github.com/castai/castware-operator/internal/castai/auth"
	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/config"
	"github.com/castai/castware-operator/internal/helm"
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
			log := logrus.StandardLogger().WithField("gitCommit", version.GitCommit).WithField(
				"version", version.Version,
			)
			controllerruntime.SetLogger(logrusr.New(log))

			restConfig := controllerruntime.GetConfigOrDie()
			runtimeClient, err := cluster.New(
				restConfig, func(options *cluster.Options) {
					options.Scheme = scheme
					options.Client.Cache = &client.CacheOptions{
						DisableFor: []client.Object{
							&v1.Secret{},
						},
					}
				},
			)
			if err != nil {
				return fmt.Errorf("failed to create cluster runtimeClient: %w", err)
			}

			ctx, cancel := context.WithTimeout(controllerruntime.SetupSignalHandler(), time.Minute*5)
			defer cancel()

			go func() {
				err = runtimeClient.Start(ctx)
				if err != nil {
					log.WithError(err).Error("failed to start cluster runtimeClient")
					cancel()
				}
			}()

			cacheSynced := runtimeClient.GetCache().WaitForCacheSync(ctx)
			if !cacheSynced {
				return errors.New("failed to sync cache")
			}

			chartLoader := helm.NewChartLoader(log)
			helmClient := helm.NewClient(log, chartLoader, restConfig)

			helmRelease, err := helmClient.GetRelease(
				helm.GetReleaseOptions{
					Namespace:   cfg.PodNamespace,
					ReleaseName: cfg.HelmReleaseName,
				},
			)

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

func newPreflightInstallCheckCmd() *cobra.Command {
	preflightInstallCheckCmd := &cobra.Command{
		Use:   "preflight-install-check",
		Short: "Checks that the operator can be installed",
		Long: "Preflight install check command validates that the operator can be installed " +
			"by checking HTTP connectivity and helm availability.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.GetFromEnvironment()
			if err != nil {
				logrus.StandardLogger().Fatalf("failed to get config from environment: %v", err)
			}
			logrus.StandardLogger().SetLevel(cfg.LogLevel.Level())
			log := logrus.StandardLogger().WithField("gitCommit", version.GitCommit).WithField(
				"version", version.Version,
			)
			controllerruntime.SetLogger(logrusr.New(log))

			ctx, cancel := context.WithTimeout(controllerruntime.SetupSignalHandler(), time.Minute*5)
			defer cancel()

			// Validate namespace is castai-agent
			if cfg.PodNamespace != "castai-agent" {
				return fmt.Errorf(`
==========================================
PREFLIGHT CHECK FAILED
Operator must be installed in namespace 'castai-agent'
==========================================
Current namespace: %s

Action: Install with --namespace castai-agent
==========================================`, cfg.PodNamespace)
			}

			// Get API key from environment
			apiKey := os.Getenv("API_KEY")
			if apiKey == "" {
				log.Error("API_KEY environment variable not set")
				return errors.New("API_KEY environment variable not set. Please provide a valid API key to connect to CAST AI")
			}

			// Get API URL from environment
			apiURL := os.Getenv("API_URL")
			if apiURL == "" {
				log.Error("API_URL environment variable not set")
				return errors.New("API_URL environment variable not set")
			}

			// Get helm repo URL from environment
			helmRepoURL := os.Getenv("HELM_REPO_URL")
			if helmRepoURL == "" {
				log.Error("HELM_REPO_URL environment variable not set")
				return errors.New("HELM_REPO_URL environment variable not set")
			}

			// Check 1: HTTP Client - call GetComponentByName for castware-operator
			log.Info("Running preflight check 1: HTTP client connectivity")
			if err := checkHTTPClient(ctx, log, cfg, apiURL, apiKey); err != nil {
				return fmt.Errorf("preflight check failed: HTTP client connectivity: %w", err)
			}
			log.Info("HTTP client preflight check passed")

			// Check 2: Helm Availability - pull castai-agent chart
			log.Info("Running preflight check 2: helm availability")
			if err := checkHelmAvailability(ctx, log, helmRepoURL); err != nil {
				return fmt.Errorf("preflight check failed: helm availability: %w", err)
			}
			log.Info("Helm availability preflight check passed")

			log.Info("All preflight install checks passed successfully")
			return nil
		},
	}

	return preflightInstallCheckCmd
}

// checkHTTPClient validates that the operator can connect to the CAST AI API
// by attempting to call GetComponentByName for the castware-operator component.
func checkHTTPClient(
	ctx context.Context,
	log logrus.FieldLogger,
	cfg *config.Config,
	apiURL string,
	apiKey string,
) error {
	log.Info("Checking HTTP client connectivity to CAST AI API")

	// Create CAST AI client
	authProvider := auth.NewStaticAuth(apiKey)
	restClient := castai.NewRestyClient(cfg, apiURL, authProvider)
	apiClient := castai.NewClient(log, cfg, restClient)

	// Attempt to get castware-operator component
	log.Info("Attempting to call GetComponentByName for castware-operator")
	component, err := apiClient.GetComponentByName(ctx, "castware-operator")
	if err != nil {
		return fmt.Errorf(
			`
==========================================
PREFLIGHT CHECK FAILED: Cannot connect to CAST AI API
==========================================
API URL: %s

Possible causes:
  1. Invalid API key - verify your credentials
  2. Network issue - check firewall/proxy settings
  3. Wrong API URL - verify the endpoint

Error details: %v
==========================================`, apiURL, err,
		)
	}

	// Validate that we got a valid component response
	if component == nil || component.Id == "" || component.Name == "" {
		return errors.New(
			`
==========================================
PREFLIGHT CHECK FAILED: Invalid API response
==========================================
Received empty component data - API may be unavailable
==========================================`,
		)
	}

	log.Infof(
		"Successfully retrieved castware-operator component from CAST AI (ID: %s, Latest Version: %s)",
		component.Id,
		component.LatestVersion,
	)
	return nil
}

// checkHelmAvailability validates that helm can access the castai-agent chart
// from the configured helm repository by checking the helm index and pulling the chart.
func checkHelmAvailability(ctx context.Context, log logrus.FieldLogger, helmRepoURL string) error {
	log.Infof("Checking helm availability by accessing helm index at %s", helmRepoURL)

	// Download and parse the helm repository index
	r, err := helm.NewHelmRepo(helmRepoURL)
	if err != nil {
		return fmt.Errorf(
			"unable to initialize helm repository %s: %w. "+
				"Please verify the helm repository URL is valid",
			helmRepoURL,
			err,
		)
	}

	log.Info("Downloading helm repository index")
	index, err := r.DownloadIndex()
	if err != nil {

		return fmt.Errorf(
			`
==========================================
PREFLIGHT CHECK FAILED
Cannot access Helm repository
==========================================
Helm Repo URL: %s

Possible causes:
  • Network connectivity issue
  • Incorrect helm repository URL
  • Repository temporarily unavailable

Action: Verify helm repository URL and network
==========================================`, helmRepoURL,
		)
	}

	// Check if castai-agent chart exists in the index
	log.Info("Checking if castai-agent chart exists in helm repository")
	chartEntries, exists := index.Entries[components.ComponentNameAgent]
	if !exists || len(chartEntries) == 0 {
		return fmt.Errorf(
			"castai-agent chart not found in helm repository %s. "+
				"Please verify the helm repository URL is correct",
			helmRepoURL,
		)
	}

	log.Infof(
		"Found %d versions of castai-agent chart in helm repository",
		len(chartEntries),
	)

	// Try to pull the latest version to verify the chart is actually accessible
	latestVersion := chartEntries[0].Version
	log.Infof("Attempting to pull castai-agent chart version %s", latestVersion)

	chartLoader := helm.NewChartLoader(log)
	chartSource := &helm.ChartSource{
		RepoURL: helmRepoURL,
		Name:    components.ComponentNameAgent,
		Version: latestVersion,
	}

	chart, err := chartLoader.Load(ctx, chartSource)
	if err != nil {

		return fmt.Errorf(
			`
==========================================
PREFLIGHT CHECK FAILED
Cannot download Helm chart
==========================================
Chart: castai-agent version %s
Repo: %s

Action: Verify helm repository is accessible and chart exists
==========================================`, latestVersion, helmRepoURL,
		)
	}

	// Validate that we got a valid chart
	if chart == nil || chart.Metadata == nil || chart.Metadata.Name == "" {
		return fmt.Errorf(
			"received invalid chart data for castai-agent from %s. "+
				"Please verify the helm repository is functioning correctly",
			helmRepoURL,
		)
	}

	log.Infof(
		"Successfully pulled and validated castai-agent chart version %s (chart name: %s)",
		chart.Metadata.Version,
		chart.Metadata.Name,
	)
	return nil
}
