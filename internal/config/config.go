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
	LogLevel       LogLevel      `envconfig:"LOG_LEVEL" default:"info"`
	RequestTimeout time.Duration `envconfig:"REQUEST_TIMEOUT" default:"10s"`
}

func GetFromEnvironment() (*Config, error) {
	cfg := &Config{}
	if err := envconfig.Process("", cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}
