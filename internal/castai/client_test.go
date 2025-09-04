package castai

import (
	"context"
	"encoding/json"
	"testing"

	mock_auth "github.com/castai/castware-operator/internal/castai/auth/mock"
	"github.com/castai/castware-operator/internal/config"
	"github.com/golang/mock/gomock"
	"github.com/jarcoal/httpmock"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func TestClientMe(t *testing.T) {
	cfg := &config.Config{LogLevel: config.LogLevel(logrus.DebugLevel)}

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
		client := NewClient(logrus.New(), restyClient)

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
		client := NewClient(logrus.New(), restyClient)

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
		client := NewClient(logrus.New(), restyClient)

		mockAuth.EXPECT().ApiKey().Return("")

		user, err := client.Me(ctx)
		r.ErrorIs(err, ErrNoApiKey)
		r.Nil(user)
	})

}

func TestClientGetComponentByName(t *testing.T) {
	cfg := &config.Config{LogLevel: config.LogLevel(logrus.DebugLevel)}

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
		client := NewClient(logrus.New(), restyClient)

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
		client := NewClient(logrus.New(), restyClient)

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
		client := NewClient(logrus.New(), restyClient)

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
		client := NewClient(logrus.New(), restyClient)

		mockAuth.EXPECT().ApiKey().Return("")

		component, err := client.GetComponentByName(ctx, "test-component")
		r.ErrorIs(err, ErrNoApiKey)
		r.Nil(component)
	})
}

func TestClientRecordActionResult(t *testing.T) {
	cfg := &config.Config{LogLevel: config.LogLevel(logrus.DebugLevel)}

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
		client := NewClient(logrus.New(), restyClient)

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
		client := NewClient(logrus.New(), restyClient)

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
		client := NewClient(logrus.New(), restyClient)

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
		client := NewClient(logrus.New(), restyClient)

		mockAuth.EXPECT().ApiKey().Return("")

		actionResult := &ComponentActionResult{
			Name:   "test-component",
			Action: Action_UPGRADE,
		}

		err := client.RecordActionResult(ctx, "test-cluster", actionResult)
		r.ErrorIs(err, ErrNoApiKey)
	})
}
