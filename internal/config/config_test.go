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
}
