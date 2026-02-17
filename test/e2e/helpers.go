package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/castai/castware-operator/test/utils"
	//nolint:staticcheck
	. "github.com/onsi/gomega"
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

// component represents a Cast AI component
type component struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	UsedVersion   string `json:"usedVersion"`
	LatestVersion string `json:"latestVersion"`
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

// VerifyStatusCondition checks that a component has a specific status condition
func (h *ComponentHelper) VerifyStatusCondition(componentName, conditionType string) error {
	cmd := exec.Command("kubectl", "get", "component", componentName,
		"-n", h.namespace,
		"-o", "jsonpath={.status.conditions}")
	output, err := utils.Run(cmd)
	if err != nil {
		return fmt.Errorf("failed to get component status: %w", err)
	}

	expectedCondition := fmt.Sprintf(`"type":"%s"`, conditionType)
	if !strings.Contains(output, expectedCondition) {
		return fmt.Errorf("component should have %s condition", conditionType)
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
