package castai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	mock_auth "github.com/castai/castware-operator/internal/castai/auth/mock"
	"github.com/castai/castware-operator/internal/config"
	"github.com/golang/mock/gomock"
	"github.com/jarcoal/httpmock"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func TestClientMe(t *testing.T) {
	cfg := &config.Config{LogLevel: config.LogLevel(logrus.DebugLevel), RequestTimeout: time.Second * 10}

	t.Run("should call api when api key is available", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockAuth := mock_auth.NewMockAuth(ctrl)

		restyClient := NewRestyClient(cfg, "https://api.castai.test", mockAuth)
		httpmock.ActivateNonDefault(restyClient.GetClient())
		t.Cleanup(func() {
			httpmock.Deactivate()
		})
		client := NewClient(logrus.New(), cfg, restyClient)

		responder, err := httpmock.NewJsonResponder(200, json.RawMessage(`{"id": "9ba1d646-96a4-409e-a1d6-4696a4909e90", "username": "test"}`))
		r.NoError(err)
		httpmock.RegisterResponder("GET", "https://api.castai.test/v1/me", responder)

		mockAuth.EXPECT().ApiKey().Return("test-api-key")
		httpmock.HeaderIs("X-API-Key", "test-api-key")
		httpmock.HeaderIs("User-Agent", "castai-castware-operator/0")
		user, err := client.Me(ctx)
		r.NoError(err)
		r.NotNil(user)
	})

	t.Run("should return error when status code is not 200", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockAuth := mock_auth.NewMockAuth(ctrl)
		restyClient := NewRestyClient(cfg, "https://api.castai.test", mockAuth)
		httpmock.ActivateNonDefault(restyClient.GetClient())
		t.Cleanup(func() {
			httpmock.Deactivate()
		})
		client := NewClient(logrus.New(), cfg, restyClient)

		responder, err := httpmock.NewJsonResponder(401, json.RawMessage(`{}`))
		r.NoError(err)
		httpmock.RegisterResponder("GET", "https://api.castai.test/v1/me", responder)

		mockAuth.EXPECT().ApiKey().Return("test-api-key")
		httpmock.HeaderIs("X-API-Key", "test-api-key")
		httpmock.HeaderIs("User-Agent", "castai-castware-operator/0")
		user, err := client.Me(ctx)
		r.Error(err)
		r.Nil(user)
	})

	t.Run("should return error when api key is not available", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockAuth := mock_auth.NewMockAuth(ctrl)
		restyClient := NewRestyClient(cfg, "https://api.castai.test", mockAuth)
		httpmock.ActivateNonDefault(restyClient.GetClient())
		t.Cleanup(func() {
			httpmock.Deactivate()
		})
		client := NewClient(logrus.New(), cfg, restyClient)

		mockAuth.EXPECT().ApiKey().Return("")

		user, err := client.Me(ctx)
		r.ErrorIs(err, ErrNoApiKey)
		r.Nil(user)
	})

}

