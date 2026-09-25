package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/castai/castware-operator/test/utils"
	//nolint:staticcheck
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// Constants from e2e_test.go
const clusterYaml = `apiVersion: castware.cast.ai/v1alpha1
kind: Cluster
metadata:
  name: %s
  namespace: %s
spec:
  provider: gke
  apiKeySecret: %s
  api:
    apiUrl: "%s"
  terraform: false
`

const componentYaml = `apiVersion: castware.cast.ai/v1alpha1
kind: Component
metadata:
  name: %s
  namespace: %s
spec:
  cluster: castai
  component: %s
  enabled: true
  values:
    additionalEnv:
      GKE_CLUSTER_NAME: castware-operator-e2e
      GKE_LOCATION: e2e
      GKE_PROJECT_ID: e2e
      GKE_REGION: e2e
`

// umbrellaComponentYaml is a fmt template for an umbrella Component CR. The
// last %s carries extra indented spec lines — e.g. "  readonly: true",
// "  migrate: true", "  releaseName: foo" or a "  values:" block with
// 4-space-indented children — and may be empty.
const umbrellaComponentYaml = `apiVersion: castware.cast.ai/v1alpha1
kind: Component
metadata:
  name: %s
  namespace: %s
spec:
  cluster: %s
  component: castai-umbrella
  enabled: true
%s
`

// component represents a Cast AI component
type component struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	UsedVersion   string `json:"usedVersion"`
	LatestVersion string `json:"latestVersion"`
}

// castAIComponentInfo is a component from the Cast AI component registry
type castAIComponentInfo struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	HelmChart     string   `json:"helmChart"`
	Dependencies  []string `json:"dependencies"`
	LatestVersion string   `json:"latestVersion"`
	ReleaseName   string   `json:"releaseName"`
}

// disablePreflightChecks forces PREFLIGHT_CHECKS=false in an onboarding script.
// It replaces an explicit PREFLIGHT_CHECKS=true assignment if present and also
// exports PREFLIGHT_CHECKS=false, because the script defaults to enabled via
// ${PREFLIGHT_CHECKS:-true} when no assignment exists.
func disablePreflightChecks(script string) string {
	script = strings.ReplaceAll(script, "PREFLIGHT_CHECKS=true", "PREFLIGHT_CHECKS=false")
	return "export PREFLIGHT_CHECKS=false\n" + script
}

// podReady checks if a pod line indicates the pod is ready
func podReady(line string) bool {
	return len(line) > 0 && (line[len(line)-4:] == "True" || line[len(line)-1:] == "T")
}

// ComponentHelper provides helper methods for component operations
type ComponentHelper struct {
	namespace string
}

func NewComponentHelper(ns string) *ComponentHelper {
	return &ComponentHelper{namespace: ns}
}

// GetCurrentVersion retrieves the current version of a component
func (h *ComponentHelper) GetCurrentVersion(componentName string) (string, error) {
	cmd := exec.Command("kubectl", "get", "component", componentName,
		"-n", h.namespace,
		"-o", "jsonpath={.status.currentVersion}")
	return utils.Run(cmd)
}

// VerifyVersion checks that a component has reached the expected version
func (h *ComponentHelper) VerifyVersion(g Gomega, componentName, expectedVersion string) {
	version, err := h.GetCurrentVersion(componentName)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to get component version")
	g.Expect(version).
		To(Equal(expectedVersion), fmt.Sprintf("Component %s version should be %s", componentName, expectedVersion))
}

// VerifyVersionIsSet checks that a component has any version set
func (h *ComponentHelper) VerifyVersionIsSet(g Gomega, componentName string) {
	version, err := h.GetCurrentVersion(componentName)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to get component CR")
	g.Expect(version).NotTo(BeEmpty(), "Version is not set")
}

// VerifyVersionChanged checks that a component version has changed from the previous version
func (h *ComponentHelper) VerifyVersionChanged(g Gomega, componentName, previousVersion string) {
	version, err := h.GetCurrentVersion(componentName)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to get component version")
	g.Expect(version).NotTo(BeEmpty(), "Version should be set")
	g.Expect(version).NotTo(Equal(previousVersion), "Version should have changed from previous version")
}

