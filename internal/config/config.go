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
	OperatorName            string        `envconfig:"OPERATOR_NAME" default:"castware-operator"`
	ServiceName             string        `envconfig:"SERVICE_NAME" default:"castware-operator"`
	HelmReleaseName         string        `envconfig:"HELM_RELEASE_NAME" default:"castware-operator"`
	CertsRotation           bool          `envconfig:"CERTS_ROTATION" default:"false"`
	PodsReadyTimeout        time.Duration `envconfig:"PODS_READY_TIMEOUT" default:"5m"`
	PodsStatusCheckInterval time.Duration `envconfig:"PODS_STATUS_CHECK_INTERVAL" default:"5s"`

	// Log ingestion ships the operator's structured logs to the CAST AI
	// ComponentsAPI.IngestLogs endpoint (POST /v1/clusters/{cluster_id}/components/{component}/logs)
	// via a logrus hook. Enabled by default; set LOG_INGEST_ENABLED=false to disable.
	LogIngestEnabled bool `envconfig:"LOG_INGEST_ENABLED" default:"true"`
	// LogIngestLevel is the minimum severity shipped to the ingest endpoint. It is a pointer so
	// that an unset value means "inherit the stdout logger level" rather than a fixed default.
	// A stricter level (e.g. "warn") than LOG_LEVEL reduces ingest volume without affecting stdout.
	LogIngestLevel          *string       `envconfig:"LOG_INGEST_LEVEL"`
	LogIngestBatchSize      int           `envconfig:"LOG_INGEST_BATCH_SIZE" default:"100"`
	LogIngestFlushInterval  time.Duration `envconfig:"LOG_INGEST_FLUSH_INTERVAL" default:"10s"`
	LogIngestRequestTimeout time.Duration `envconfig:"LOG_INGEST_REQUEST_TIMEOUT" default:"10s"`
}

// LogIngestLevelOr returns the configured ingest log level, or fallback when LOG_INGEST_LEVEL is
// unset. An invalid value also falls back so that a misconfiguration never silently drops all logs.
func (c *Config) LogIngestLevelOr(fallback logrus.Level) logrus.Level {
	if c.LogIngestLevel == nil || *c.LogIngestLevel == "" {
		return fallback
	}
	lvl, err := logrus.ParseLevel(*c.LogIngestLevel)
	if err != nil {
		return fallback
	}
	return lvl
}

func GetFromEnvironment() (*Config, error) {
	cfg := &Config{}
	if err := envconfig.Process("", cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}
