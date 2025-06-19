//go:generate mockgen -destination ./mock/client.go . CastAIClient
package castai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/castai/castware-operator/internal/castai/auth"
	"github.com/go-resty/resty/v2"
	"github.com/sirupsen/logrus"
)

const (
	headerAPIKey    = "X-API-Key" // nolint: gosec
	headerUserAgent = "User-Agent"
)

var ErrNoApiKey = errors.New("no api key")

type CastAIClient interface {
	Me(ctx context.Context) (*User, error)
}
type Client struct {
	log  *logrus.Logger
	rest *resty.Client
}

// NewClient returns new Client for communicating with Cast AI.
func NewClient(log *logrus.Logger, rest *resty.Client) CastAIClient {
	return &Client{
		log:  log,
		rest: rest,
	}
}

func NewRestyClient(apiURL string, level logrus.Level, version string, auth auth.Auth, defaultTimeout time.Duration) *resty.Client {

	client := resty.NewWithClient(&http.Client{
		Timeout: defaultTimeout,
	})

	client.SetBaseURL(apiURL)
	client.OnBeforeRequest(func(c *resty.Client, r *resty.Request) error {
		apiKey := auth.ApiKey()
		if apiKey == "" {
			return ErrNoApiKey
		}
		c.SetHeader(headerAPIKey, apiKey)
		return nil
	})
	client.Header.Set(headerUserAgent, "castai-castware-operator/"+version)
	if level == logrus.TraceLevel {
		client.SetDebug(true)
	}

	return client
}

// Me gets profile for current user.
func (c *Client) Me(ctx context.Context) (*User, error) {
	resp, err := c.rest.R().SetContext(ctx).Get("/v1/me")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode() != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode())
	}
	user := &User{}
	err = json.Unmarshal(resp.Body(), user)
	if err != nil {
		return nil, err
	}
	return user, nil
}
