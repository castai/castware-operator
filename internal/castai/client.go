//go:generate mockgen -destination ./mock/client.go . CastAIClient
package castai

import (
	"castai-agent/pkg/castai"
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
	castai.Client
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
		return fmt.Errorf("error calling api %s: %d - %s", resp.Request.URL, resp.StatusCode(), string(resp.Body()))
	}
	return fmt.Errorf("error calling api %s: %d - %s", resp.Request.URL, resp.StatusCode(), apiError.Message)
}

// Me gets profile for current user.
func (c *Client) Me(ctx context.Context) (*User, error) {
	result := &User{}
	resp, err := c.rest.R().SetResult(result).SetContext(ctx).Get("/v1/me")
	if err != nil {
		return nil, err
	}
	err = c.toError(resp)
	if err != nil {
		return nil, err
	}

	return result, nil
}

// RegisterCluster registers a new cluster in the mothership and returns a cluster id,
// if the cluster already exists the id of the existing cluster is returned
func (c *Client) RegisterCluster(ctx context.Context, req *castai.RegisterClusterRequest) (*castai.RegisterClusterResponse, error) {
	result := &castai.RegisterClusterResponse{}
	resp, err := c.rest.R().SetBody(req).SetResult(result).SetContext(ctx).Post("/v1/kubernetes/external-clusters")
	if err != nil {
		return nil, err
	}
	err = c.toError(resp)
	if err != nil {
		return nil, err
	}

	return result, nil
}

// ExchangeAgentTelemetry is not supported in castware operator, this function is here only to implement castai.Client interface needed for cluster registration
func (c *Client) ExchangeAgentTelemetry(ctx context.Context, clusterID string, req *castai.AgentTelemetryRequest) (*castai.AgentTelemetryResponse, error) {
	return nil, errors.New("not supported")
}

// SendDelta is not supported in castware operator, this function is here only to implement castai.Client interface needed for cluster registration
func (c *Client) SendDelta(ctx context.Context, clusterID string, delta *castai.Delta) error {
	return errors.New("not supported")
}

// SendLogEvent is not supported in castware operator, this function is here only to implement castai.Client interface needed for cluster registration
func (c *Client) SendLogEvent(ctx context.Context, clusterID string, req *castai.IngestAgentLogsRequest) (*castai.IngestAgentLogsResponse, error) {
	return nil, errors.New("not supported")
}