func TestClientGetComponentByName(t *testing.T) {
	cfg := &config.Config{LogLevel: config.LogLevel(logrus.DebugLevel), RequestTimeout: time.Second * 10}

	t.Run("should call api when api key is available", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockAuth := mock_auth.NewMockAuth(ctrl)

		restyClient := NewRestyClient(cfg, "https://api.castai.test", mockAuth)
		httpmock.ActivateNonDefault(restyClient.GetClient())
		t.Cleanup(func() {
			httpmock.Deactivate()
		})
		client := NewClient(logrus.New(), cfg, restyClient)

		responder, err := httpmock.NewJsonResponder(200, json.RawMessage(`{
			"id": "comp-123",
			"name": "test-component",
			"helmChart": "test/chart",
			"dependencies": ["dep1", "dep2"],
			"latestVersion": "1.2.3"
		}`))
		r.NoError(err)
		httpmock.RegisterResponder("GET", "https://api.castai.test/cluster-management/v1/components:getByName?name=test-component", responder)

		mockAuth.EXPECT().ApiKey().Return("test-api-key")
		httpmock.HeaderIs("X-API-Key", "test-api-key")
		httpmock.HeaderIs("User-Agent", "castai-castware-operator/0")
		component, err := client.GetComponentByName(ctx, "test-component")
		r.NoError(err)
		r.NotNil(component)
		r.Equal("comp-123", component.Id)
		r.Equal("test-component", component.Name)
		r.Equal("test/chart", component.HelmChart)
		r.Equal([]string{"dep1", "dep2"}, component.Dependencies)
		r.Equal("1.2.3", component.LatestVersion)
	})

	t.Run("should return ErrNotFound when status code is 404", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockAuth := mock_auth.NewMockAuth(ctrl)
		restyClient := NewRestyClient(cfg, "https://api.castai.test", mockAuth)
		httpmock.ActivateNonDefault(restyClient.GetClient())
		t.Cleanup(func() {
			httpmock.Deactivate()
		})
		client := NewClient(logrus.New(), cfg, restyClient)

		responder, err := httpmock.NewJsonResponder(404, json.RawMessage(`{}`))
		r.NoError(err)
		httpmock.RegisterResponder("GET", "https://api.castai.test/cluster-management/v1/components:getByName?name=nonexistent-component", responder)

		mockAuth.EXPECT().ApiKey().Return("test-api-key")
		httpmock.HeaderIs("X-API-Key", "test-api-key")
		httpmock.HeaderIs("User-Agent", "castai-castware-operator/0")
		component, err := client.GetComponentByName(ctx, "nonexistent-component")
		r.ErrorIs(err, ErrNotFound)
		r.Nil(component)
	})

	t.Run("should return error when status code is not 200 or 404", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockAuth := mock_auth.NewMockAuth(ctrl)
		restyClient := NewRestyClient(cfg, "https://api.castai.test", mockAuth)
		httpmock.ActivateNonDefault(restyClient.GetClient())
		t.Cleanup(func() {
			httpmock.Deactivate()
		})
		client := NewClient(logrus.New(), cfg, restyClient)

		responder, err := httpmock.NewJsonResponder(500, json.RawMessage(`{}`))
		r.NoError(err)
		httpmock.RegisterResponder("GET", "https://api.castai.test/cluster-management/v1/components:getByName?name=test-component", responder)

		mockAuth.EXPECT().ApiKey().Return("test-api-key")
		httpmock.HeaderIs("X-API-Key", "test-api-key")
		httpmock.HeaderIs("User-Agent", "castai-castware-operator/0")
		component, err := client.GetComponentByName(ctx, "test-component")
		r.Error(err)
		r.Nil(component)
	})

	t.Run("should return error when api key is not available", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockAuth := mock_auth.NewMockAuth(ctrl)
		restyClient := NewRestyClient(cfg, "https://api.castai.test", mockAuth)
		httpmock.ActivateNonDefault(restyClient.GetClient())
		t.Cleanup(func() {
			httpmock.Deactivate()
		})
		client := NewClient(logrus.New(), cfg, restyClient)

		mockAuth.EXPECT().ApiKey().Return("")

		component, err := client.GetComponentByName(ctx, "test-component")
		r.ErrorIs(err, ErrNoApiKey)
		r.Nil(component)
	})
}

