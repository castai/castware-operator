package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/test/utils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
)

// namespace where the project is deployed in
const namespace = "castai-agent"

// serviceAccountName created for the project
const serviceAccountName = "castware-operator-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "castware-operator"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "castware-operator-metrics-binding"

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
  helmRepoURL: "https://castai.github.io/helm-charts"
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

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string
	var clusterID string
	var organizationID string
	var apiKey string
	var apiURL = "https://api.dev-master.cast.ai"
	var agentInstalled bool
	var spotHandlerInstalled bool
	var versionBeforeDowngrade string
	var operatorChartPath string

	// Extract image repository and tag from projectImage (format: repository:tag)
	imageParts := strings.Split(projectImage, ":")
	Expect(imageParts).To(HaveLen(2), "invalid projectImage format")

	fetchFromAPI := func(apiURL string, httpMethod string, responseBody interface{}) error {
		req, err := http.NewRequest(httpMethod, apiURL, nil)
		if err != nil {
			return fmt.Errorf("failed to create HTTP request for URL %s: %w", apiURL, err)
		}
		req.Header.Set("X-API-Key", apiKey)

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("failed to execute HTTP GET request to %s: %w", apiURL, err)
		}
		//nolint:errcheck
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to read HTTP response body from %s: %w", apiURL, err)
		}
		if resp.StatusCode > 299 {
			return fmt.Errorf("failed to get response from Cast AI API at %s: %s", apiURL, string(body))
		}

		if responseBody != nil {
			switch t := responseBody.(type) {
			case *string:
				*t = string(body)
			default:
				if err = json.Unmarshal(body, responseBody); err != nil {
					return fmt.Errorf("failed to unmarshal JSON response from %s: %w", apiURL, err)
				}
			}
		}

		return nil
	}

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		wd, _ := os.Getwd()
		fmt.Println("Running e2e tests...", wd)
		apiKey = os.Getenv("API_KEY")
		Expect(apiKey).NotTo(BeEmpty(), "API_KEY environment variable is not set")

		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		// Get project root directory (two levels up from test/e2e)
		operatorChartPath = filepath.Join(wd, "charts", "castai-castware-operator")

		By("installing helm chart")
		// Use helm upgrade --install command as defined in local/install-local.sh
		cmd = exec.Command("helm", "upgrade", "--install", "castware-operator",
			"--namespace", namespace,
			"--set", fmt.Sprintf("image.repository=%s", imageParts[0]),
			"--set", fmt.Sprintf("image.tag=%s", imageParts[1]),
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
			operatorChartPath,
		)

		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install helm chart")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("cleaning up the curl pod for metrics")

		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		// Delete cluster from Cast AI API if cluster ID was set
		if clusterID != "" && apiKey != "" && apiURL != "" {
			By(fmt.Sprintf("deleting cluster from Cast AI API: %s", clusterID))
			deleteURL := fmt.Sprintf("%s/v1/kubernetes/external-clusters/%s", apiURL, clusterID)
			err := fetchFromAPI(deleteURL, http.MethodDelete, nil)
			if err != nil {
				fmt.Printf("Failed delete cluster: %v\n", err)
			}
		}

		By("deleting agent CR")
		if agentInstalled {
			cmd = exec.Command("kubectl", "delete", "component", "castai-agent", "-n", namespace)
			_, _ = utils.Run(cmd)
		}

		By("deleting spot handler CR")
		if spotHandlerInstalled {
			cmd = exec.Command("kubectl", "delete", "component", "spot-handler", "-n", namespace)
			_, _ = utils.Run(cmd)
		}
		By("deleting cluster controller CR")
		if spotHandlerInstalled {
			cmd = exec.Command("kubectl", "delete", "component", "cluster-controller", "-n", namespace)
			_, _ = utils.Run(cmd)
		}

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling helm release")
		cmd = exec.Command("helm", "uninstall", "castware-operator", "-n", namespace)
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)

		By("removing metrics cluster role binding")
		cmd = exec.Command("kubectl", "delete", "clusterrolebinding", metricsRoleBindingName)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(5 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the name of the controller-manager pod
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "app.kubernetes.io/instance=castware-operator",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("castware-operator"))

				// Validate the pod's status
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			Skip("Metrics not supported yet")
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=castware-operator-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("waiting for the metrics endpoint to be ready")
			verifyMetricsEndpointReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "endpoints", metricsServiceName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("8080"), "Metrics endpoint is not ready")
			}
			Eventually(verifyMetricsEndpointReady).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Serving metrics server"),
					"Metrics server not yet started")
				g.Expect(output).To(ContainSubstring("logger=controller-runtime.metrics"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted).Should(Succeed())

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": ["curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8080/metrics"],
							"securityContext": {
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccount": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 10*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			metricsOutput := getMetricsOutput()
			Expect(metricsOutput).To(ContainSubstring(
				"controller_runtime_reconcile_total",
			))
		})

		It("should have CA injection for mutating webhooks", func() {
			By("checking CA injection for mutating webhooks")
			verifyCAInjection := func(g Gomega) {
				cmd := exec.Command("kubectl", "get",
					"mutatingwebhookconfigurations.admissionregistration.k8s.io",
					"castware-operator-mutating-webhook-configuration",
					"-o", "go-template={{ range .webhooks }}{{ .clientConfig.caBundle }}{{ end }}")
				mwhOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(mwhOutput)).To(BeNumerically(">", 10))
			}
			Eventually(verifyCAInjection).Should(Succeed())
		})

		It("should have CA injection for validating webhooks", func() {
			By("checking CA injection for validating webhooks")
			verifyCAInjection := func(g Gomega) {
				cmd := exec.Command("kubectl", "get",
					"validatingwebhookconfigurations.admissionregistration.k8s.io",
					"castware-operator-validating-webhook-configuration",
					"-o", "go-template={{ range .webhooks }}{{ .clientConfig.caBundle }}{{ end }}")
				vwhOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(vwhOutput)).To(BeNumerically(">", 10))
			}
			Eventually(verifyCAInjection).Should(Succeed())
		})

		It("should onboard a cluster and get a cluster ID", func() {
			Expect(apiKey).NotTo(BeEmpty(), "API_KEY env variable is not set")

			secretName := "castware-api-key-test"
			clusterName := "castai"

			By("creating API key secret")
			cmd := exec.Command("kubectl", "create", "secret", "generic", secretName,
				"--from-literal=API_KEY="+apiKey,
				"-n", namespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create API key secret")

			By("creating a cluster custom resource")
			clusterYAML := fmt.Sprintf(clusterYaml, clusterName, namespace, secretName, apiURL)

			clusterFile := filepath.Join("/tmp", fmt.Sprintf("%s-cluster.yaml", clusterName))
			err = os.WriteFile(clusterFile, []byte(clusterYAML), os.FileMode(0o644))
			Expect(err).NotTo(HaveOccurred(), "Failed to write cluster manifest")

			cmd = exec.Command("kubectl", "apply", "-f", clusterFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create cluster CR")

			By("waiting for the cluster to be onboarded and get a cluster ID")
			verifyClusterID := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "cluster", clusterName,
					"-n", namespace,
					"-o", "jsonpath={.spec.cluster.clusterID}",
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get cluster CR")
				g.Expect(output).NotTo(BeEmpty(), "Cluster ID is not set")
				// Verify it's a valid UUID format
				// nolint: lll
				g.Expect(output).To(MatchRegexp(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89ABab][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`),
					"Cluster ID does not match UUID format")
				// Store the cluster ID for cleanup
				clusterID = output
			}
			Eventually(verifyClusterID, 5*time.Minute).Should(Succeed())

			By("verifying cluster name and location are also populated")
			cmd = exec.Command("kubectl", "get", "cluster", clusterName, "-n", namespace, "-o", "jsonpath={.spec.cluster}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get cluster metadata")
			Expect(output).To(ContainSubstring("clusterID"), "Cluster metadata should contain clusterID")

			getClusterURL := fmt.Sprintf("%s/v1/kubernetes/external-clusters/%s", apiURL, clusterID)
			clusterResp := map[string]interface{}{}
			err = fetchFromAPI(getClusterURL, http.MethodGet, &clusterResp)
			Expect(err).ToNot(HaveOccurred())
			organizationID = clusterResp["organizationId"].(string)
		})

		It("should install castai-agent", func() {
			By("creating a component custom resource")
			componentYAML := fmt.Sprintf(componentYaml, components.ComponentNameAgent, namespace, components.ComponentNameAgent)

			componentFile := filepath.Join("/tmp", fmt.Sprintf("%s-component.yaml", components.ComponentNameAgent))
			err := os.WriteFile(componentFile, []byte(componentYAML), os.FileMode(0o644))
			Expect(err).NotTo(HaveOccurred(), "Failed to write component manifest")

			cmd := exec.Command("kubectl", "apply", "-f", componentFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create component CR")

			By("waiting for castai-agent component to have a version")
			verifyComponent := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "component", components.ComponentNameAgent,
					"-n", namespace,
					"-o", "jsonpath={.status.currentVersion}",
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get component CR")
				g.Expect(output).NotTo(BeEmpty(), "Version is not set")
			}
			Eventually(verifyComponent, 5*time.Minute).Should(Succeed())

			agentInstalled = true

			By("verifying at least one castai-agent pod is in ready state")
			verifyAgentPodReady := func(g Gomega) {
				// Get pods with label app.kubernetes.io/name=castai-agent
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", "app.kubernetes.io/name=castai-agent",
					"-n", namespace,
					"-o", "jsonpath={range .items[*]}{.metadata.name}{'|'}{.status.conditions[?(@.type=='Ready')].status}{'\\n'}{end}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get castai-agent pods")
				g.Expect(output).NotTo(BeEmpty(), "No castai-agent pods found")

				// Check that at least one pod has Ready=True
				lines := utils.GetNonEmptyLines(output)
				g.Expect(lines).ToNot(BeEmpty(), "No castai-agent pods found")

				foundReady := false
				for _, line := range lines {
					if podReady(line) {
						foundReady = true
						break
					}
				}
				g.Expect(foundReady).To(BeTrue(), "No castai-agent pods are in Ready state")
			}
			Eventually(verifyAgentPodReady, 5*time.Minute).Should(Succeed())

			By("verifying component status conditions")
			cmd = exec.Command("kubectl", "get", "component", components.ComponentNameAgent,
				"-n", namespace,
				"-o", "jsonpath={.status.conditions}",
			)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get component metadata")
			Expect(output).To(ContainSubstring(`"type":"Available"`), "Component should be in Available status")

			getClusterURL := fmt.Sprintf("%s/v1/kubernetes/external-clusters/%s", apiURL, clusterID)
			clusterResp := map[string]interface{}{}
			err = fetchFromAPI(getClusterURL, http.MethodGet, &clusterResp)
			Expect(err).ToNot(HaveOccurred())
			Expect(clusterResp["status"]).To(Equal("ready"))
			Expect(clusterResp["castwareInstallMethod"]).To(Equal("OPERATOR"))
		})

		It("should downgrade castai-agent", func() {
			By("getting current castai-agent version")
			cmd := exec.Command("kubectl", "get", "component", components.ComponentNameAgent,
				"-n", namespace,
				"-o", "jsonpath={.status.currentVersion}",
			)
			currentVersion, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get component current version")
			Expect(currentVersion).NotTo(BeEmpty(), "Current version is not set")

			By(fmt.Sprintf("current version is: %s", currentVersion))
			versionBeforeDowngrade = currentVersion

			// Define a known older version to downgrade to
			downgradeVersion := "0.125.0"

			By(fmt.Sprintf("patching component to downgrade to version %s", downgradeVersion))
			patchJSON := fmt.Sprintf(`{"spec":{"version":"%s"}}`, downgradeVersion)
			cmd = exec.Command("kubectl", "patch", "component", components.ComponentNameAgent,
				"-n", namespace,
				"--type=merge",
				"-p", patchJSON)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to patch component with downgrade version")

			By("waiting for the component to be downgraded")
			verifyDowngrade := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "component", components.ComponentNameAgent,
					"-n", namespace,
					"-o", "jsonpath={.status.currentVersion}",
				)
				version, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get component version")
				g.Expect(version).To(Equal(downgradeVersion), "Component version should match downgrade version")
			}
			Eventually(verifyDowngrade, 5*time.Minute).Should(Succeed())

			By("verifying at least one castai-agent pod is ready after downgrade")
			verifyAgentPodReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", "app.kubernetes.io/name=castai-agent",
					"-n", namespace,
					"-o", "jsonpath={range .items[*]}{.metadata.name}{'|'}{.status.conditions[?(@.type=='Ready')].status}{'\\n'}{end}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get castai-agent pods")
				g.Expect(output).NotTo(BeEmpty(), "No castai-agent pods found")

				lines := utils.GetNonEmptyLines(output)
				g.Expect(lines).ToNot(BeEmpty(), "No castai-agent pods found")

				foundReady := false
				for _, line := range lines {
					if podReady(line) {
						foundReady = true
						break
					}
				}
				g.Expect(foundReady).To(BeTrue(), "No castai-agent pods are in Ready state after downgrade")
			}
			Eventually(verifyAgentPodReady, 5*time.Minute).Should(Succeed())

			cmd = exec.Command("kubectl", "get", "deployments",
				"-l", "helm.sh/chart=castai-agent-"+downgradeVersion,
				"-n", namespace,
				"-o", "jsonpath={range .items[*]}{.metadata.name}{end}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get castai-agent deployment")
			Expect(output).NotTo(BeEmpty(), "No castai-agent deployment found")

			By("verifying component status is Available after downgrade")
			cmd = exec.Command("kubectl", "get", "component", components.ComponentNameAgent,
				"-n", namespace,
				"-o", "jsonpath={.status.conditions}",
			)
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get component status")
			Expect(output).To(ContainSubstring(`"type":"Available"`), "Component should be in Available status after downgrade")

			getClusterURL := fmt.Sprintf("%s/cluster-management/v1/organizations/%s/clusters/%s/components:view",
				apiURL, organizationID, clusterID)
			componentsResp := struct {
				Components []component `json:"components"`
			}{}
			err = fetchFromAPI(getClusterURL, http.MethodGet, &componentsResp)
			Expect(err).ToNot(HaveOccurred())
			agentComponent, ok := lo.Find(componentsResp.Components, func(item component) bool {
				return item.Name == components.ComponentNameAgent
			})
			Expect(ok).To(BeTrue(), "Failed to find castai-agent component")
			Expect(agentComponent.UsedVersion).To(Equal(downgradeVersion))
		})

		It("should upgrade castai-agent", func() {
			componentName := "castai-agent"

			By("getting current castai-agent version before upgrade")
			cmd := exec.Command("kubectl", "get", "component", componentName,
				"-n", namespace,
				"-o", "jsonpath={.status.currentVersion}",
			)
			versionBeforeUpgrade, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get component current version")
			Expect(versionBeforeUpgrade).NotTo(BeEmpty(), "Current version is not set")

			By(fmt.Sprintf("current version before upgrade is: %s", versionBeforeUpgrade))

			By("patching component to upgrade to latest version by setting version to empty string")
			patchJSON := `{"spec":{"version":""}}`
			cmd = exec.Command("kubectl", "patch", "component", componentName,
				"-n", namespace,
				"--type=merge",
				"-p", patchJSON)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to patch component to upgrade")

			By("waiting for the component to be upgraded to a newer version")
			verifyUpgrade := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "component", componentName,
					"-n", namespace,
					"-o", "jsonpath={.status.currentVersion}",
				)
				currentVersion, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get component version")
				g.Expect(currentVersion).NotTo(BeEmpty(), "Version should be set")
				g.Expect(currentVersion).NotTo(Equal(versionBeforeUpgrade), "Version should have changed from previous version")
			}
			Eventually(verifyUpgrade, 5*time.Minute).Should(Succeed())

			By("getting new version after upgrade")
			cmd = exec.Command("kubectl", "get", "component", componentName,
				"-n", namespace,
				"-o", "jsonpath={.status.currentVersion}",
			)
			versionAfterUpgrade, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get component version after upgrade")
			By(fmt.Sprintf("upgraded to version: %s", versionAfterUpgrade))

			By("verifying at least one castai-agent pod is ready after upgrade")
			verifyAgentPodReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", "app.kubernetes.io/name=castai-agent",
					"-n", namespace,
					"-o", "jsonpath={range .items[*]}{.metadata.name}{'|'}{.status.conditions[?(@.type=='Ready')].status}{'\\n'}{end}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get castai-agent pods")
				g.Expect(output).NotTo(BeEmpty(), "No castai-agent pods found")

				lines := utils.GetNonEmptyLines(output)
				g.Expect(lines).ToNot(BeEmpty(), "No castai-agent pods found")

				foundReady := false
				for _, line := range lines {
					if podReady(line) {
						foundReady = true
						break
					}
				}
				g.Expect(foundReady).To(BeTrue(), "No castai-agent pods are in Ready state after upgrade")
			}
			Eventually(verifyAgentPodReady, 5*time.Minute).Should(Succeed())

			cmd = exec.Command("kubectl", "get", "deployments",
				"-l", "helm.sh/chart=castai-agent-"+strings.TrimSpace(versionAfterUpgrade),
				"-n", namespace,
				"-o", "jsonpath={range .items[*]}{.metadata.name}{end}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get castai-agent deployment")
			Expect(output).NotTo(BeEmpty(), "No castai-agent deployment found")

			By("verifying component status is Available after upgrade")
			cmd = exec.Command("kubectl", "get", "component", componentName,
				"-n", namespace,
				"-o", "jsonpath={.status.conditions}",
			)
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get component status")
			Expect(output).To(ContainSubstring(`"type":"Available"`), "Component should be in Available status after upgrade")

			getClusterURL := fmt.Sprintf("%s/cluster-management/v1/organizations/%s/clusters/%s/components:view",
				apiURL, organizationID, clusterID)
			componentsResp := struct {
				Components []component `json:"components"`
			}{}
			err = fetchFromAPI(getClusterURL, http.MethodGet, &componentsResp)
			Expect(err).ToNot(HaveOccurred())
			agentComponent, ok := lo.Find(componentsResp.Components, func(item component) bool {
				return item.Name == components.ComponentNameAgent
			})
			Expect(ok).To(BeTrue(), "Failed to find castai-agent component")
			Expect(agentComponent.LatestVersion).ToNot(BeEmpty(), "Failed to get latest version of castai-agent")
			Expect(agentComponent.UsedVersion).To(Equal(versionBeforeDowngrade))
		})

		It("should install spot-handler", func() {
			By("creating a component custom resource")
			componentYAML := fmt.Sprintf(componentYaml, components.ComponentNameSpotHandler,
				namespace, components.ComponentNameSpotHandler)
			componentYAML += "    phase2Permissions: false"

			componentFile := filepath.Join("/tmp", fmt.Sprintf("%s-component.yaml", components.ComponentNameSpotHandler))
			err := os.WriteFile(componentFile, []byte(componentYAML), os.FileMode(0o644))
			Expect(err).NotTo(HaveOccurred(), "Failed to write component manifest")

			cmd := exec.Command("kubectl", "apply", "-f", componentFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create component CR")

			spotHandlerInstalled = true

			By("waiting for spot-handler component to have a version")
			verifyComponent := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "component", components.ComponentNameSpotHandler,
					"-n", namespace,
					"-o", "jsonpath={.status.currentVersion}",
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get component CR")
				g.Expect(output).NotTo(BeEmpty(), "Version is not set")
			}
			Eventually(verifyComponent, 5*time.Minute).Should(Succeed())

			By("verifying that spot-handler daemonset is in ready state")
			verifyPodReady := func(g Gomega) {
				// Get pods with label app.kubernetes.io/name=spot-handler
				cmd := exec.Command("kubectl", "get", "daemonsets",
					"-l", "app.kubernetes.io/instance=spot-handler",
					"-n", namespace,
					"-o", "jsonpath={range .items[*]}{.metadata.name}{'|'}{.status.conditions[?(@.type=='Ready')].status}{'\\n'}{end}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get spot-handler daemonset")
				g.Expect(output).NotTo(BeEmpty(), "No spot-handler daemonsets found")
			}
			Eventually(verifyPodReady, 5*time.Minute).Should(Succeed())

			By("verifying component status conditions")
			cmd = exec.Command("kubectl", "get", "component", components.ComponentNameSpotHandler,
				"-n", namespace,
				"-o", "jsonpath={.status.conditions}",
			)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get component metadata")
			Expect(output).To(ContainSubstring(`"type":"Available"`), "Component should be in Available status")
		})

		It("should downgrade spot-handler", func() {
			By("getting current spot-handler version")
			cmd := exec.Command("kubectl", "get", "component", components.ComponentNameSpotHandler,
				"-n", namespace,
				"-o", "jsonpath={.status.currentVersion}",
			)
			currentVersion, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get component current version")
			Expect(currentVersion).NotTo(BeEmpty(), "Current version is not set")

			By(fmt.Sprintf("current version is: %s", currentVersion))
			versionBeforeDowngrade = currentVersion

			// Spot handler supports phase1 only permissions from 0.29.0 onwards,
			// downgrading to a lower version won't work in phase1 because the operator doesn't have permissions.
			downgradeVersion := "0.29.0"

			By(fmt.Sprintf("patching component to downgrade to version %s", downgradeVersion))
			patchJSON := fmt.Sprintf(`{"spec":{"version":"%s"}}`, downgradeVersion)
			cmd = exec.Command("kubectl", "patch", "component", components.ComponentNameSpotHandler,
				"-n", namespace,
				"--type=merge",
				"-p", patchJSON)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to patch component with downgrade version")

			By("waiting for the component to be downgraded")
			verifyDowngrade := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "component", components.ComponentNameSpotHandler,
					"-n", namespace,
					"-o", "jsonpath={.status.currentVersion}",
				)
				version, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get component version")
				g.Expect(version).To(Equal(downgradeVersion), "Component version should match downgrade version")
			}
			Eventually(verifyDowngrade, 5*time.Minute).Should(Succeed())

			By("verifying that spot-handler daemonset ready after downgrade")
			verifyPodReady := func(g Gomega) {
				// Get pods with label app.kubernetes.io/name=spot-handler
				cmd := exec.Command("kubectl", "get", "daemonsets",
					"-l", "helm.sh/chart=castai-spot-handler-"+downgradeVersion,
					"-n", namespace,
					"-o", "jsonpath={range .items[*]}{.metadata.name}{'|'}{.status.conditions[?(@.type=='Ready')].status}{'\\n'}{end}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get spot-handler daemonset")
				g.Expect(output).NotTo(BeEmpty(), "No spot-handler daemonsets found")
			}
			// kubectl get daemonsets
			Eventually(verifyPodReady, 5*time.Minute).Should(Succeed())

			By("verifying component status is Available after downgrade")
			cmd = exec.Command("kubectl", "get", "component", components.ComponentNameSpotHandler,
				"-n", namespace,
				"-o", "jsonpath={.status.conditions}",
			)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get component status")
			Expect(output).To(ContainSubstring(`"type":"Available"`), "Component should be in Available status after downgrade")

			getClusterURL := fmt.Sprintf("%s/cluster-management/v1/organizations/%s/clusters/%s/components:view",
				apiURL, organizationID, clusterID)
			componentsResp := struct {
				Components []component `json:"components"`
			}{}
			err = fetchFromAPI(getClusterURL, http.MethodGet, &componentsResp)
			Expect(err).ToNot(HaveOccurred())
			agentComponent, ok := lo.Find(componentsResp.Components, func(item component) bool {
				return item.Name == components.ComponentNameSpotHandler
			})
			Expect(ok).To(BeTrue(), "Failed to find spot-handler component")
			Expect(agentComponent.UsedVersion).To(Equal(downgradeVersion))
		})

		It("should upgrade spot-handler", func() {
			Skip("Spot handler has only one compatible version, so upgrade test is not possible")
			By("getting current spot-handler version before upgrade")
			cmd := exec.Command("kubectl", "get", "component", components.ComponentNameSpotHandler,
				"-n", namespace,
				"-o", "jsonpath={.status.currentVersion}",
			)
			versionBeforeUpgrade, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get component current version")
			Expect(versionBeforeUpgrade).NotTo(BeEmpty(), "Current version is not set")

			By(fmt.Sprintf("current version before upgrade is: %s", versionBeforeUpgrade))

			By("patching component to upgrade to latest version by setting version to empty string")
			patchJSON := `{"spec":{"version":""}}`
			cmd = exec.Command("kubectl", "patch", "component", components.ComponentNameSpotHandler,
				"-n", namespace,
				"--type=merge",
				"-p", patchJSON)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to patch component to upgrade")

			By("waiting for the component to be upgraded to a newer version")
			verifyUpgrade := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "component", components.ComponentNameSpotHandler,
					"-n", namespace,
					"-o", "jsonpath={.status.currentVersion}",
				)
				currentVersion, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get component version")
				g.Expect(currentVersion).NotTo(BeEmpty(), "Version should be set")
				g.Expect(currentVersion).NotTo(Equal(versionBeforeUpgrade), "Version should have changed from previous version")
			}
			Eventually(verifyUpgrade, 5*time.Minute).Should(Succeed())

			By("getting new version after upgrade")
			cmd = exec.Command("kubectl", "get", "component", components.ComponentNameSpotHandler,
				"-n", namespace,
				"-o", "jsonpath={.status.currentVersion}",
			)
			versionAfterUpgrade, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get component version after upgrade")
			By(fmt.Sprintf("upgraded to version: %s", versionAfterUpgrade))

			By("verifying that spot-handler daemonset ready after upgrade")
			verifyPodReady := func(g Gomega) {
				// Get pods with label app.kubernetes.io/name=spot-handler
				cmd := exec.Command("kubectl", "get", "daemonsets",
					"-l", "app.kubernetes.io/instance=spot-handler",
					"-n", namespace,
					"-o", "jsonpath={range .items[*]}{.metadata.name}{'|'}{.status.conditions[?(@.type=='Ready')].status}{'\\n'}{end}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get spot-handler daemonset")
				g.Expect(output).NotTo(BeEmpty(), "No spot-handler daemonsets found")
			}
			Eventually(verifyPodReady, 5*time.Minute).Should(Succeed())

			By("verifying component status is Available after upgrade")
			cmd = exec.Command("kubectl", "get", "component", components.ComponentNameSpotHandler,
				"-n", namespace,
				"-o", "jsonpath={.status.conditions}",
			)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get component status")
			Expect(output).To(ContainSubstring(`"type":"Available"`), "Component should be in Available status after upgrade")

			getClusterURL := fmt.Sprintf("%s/cluster-management/v1/organizations/%s/clusters/%s/components:view",
				apiURL, organizationID, clusterID)
			componentsResp := struct {
				Components []component `json:"components"`
			}{}
			err = fetchFromAPI(getClusterURL, http.MethodGet, &componentsResp)
			Expect(err).ToNot(HaveOccurred())
			agentComponent, ok := lo.Find(componentsResp.Components, func(item component) bool {
				return item.Name == components.ComponentNameSpotHandler
			})
			Expect(ok).To(BeTrue(), "Failed to find spot-handler component")
			Expect(agentComponent.LatestVersion).ToNot(BeEmpty(), "Failed to get latest version of spot-handler")
			Expect(agentComponent.UsedVersion).To(Equal(versionBeforeDowngrade))
		})

		It("should onboard phase2", func() {
			By("getting phase2 script")

			scriptResp := struct {
				Script string `json:"script"`
			}{}
			// nolint: lll
			getPhase2URL := fmt.Sprintf("%s/v1/kubernetes/external-clusters/%s/credentials-script?crossRole=true&nvidiaDevicePlugin=false&installSecurityAgent=true&installAutoscalerAgent=true&installGpuMetricsExporter=false&installNetflowExporter=false&installWorkloadAutoscaler=true&installPodMutator=false&installOmni=false",
				apiURL, clusterID)
			err := fetchFromAPI(getPhase2URL, http.MethodGet, &scriptResp)
			Expect(err).NotTo(HaveOccurred(), "Failed to get phase2 script")

			cmd := exec.Command("bash", "-c", scriptResp.Script)
			output, _ := utils.Run(cmd)
			// Phase2 script returns an error, but it's expected because it tries to
			// run "gcloud container clusters describe", but the cluster is not running in GKE.
			// Checking successful install of spot-handler and cluster-controller is enough for this test.
			Expect(output).To(ContainSubstring("cluster-controller ready with version"), "Failed to install cluster-controller")
			Expect(output).To(ContainSubstring("spot-handler ready with version "), "Phase2 spot handler install failed")
			// err = fetchFromAPI(getClusterURL, http.MethodGet, &componentsResp)
		})

		It("should offboard the operator and all components", func() {
			By("uninstalling the operator")
			cmd := exec.Command("helm", "uninstall", "castware-operator", "-n", namespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to uninstall helm release")

			By("verifying that CRDs don't exist anymore")
			verifyCRDsGone := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "crds", "-o", "name")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get CRDs")
				g.Expect(output).NotTo(ContainSubstring("castware.cast.ai"), "CRDs should be deleted")
			}
			Eventually(verifyCRDsGone).Should(Succeed())

			By("verifying that castai-agent still exists")
			cmd = exec.Command("kubectl", "get", "deployment", "-l", "app.kubernetes.io/name=castai-agent", "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "castai-agent should still exist after operator uninstall")

			By("verifying that spot-handler still exists")
			cmd = exec.Command("kubectl", "get", "daemonset", "-l", "app.kubernetes.io/instance=spot-handler", "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "spot-handler should still exist after operator uninstall")

			By("verifying that cluster-controller still exists")
			cmd = exec.Command("kubectl", "get", "deployment",
				"-l", "app.kubernetes.io/name=cluster-controller",
				"-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "cluster-controller should still exist after operator uninstall")

			By("deleting the namespace")
			cmd = exec.Command("kubectl", "delete", "ns", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete namespace")

			By("verifying that namespace is deleted")
			verifyNamespaceGone := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "ns", namespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "Namespace should be deleted")
			}
			Eventually(verifyNamespaceGone, 5*time.Minute).Should(Succeed())
		})

		It("should onboard agent and spot handler with legacy script", func() {
			By("getting phase1 script")

			var scriptResp string
			// nolint: lll
			getScriptURL := fmt.Sprintf("%s/v1/agent.sh?provider=gke", apiURL)
			err := fetchFromAPI(getScriptURL, http.MethodGet, &scriptResp)
			Expect(err).NotTo(HaveOccurred(), "Failed to get phase1 script")

			cmd := exec.Command("bash", "-c", scriptResp)
			output, _ := utils.Run(cmd)
			Expect(output).To(ContainSubstring("deployment.apps/castai-agent created"), "Agent not installed")
			Expect(output).To(ContainSubstring("daemonset.apps/castai-spot-handler created"), "Spot handler not installed")

			By("patching castai-agent deployment to add GKE environment variables")
			patchJSON := `{
				"spec": {
					"template": {
						"spec": {
							"containers": [{
								"name": "agent",
								"env": [
									{"name": "GKE_CLUSTER_NAME", "value": "castware-operator-e2e"},
									{"name": "GKE_LOCATION", "value": "e2e"},
									{"name": "GKE_PROJECT_ID", "value": "e2e"},
									{"name": "GKE_REGION", "value": "e2e"}
								]
							}]
						}
					}
				}
			}`
			cmd = exec.Command("kubectl", "patch", "deployment", "castai-agent",
				"-n", namespace,
				"--type=strategic",
				"-p", patchJSON)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to patch castai-agent deployment")

			By("waiting for deployment to be updated")
			verifyDeploymentUpdated := func(g Gomega) {
				cmd := exec.Command("kubectl", "rollout", "status", "deployment/castai-agent", "-n", namespace, "--timeout=60s")
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Deployment rollout failed")
			}
			Eventually(verifyDeploymentUpdated, 2*time.Minute).Should(Succeed())

			By("verifying at least one castai-agent pod is in ready state")
			verifyAgentPodReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", "app.kubernetes.io/name=castai-agent",
					"-n", namespace,
					"-o", "jsonpath={range .items[*]}{.metadata.name}{'|'}{.status.conditions[?(@.type=='Ready')].status}{'\\n'}{end}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get castai-agent pods")
				g.Expect(output).NotTo(BeEmpty(), "No castai-agent pods found")

				lines := utils.GetNonEmptyLines(output)
				g.Expect(lines).ToNot(BeEmpty(), "No castai-agent pods found")

				foundReady := false
				for _, line := range lines {
					if podReady(line) {
						foundReady = true
						break
					}
				}
				g.Expect(foundReady).To(BeTrue(), "No castai-agent pods are in Ready state")
			}
			Eventually(verifyAgentPodReady, 5*time.Minute).Should(Succeed())
		})

		It("should install the operator and take over agent and spot handler", func() {
			By("installing the operator")
			cmd := exec.Command("helm", "upgrade", "--install", "castware-operator",
				"--namespace", namespace,
				"--set", fmt.Sprintf("image.repository=%s", imageParts[0]),
				"--set", fmt.Sprintf("image.tag=%s", imageParts[1]),
				"--set", "image.pullPolicy=IfNotPresent",
				"--set", fmt.Sprintf("apiKeySecret.apiKey=%s", apiKey),
				"--set", fmt.Sprintf("defaultCluster.api.apiUrl=%s", apiURL),
				"--set", "defaultCluster.provider=gke",
				"--set", "defaultCluster.terraform=false",
				"--set", "defaultCluster.migrationMode=autoUpgrade",
				"--set", "defaultComponents.enabled=false",
				"--set", "webhook.env.GKE_CLUSTER_NAME=castware-operator-e2e",
				"--set", "webhook.env.GKE_LOCATION=e2e",
				"--set", "webhook.env.GKE_PROJECT_ID=e2e",
				"--set", "webhook.env.GKE_REGION=e2e",
				"--atomic",
				"--timeout", "5m",
				operatorChartPath,
			)

			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to install helm release")

			By("waiting for castai-agent component to be ready")
			verifyAgentComponent := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "component", components.ComponentNameAgent,
					"-n", namespace,
					"-o", "jsonpath={.status.currentVersion}",
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get castai-agent component CR")
				g.Expect(output).NotTo(BeEmpty(), "castai-agent version is not set")
			}
			Eventually(verifyAgentComponent, 5*time.Minute).Should(Succeed())

			By("verifying castai-agent component status is Available")
			cmd = exec.Command("kubectl", "get", "component", components.ComponentNameAgent,
				"-n", namespace,
				"-o", "jsonpath={.status.conditions}",
			)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get castai-agent component status")
			Expect(output).To(ContainSubstring(`"type":"Available"`), "castai-agent component should be Available")

			By("waiting for spot-handler component to be ready")
			verifySpotHandlerComponent := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "component", components.ComponentNameSpotHandler,
					"-n", namespace,
					"-o", "jsonpath={.status.currentVersion}",
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get spot-handler component CR")
				g.Expect(output).NotTo(BeEmpty(), "spot-handler version is not set")
			}
			Eventually(verifySpotHandlerComponent, 5*time.Minute).Should(Succeed())

			By("verifying spot-handler component status is Available")
			cmd = exec.Command("kubectl", "get", "component", components.ComponentNameSpotHandler,
				"-n", namespace,
				"-o", "jsonpath={.status.conditions}",
			)
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get spot-handler component status")
			Expect(output).To(ContainSubstring(`"type":"Available"`), "spot-handler component should be Available")
		})

		It("should onboard phase2", func() {
			By("getting phase2 script")

			scriptResp := struct {
				Script string `json:"script"`
			}{}
			// nolint: lll
			getPhase2URL := fmt.Sprintf("%s/v1/kubernetes/external-clusters/%s/credentials-script?crossRole=true&nvidiaDevicePlugin=false&installSecurityAgent=true&installAutoscalerAgent=true&installGpuMetricsExporter=false&installNetflowExporter=false&installWorkloadAutoscaler=true&installPodMutator=false&installOmni=false",
				apiURL, clusterID)
			err := fetchFromAPI(getPhase2URL, http.MethodGet, &scriptResp)
			Expect(err).NotTo(HaveOccurred(), "Failed to get phase2 script")

			cmd := exec.Command("bash", "-c", scriptResp.Script)
			output, _ := utils.Run(cmd)
			// Phase2 script returns an error, but it's expected because it tries to
			// run "gcloud container clusters describe", but the cluster is not running in GKE.
			// Checking successful install of spot-handler and cluster-controller is enough for this test.
			Expect(output).To(ContainSubstring("cluster-controller ready with version"), "Failed to install cluster-controller")
			Expect(output).To(ContainSubstring("spot-handler ready with version "), "Phase2 spot handler install failed")

			By("verifying spot-handler component CR exists and is ready")
			verifySpotHandlerComponent := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "component", components.ComponentNameSpotHandler,
					"-n", namespace,
					"-o", "jsonpath={.status.currentVersion}",
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get spot-handler component CR")
				g.Expect(output).NotTo(BeEmpty(), "spot-handler component version is not set")
			}
			Eventually(verifySpotHandlerComponent, 5*time.Minute).Should(Succeed())

			By("verifying spot-handler component status is Available")
			cmd = exec.Command("kubectl", "get", "component", components.ComponentNameSpotHandler,
				"-n", namespace,
				"-o", "jsonpath={.status.conditions}",
			)
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get spot-handler component status")
			Expect(output).To(ContainSubstring(`"type":"Available"`), "spot-handler component should be Available")

			By("verifying spot-handler has phase2Permissions=true from helm values")
			cmd = exec.Command("helm", "get", "values", "spot-handler", // castai-spot-handler
				"-n", namespace,
				"-o", "json",
			)
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get spot-handler helm values")
			Expect(output).To(ContainSubstring(`"phase2Permissions":true`),
				"spot-handler should have phase2Permissions enabled in helm values")

			By("verifying cluster-controller component CR exists and is ready")
			verifyClusterControllerComponent := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "component", components.ComponentNameClusterController,
					"-n", namespace,
					"-o", "jsonpath={.status.currentVersion}",
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get cluster-controller component CR")
				g.Expect(output).NotTo(BeEmpty(), "cluster-controller component version is not set")
			}
			Eventually(verifyClusterControllerComponent, 5*time.Minute).Should(Succeed())

			By("verifying cluster-controller component status is Available")
			cmd = exec.Command("kubectl", "get", "component", components.ComponentNameClusterController,
				"-n", namespace,
				"-o", "jsonpath={.status.conditions}",
			)
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get cluster-controller component status")
			Expect(output).To(ContainSubstring(`"type":"Available"`), "cluster-controller component should be Available")
		})
		// +kubebuilder:scaffold:e2e-webhooks-checks
	})
})

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	// Temporary file to store the token request
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		// Execute kubectl command to create the token
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		// Parse the JSON output to extract the token
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() string {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	metricsOutput, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
	Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
	return metricsOutput
}

func podReady(line string) bool {
	return len(line) > 0 && (line[len(line)-4:] == "True" || line[len(line)-1:] == "T")
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}

type component struct {
	Name          string `json:"name"`
	UsedVersion   string `json:"usedVersion"`
	LatestVersion string `json:"latestVersion"`
}
