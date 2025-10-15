package selfupgrade

import (
	"context"
	"errors"
	"fmt"
	"time"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/castai"
	"github.com/castai/castware-operator/internal/castai/auth"
	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/config"
	"github.com/castai/castware-operator/internal/helm"
	"github.com/sirupsen/logrus"
	"helm.sh/helm/v3/pkg/release"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	errUninstalled   = errors.New("helm release is uninstalled")
	errStatusUnknown = errors.New("helm release status is unknown")
)

func NewService(client client.Client, helmClient helm.Client, config *config.Config, log logrus.FieldLogger, clusterCrName string, clusterCrNamespace string) *Service {
	return &Service{Client: client, helmClient: helmClient, config: config, log: log, clusterCrName: clusterCrName, clusterCrNamespace: clusterCrNamespace}
}

type Service struct {
	client.Client
	helmClient         helm.Client
	config             *config.Config
	log                logrus.FieldLogger
	clusterCrName      string
	clusterCrNamespace string
}

func (s *Service) Run(ctx context.Context, targetVersion string) error {
	log := s.log.WithField("cluster", s.clusterCrName)
	cluster := &castwarev1alpha1.Cluster{}
	err := s.Get(ctx, client.ObjectKey{Name: s.clusterCrName, Namespace: s.clusterCrNamespace}, cluster)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("cluster resource not found")
			return err
		}
		// Error reading the object.
		log.WithError(err).Error("Failed to get cluster")
		return err
	}
	defer func() {
		// TODO: set UpgradeJobName to empty string
		if err := s.updateStatus(ctx, cluster); err != nil {
			log.WithError(err).Error("Failed to update status")
		}
	}()

	auth := auth.NewAuth(cluster.Namespace, cluster.Name)
	if err := auth.LoadApiKey(ctx, s.Client); err != nil {
		return fmt.Errorf("failed to load api key: %w", err)
	}
	castAiClient := castai.NewClient(s.log, castai.NewRestyClient(s.config, cluster.Spec.API.APIURL, auth))
	component, err := castAiClient.GetComponentByName(ctx, components.ComponentNameOperator)
	if err != nil {
		return fmt.Errorf("failed to get component: %w", err)
	}
	if targetVersion == "" {
		log.Infof("target version not specified, defaulting to latest version: %s", component.LatestVersion)
		targetVersion = component.LatestVersion
	}
	log = s.log.WithField("target_version", targetVersion)
	getReleaseOptions := helm.GetReleaseOptions{
		Namespace:   cluster.Namespace,
		ReleaseName: component.Name,
	}
	helmRelease, err := s.helmClient.GetRelease(getReleaseOptions)
	if err != nil {
		return fmt.Errorf("failed to get helm release: %w", err)
	}
	upgradeOptions := helm.UpgradeOptions{
		ChartSource: &helm.ChartSource{
			RepoURL: cluster.Spec.HelmRepoURL,
			Name:    helmRelease.Chart.Metadata.Name,
			Version: targetVersion,
		},
		Release:              helmRelease,
		ResetThenReuseValues: true,
		DryRun:               true,
		Recreate:             true,
	}
	_, err = s.helmClient.Upgrade(ctx, upgradeOptions)
	if err != nil {
		log.WithError(err).Error("Upgrade dry run failed")
		return fmt.Errorf("upgrade dry run failed: %w", err)
	}

	// Dry run was successful, proceed with upgrade
	upgradeOptions.DryRun = false
	previousVersion := helmRelease.Chart.Metadata.Version
	helmRelease, err = s.helmClient.Upgrade(ctx, upgradeOptions)
	if err != nil {
		log.WithError(err).Error("Upgrade run failed")
		return fmt.Errorf("upgrade run failed: %w", err)
	}
	currentVersion := helmRelease.Chart.Metadata.Version
	defer func() {
		if cluster.Spec.Cluster != nil && cluster.Spec.Cluster.ClusterID != "" {
			actionResult := &castai.ComponentActionResult{
				Name:           components.ComponentNameOperator,
				Action:         castai.Action_UPGRADE,
				CurrentVersion: currentVersion,
				Version:        previousVersion,
				Status:         castai.Status_OK,
				ReleaseName:    helmRelease.Name,
			}
			if err != nil {
				actionResult.Message = err.Error()
				actionResult.Status = castai.Status_ERROR
			}
			err = castAiClient.RecordActionResult(ctx, cluster.Spec.Cluster.ClusterID, actionResult)
			if err != nil {
				log.WithError(err).Error("Failed to record action result")
			}
		}
	}()
	log.Infof("Upgrade started, release name: %s -> %s", previousVersion, currentVersion)
	err = s.checkReleaseStatus(ctx, log, getReleaseOptions)
	if err != nil {
		// If context was canceled or timed out, return the error and do not attempt to rollback
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			log.Warn("Upgrade canceled for timeout, won't rollback")
			return err
		} else if errors.Is(err, errUninstalled) {
			log.Warn("Upgrade failed, helm release is uninstalled, won't rollback")
			return err
		}

		log.WithError(err).Warn("Upgrade failed, trying to rollback")
		rollbackErr := s.helmClient.Rollback(helm.RollbackOptions{
			Namespace:   getReleaseOptions.Namespace,
			ReleaseName: getReleaseOptions.ReleaseName,
		})
		if rollbackErr != nil {
			err = errors.Join(err, rollbackErr)
			log.WithError(rollbackErr).Error("Rollback failed")
		}
		return err
	}

	// TODO: p3 set cluster CR status to upgraded (remove upgrade job id)
	return nil
}