// PatchVersion updates the version of a component
func (h *ComponentHelper) PatchVersion(componentName, version string) error {
	patchJSON := fmt.Sprintf(`{"spec":{"version":"%s"}}`, version)
	cmd := exec.Command("kubectl", "patch", "component", componentName,
		"-n", h.namespace,
		"--type=merge",
		"-p", patchJSON)
	_, err := utils.Run(cmd)
	return err
}

// componentCondition mirrors one entry of a Component CR's status.conditions.
type componentCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// getComponentConditions fetches a component's status conditions, decoded
// into a typed slice so field comparisons are exact.
func (h *ComponentHelper) getComponentConditions(componentName string) ([]componentCondition, error) {
	cmd := exec.Command("kubectl", "get", "component", componentName,
		"-n", h.namespace,
		"-o", "jsonpath={.status.conditions}")
	output, err := utils.Run(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to get component status: %w", err)
	}

	if strings.TrimSpace(output) == "" {
		return nil, nil
	}
	var conditions []componentCondition
	if err := json.Unmarshal([]byte(output), &conditions); err != nil {
		return nil, fmt.Errorf("failed to parse component status conditions: %w", err)
	}
	return conditions, nil
}

// findComponentCondition returns the first condition with the given type and,
// when reason is non-empty, the given reason.
func findComponentCondition(conditions []componentCondition, conditionType, reason string) *componentCondition {
	for i := range conditions {
		if conditions[i].Type != conditionType {
			continue
		}
		if reason != "" && conditions[i].Reason != reason {
			continue
		}
		return &conditions[i]
	}
	return nil
}

// VerifyStatusCondition checks that a component has a specific status condition
func (h *ComponentHelper) VerifyStatusCondition(componentName, conditionType string) error {
	conditions, err := h.getComponentConditions(componentName)
	if err != nil {
		return err
	}
	if findComponentCondition(conditions, conditionType, "") == nil {
		return fmt.Errorf("component should have %s condition, got %+v", conditionType, conditions)
	}
	return nil
}

// CreateFromYAML creates a component CR from a YAML template
func (h *ComponentHelper) CreateFromYAML(componentName, componentType string, additionalYAML string) error {
	componentYAML := fmt.Sprintf(componentYaml, componentName, h.namespace, componentType)
	if additionalYAML != "" {
		componentYAML += additionalYAML
	}

	componentFile := filepath.Join("/tmp", fmt.Sprintf("%s-component.yaml", componentName))
	if err := os.WriteFile(componentFile, []byte(componentYAML), os.FileMode(0o644)); err != nil {
		return fmt.Errorf("failed to write component manifest: %w", err)
	}

	cmd := exec.Command("kubectl", "apply", "-f", componentFile)
	_, err := utils.Run(cmd)
	return err
}

// CreateUmbrellaFromYAML creates an umbrella Component CR from the YAML
// template. The extraSpecYAML argument carries additional indented spec
// lines ("  readonly: true", "  migrate: true", "  releaseName: foo" or a
// "  values:" block with 4-space-indented children) and may be empty. The
// returned error carries admission webhook denials in its message.
func (h *ComponentHelper) CreateUmbrellaFromYAML(componentName, clusterName, extraSpecYAML string) error {
	manifest := fmt.Sprintf(umbrellaComponentYaml, componentName, h.namespace, clusterName, extraSpecYAML)
	return h.ApplyYAML(fmt.Sprintf("%s-umbrella", componentName), manifest)
}

