package crd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/sirupsen/logrus"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
)

const yamlBufferSize = 4 * 1024

// Installer takes care of ensuring CRDs exist on the cluster the operator runs in.
type Installer struct {
	client apiextensionsclient.Interface
	log    logrus.FieldLogger
}

// NewInstaller builds an Installer backed by the provided rest.Config.
func NewInstaller(cfg *rest.Config, log logrus.FieldLogger) (*Installer, error) {
	clientset, err := apiextensionsclient.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating apiextensions client: %w", err)
	}

	return &Installer{
		client: clientset,
		log:    log,
	}, nil
}

// EnsureFromFS reads the provided manifests and makes sure the referenced CRDs exist.
func (i *Installer) EnsureFromFS(ctx context.Context, filesystem fs.FS, filenames []string) error {
	for _, name := range filenames {
		if err := i.ensureFile(ctx, filesystem, name); err != nil {
			return err
		}
	}

	return nil
}

func (i *Installer) ensureFile(ctx context.Context, filesystem fs.FS, name string) error {
	content, err := fs.ReadFile(filesystem, name)
	if err != nil {
		return fmt.Errorf("reading embedded CRD %s: %w", name, err)
	}

	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(content), yamlBufferSize)

	for {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := decoder.Decode(crd); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return fmt.Errorf("decoding CRD manifest %s: %w", name, err)
		}

		if crd.GetName() == "" {
			continue
		}

		if err := i.ensureCRD(ctx, crd); err != nil {
			return fmt.Errorf("ensuring CRD %s from %s: %w", crd.GetName(), name, err)
		}
	}

	return nil
}

func (i *Installer) ensureCRD(ctx context.Context, crd *apiextensionsv1.CustomResourceDefinition) error {
	logger := i.log.WithField("crd", crd.GetName())

	_, err := i.client.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, crd, metav1.CreateOptions{})
	if err == nil {
		logger.Info("CRD installed")
		return nil
	}

	if apierrors.IsAlreadyExists(err) {
		logger.Debug("CRD already exists")
		return nil
	}

	return err
}