func (s *Service) checkReleaseStatus(ctx context.Context, log *logrus.Entry, getReleaseOptions helm.GetReleaseOptions) error {
	t := time.NewTicker(time.Second * 5)
	defer t.Stop()
	var (
		helmRelease *release.Release
		err         error
	)

	for {
		select {
		case <-t.C:
			err = wait.ExponentialBackoff(retry.DefaultBackoff, func() (bool, error) {
				helmRelease, err = s.helmClient.GetRelease(getReleaseOptions)
				if err != nil {
					log.WithError(err).Error("Failed to get helm release")
					return false, err
				}
				return true, nil
			})
			if err != nil {
				return err
			}

			switch helmRelease.Info.Status {
			case release.StatusDeployed:
				log.Info("Upgrade successful")
				// TODO: p4 - check pod readiness
				return nil
			case release.StatusFailed:
				return fmt.Errorf("helm is in failed status: %s", helmRelease.Info.Description)
			case release.StatusUninstalled, release.StatusUninstalling:
				log.Warnf("Upgrade failed, helm release is uninstalled (%s)", helmRelease.Info.Description)
				return errUninstalled
			case release.StatusSuperseded:
				// Nothing to do, just wait for the ticker and get the latest status
			case release.StatusPendingUpgrade, release.StatusPendingInstall, release.StatusPendingRollback:
				log.Info("Upgrade still in progress, waiting for it to complete")
			case release.StatusUnknown:
				// Status is unknown, it shouldn't happen, but if it does,
				// the only thing we can do is trying to rollback.
				log.Warn("Helm release status is unknown")
				return errStatusUnknown
			}
		case <-ctx.Done():
			log.Error("Upgrade canceled for timeout")
			return ctx.Err()
		}
	}
}

func (s *Service) updateStatus(ctx context.Context, cluster *castwarev1alpha1.Cluster) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var latestCluster castwarev1alpha1.Cluster
		if err := s.Get(ctx, types.NamespacedName{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
		}, &latestCluster); err != nil {
			return err
		}

		// Modify latestCluster.Status here
		latestCluster.Status = cluster.Status

		return s.Status().Update(ctx, &latestCluster)
	})
}
