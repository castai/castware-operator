package cleanup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/castai/castware-operator/api/v1alpha1"
	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/controller"
	"github.com/sirupsen/logrus"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	// defaultUmbrellaFinalizerWait bounds the wait for the operator to
	// resolve the umbrella CRs' finalizers during operator teardown.
	defaultUmbrellaFinalizerWait = 2 * time.Minute

	umbrellaFinalizerPollInterval = time.Second
)

func NewService(client client.Client, log logrus.FieldLogger) *Service {
	return &Service{Client: client, log: log, umbrellaFinalizerWait: defaultUmbrellaFinalizerWait}
}

type Service struct {
	client.Client
	log logrus.FieldLogger
	// umbrellaFinalizerWait bounds the wait for the operator to resolve the
	// umbrella CRs' finalizers; zero falls back to the default.
	umbrellaFinalizerWait time.Duration
}

func (s *Service) Run(ctx context.Context) error {
	componentCRs := v1alpha1.ComponentList{}
	if err := s.List(ctx, &componentCRs); err != nil {
		s.log.WithError(err).Error("failed to list component CRs")
		return err
	}
	var umbrellaKeys []client.ObjectKey
	for _, component := range componentCRs.Items {
		if component.Spec.Component == components.ComponentNameUmbrella {
			if !controllerutil.ContainsFinalizer(&component, controller.ComponentFinalizer) {
				// Unexpected: the operator adds this finalizer on every
				// reconcile, so its absence bypasses the handoff — make it
				// visible; the CR is deleted directly below.
				s.log.WithField("component", client.ObjectKeyFromObject(&component).String()).
					Warn("umbrella component CR has no cleanup finalizer, deleting it without the label-gated handoff")
			} else {
				// CID-1052: preserve the umbrella's helm release. The finalizer
				// is not stripped: the still-running operator resolves it via the
				// delete-candidate label without uninstalling.
				if component.Labels == nil {
					component.Labels = map[string]string{}
				}
				component.Labels[controller.LabelDeleteCandidate] = "true"
				if err := s.Update(ctx, &component); err != nil {
					s.log.WithError(err).Error("failed to mark umbrella component CR as delete candidate")
					return err
				}
				umbrellaKeys = append(umbrellaKeys, client.ObjectKey{Namespace: component.Namespace, Name: component.Name})
			}
		} else if controllerutil.ContainsFinalizer(&component, controller.ComponentFinalizer) {
			controllerutil.RemoveFinalizer(&component, controller.ComponentFinalizer)
			if component.Labels == nil {
				component.Labels = map[string]string{}
			}
			component.Labels[controller.LabelDeleteCandidate] = "true"
			if err := s.Update(ctx, &component); err != nil {
				s.log.WithError(err).Error("failed to remove finalizer from component CR")
				return err
			}
		}
		if err := s.Delete(ctx, &component); err != nil {
			s.log.WithError(err).Error("failed to delete component CR")
			return err
		}
	}
	// Wait for the operator to resolve the umbrella CRs' finalizers, so they
	// are fully removed before the CRDs are deleted (else the CRs and the
	// components CRD could get stuck in Terminating).
	if err := s.waitForUmbrellaCRsRemoved(ctx, umbrellaKeys); err != nil {
		return err
	}
	s.log.Info("component CRs deleted")

	clusterCRs := v1alpha1.ClusterList{}
	if err := s.List(ctx, &clusterCRs); err != nil {
		s.log.WithError(err).Error("failed to list cluster CRs")
		return err
	}
	for _, clusterCR := range clusterCRs.Items {
		if err := s.Delete(ctx, &clusterCR); err != nil {
			s.log.WithError(err).Error("failed to delete cluster CR")
			return err
		}
	}
	s.log.Info("cluster CRs deleted")

	// Delete the operator's CRDs only; umbrella subcomponent CRDs are owned
	// by the umbrella chart, must survive the uninstall, and are re-adopted
	// on reinstall (CID-1047).
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
		if err := s.Delete(ctx, crd); err != nil {
			s.log.WithError(err).WithField("crd", crdName).Error("failed to delete CRD")
			return err
		}
		s.log.WithField("crd", crdName).Info("CRD deleted")
	}

	s.log.Info("cleanup completed")
	return nil
}

// waitForUmbrellaCRsRemoved waits for the operator to fully delete the
// umbrella CRs above (finalizer resolution) before the operator CRDs are
// removed; on timeout the finalizer is stripped here as a fallback.
func (s *Service) waitForUmbrellaCRsRemoved(ctx context.Context, keys []client.ObjectKey) error {
	for _, key := range keys {
		if err := s.waitForUmbrellaCRRemoved(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) waitForUmbrellaCRRemoved(ctx context.Context, key client.ObjectKey) error {
	timeout := s.umbrellaFinalizerWait
	if timeout <= 0 {
		timeout = defaultUmbrellaFinalizerWait
	}
	err := wait.PollUntilContextTimeout(ctx, umbrellaFinalizerPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		var component v1alpha1.Component
		err := s.Get(ctx, key, &component)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
	if err == nil {
		return nil
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("wait for umbrella component CR %s deletion: %w", key, err)
	}

	// The operator did not resolve the finalizer in time (e.g. it is already
	// gone): strip it ourselves so the CR — and the CRD after it — can
	// complete deletion; the umbrella release stays preserved either way.
	s.log.WithField("component", key.String()).Warn("operator did not remove the umbrella finalizer in time, removing it")
	var component v1alpha1.Component
	if err := s.Get(ctx, key, &component); err != nil {
		return fmt.Errorf("get umbrella component CR %s: %w", key, err)
	}
	controllerutil.RemoveFinalizer(&component, controller.ComponentFinalizer)
	if err := s.Update(ctx, &component); err != nil {
		return fmt.Errorf("remove finalizer from umbrella component CR %s: %w", key, err)
	}
	return nil
}
