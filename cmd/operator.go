package main

import (
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/bombsimon/logrusr/v4"
	"github.com/castai/castware-operator/internal/castai"
	"github.com/castai/castware-operator/internal/config"
	"github.com/castai/castware-operator/internal/controller"
	"github.com/castai/castware-operator/internal/helm"
	"github.com/castai/castware-operator/internal/webhook/v1alpha1"
	"github.com/open-policy-agent/cert-controller/pkg/rotator"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

type operatorArgs struct {
	metricsAddr          string
	probeAddr            string
	metricsCertPath      string
	metricsCertName      string
	metricsCertKey       string
	enableLeaderElection bool
	secureMetrics        bool
	enableHTTP2          bool
}

func setupCertRotator(mgr manager.Manager, cfg *config.Config) (chan struct{}, error) {
	isReady := make(chan struct{})
	if !cfg.CertsRotation {
		close(isReady)
		return isReady, nil
	}
	webhooksCertName := fmt.Sprintf("%s-webhooks", cfg.ServiceName)
	dnsName := fmt.Sprintf("%s-webhook-service.%s.svc", cfg.ServiceName, cfg.PodNamespace)
	if os.Getenv("WEBHOOK_SERVICE_DNS_NAME") != "" {
		dnsName = os.Getenv("WEBHOOK_SERVICE_DNS_NAME")
	}
	err := rotator.AddRotator(mgr, &rotator.CertRotator{
		SecretKey: types.NamespacedName{
			Name:      cfg.CertsSecret,
			Namespace: cfg.PodNamespace,
		},
		CertDir:                cfg.CertDir,
		CAName:                 fmt.Sprintf("%v-ca", webhooksCertName),
		CAOrganization:         webhooksCertName,
		DNSName:                dnsName,
		IsReady:                isReady,
		RestartOnSecretRefresh: true,
		Webhooks: []rotator.WebhookInfo{
			{
				Name: fmt.Sprintf("%s-mutating-webhook-configuration", cfg.ServiceName),
				Type: rotator.Mutating,
			},
			{
				Name: fmt.Sprintf("%s-validating-webhook-configuration", cfg.ServiceName),
				Type: rotator.Validating,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("setting up cert rotation: %w", err)
	}

	return isReady, nil
}

// nolint:gocyclo
func runOperator(args operatorArgs) error {
	var tlsOpts []func(*tls.Config)

	cfg, err := config.GetFromEnvironment()
	if err != nil {
		logrus.StandardLogger().Fatalf("failed to get config from environment: %v", err)
	}

	logrus.StandardLogger().SetLevel(cfg.LogLevel.Level())
	log := logrus.StandardLogger().WithField("gitCommit", version.GitCommit).WithField("version", version.Version)

	castai.SetVersion(version)

	controllerruntime.SetLogger(logrusr.New(log))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !args.enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Create watchers for metrics and webhooks certificates
	var metricsCertWatcher *certwatcher.CertWatcher

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts

	var port int
	if portEnv := os.Getenv("WEBHOOK_PORT"); portEnv != "" {
		var err error
		if port, err = strconv.Atoi(portEnv); err != nil {
			setupLog.Error(err, "Failed to parse WEBHOOK_PORT environment variable")
			return fmt.Errorf("failed to parse WEBHOOK_PORT: %w", err)
		}
	}

	webhookServer := webhook.NewServer(webhook.Options{
		Port:    port,
		CertDir: cfg.CertDir,
		TLSOpts: webhookTLSOpts,
	})

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := server.Options{
		BindAddress:   args.metricsAddr,
		SecureServing: args.secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if args.secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	if len(args.metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", args.metricsCertPath,
			"metrics-cert-name", args.metricsCertName,
			"metrics-cert-key", args.metricsCertKey,
		)

		var err error
		metricsCertWatcher, err = certwatcher.New(
			filepath.Join(args.metricsCertPath, args.metricsCertName),
			filepath.Join(args.metricsCertPath, args.metricsCertKey),
		)
		if err != nil {
			setupLog.Error(err, "to initialize metrics certificate watcher", "error", err)
			os.Exit(1)
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	log.Info("Starting manager")
	restConfig := controllerruntime.GetConfigOrDie()

	// Create Kubernetes clientset for direct API access
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		setupLog.Error(err, "unable to create Kubernetes clientset")
		os.Exit(1)
	}

	mgr, err := controllerruntime.NewManager(restConfig, controllerruntime.Options{
		Logger:                 controllerruntime.Log,
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: args.probeAddr,
		LeaderElection:         args.enableLeaderElection,
		LeaderElectionID:       "efe8a6a1.cast.ai",
		Cache:                  cache.Options{},
		Client: client.Options{
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{
					&v1.Secret{},
				},
			},
		},
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}
	log.Info("Manager started")

	chartLoader := helm.NewChartLoader(log)
	helmClient := helm.NewClient(log, chartLoader, restConfig)

	// nolint:goconst
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		log.Info("Setting up webhook certificate")
		certReady, err := setupCertRotator(mgr, cfg)
		if err != nil {
			setupLog.Error(err, "unable to start cert rotator")
			os.Exit(1)
		}
		go func() {
			<-certReady
			log.Info("Starting webhook server")
			if err = v1alpha1.SetupClusterWebhookWithManager(mgr, log); err != nil {
				setupLog.Error(err, "unable to create webhook", "webhook", "Cluster")
				os.Exit(1)
			}
			if err = v1alpha1.SetupComponentWebhookWithManager(mgr, log, chartLoader, helmClient); err != nil {
				setupLog.Error(err, "unable to create webhook", "webhook", "Component")
				os.Exit(1)
			}
		}()
	} else {
		log.Warn("webhooks disabled")
	}

	if err = (&controller.ComponentReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		Config:     cfg,
		Log:        log,
		HelmClient: helmClient,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Component")
		os.Exit(1)
	}

	if err = (&controller.ClusterReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Config:      cfg,
		Log:         log,
		HelmClient:  helmClient,
		ChartLoader: chartLoader,
		RestConfig:  restConfig,
		Clientset:   clientset,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Cluster")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if metricsCertWatcher != nil {
		setupLog.Info("Adding metrics certificate watcher to manager")
		if err := mgr.Add(metricsCertWatcher); err != nil {
			setupLog.Error(err, "unable to add metrics certificate watcher to manager")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(controllerruntime.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		return fmt.Errorf("problem running manager: %w", err)
	}

	return nil
}