func TestClientRecordActionResult(t *testing.T) {
	cfg := &config.Config{LogLevel: config.LogLevel(logrus.DebugLevel), RequestTimeout: time.Second * 10}

	t.Run("should call api when api key is available", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockAuth := mock_auth.NewMockAuth(ctrl)

		restyClient := NewRestyClient(cfg, "https://api.castai.test", mockAuth)
		httpmock.ActivateNonDefault(restyClient.GetClient())
		t.Cleanup(func() {
			httpmock.Deactivate()
		})
		client := NewClient(logrus.New(), cfg, restyClient)

		responder, err := httpmock.NewJsonResponder(200, json.RawMessage(`{}`))
		r.NoError(err)
		httpmock.RegisterResponder("POST", "https://api.castai.test/cluster-management/v1/clusters/test-cluster/components:recordActionResult", responder)

		mockAuth.EXPECT().ApiKey().Return("test-api-key")
		httpmock.HeaderIs("X-API-Key", "test-api-key")
		httpmock.HeaderIs("User-Agent", "castai-castware-operator/0")

		actionResult := &ComponentActionResult{
			Name:           "test-component",
			Action:         Action_UPGRADE,
			CurrentVersion: "1.0.0",
			Version:        "1.1.0",
			Status:         Status_OK,
			ReleaseName:    "test-release",
			Message:        "Successfully updated",
		}

		err = client.RecordActionResult(ctx, "test-cluster", actionResult)
		r.NoError(err)
	})

	t.Run("should return ErrNotFound when status code is 404", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockAuth := mock_auth.NewMockAuth(ctrl)
		restyClient := NewRestyClient(cfg, "https://api.castai.test", mockAuth)
		httpmock.ActivateNonDefault(restyClient.GetClient())
		t.Cleanup(func() {
			httpmock.Deactivate()
		})
		client := NewClient(logrus.New(), cfg, restyClient)

		responder, err := httpmock.NewJsonResponder(404, json.RawMessage(`{}`))
		r.NoError(err)
		httpmock.RegisterResponder("POST", "https://api.castai.test/cluster-management/v1/clusters/nonexistent-cluster/components:recordActionResult", responder)

		mockAuth.EXPECT().ApiKey().Return("test-api-key")
		httpmock.HeaderIs("X-API-Key", "test-api-key")
		httpmock.HeaderIs("User-Agent", "castai-castware-operator/0")

		actionResult := &ComponentActionResult{
			Name:   "test-component",
			Action: Action_UPGRADE,
		}

		err = client.RecordActionResult(ctx, "nonexistent-cluster", actionResult)
		r.ErrorIs(err, ErrNotFound)
	})

	t.Run("should return error when status code is not 200 or 404", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockAuth := mock_auth.NewMockAuth(ctrl)
		restyClient := NewRestyClient(cfg, "https://api.castai.test", mockAuth)
		httpmock.ActivateNonDefault(restyClient.GetClient())
		t.Cleanup(func() {
			httpmock.Deactivate()
		})
		client := NewClient(logrus.New(), cfg, restyClient)

		responder, err := httpmock.NewJsonResponder(500, json.RawMessage(`{}`))
		r.NoError(err)
		httpmock.RegisterResponder("POST", "https://api.castai.test/cluster-management/v1/clusters/test-cluster/components:recordActionResult", responder)

		mockAuth.EXPECT().ApiKey().Return("test-api-key")
		httpmock.HeaderIs("X-API-Key", "test-api-key")
		httpmock.HeaderIs("User-Agent", "castai-castware-operator/0")

		actionResult := &ComponentActionResult{
			Name:   "test-component",
			Action: Action_UPGRADE,
		}

		err = client.RecordActionResult(ctx, "test-cluster", actionResult)
		r.Error(err)
	})

	t.Run("should return error when api key is not available", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockAuth := mock_auth.NewMockAuth(ctrl)
		restyClient := NewRestyClient(cfg, "https://api.castai.test", mockAuth)
		httpmock.ActivateNonDefault(restyClient.GetClient())
		t.Cleanup(func() {
			httpmock.Deactivate()
		})
		client := NewClient(logrus.New(), cfg, restyClient)

		mockAuth.EXPECT().ApiKey().Return("")

		actionResult := &ComponentActionResult{
			Name:   "test-component",
			Action: Action_UPGRADE,
		}

		err := client.RecordActionResult(ctx, "test-cluster", actionResult)
		r.ErrorIs(err, ErrNoApiKey)
	})
}

