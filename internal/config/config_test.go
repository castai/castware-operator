package config

import (
	"os"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func TestGetFromEnvironment(t *testing.T) {

	t.Run("should set log level from env", func(t *testing.T) {
		r := require.New(t)
		r.NoError(os.Setenv("LOG_LEVEL", "debug"))
		cfg, err := GetFromEnvironment()
		r.NoError(err)
		r.Equal(logrus.DebugLevel, cfg.LogLevel.Level())
	})

	t.Run("should set timeout from env", func(t *testing.T) {
		r := require.New(t)
		r.NoError(os.Setenv("REQUEST_TIMEOUT", "5m"))
		cfg, err := GetFromEnvironment()
		r.NoError(err)
		r.Equal(time.Minute*5, cfg.RequestTimeout)
	})
	t.Run("should set polling timeout from env", func(t *testing.T) {
		r := require.New(t)
		r.NoError(os.Setenv("POLL_ACTIONS_TIMEOUT", "15m"))
		cfg, err := GetFromEnvironment()
		r.NoError(err)
		r.Equal(time.Minute*15, cfg.PollActionsTimeout)
	})

	t.Run("log ingest enabled by default", func(t *testing.T) {
		r := require.New(t)
		r.NoError(os.Unsetenv("LOG_INGEST_ENABLED"))
		cfg, err := GetFromEnvironment()
		r.NoError(err)
		r.True(cfg.LogIngestEnabled)
		r.Nil(cfg.LogIngestLevel)
	})

	t.Run("log ingest disabled via env", func(t *testing.T) {
		r := require.New(t)
		r.NoError(os.Setenv("LOG_INGEST_ENABLED", "false"))
		cfg, err := GetFromEnvironment()
		r.NoError(err)
		r.False(cfg.LogIngestEnabled)
	})

	t.Run("log ingest level unset falls back to stdout level", func(t *testing.T) {
		r := require.New(t)
		r.NoError(os.Unsetenv("LOG_INGEST_LEVEL"))
		cfg, err := GetFromEnvironment()
		r.NoError(err)
		r.Equal(logrus.WarnLevel, cfg.LogIngestLevelOr(logrus.WarnLevel))
	})

	t.Run("log ingest level set overrides fallback", func(t *testing.T) {
		r := require.New(t)
		r.NoError(os.Setenv("LOG_INGEST_LEVEL", "error"))
		cfg, err := GetFromEnvironment()
		r.NoError(err)
		r.Equal(logrus.ErrorLevel, cfg.LogIngestLevelOr(logrus.InfoLevel))
	})

	t.Run("log ingest level invalid falls back", func(t *testing.T) {
		r := require.New(t)
		r.NoError(os.Setenv("LOG_INGEST_LEVEL", "not-a-level"))
		cfg, err := GetFromEnvironment()
		r.NoError(err)
		r.Equal(logrus.InfoLevel, cfg.LogIngestLevelOr(logrus.InfoLevel))
	})
}

func TestLogVersion(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// Preserves a leading "v".
	r.Equal("castware-operator/v0.9.2", (&CastwareOperatorVersion{Version: "v0.9.2"}).LogVersion())
	// Adds a leading "v" when missing.
	r.Equal("castware-operator/v0.9.2", (&CastwareOperatorVersion{Version: "0.9.2"}).LogVersion())
}