// ApplyYAML applies a manifest from a unique temporary file (removed after
// the attempt) and returns the kubectl error verbatim — its message carries
// admission webhook denials.
func (h *ComponentHelper) ApplyYAML(fileName, manifest string) error {
	file, err := os.CreateTemp("", fmt.Sprintf("%s-*.yaml", fileName))
	if err != nil {
		return fmt.Errorf("failed to create manifest file: %w", err)
	}
	//nolint:errcheck
	defer os.Remove(file.Name())

	if _, err := file.Write([]byte(manifest)); err != nil {
		_ = file.Close()
		return fmt.Errorf("failed to write manifest: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("failed to close manifest file: %w", err)
	}

	cmd := exec.Command("kubectl", "apply", "-f", file.Name())
	_, err = utils.Run(cmd)
	return err
}

// ListNames returns the names of all Component CRs in the namespace
func (h *ComponentHelper) ListNames() ([]string, error) {
	cmd := exec.Command("kubectl", "get", "components",
		"-n", h.namespace,
		"-o", "jsonpath={range .items[*]}{.metadata.name}{'\\n'}{end}")
	output, err := utils.Run(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to list components: %w", err)
	}
	return utils.GetNonEmptyLines(output), nil
}

// GetField retrieves an arbitrary field of a component CR via jsonpath
func (h *ComponentHelper) GetField(componentName, jsonpath string) (string, error) {
	cmd := exec.Command("kubectl", "get", "component", componentName,
		"-n", h.namespace,
		"-o", fmt.Sprintf("jsonpath=%s", jsonpath))
	return utils.Run(cmd)
}

// VerifySpecReadonly checks that a component's spec.readonly matches the
// expected value. An unset field matches an expected false.
func (h *ComponentHelper) VerifySpecReadonly(g Gomega, componentName string, expected bool) {
	value, err := h.GetField(componentName, "{.spec.readonly}")
	g.Expect(err).NotTo(HaveOccurred(), "Failed to get component spec.readonly")
	if expected {
		g.Expect(value).To(Equal("true"),
			fmt.Sprintf("Component %s spec.readonly should be true", componentName))
	} else {
		g.Expect(value).To(Or(Equal("false"), BeEmpty()),
			fmt.Sprintf("Component %s spec.readonly should be false or unset", componentName))
	}
}

// VerifyStatusConditionReason checks that a component has a status
// condition with the given type whose reason matches the expected reason.
// The type and reason are matched as substrings of the serialized conditions,
// which is exact enough because condition types and reasons are unique.
func (h *ComponentHelper) VerifyStatusConditionReason(componentName, conditionType, reason string) error {
	conditions, err := h.getComponentConditions(componentName)
	if err != nil {
		return err
	}
	if findComponentCondition(conditions, conditionType, reason) == nil {
		return fmt.Errorf("component should have a %s condition with reason %s, got %+v",
			conditionType, reason, conditions)
	}
	return nil
}

// PodHelper provides helper methods for pod operations
type PodHelper struct {
	namespace string
}

func NewPodHelper(ns string) *PodHelper {
	return &PodHelper{namespace: ns}
}

// VerifyPodsReady checks that at least one pod with the given label is ready
func (h *PodHelper) VerifyPodsReady(g Gomega, labelKey, labelValue string) {
	cmd := exec.Command("kubectl", "get", "pods",
		"-l", fmt.Sprintf("%s=%s", labelKey, labelValue),
		"-n", h.namespace,
		"-o", "jsonpath={range .items[*]}{.metadata.name}{'|'}{.status.conditions[?(@.type=='Ready')].status}{'\\n'}{end}")
	output, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get %s pods", labelValue))
	g.Expect(output).NotTo(BeEmpty(), fmt.Sprintf("No %s pods found", labelValue))

	lines := utils.GetNonEmptyLines(output)
	g.Expect(lines).ToNot(BeEmpty(), fmt.Sprintf("No %s pods found", labelValue))

	foundReady := false
	for _, line := range lines {
		if podReady(line) {
			foundReady = true
			break
		}
	}
	g.Expect(foundReady).To(BeTrue(), fmt.Sprintf("No %s pods are in Ready state", labelValue))
}

// GetPodIdentities returns "<name>|<creationTimestamp>" lines for the pods
// with the given label, capturing the pod identity for before/after
// comparisons (e.g. asserting the agent is never restarted by a migration).
func (h *PodHelper) GetPodIdentities(labelKey, labelValue string) (string, error) {
	cmd := exec.Command("kubectl", "get", "pods",
		"-l", fmt.Sprintf("%s=%s", labelKey, labelValue),
		"-n", h.namespace,
		"-o", "jsonpath={range .items[*]}{.metadata.name}{'|'}{.metadata.creationTimestamp}{'\\n'}{end}")
	return utils.Run(cmd)
}

// DeploymentHelper provides helper methods for deployment operations
type DeploymentHelper struct {
	namespace string
}

func NewDeploymentHelper(ns string) *DeploymentHelper {
	return &DeploymentHelper{namespace: ns}
}

// VerifyDeploymentExists checks that a deployment with a specific version label exists
func (h *DeploymentHelper) VerifyDeploymentExists(deploymentLabel, version string) error {
	cmd := exec.Command("kubectl", "get", "deployments",
		"-l", fmt.Sprintf("%s-%s", deploymentLabel, version),
		"-n", h.namespace,
		"-o", "jsonpath={range .items[*]}{.metadata.name}{end}")
	output, err := utils.Run(cmd)
	if err != nil {
		return fmt.Errorf("failed to get deployment: %w", err)
	}
	if output == "" {
		return fmt.Errorf("no deployment found with version %s", version)
	}
	return nil
}

// ClusterHelper provides helper methods for cluster operations
type ClusterHelper struct {
	namespace string
}

func NewClusterHelper(ns string) *ClusterHelper {
	return &ClusterHelper{namespace: ns}
}

// CreateFromYAML creates a cluster CR from a YAML template
func (h *ClusterHelper) CreateFromYAML(clusterName, secretName, apiURL string) error {
	clusterYAML := fmt.Sprintf(clusterYaml, clusterName, h.namespace, secretName, apiURL)

	clusterFile := filepath.Join("/tmp", fmt.Sprintf("%s-cluster.yaml", clusterName))
	if err := os.WriteFile(clusterFile, []byte(clusterYAML), os.FileMode(0o644)); err != nil {
		return fmt.Errorf("failed to write cluster manifest: %w", err)
	}

	cmd := exec.Command("kubectl", "apply", "-f", clusterFile)
	_, err := utils.Run(cmd)
	return err
}

// GetClusterID retrieves the cluster ID from a cluster CR
func (h *ClusterHelper) GetClusterID(clusterName string) (string, error) {
	cmd := exec.Command("kubectl", "get", "cluster", clusterName,
		"-n", h.namespace,
		"-o", "jsonpath={.spec.cluster.clusterID}")
	return utils.Run(cmd)
}

// VerifyClusterID checks that a cluster has a valid cluster ID
func (h *ClusterHelper) VerifyClusterID(g Gomega, clusterName string) string {
	output, err := h.GetClusterID(clusterName)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to get cluster CR")
	g.Expect(output).NotTo(BeEmpty(), "Cluster ID is not set")
	// Verify it's a valid UUID format
	// nolint: lll
	g.Expect(output).To(MatchRegexp(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89ABab][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`),
		"Cluster ID does not match UUID format")
	return output
}

// HelmHelper provides helper methods for Helm operations
type HelmHelper struct {
	namespace string
}

func NewHelmHelper(ns string) *HelmHelper {
	return &HelmHelper{namespace: ns}
}

// InstallOperator installs the castware-operator Helm chart with common settings
func (h *HelmHelper) InstallOperator(
	imageRepo,
	imageTag,
	apiKey,
	apiURL,
	chartPath,
	migrationMode string,
	additionalFlags ...string) error {
	args := []string{
		"upgrade", "--install", "castware-operator",
		"--namespace", h.namespace,
		"--set", fmt.Sprintf("image.repository=%s", imageRepo),
		"--set", fmt.Sprintf("image.tag=%s", imageTag),
		"--set", "image.pullPolicy=IfNotPresent",
		"--set", fmt.Sprintf("apiKeySecret.apiKey=%s", apiKey),
		"--set", fmt.Sprintf("defaultCluster.api.apiUrl=%s", apiURL),
		"--set", "defaultCluster.provider=gke",
		"--set", "defaultCluster.terraform=false",
		"--set", "defaultComponents.enabled=false",
		"--set", "webhook.env.GKE_CLUSTER_NAME=castware-operator-e2e",
		"--set", "webhook.env.GKE_LOCATION=e2e",
		"--set", "webhook.env.GKE_PROJECT_ID=e2e",
		"--set", "webhook.env.GKE_REGION=e2e",
		"--atomic",
		"--timeout", "5m",
	}

	if migrationMode != "" {
		args = append(args, "--set", fmt.Sprintf("defaultCluster.migrationMode=%s", migrationMode))
	}

	args = append(args, additionalFlags...)
	args = append(args, chartPath)

	cmd := exec.Command("helm", args...)
	_, err := utils.Run(cmd)
	return err
}

// UninstallOperator uninstalls the castware-operator Helm release
func (h *HelmHelper) UninstallOperator() error {
	cmd := exec.Command("helm", "uninstall", "castware-operator", "-n", h.namespace)
	_, err := utils.Run(cmd)
	return err
}

// ReleaseExists checks whether a helm release exists in the namespace.
// A "not found" status is reported as (false, nil), not an error.
func (h *HelmHelper) ReleaseExists(releaseName string) (bool, error) {
	cmd := exec.Command("helm", "status", releaseName, "-n", h.namespace, "-o", "json")
	_, err := utils.Run(cmd)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// GetReleaseRevision returns the current revision of a helm release
func (h *HelmHelper) GetReleaseRevision(releaseName string) (int, error) {
	cmd := exec.Command("helm", "status", releaseName, "-n", h.namespace, "-o", "json")
	output, err := utils.Run(cmd)
	if err != nil {
		return 0, fmt.Errorf("failed to get helm release status: %w", err)
	}
	var status struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		return 0, fmt.Errorf("failed to unmarshal helm release status: %w", err)
	}
	return status.Version, nil
}

// GetReleaseHistoryCount returns the number of revisions in a release's history
func (h *HelmHelper) GetReleaseHistoryCount(releaseName string) (int, error) {
	cmd := exec.Command("helm", "history", releaseName, "-n", h.namespace, "-o", "json")
	output, err := utils.Run(cmd)
	if err != nil {
		return 0, fmt.Errorf("failed to get helm release history: %w", err)
	}
	var history []struct {
		Revision int `json:"revision"`
	}
	if err := json.Unmarshal([]byte(output), &history); err != nil {
		return 0, fmt.Errorf("failed to unmarshal helm release history: %w", err)
	}
	return len(history), nil
}

// GetReleaseValuesJSON returns the user-supplied values of a helm release as JSON
func (h *HelmHelper) GetReleaseValuesJSON(releaseName string) (string, error) {
	cmd := exec.Command("helm", "get", "values", releaseName, "-n", h.namespace, "-o", "json")
	return utils.Run(cmd)
}

// GetReleaseCRDNames extracts the CustomResourceDefinition names rendered by
// a helm release, parsed from its manifest. Used to verify which CRDs belong
// to a release so tests can assert they survive teardowns of other releases.
func (h *HelmHelper) GetReleaseCRDNames(releaseName string) ([]string, error) {
	cmd := exec.Command("helm", "get", "manifest", releaseName, "-n", h.namespace)
	output, err := utils.Run(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to get helm release manifest: %w", err)
	}

	return crdNamesFromManifest(output)
}

// crdNamesFromManifest extracts the CustomResourceDefinition names from a
// multi-document helm manifest by decoding each document structurally, so
// nested name: fields (ownerReferences, spec.names, ...) cannot be mistaken
// for metadata.name.
func crdNamesFromManifest(manifest string) ([]string, error) {
	names := map[string]struct{}{}
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(manifest), 4096)
	for {
		var doc struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := decoder.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("failed to decode helm release manifest: %w", err)
		}
		if doc.Kind == "CustomResourceDefinition" && doc.Metadata.Name != "" {
			names[doc.Metadata.Name] = struct{}{}
		}
	}

	result := make([]string, 0, len(names))
	for n := range names {
		result = append(result, n)
	}
	sort.Strings(result)
	return result, nil
}

// InstallChart upgrades or installs an arbitrary helm chart in the namespace
func (h *HelmHelper) InstallChart(releaseName, chartRef string, additionalFlags ...string) error {
	args := []string{ //nolint:prealloc
		"upgrade", "--install", releaseName,
		"--namespace", h.namespace,
		"--create-namespace",
		"--timeout", "10m",
	}
	args = append(args, additionalFlags...)
	args = append(args, chartRef)

	cmd := exec.Command("helm", args...)
	_, err := utils.Run(cmd)
	return err
}

// UninstallRelease uninstalls an arbitrary helm release from the namespace
func (h *HelmHelper) UninstallRelease(releaseName string) error {
	cmd := exec.Command("helm", "uninstall", releaseName, "-n", h.namespace)
	_, err := utils.Run(cmd)
	return err
}

// InstallStandaloneChart installs a standalone chart by hand with flattened
// --set values (map iteration order is irrelevant; the values are
// independent).
func (h *HelmHelper) InstallStandaloneChart(releaseName, chartRef string, values map[string]string) error {
	flags := make([]string, 0, 2*len(values))
	for key, value := range values {
		flags = append(flags, "--set", key+"="+value)
	}
	return h.InstallChart(releaseName, chartRef, flags...)
}

// ListReleaseNames returns the names of all helm releases in the namespace
func (h *HelmHelper) ListReleaseNames() ([]string, error) {
	cmd := exec.Command("helm", "list", "-n", h.namespace, "-o", "json")
	output, err := utils.Run(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to list helm releases: %w", err)
	}
	var releases []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(output), &releases); err != nil {
		return nil, fmt.Errorf("failed to unmarshal helm release list: %w", err)
	}
	names := make([]string, 0, len(releases))
	for _, r := range releases {
		names = append(names, r.Name)
	}
	return names, nil
}

// APIHelper provides helper methods for Cast AI API operations
type APIHelper struct {
	apiKey string
	apiURL string
}

func NewAPIHelper(apiKey, apiURL string) *APIHelper {
	return &APIHelper{
		apiKey: apiKey,
		apiURL: apiURL,
	}
}

// FetchFromAPI makes an HTTP request to the Cast AI API
func (h *APIHelper) FetchFromAPI(url string, method string, requestBody interface{}, responseBody interface{}) error {
	return h.fetchFromAPI(url, method, requestBody, responseBody)
}

// FetchFromAPIWithRetry fetches an API resource with a bounded retry for
// transient edge failures: the dev API's nginx front has been observed to
// return intermittent 401s mid-run while the same request succeeded minutes
// earlier with the same key. Used for the onboarding script fetches, which
// the legacy-script specs execute against the dev environment.
func (h *APIHelper) FetchFromAPIWithRetry(
	url string, method string, requestBody interface{}, responseBody interface{},
) error {
	const maxAttempts = 3
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err = h.fetchFromAPI(url, method, requestBody, responseBody)
		if err == nil {
			return nil
		}
		if attempt < maxAttempts {
			time.Sleep(5 * time.Second)
		}
	}
	return err
}

// fetchFromAPI performs the HTTP request shared by FetchFromAPI and
// FetchFromAPIWithRetry.
func (h *APIHelper) fetchFromAPI(url string, method string, requestBody interface{}, responseBody interface{}) error {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create HTTP request for URL %s: %w", url, err)
	}
	req.Header.Set("X-API-Key", h.apiKey)

	if requestBody != nil {
		b, err := json.Marshal(requestBody)
		if err != nil {
			return fmt.Errorf("failed to marshal JSON request body: %w", err)
		}

		req.Body = io.NopCloser(bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute HTTP request to %s: %w", url, err)
	}
	//nolint:errcheck
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read HTTP response body from %s: %w", url, err)
	}
	if resp.StatusCode > 299 {
		return fmt.Errorf("failed to get response from Cast AI API at %s: %s", url, string(body))
	}

	if responseBody != nil {
		switch t := responseBody.(type) {
		case *string:
			*t = string(body)
		default:
			if err = json.Unmarshal(body, responseBody); err != nil {
				return fmt.Errorf("failed to unmarshal JSON response from %s: %w", url, err)
			}
		}
	}

	return nil
}

// GetCluster retrieves cluster information from the API
func (h *APIHelper) GetCluster(clusterID string) (map[string]interface{}, error) {
	url := fmt.Sprintf("%s/v1/kubernetes/external-clusters/%s", h.apiURL, clusterID)
	var resp map[string]interface{}
	err := h.FetchFromAPI(url, http.MethodGet, nil, &resp)
	return resp, err
}

// DeleteCluster deletes a cluster from the Cast AI API
func (h *APIHelper) DeleteCluster(clusterID string) error {
	url := fmt.Sprintf("%s/v1/kubernetes/external-clusters/%s", h.apiURL, clusterID)
	return h.FetchFromAPI(url, http.MethodDelete, nil, nil)
}

// GetClusterComponents retrieves components for a cluster
func (h *APIHelper) GetClusterComponents(organizationID, clusterID string) ([]component, error) {
	url := fmt.Sprintf("%s/cluster-management/v1/organizations/%s/clusters/%s/components:view",
		h.apiURL, organizationID, clusterID)

	var resp struct {
		Components []component `json:"components"`
	}
	err := h.FetchFromAPI(url, http.MethodGet, nil, &resp)
	return resp.Components, err
}

// GetComponentByName retrieves component registry information by component name
func (h *APIHelper) GetComponentByName(name string) (*castAIComponentInfo, error) {
	query := url.Values{"name": []string{name}}
	endpoint := fmt.Sprintf("%s/cluster-management/v1/components:getByName?%s", h.apiURL, query.Encode())
	var resp castAIComponentInfo
	if err := h.FetchFromAPI(endpoint, http.MethodGet, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetUmbrellaReleaseName resolves the helm release name Mothership expects
// for the umbrella component, falling back to the component name
func (h *APIHelper) GetUmbrellaReleaseName() (string, error) {
	umbrella, err := h.GetComponentByName("castai-umbrella")
	if err != nil {
		return "", err
	}
	if umbrella.ReleaseName == "" {
		return "castai-umbrella", nil
	}
	return umbrella.ReleaseName, nil
}

// SecretHelper provides helper methods for secret operations
type SecretHelper struct {
	namespace string
}

func NewSecretHelper(ns string) *SecretHelper {
	return &SecretHelper{namespace: ns}
}

// CreateAPIKeySecret creates a secret with an API key
func (h *SecretHelper) CreateAPIKeySecret(secretName, apiKey string) error {
	cmd := exec.Command("kubectl", "create", "secret", "generic", secretName,
		"--from-literal=API_KEY="+apiKey,
		"-n", h.namespace)
	_, err := utils.Run(cmd)
	return err
}

// NamespaceHelper provides helper methods for namespace operations
type NamespaceHelper struct{}

func NewNamespaceHelper() *NamespaceHelper {
	return &NamespaceHelper{}
}

// Create creates a namespace
func (h *NamespaceHelper) Create(namespace string) error {
	cmd := exec.Command("kubectl", "create", "ns", namespace)
	_, err := utils.Run(cmd)
	return err
}

// Delete deletes a namespace
func (h *NamespaceHelper) Delete(namespace string) error {
	cmd := exec.Command("kubectl", "delete", "ns", namespace)
	_, err := utils.Run(cmd)
	return err
}

// VerifyDeleted checks that a namespace is deleted
func (h *NamespaceHelper) VerifyDeleted(g Gomega, namespace string) {
	cmd := exec.Command("kubectl", "get", "ns", namespace)
	_, err := utils.Run(cmd)
	g.Expect(err).To(HaveOccurred(), "Namespace should be deleted")
}

// GetUID retrieves the UID of a namespace
func (h *NamespaceHelper) GetUID(namespace string) (string, error) {
	cmd := exec.Command("kubectl", "get", "namespace", namespace, "-o", "jsonpath={.metadata.uid}")
	return utils.Run(cmd)
}

// LabelNamespace adds labels to a namespace
func (h *NamespaceHelper) LabelNamespace(namespace string, labels ...string) error {
	args := append([]string{"label", "namespace", namespace}, labels...)
	cmd := exec.Command("kubectl", args...)
	_, err := utils.Run(cmd)
	return err
}

// AnnotateNamespace adds annotations to a namespace
func (h *NamespaceHelper) AnnotateNamespace(namespace string, annotations ...string) error {
	args := append([]string{"annotate", "namespace", namespace}, annotations...)
	cmd := exec.Command("kubectl", args...)
	_, err := utils.Run(cmd)
	return err
}

// FindComponentByName finds a component in a list by name
func FindComponentByName(components []component, name string) (component, bool) {
	for _, c := range components {
		if c.Name == name {
			return c, true
		}
	}
	return component{}, false
}

// getOperatorLogs fetches the castware-operator controller logs. The label
// selector matches all operator pods (--prefix keeps the output readable
// when an upgrade briefly leaves two pods).
// nolint:unparam // namespace kept for symmetry with the other helpers.
func getOperatorLogs(namespace string) (string, error) {
	cmd := exec.Command("kubectl", "logs",
		"-l", "app.kubernetes.io/instance=castware-operator",
		"-n", namespace,
		"--tail=3000",
		"--prefix",
	)
	return utils.Run(cmd)
}

// crdExists checks whether a CRD with the given name exists. A "not found"
// status is reported as (false, nil), not an error.
func crdExists(name string) (bool, error) {
	cmd := exec.Command("kubectl", "get", "crd", name, "-o", "name")
	_, err := utils.Run(cmd)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// stringSliceContains reports whether the slice contains the value.
func stringSliceContains(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}