func TestClientValidateComponentInstall(t *testing.T) {
	cfg := &config.Config{LogLevel: config.LogLevel(logrus.DebugLevel), RequestTimeout: time.Second * 10}
	req := &ValidateComponentInstallRequest{
		ClusterID:     "test-cluster",
		ComponentName: "castai-umbrella",
		TargetVersion: "1.0.0",
		ComponentParams: map[string]any{
			"tags": map[string]any{"readonly": true},
		},
	}

	t.Run("should return allowed when api allows the install", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockAuth := mock_auth.NewMockAuth(ctrl)

		restyClient := NewRestyClient(cfg, "https://api.castai.test", mockAuth)
		httpmock.ActivateNonDefault(restyClient.GetClient())
		t.Cleanup(func() {
			httpmock.Deactivate()
		})
		client := NewClient(logrus.New(), cfg, restyClient)

		// Capture the request body so the test locks in the payload the server's
		// permission check consumes (component_params selects the required RBAC
		// condition sets).
		var body []byte
		httpmock.RegisterResponder("POST", "https://api.castai.test/cluster-management/v1/clusters/test-cluster/components:validateInstallation",
			func(r *http.Request) (*http.Response, error) {
				body, _ = io.ReadAll(r.Body)
				resp := httpmock.NewStringResponse(200, `{"allowed": true}`)
				resp.Header.Set("Content-Type", "application/json")
				return resp, nil
			})

		mockAuth.EXPECT().ApiKey().Return("test-api-key")
		httpmock.HeaderIs("X-API-Key", "test-api-key")
		httpmock.HeaderIs("User-Agent", "castware-castware-operator/0")

		resp, err := client.ValidateComponentInstall(ctx, req)
		r.NoError(err)
		r.NotNil(resp)
		r.True(resp.Allowed)
		r.Empty(resp.BlockReason)

		var sent map[string]any
		r.NoError(json.Unmarshal(body, &sent))
		r.Equal("test-cluster", sent["cluster_id"])
		r.Equal("castai-umbrella", sent["component_name"])
		r.Equal("1.0.0", sent["target_version"])
		r.Contains(sent, "component_params")
		params, ok := sent["component_params"].(map[string]any)
		r.True(ok, "component_params should be an object")
		tags, ok := params["tags"].(map[string]any)
		r.True(ok, "component_params.tags should be an object")
		r.Equal(true, tags["readonly"])
	})

	t.Run("should return blocked with block reason when api denies the install", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockAuth := mock_auth.NewMockAuth(ctrl)

		restyClient := NewRestyClient(cfg, "https://api.castai.test", mockAuth)
		httpmock.ActivateNonDefault(restyClient.GetClient())
		t.Cleanup(func() {
			httpmock.Deactivate()
		})
		client := NewClient(logrus.New(), cfg, restyClient)

		// The API denies with a 4xx carrying a validation error body; the client
		// folds it into Allowed=false + BlockReason instead of returning an error.
		responder, err := httpmock.NewJsonResponder(400, json.RawMessage(`{"allowed": false, "blockReason": "service account lacks permissions for the umbrella chart"}`))
		r.NoError(err)
		httpmock.RegisterResponder("POST", "https://api.castai.test/cluster-management/v1/clusters/test-cluster/components:validateInstallation", responder)

		mockAuth.EXPECT().ApiKey().Return("test-api-key")
		httpmock.HeaderIs("X-API-Key", "test-api-key")
		httpmock.HeaderIs("User-Agent", "castware-castware-operator/0")

		resp, err := client.ValidateComponentInstall(ctx, req)
		r.NoError(err, "a denial is a result, not a transport error")
		r.NotNil(resp)
		r.False(resp.Allowed)
		r.Equal("service account lacks permissions for the umbrella chart", resp.BlockReason)
	})

	t.Run("should return error when the request fails at the transport level", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockAuth := mock_auth.NewMockAuth(ctrl)

		restyClient := NewRestyClient(cfg, "https://api.castai.test", mockAuth)
		httpmock.ActivateNonDefault(restyClient.GetClient())
		t.Cleanup(func() {
			httpmock.Deactivate()
		})
		client := NewClient(logrus.New(), cfg, restyClient)

		// No responder registered for this cluster: the request fails at the
		// transport level (the only path that returns an error). A response that
		// arrives but is not a clean verdict — including an unparseable 500 body —
		// is folded into Allowed=false + BlockReason, the same semantics as
		// ValidateComponentUpgrade.
		transportReq := &ValidateComponentInstallRequest{
			ClusterID:     "unreachable-cluster",
			ComponentName: req.ComponentName,
			TargetVersion: req.TargetVersion,
		}

		mockAuth.EXPECT().ApiKey().Return("test-api-key")

		resp, err := client.ValidateComponentInstall(ctx, transportReq)
		r.Error(err)
		r.Nil(resp)
	})

	t.Run("should return error when api key is not available", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockAuth := mock_auth.NewMockAuth(ctrl)

		restyClient := NewRestyClient(cfg, "https://api.castai.test", mockAuth)
		httpmock.ActivateNonDefault(restyClient.GetClient())
		t.Cleanup(func() {
			httpmock.Deactivate()
		})
		client := NewClient(logrus.New(), cfg, restyClient)

		mockAuth.EXPECT().ApiKey().Return("")

		resp, err := client.ValidateComponentInstall(ctx, req)
		r.ErrorIs(err, ErrNoApiKey)
		r.Nil(resp)
	})
}
