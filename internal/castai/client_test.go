package castai

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	mock_auth "github.com/castai/castware-operator/internal/castai/auth/mock"
	"github.com/golang/mock/gomock"
	"github.com/jarcoal/httpmock"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func TestClientMe(t *testing.T) {

	t.Run("should call api when api key is available", func(t *testing.T) {
		r := require.New(t)
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		mockAuth := mock_auth.NewMockAuth(ctrl)
		restyClient := NewRestyClient("https://api.castai.test", logrus.TraceLevel, "0", mockAuth, time.Second*5)
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
		restyClient := NewRestyClient("https://api.castai.test", logrus.TraceLevel, "0", mockAuth, time.Second*5)
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
		restyClient := NewRestyClient("https://api.castai.test", logrus.TraceLevel, "0", mockAuth, time.Second*5)
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
