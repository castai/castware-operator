//go:generate mockgen -destination ./mock/auth.go . Auth
package auth

import (
	"context"
	"fmt"
	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sync"
)

type Auth interface {
	LoadApiKey(ctx context.Context, r client.Reader) error
	GetApiKey(ctx context.Context, r client.Reader) (string, error)
	ApiKey() string
}
type auth struct {
	clusterCR string
	namespace string
	apiKey    string
	lock      sync.RWMutex
}

func NewAuth(namespace, clusterCR string) Auth {
	return &auth{clusterCR: clusterCR, namespace: namespace}
}

// GetApiKey returns the api key extracted from the secret specified in cluster CR
func (a *auth) GetApiKey(ctx context.Context, r client.Reader) (string, error) {
	cluster := &castwarev1alpha1.Cluster{}
	err := r.Get(ctx, client.ObjectKey{Namespace: a.namespace, Name: a.clusterCR}, cluster)
	if err != nil {
		return "", err
	}
	apiKeySecret := &corev1.Secret{}
	err = r.Get(ctx, client.ObjectKey{Namespace: a.namespace, Name: cluster.Spec.APIKeySecret}, apiKeySecret)
	if err != nil {
		return "", err
	}
	apiKey, ok := apiKeySecret.Data["API_KEY"]
	if !ok {
		return "", fmt.Errorf("no API_KEY field found in secret")
	}

	// TODO: encode base64?
	return string(apiKey), nil
}

// LoadApiKey extracts the api key from the secret specified in cluster CR and loads it in the cache
func (a *auth) LoadApiKey(ctx context.Context, r client.Reader) error {
	a.lock.Lock()
	defer a.lock.Unlock()
	var err error
	// TODO: decode base64?
	a.apiKey, err = a.GetApiKey(ctx, r)
	if err != nil {
		return err
	}
	return nil
}

// ApiKey returns the api key stored in the cache
func (a *auth) ApiKey() string {
	a.lock.RLock()
	defer a.lock.RUnlock()
	return a.apiKey
}
