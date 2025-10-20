package config

import (
	"time"

	"github.com/kelseyhightower/envconfig"
	"github.com/sirupsen/logrus"
)

type LogLevel logrus.Level

func (l *LogLevel) Decode(value string) error {
	lvl, err := logrus.ParseLevel(value)
	if err != nil {
		return err
	}
	*l = LogLevel(lvl)
	return nil
}

func (l LogLevel) Level() logrus.Level {
	return logrus.Level(l)
}

type Config struct {
	LogLevel                LogLevel      `envconfig:"LOG_LEVEL" default:"info"`
	RequestTimeout          time.Duration `envconfig:"REQUEST_TIMEOUT" default:"10s"`
	PollActionsTimeout      time.Duration `envconfig:"POLL_ACTIONS_TIMEOUT" default:"10m"`
	CertDir                 string        `envconfig:"CERTS_DIR" default:"/certs"`
	CertsSecret             string        `envconfig:"CERTS_SECRET" default:"castware-operator-certs"`
	PodNamespace            string        `envconfig:"POD_NAMESPACE"`
	ServiceName             string        `envconfig:"SERVICE_NAME" default:"castware-operator"`
	HelmReleaseName         string        `envconfig:"HELM_RELEASE_NAME" default:"castware-operator"`
	CertsRotation           bool          `envconfig:"CERTS_ROTATION" default:"false"`
	PodsReadyTimeout        time.Duration `envconfig:"PODS_READY_TIMEOUT" default:"5m"`
	PodsStatusCheckInterval time.Duration `envconfig:"PODS_STATUS_CHECK_INTERVAL" default:"5s"`
}

func GetFromEnvironment() (*Config, error) {
	cfg := &Config{}
	if err := envconfig.Process("", cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}
