package main

import (
	"os"

	"github.com/castai/castware-operator/internal/castai"
	"github.com/castai/castware-operator/internal/config"
	"github.com/spf13/cobra"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
	version  config.CastwareOperatorVersion
)

// These should be set via `go build` during a release.
var (
	GitCommit = "undefined"
	GitRef    = "no-ref"
	Version   = "local"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(castwarev1alpha1.AddToScheme(scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func newRootCmd() *cobra.Command {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool

	rootCmd := &cobra.Command{
		Use:   "castware-operator",
		Short: "Cast AI component lifecycle management",
		Long:  "Automates the installation, updates, and version management of Cast AI components in your cluster.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOperator(operatorArgs{
				metricsAddr:          metricsAddr,
				probeAddr:            probeAddr,
				metricsCertPath:      metricsCertPath,
				metricsCertName:      metricsCertName,
				metricsCertKey:       metricsCertKey,
				enableLeaderElection: enableLeaderElection,
				secureMetrics:        secureMetrics,
				enableHTTP2:          enableHTTP2,
			})
		},
	}

	rootCmd.Flags().StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	rootCmd.Flags().StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	rootCmd.Flags().BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	rootCmd.Flags().BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	rootCmd.Flags().StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	rootCmd.Flags().StringVar(&metricsCertName, "metrics-cert-name", "tls.crt",
		"The name of the metrics server certificate file.")
	rootCmd.Flags().StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	rootCmd.Flags().BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")

	return rootCmd
}

func main() {
	rootCmd := newRootCmd()
	rootCmd.AddCommand(newUpgradeCmd())
	rootCmd.AddCommand(newCleanupCmd())

	version = config.CastwareOperatorVersion{
		GitCommit: GitCommit,
		GitRef:    GitRef,
		Version:   Version,
	}
	castai.SetVersion(version)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
