//go:generate mockgen -source ./client.go -destination ./mock/client.go . Client

package helm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/castai/castware-operator/internal/utils"
	"github.com/sirupsen/logrus"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	memorycached "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// TODO: move?
type ChartSource struct {
	RepoURL string `json:"repoUrl"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

type InstallOptions struct {
	ChartSource     *ChartSource
	Namespace       string
	CreateNamespace bool
	ReleaseName     string
	ValuesOverrides map[string]interface{}
	DryRun          bool
}

type UninstallOptions struct {
	Namespace      string
	ReleaseName    string
	IgnoreNotFound bool
	Wait           bool
}

type UpgradeOptions struct {
	ChartSource          *ChartSource
	Release              *release.Release
	ValuesOverrides      map[string]interface{}
	MaxHistory           int
	ResetThenReuseValues bool
	DryRun               bool
	Install              bool
}

type GetReleaseOptions struct {
	Namespace   string
	ReleaseName string
	// Version is the helm release version, not the chart version. Setting it to 0 will get the last version.
	Version int
}

type RollbackOptions struct {
	Namespace   string
	ReleaseName string
}

type ForgetReleaseOptions struct {
	Namespace   string
	ReleaseName string
}

type ListReleasesOptions struct {
	Namespace string
}

func NewClient(log logrus.FieldLogger, loader ChartLoader, restConfig *rest.Config) Client {
	return &client{
		log: log,
		configurationGetter: &configurationGetter{
			log:        log,
			debug:      false,
			helmDriver: "secrets",
			k8sConfig:  restConfig,
		},
		chartLoader: loader,
	}
}

type Client interface {
	Install(ctx context.Context, opts InstallOptions) (*release.Release, error)
	Uninstall(opts UninstallOptions) (*release.UninstallReleaseResponse, error)
	Upgrade(ctx context.Context, opts UpgradeOptions) (*release.Release, error)
	Rollback(opts RollbackOptions) error
	GetRelease(opts GetReleaseOptions) (*release.Release, error)
	// ForgetRelease removes a release from Helm's storage (the driver's
	// records) without uninstalling any cluster resources. The resources
	// remain installed and adoptable by another release (the umbrella install
	// does this via action.Install.TakeOwnership). It is the safe counterpart
	// to Uninstall for the individual agent release during migration: the
	// agent's running pods are the cluster's liveness signal to Mothership,
	// so deleting them (which Uninstall would do) is avoided. Returns nil if
	// the release is not found in storage.
	ForgetRelease(opts ForgetReleaseOptions) error
	// ListReleases returns every release record in Helm storage for the
	// namespace, in any state (deployed, failed, pending, superseded, and
	// uninstalled records retained via --keep-history). The state breadth
	// matches GetRelease, which returns the latest revision regardless of
	// status, so a retained uninstalled record reads as "present" here too —
	// the fail-safe choice for conflict probing.
	ListReleases(opts ListReleasesOptions) ([]*release.Release, error)
}

type client struct {
	log                 logrus.FieldLogger
	configurationGetter ConfigurationGetter
	chartLoader         ChartLoader
}

func (c *client) Install(ctx context.Context, opts InstallOptions) (*release.Release, error) {
	ch, err := c.chartLoader.Load(ctx, opts.ChartSource)
	if err != nil {
		return nil, err
	}

	if req := ch.Metadata.Dependencies; req != nil {
		if err := action.CheckDependencies(ch, req); err != nil {
			return nil, err
		}
	}

	namespace := opts.Namespace
	cfg, err := c.configurationGetter.Get(namespace)
	if err != nil {
		return nil, err
	}

	install := action.NewInstall(cfg)
	install.Namespace = namespace
	install.CreateNamespace = opts.CreateNamespace
	install.ReleaseName = opts.ReleaseName
	install.Timeout = 10 * time.Minute
	install.TakeOwnership = true
	install.DryRun = opts.DryRun

	// Prepare user value overrides.
	values := map[string]interface{}{}
	if err := utils.MergeMaps(values, opts.ValuesOverrides); err != nil {
		return nil, err
	}

	res, err := install.Run(ch, values)
	if err != nil {
		return nil, fmt.Errorf("running chart install, name=%q: %w", ch.Name(), err)
	}
	return res, err
}

func (c *client) Uninstall(opts UninstallOptions) (*release.UninstallReleaseResponse, error) {
	cfg, err := c.configurationGetter.Get(opts.Namespace)
	if err != nil {
		return nil, err
	}

	uninstall := action.NewUninstall(cfg)
	uninstall.IgnoreNotFound = opts.IgnoreNotFound
	uninstall.Wait = opts.Wait
	res, err := uninstall.Run(opts.ReleaseName)
	if err != nil {
		return nil, fmt.Errorf("chart uninstall failed, name=%s, namespace=%s: %w", opts.ReleaseName, opts.Namespace, err)
	}
	return res, nil
}

func (c *client) Upgrade(ctx context.Context, opts UpgradeOptions) (*release.Release, error) {
	ch, err := c.chartLoader.Load(ctx, opts.ChartSource)
	if err != nil {
		return nil, err
	}

	if req := ch.Metadata.Dependencies; req != nil {
		if err := action.CheckDependencies(ch, req); err != nil {
			return nil, err
		}
	}

	namespace := opts.Release.Namespace
	cfg, err := c.configurationGetter.Get(namespace)
	if err != nil {
		return nil, err
	}

	upgrade := action.NewUpgrade(cfg)
	upgrade.Namespace = namespace
	upgrade.MaxHistory = opts.MaxHistory
	upgrade.DryRun = opts.DryRun
	// upgrade.PostRenderer = hook.NewLabelIgnoreHook(cfg.KubeClient, opts.Release)
	upgrade.ResetThenReuseValues = opts.ResetThenReuseValues
	upgrade.Install = opts.Install
	name := opts.Release.Name

	// Prepare user value overrides.
	values := map[string]interface{}{}
	if len(opts.Release.Config) > 0 {
		values = opts.Release.Config
	}
	if err := utils.MergeMaps(values, opts.ValuesOverrides); err != nil {
		return nil, err
	}

	res, err := upgrade.Run(name, ch, values)
	if err != nil {
		return nil, fmt.Errorf("running chart upgrade, name=%s: %w", name, err)
	}
	return res, nil
}

func (c *client) Rollback(opts RollbackOptions) error {
	cfg, err := c.configurationGetter.Get(opts.Namespace)
	if err != nil {
		return err
	}

	rollback := action.NewRollback(cfg)
	rollback.Recreate = true
	err = rollback.Run(opts.ReleaseName)
	if err != nil {
		return fmt.Errorf("chart rollback failed, name=%s, namespace=%s: %w", opts.ReleaseName, opts.Namespace, err)
	}
	return nil
}

func (c *client) GetRelease(opts GetReleaseOptions) (*release.Release, error) {
	cfg, err := c.configurationGetter.Get(opts.Namespace)
	if err != nil {
		return nil, err
	}

	list := action.NewGet(cfg)
	list.Version = opts.Version
	rel, err := list.Run(opts.ReleaseName)
	if err != nil {
		return nil, fmt.Errorf("getting chart release, name=%s, namespace=%s: %w", opts.ReleaseName, opts.Namespace, err)
	}
	return rel, nil
}

// ForgetRelease deletes the release record(s) from Helm's storage driver
// (secret-backed by default) without touching any cluster resources it
// rendered. It is used by the migration controller to retire the individual
// agent release after the umbrella has adopted its resources via
// TakeOwnership — uninstalling the agent release would delete the agent pods
// (the cluster's Mothership liveness signal), so the release is "forgotten"
// instead. Idempotent: a missing release is not an error.
func (c *client) ListReleases(opts ListReleasesOptions) ([]*release.Release, error) {
	cfg, err := c.configurationGetter.Get(opts.Namespace)
	if err != nil {
		return nil, err
	}

	list := action.NewList(cfg)
	list.All = true
	rels, err := list.Run()
	if err != nil {
		return nil, fmt.Errorf("listing helm releases, namespace=%s: %w", opts.Namespace, err)
	}
	return rels, nil
}

func (c *client) ForgetRelease(opts ForgetReleaseOptions) error {
	cfg, err := c.configurationGetter.Get(opts.Namespace)
	if err != nil {
		return err
	}

	// Helm's storage driver stores one record per revision. A release that was
	// installed and upgraded N times has N records (revisions 1..N) plus
	// possibly an uninstalled-history tail. List every record and delete the
	// ones whose name matches, so no trace of the individual release remains
	// for the mutual-exclusivity gate to detect (the gate probes via
	// GetRelease, which reads storage).
	//
	// Storage.Delete returns driver.ErrReleaseNotFound when a revision is gone;
	// treat that as success (idempotent forget). Other errors surface.
	rels, err := cfg.Releases.ListReleases()
	if err != nil {
		if errors.Is(err, driver.ErrReleaseNotFound) {
			return nil
		}
		return fmt.Errorf("listing helm releases for forget, namespace=%s: %w", opts.Namespace, err)
	}

	deleted := false
	for _, rel := range rels {
		if rel.Name != opts.ReleaseName {
			continue
		}
		if _, err := cfg.Releases.Delete(rel.Name, rel.Version); err != nil {
			if errors.Is(err, driver.ErrReleaseNotFound) {
				continue
			}
			return fmt.Errorf("forgetting helm release revision %d, name=%s, namespace=%s: %w", rel.Version, opts.ReleaseName, opts.Namespace, err)
		}
		deleted = true
	}
	if !deleted {
		c.log.Debugf("forgetRelease: no helm records found for %q in %s (already forgotten)", opts.ReleaseName, opts.Namespace)
	}
	return nil
}

// ConfigurationGetter wraps helm actions configuration setup for mocking in unit tests.
type ConfigurationGetter interface {
	Get(namespace string) (*action.Configuration, error)
}

type configurationGetter struct {
	log        logrus.FieldLogger
	debug      bool
	helmDriver string
	k8sConfig  *rest.Config
}

func (c *configurationGetter) Get(namespace string) (*action.Configuration, error) {
	cfg := &action.Configuration{}
	rcg := &restClientGetter{
		config:    c.k8sConfig,
		namespace: namespace,
	}
	err := cfg.Init(rcg, namespace, c.helmDriver, c.debugFuncf)
	if err != nil {
		return nil, fmt.Errorf("helm action config init: %w", err)
	}
	cfg.Log = func(s string, i ...interface{}) {
		c.log.Infof(s, i...)
	}
	return cfg, nil
}

func (c *configurationGetter) debugFuncf(format string, v ...interface{}) {
	if c.debug {
		c.log.Debug(fmt.Sprintf(format, v...))
	}
}

type restClientGetter struct {
	config    *rest.Config
	namespace string
}

func (r *restClientGetter) ToRESTConfig() (*rest.Config, error) {
	return r.config, nil
}

func (r *restClientGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	// The more groups you have, the more discovery requests you need to make.
	// given 25 groups (our groups + a few custom resources) with one-ish version each, discovery needs to make 50 requests
	// double it just so we don't end up here again for a while.  This config is only used for discovery.
	config := *r.config
	config.Burst = 100

	clientset, err := kubernetes.NewForConfig(&config)
	if err != nil {
		return nil, err
	}
	return memorycached.NewMemCacheClient(clientset.Discovery()), nil
}

func (r *restClientGetter) ToRESTMapper() (meta.RESTMapper, error) {
	discoveryClient, err := r.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}

	mapper := restmapper.NewDeferredDiscoveryRESTMapper(discoveryClient)
	expander := restmapper.NewShortcutExpander(mapper, discoveryClient, nil)
	return expander, nil
}

func (r *restClientGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	return &fakeClientConfig{
		config:    r.config,
		namespace: r.namespace,
	}
}

// fakeClientConfig is used to inject Helm required interface dependency. Helm uses only Namespace() method from ClientConfig.
// There's no straight forward way to build clientcmd.ClientConfig from k8s restConfig hence using fake one.
type fakeClientConfig struct {
	config    *rest.Config
	namespace string
}

func (f *fakeClientConfig) RawConfig() (api.Config, error) {
	return api.Config{}, nil
}

func (f *fakeClientConfig) ClientConfig() (*rest.Config, error) {
	return f.config, nil
}

func (f *fakeClientConfig) Namespace() (string, bool, error) {
	return f.namespace, f.namespace != "", nil
}

func (f *fakeClientConfig) ConfigAccess() clientcmd.ConfigAccess {
	return &clientcmd.PathOptions{}
}
