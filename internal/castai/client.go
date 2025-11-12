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
	"github.com/samber/lo"
	"github.com/sirupsen/logrus"
)

const (
	headerAPIKey    = "X-API-Key" // nolint: gosec
	headerUserAgent = "User-Agent"
)

var (
	ErrNoApiKey = errors.New("no api key")
	ErrNotFound = errors.New("not found")
	version     config.CastwareOperatorVersion
)

type CastAIClient interface {
	castai.Client
	Me(ctx context.Context) (*User, error)
	GetCluster(ctx context.Context, clusterID string) (*Cluster, error)
	GetComponentByName(ctx context.Context, name string) (*Component, error)
	RecordActionResult(ctx context.Context, clusterID string, req *ComponentActionResult) error
	PollActions(ctx context.Context, clusterID string) (*PollActionsResponse, error)
	AckAction(ctx context.Context, clusterID, actionID string, error error) error
	ValidateComponentUpgrade(ctx context.Context, req *ValidateComponentUpgradeRequest) (*ValidateComponentUpgradeResponse, error)
}
type Client struct {
	log    logrus.FieldLogger
	config *config.Config
	rest   *resty.Client
}

func SetVersion(v config.CastwareOperatorVersion) {
	version = v
}

func GetVersion() config.CastwareOperatorVersion {
	return version
}

// NewClient returns new Client for communicating with Cast AI.
func NewClient(log logrus.FieldLogger, config *config.Config, rest *resty.Client) CastAIClient {
	return &Client{
		log:    log,
		config: config,
		rest:   rest,
	}
}

// NewRestyClient returns a new authenticated rest client to send requests to the specified API.
func NewRestyClient(config *config.Config, apiURL string, auth auth.Auth) *resty.Client {

	client := resty.NewWithClient(&http.Client{})

	client.SetBaseURL(apiURL)
	client.OnBeforeRequest(func(c *resty.Client, r *resty.Request) error {
		apiKey := auth.ApiKey()
		if apiKey == "" {
			return ErrNoApiKey
		}
		c.SetHeader(headerAPIKey, apiKey)
		return nil
	})
	client.Header.Set(headerUserAgent, "castai-castware-operator/"+version.String())
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
	ctx, cancel := context.WithTimeout(ctx, c.config.RequestTimeout)
	defer cancel()
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

// GetCluster gets a cluster by id.
func (c *Client) GetCluster(ctx context.Context, clusterID string) (*Cluster, error) {
	ctx, cancel := context.WithTimeout(ctx, c.config.RequestTimeout)
	defer cancel()

	result := &Cluster{}
	resp, err := c.rest.R().
		SetResult(result).
		SetContext(ctx).
		SetPathParam("clusterId", clusterID).
		Get("/v1/kubernetes/external-clusters/{clusterId}")
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
	ctx, cancel := context.WithTimeout(ctx, c.config.RequestTimeout)
	defer cancel()

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

// GetComponentByName retrieves a component by its name.
func (c *Client) GetComponentByName(ctx context.Context, name string) (*Component, error) {
	ctx, cancel := context.WithTimeout(ctx, c.config.RequestTimeout)
	defer cancel()

	resp, err := c.rest.R().
		SetContext(ctx).
		SetQueryParam("name", name).
		Get("cluster-management/v1/components:getByName")
	if err != nil {
		return nil, err
	}
	err = c.toError(resp)
	if err != nil {
		if resp.StatusCode() == http.StatusNotFound {
			return nil, ErrNotFound
		}
		return nil, err
	}
	component := &Component{}
	err = json.Unmarshal(resp.Body(), component)
	if err != nil {
		return nil, err
	}
	return component, nil
}

// RecordActionResult recors the results of an action performed on a component.
func (c *Client) RecordActionResult(ctx context.Context, clusterID string, req *ComponentActionResult) error {
	ctx, cancel := context.WithTimeout(ctx, c.config.RequestTimeout)
	defer cancel()

	resp, err := c.rest.R().
		SetContext(ctx).
		SetPathParam("clusterId", clusterID).
		SetBody(req).
		Post("cluster-management/v1/clusters/{clusterId}/components:recordActionResult")
	if err != nil {
		return err
	}
	err = c.toError(resp)
	if err != nil {
		if resp.StatusCode() == http.StatusNotFound {
			return ErrNotFound
		}
		return err
	}
	return nil
}

func (c *Client) PollActions(ctx context.Context, clusterID string) (*PollActionsResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, c.config.PollActionsTimeout)
	defer cancel()

	result := &PollActionsResponse{}
	resp, err := c.rest.R().
		SetContext(ctx).
		SetResult(result).
		SetPathParam("clusterId", clusterID).
		SetQueryParam("max_actions", "10").
		Get("cluster-management/v1/clusters/{clusterId}/lifecycle-actions:poll")
	if err != nil {
		return nil, err
	}
	err = c.toError(resp)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Client) AckAction(ctx context.Context, clusterID, actionID string, error error) error {
	ctx, cancel := context.WithTimeout(ctx, c.config.RequestTimeout)
	defer cancel()

	req := &AckActionRequest{}
	if error != nil {
		req.Error = lo.ToPtr(error.Error())
	}
	resp, err := c.rest.R().
		SetContext(ctx).
		SetPathParam("clusterId", clusterID).
		SetPathParam("actionId", actionID).
		SetBody(req).
		Post("/cluster-management/v1/clusters/{clusterId}/lifecycle-actions/{actionId}:ack")
	if err != nil {
		return err
	}
	err = c.toError(resp)
	if err != nil {
		return err
	}
	return nil
}

func (c *Client) ValidateComponentUpgrade(ctx context.Context, req *ValidateComponentUpgradeRequest) (*ValidateComponentUpgradeResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, c.config.RequestTimeout)
	defer cancel()

	result := &ValidateComponentUpgradeResponse{}
	resp, err := c.rest.R().
		SetContext(ctx).
		SetResult(result).
		SetPathParam("clusterId", req.ClusterID).
		SetBody(req).
		Post("cluster-management/v1/clusters/{clusterId}/components:validateUpgrade")
	if err != nil {
		return nil, err
	}
	err = c.toError(resp)
	if err != nil {
		return nil, err
	}
	return result, nil
}
