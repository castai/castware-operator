//go:generate mockgen -destination ./mock/client.go . CastAIClient
package castai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/castai/castware-operator/internal/castai/auth"
	"github.com/castai/castware-operator/internal/config"
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

func NewRestyClient(config *config.Config, apiURL string, auth auth.Auth, version string) *resty.Client {

	client := resty.NewWithClient(&http.Client{
		Timeout: config.RequestTimeout,
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
	if config.LogLevel.Level() == logrus.TraceLevel {
		client.SetDebug(true)
	}

	return client
}

// toError converts an unsuccessful response to an error
func (c *Client) toError(resp *resty.Response) error {
	if resp.IsSuccess() {
		return nil
	}
	apiError := ApiError{}
	err := json.Unmarshal(resp.Body(), &apiError)
	if err != nil {
		return fmt.Errorf("error calling api %s: %d - %w", resp.Request.URL, resp.StatusCode(), err)
	}
	return fmt.Errorf("error calling api %s: %d - %s", resp.Request.URL, resp.StatusCode(), apiError.Message)
}

// Me gets profile for current user.
func (c *Client) Me(ctx context.Context) (*User, error) {
	resp, err := c.rest.R().SetContext(ctx).Get("/v1/me")
	if err != nil {
		return nil, err
	}
	err = c.toError(resp)
	if err != nil {
		return nil, err
	}
	user := &User{}
	err = json.Unmarshal(resp.Body(), user)
	if err != nil {
		return nil, err
	}
	return user, nil
}
