package cleanup

import (
	"context"

	"github.com/castai/castware-operator/api/v1alpha1"
	"github.com/castai/castware-operator/internal/controller"
	"github.com/sirupsen/logrus"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func NewService(client client.Client, log logrus.FieldLogger) *Service {
	return &Service{Client: client, log: log}
}

type Service struct {
	client.Client
	log logrus.FieldLogger
}

func (s *Service) Run(ctx context.Context) error {
	componentCRs := v1alpha1.ComponentList{}
	if err := s.List(ctx, &componentCRs); err != nil {
		s.log.WithError(err).Error("failed to list component CRs")
		return err
	}
	for _, component := range componentCRs.Items {
		if controllerutil.ContainsFinalizer(&component, controller.ComponentFinalizer) {
			controllerutil.RemoveFinalizer(&component, controller.ComponentFinalizer)
			if component.Labels == nil {
				component.Labels = map[string]string{}
			}
			component.Labels["castware.cast.ai/delete-candidate"] = "true"
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
		if err := s.Delete(ctx, crd); err != nil {
			s.log.WithError(err).WithField("crd", crdName).Error("failed to delete CRD")
			return err
		}
		s.log.WithField("crd", crdName).Info("CRD deleted")
	}

	s.log.Info("cleanup completed")
	return nil
}
