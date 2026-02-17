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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"

	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/test/utils"
)

// namespace where the project is deployed in
const namespace = "castai-agent"

// serviceAccountName created for the project
const serviceAccountName = "castware-operator-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "castware-operator"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "castware-operator-metrics-binding"

// patch for castai-agent deployment to add GKE environment variables
const patchAgentDeploymentJSON = `{
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

const patchChartMuseumDeploymentJSON = `{
	"spec": {
		"template": {
			"spec": {
				"containers": [{
					"name": "chartmuseum",
					"env": [
						{"name": "CHART_URL", "value": "http://chartmuseum.registry.svc.cluster.local:8080"}
					]
				}]
			}
		}
	}
}`

// To run the tests enable wire-castware-skip-version-check feature flag for the test organization,
// otherwise the self upgrade test will fail

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string
	var clusterID string
	var organizationID string
	var apiKey string
	var agentInstalled bool
	var spotHandlerInstalled bool
	var versionBeforeDowngrade string
	var operatorChartPath string
	var helmRegistryManifestPath string

	// Helper instances
	var componentHelper *ComponentHelper
	var podHelper *PodHelper
	var deploymentHelper *DeploymentHelper
	var clusterHelper *ClusterHelper
	var helmHelper *HelmHelper
	var apiHelper *APIHelper
	var secretHelper *SecretHelper
	var namespaceHelper *NamespaceHelper

	// Extract image repository and tag from projectImage (format: repository:tag)
	imageParts := strings.Split(projectImage, ":")
	Expect(imageParts).To(HaveLen(2), "invalid projectImage format")

	var apiURL = os.Getenv("API_URL")
	if apiURL == "" {
		apiURL = "https://api.dev-master.cast.ai"
	}

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		wd, _ := os.Getwd()
		fmt.Println("Running e2e tests...", wd)
		apiKey = os.Getenv("API_KEY")
		Expect(apiKey).NotTo(BeEmpty(), "API_KEY environment variable is not set")

		// Initialize helper instances
		componentHelper = NewComponentHelper(namespace)
		podHelper = NewPodHelper(namespace)
		deploymentHelper = NewDeploymentHelper(namespace)
		clusterHelper = NewClusterHelper(namespace)
		helmHelper = NewHelmHelper(namespace)
		apiHelper = NewAPIHelper(apiKey, apiURL)
		secretHelper = NewSecretHelper(namespace)
		namespaceHelper = NewNamespaceHelper()

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
		helmRegistryManifestPath = filepath.Join(wd, "local", "registry.yaml")

		By("installing helm registry")
		cmd = exec.Command("kubectl", "apply", "-f", helmRegistryManifestPath)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install helm registry")

		Eventually(verifyPodReady, 5*time.Minute).WithArguments("app", "chartmuseum", "registry").Should(Succeed())

		cmd = exec.Command("helm", "repo", "add", "local", "http://localhost:5001/helm-charts")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed add helm registry")

		By("adding helm chart to local registry")
		cmd = exec.Command("helm", "package", "./charts/castai-castware-operator")
		output, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to package helm chart")
		chartPath := strings.Split(output, "/")
		chartPackage := strings.TrimSuffix(chartPath[len(chartPath)-1], "\n")

		By(fmt.Sprintf("uploading helm chart %s to local registry", chartPackage))
		// Upload helm chart to ChartMuseum using native Go HTTP client
		chartFile, err := os.Open(chartPackage)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to open chart package: %s", chartPackage))
		// nolint: errcheck
		defer chartFile.Close()

		resp, err := http.Post("http://localhost:5001/helm-charts/api/charts", "application/gzip", chartFile)
		Expect(err).NotTo(HaveOccurred(), "Failed to upload helm chart")
		// nolint: errcheck
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred(), "Failed to read response body")
		output = string(body)

		Expect(resp.StatusCode).To(Equal(http.StatusCreated),
			fmt.Sprintf("Failed to upload helm chart: %s - Status: %d, Response: %s", chartPackage, resp.StatusCode, output))
		Expect(output).To(ContainSubstring("{\"saved\":true}"),
			fmt.Sprintf("Failed to upload helm chart: %s - %s", chartPackage, output))

		cmd = exec.Command("helm", "repo", "update", "local")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to update helm repo")

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
			"local/castware-operator",
		)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install helm chart")

		By("patching chartmuseum deployment to use internal registry url")
		cmd = exec.Command("kubectl", "patch", "deployment", "chartmuseum",
			"-n", "registry",
			"--type=strategic",
			"-p", patchChartMuseumDeploymentJSON)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to patch chartmuseum deployment")

		cmd = exec.Command("kubectl", "rollout", "restart", "deployment", "chartmuseum", "-n", "registry")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to restart chartmuseum deployment")

	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("cleaning up the curl pod for metrics")

		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		// Delete cluster from Cast AI API if cluster ID was set
		if clusterID != "" && apiHelper != nil {
			By(fmt.Sprintf("deleting cluster from Cast AI API: %s", clusterID))
			err := apiHelper.DeleteCluster(clusterID)
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
		_ = helmHelper.UninstallOperator()

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)

		By("removing metrics cluster role binding")
		cmd = exec.Command("kubectl", "delete", "clusterrolebinding", metricsRoleBindingName)
		_, _ = utils.Run(cmd)

		By("deleting helm registry")
		cmd = exec.Command("kubectl", "delete", "namespace", "registry")
		_, _ = utils.Run(cmd)

		By("deleting cluster roles and cluster role bindings")
		err := deleteClusterRoleResourcesWithAnnotation()
		Expect(err).NotTo(HaveOccurred(), "castai cluster roles should be deleted")
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
			err := secretHelper.CreateAPIKeySecret(secretName, apiKey)
			Expect(err).NotTo(HaveOccurred(), "Failed to create API key secret")

			By("creating a cluster custom resource")
			err = clusterHelper.CreateFromYAML(clusterName, secretName, apiURL)
			Expect(err).NotTo(HaveOccurred(), "Failed to create cluster CR")

			By("waiting for the cluster to be onboarded and get a cluster ID")
			verifyClusterID := func(g Gomega) {
				clusterID = clusterHelper.VerifyClusterID(g, clusterName)
			}
			Eventually(verifyClusterID, 5*time.Minute).Should(Succeed())

			By("verifying cluster name and location are also populated")
			cmd := exec.Command("kubectl", "get", "cluster", clusterName, "-n", namespace, "-o", "jsonpath={.spec.cluster}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get cluster metadata")
			Expect(output).To(ContainSubstring("clusterID"), "Cluster metadata should contain clusterID")

			clusterResp, err := apiHelper.GetCluster(clusterID)
			Expect(err).ToNot(HaveOccurred())
			organizationID = clusterResp["organizationId"].(string)
		})

		It("should install castai-agent", func() {
			By("creating a component custom resource")
			err := componentHelper.CreateFromYAML(
				components.ComponentNameAgent,
				components.ComponentNameAgent,
				"",
			)
			Expect(err).NotTo(HaveOccurred(), "Failed to create component CR")

			By("waiting for castai-agent component to have a version")
			Eventually(componentHelper.VerifyVersionIsSet, 5*time.Minute).
				WithArguments(components.ComponentNameAgent).
				Should(Succeed())

			agentInstalled = true

			By("verifying at least one castai-agent pod is in ready state")
			Eventually(podHelper.VerifyPodsReady, 5*time.Minute).
				WithArguments("app.kubernetes.io/name", "castai-agent").
				Should(Succeed())

			By("verifying component status conditions")
			err = componentHelper.VerifyStatusCondition(components.ComponentNameAgent, "Available")
			Expect(err).NotTo(HaveOccurred(), "Component should be in Available status")

			clusterResp, err := apiHelper.GetCluster(clusterID)
			Expect(err).ToNot(HaveOccurred())
			Expect(clusterResp["status"]).To(Equal("ready"))
			Expect(clusterResp["castwareInstallMethod"]).To(Equal("OPERATOR"))
		})

		It("should downgrade castai-agent", func() {
			By("getting current castai-agent version")
			currentVersion, err := componentHelper.GetCurrentVersion(components.ComponentNameAgent)
			Expect(err).NotTo(HaveOccurred(), "Failed to get component current version")
			Expect(currentVersion).NotTo(BeEmpty(), "Current version is not set")

			By(fmt.Sprintf("current version is: %s", currentVersion))
			versionBeforeDowngrade = currentVersion

			// Define a known older version to downgrade to
			downgradeVersion := "0.125.0"

			By(fmt.Sprintf("patching component to downgrade to version %s", downgradeVersion))
			err = componentHelper.PatchVersion(components.ComponentNameAgent, downgradeVersion)
			Expect(err).NotTo(HaveOccurred(), "Failed to patch component with downgrade version")

			By("waiting for the component to be downgraded")
			Eventually(componentHelper.VerifyVersion, 5*time.Minute).
				WithArguments(components.ComponentNameAgent, downgradeVersion).
				Should(Succeed())

			By("verifying at least one castai-agent pod is ready after downgrade")
			Eventually(podHelper.VerifyPodsReady, 5*time.Minute).
				WithArguments("app.kubernetes.io/name", "castai-agent").
				Should(Succeed())

			err = deploymentHelper.VerifyDeploymentExists("helm.sh/chart=castai-agent", downgradeVersion)
			Expect(err).NotTo(HaveOccurred(), "Failed to get castai-agent deployment")

			By("verifying component status is Available after downgrade")
			err = componentHelper.VerifyStatusCondition(components.ComponentNameAgent, "Available")
			Expect(err).NotTo(HaveOccurred(), "Component should be in Available status after downgrade")

			componentList, err := apiHelper.GetClusterComponents(organizationID, clusterID)
			Expect(err).ToNot(HaveOccurred())
			agentComponent, ok := FindComponentByName(componentList, components.ComponentNameAgent)
			Expect(ok).To(BeTrue(), "Failed to find castai-agent component")
			Expect(agentComponent.UsedVersion).To(Equal(downgradeVersion))
		})

		It("should upgrade castai-agent", func() {
			componentName := "castai-agent"

			By("getting current castai-agent version before upgrade")
			versionBeforeUpgrade, err := componentHelper.GetCurrentVersion(componentName)
			Expect(err).NotTo(HaveOccurred(), "Failed to get component current version")
			Expect(versionBeforeUpgrade).NotTo(BeEmpty(), "Current version is not set")

			By(fmt.Sprintf("current version before upgrade is: %s", versionBeforeUpgrade))

			By("patching component to upgrade to latest version by setting version to empty string")
			err = componentHelper.PatchVersion(componentName, "")
			Expect(err).NotTo(HaveOccurred(), "Failed to patch component to upgrade")

			By("waiting for the component to be upgraded to a newer version")
			Eventually(componentHelper.VerifyVersionChanged, 5*time.Minute).
				WithArguments(componentName, versionBeforeUpgrade).
				Should(Succeed())

			By("getting new version after upgrade")
			versionAfterUpgrade, err := componentHelper.GetCurrentVersion(componentName)
			Expect(err).NotTo(HaveOccurred(), "Failed to get component version after upgrade")
			By(fmt.Sprintf("upgraded to version: %s", versionAfterUpgrade))

			By("verifying at least one castai-agent pod is ready after upgrade")
			Eventually(podHelper.VerifyPodsReady, 5*time.Minute).
				WithArguments("app.kubernetes.io/name", "castai-agent").
				Should(Succeed())

			err = deploymentHelper.VerifyDeploymentExists("helm.sh/chart=castai-agent", strings.TrimSpace(versionAfterUpgrade))
			Expect(err).NotTo(HaveOccurred(), "Failed to get castai-agent deployment")

			By("verifying component status is Available after upgrade")
			err = componentHelper.VerifyStatusCondition(componentName, "Available")
			Expect(err).NotTo(HaveOccurred(), "Component should be in Available status after upgrade")

			componentList, err := apiHelper.GetClusterComponents(organizationID, clusterID)
			Expect(err).ToNot(HaveOccurred())
			agentComponent, ok := FindComponentByName(componentList, componentName)
			Expect(ok).To(BeTrue(), "Failed to find castai-agent component")
			Expect(agentComponent.LatestVersion).ToNot(BeEmpty(), "Failed to get latest version of castai-agent")
			Expect(agentComponent.UsedVersion).To(Equal(versionBeforeDowngrade))
		})

		It("should install spot-handler", func() {
			By("creating a component custom resource")
			err := componentHelper.CreateFromYAML(
				components.ComponentNameSpotHandler,
				components.ComponentNameSpotHandler,
				"    phase2Permissions: false",
			)
			Expect(err).NotTo(HaveOccurred(), "Failed to create component CR")

			spotHandlerInstalled = true

			By("waiting for spot-handler component to have a version")
			Eventually(componentHelper.VerifyVersionIsSet, 5*time.Minute).
				WithArguments(components.ComponentNameSpotHandler).
				Should(Succeed())

			By("verifying that spot-handler daemonset is in ready state")
			verifyPodReady := func(g Gomega) {
				// Get pods with label app.kubernetes.io/name=castai-spot-handler
				cmd := exec.Command("kubectl", "get", "daemonsets",
					"-l", "app.kubernetes.io/instance=castai-spot-handler",
					"-n", namespace,
					"-o", "jsonpath={range .items[*]}{.metadata.name}{'|'}{.status.conditions[?(@.type=='Ready')].status}{'\\n'}{end}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get spot-handler daemonset")
				g.Expect(output).NotTo(BeEmpty(), "No spot-handler daemonsets found")
			}
			Eventually(verifyPodReady, 5*time.Minute).Should(Succeed())

			By("verifying component status conditions")
			err = componentHelper.VerifyStatusCondition(components.ComponentNameSpotHandler, "Available")
			Expect(err).NotTo(HaveOccurred(), "Component should be in Available status")
		})

		It("should downgrade spot-handler", func() {
			By("getting current spot-handler version")
			currentVersion, err := componentHelper.GetCurrentVersion(components.ComponentNameSpotHandler)
			Expect(err).NotTo(HaveOccurred(), "Failed to get component current version")
			Expect(currentVersion).NotTo(BeEmpty(), "Current version is not set")

			By(fmt.Sprintf("current version is: %s", currentVersion))
			versionBeforeDowngrade = currentVersion

			// Spot handler supports phase1 only permissions from 0.29.0 onwards,
			// downgrading to a lower version won't work in phase1 because the operator doesn't have permissions.
			downgradeVersion := "0.29.0"

			By(fmt.Sprintf("patching component to downgrade to version %s", downgradeVersion))
			err = componentHelper.PatchVersion(components.ComponentNameSpotHandler, downgradeVersion)
			Expect(err).NotTo(HaveOccurred(), "Failed to patch component with downgrade version")

			By("waiting for the component to be downgraded")
			Eventually(componentHelper.VerifyVersion, 5*time.Minute).
				WithArguments(components.ComponentNameSpotHandler, downgradeVersion).
				Should(Succeed())

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
			err = componentHelper.VerifyStatusCondition(components.ComponentNameSpotHandler, "Available")
			Expect(err).NotTo(HaveOccurred(), "Component should be in Available status after downgrade")

			componentList, err := apiHelper.GetClusterComponents(organizationID, clusterID)
			Expect(err).ToNot(HaveOccurred())
			spotHandlerComponent, ok := FindComponentByName(componentList, components.ComponentNameSpotHandler)
			Expect(ok).To(BeTrue(), "Failed to find spot-handler component")
			Expect(spotHandlerComponent.UsedVersion).To(Equal(downgradeVersion))
		})

		It("should upgrade spot-handler", func() {
			Skip("Spot handler has only one compatible version, so upgrade test is not possible")
			By("getting current spot-handler version before upgrade")
			versionBeforeUpgrade, err := componentHelper.GetCurrentVersion(components.ComponentNameSpotHandler)
			Expect(err).NotTo(HaveOccurred(), "Failed to get component current version")
			Expect(versionBeforeUpgrade).NotTo(BeEmpty(), "Current version is not set")

			By(fmt.Sprintf("current version before upgrade is: %s", versionBeforeUpgrade))

			By("patching component to upgrade to latest version by setting version to empty string")
			err = componentHelper.PatchVersion(components.ComponentNameSpotHandler, "")
			Expect(err).NotTo(HaveOccurred(), "Failed to patch component to upgrade")

			By("waiting for the component to be upgraded to a newer version")
			Eventually(componentHelper.VerifyVersionChanged, 5*time.Minute).
				WithArguments(components.ComponentNameSpotHandler, versionBeforeUpgrade).
				Should(Succeed())

			By("getting new version after upgrade")
			versionAfterUpgrade, err := componentHelper.GetCurrentVersion(components.ComponentNameSpotHandler)
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
			err = componentHelper.VerifyStatusCondition(components.ComponentNameSpotHandler, "Available")
			Expect(err).NotTo(HaveOccurred(), "Component should be in Available status after upgrade")

			componentList, err := apiHelper.GetClusterComponents(organizationID, clusterID)
			Expect(err).ToNot(HaveOccurred())
			spotHandlerComponent, ok := FindComponentByName(componentList, components.ComponentNameSpotHandler)
			Expect(ok).To(BeTrue(), "Failed to find spot-handler component")
			Expect(spotHandlerComponent.LatestVersion).ToNot(BeEmpty(), "Failed to get latest version of spot-handler")
			Expect(spotHandlerComponent.UsedVersion).To(Equal(versionBeforeDowngrade))
		})

		It("should onboard phase2", func() {
			By("getting phase2 script")

			scriptResp := struct {
				Script string `json:"script"`
			}{}
			// nolint: lll
			getPhase2URL := fmt.Sprintf("%s/v1/kubernetes/external-clusters/%s/credentials-script?crossRole=true&nvidiaDevicePlugin=false&installSecurityAgent=true&installAutoscalerAgent=true&installGpuMetricsExporter=false&installNetflowExporter=false&installWorkloadAutoscaler=true&installPodMutator=false&installOmni=false",
				apiURL, clusterID)
			err := apiHelper.FetchFromAPI(getPhase2URL, http.MethodGet, nil, &scriptResp)
			Expect(err).NotTo(HaveOccurred(), "Failed to get phase2 script")

			phase2Script := strings.ReplaceAll(scriptResp.Script, "PREFLIGHT_CHECKS=true", "PREFLIGHT_CHECKS=false")
			cmd := exec.Command("bash", "-c", phase2Script)
			output, _ := utils.Run(cmd)
			// Phase2 script returns an error, but it's expected because it tries to
			// run "gcloud container clusters describe", but the cluster is not running in GKE.
			// Checking successful install of spot-handler and cluster-controller is enough for this test.
			Expect(output).To(ContainSubstring("cluster-controller ready with version"), "Failed to install cluster-controller")
			Expect(output).To(ContainSubstring("spot-handler ready with version "), "Phase2 spot handler install failed")
		})

		It("should offboard the operator and all components", func() {
			By("uninstalling the operator")
			err := helmHelper.UninstallOperator()
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
			cmd := exec.Command("kubectl", "get", "deployment", "-l", "app.kubernetes.io/name=castai-agent", "-n", namespace)
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
			err = namespaceHelper.Delete(namespace)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete namespace")

			By("verifying that namespace is deleted")
			Eventually(namespaceHelper.VerifyDeleted, 5*time.Minute).
				WithArguments(namespace).
				Should(Succeed())
		})

		It("should onboard agent and spot handler with legacy script", func() {
			By("getting phase1 script")

			var scriptResp string
			// nolint: lll
			getScriptURL := fmt.Sprintf("%s/v1/agent.sh?provider=gke", apiURL)
			err := apiHelper.FetchFromAPI(getScriptURL, http.MethodGet, nil, &scriptResp)
			Expect(err).NotTo(HaveOccurred(), "Failed to get phase1 script")

			cmd := exec.Command("bash", "-c", scriptResp)
			output, _ := utils.Run(cmd)
			Expect(output).To(ContainSubstring("deployment.apps/castai-agent created"), "Agent not installed")
			Expect(output).To(ContainSubstring("daemonset.apps/castai-spot-handler created"), "Spot handler not installed")

			By("patching castai-agent deployment to add GKE environment variables")
			cmd = exec.Command("kubectl", "patch", "deployment", "castai-agent",
				"-n", namespace,
				"--type=strategic",
				"-p", patchAgentDeploymentJSON)
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
			Eventually(podHelper.VerifyPodsReady, 5*time.Minute).
				WithArguments("app.kubernetes.io/name", "castai-agent").
				Should(Succeed())
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
			Eventually(componentHelper.VerifyVersionIsSet, 5*time.Minute).
				WithArguments(components.ComponentNameAgent).
				Should(Succeed())

			By("verifying castai-agent component status is Available")
			err = componentHelper.VerifyStatusCondition(components.ComponentNameAgent, "Available")
			Expect(err).NotTo(HaveOccurred(), "castai-agent component should be Available")

			By("waiting for spot-handler component to be ready")
			Eventually(componentHelper.VerifyVersionIsSet, 5*time.Minute).
				WithArguments(components.ComponentNameSpotHandler).
				Should(Succeed())

			By("verifying spot-handler component status is Available")
			err = componentHelper.VerifyStatusCondition(components.ComponentNameSpotHandler, "Available")
			Expect(err).NotTo(HaveOccurred(), "spot-handler component should be Available")
		})

		It("should not delete namespace when migrating from legacy agent installation", func() {
			By("deleting any existing operator installation")
			cmd := exec.Command("helm", "uninstall", "castware-operator", "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)

			By("waiting for CRDs to be deleted")
			verifyCRDsGone := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "crds", "-o", "name")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get CRDs")
				g.Expect(output).NotTo(ContainSubstring("castware.cast.ai"), "CRDs should be deleted")
			}
			Eventually(verifyCRDsGone, 2*time.Minute).Should(Succeed())

			By("deleting the namespace if it exists")
			cmd = exec.Command("kubectl", "delete", "ns", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)

			By("waiting for namespace to be fully deleted")
			Eventually(namespaceHelper.VerifyDeleted, 2*time.Minute).
				WithArguments(namespace).
				Should(Succeed())

			By("installing legacy castai-agent with createNamespace=true")
			cmd = exec.Command("kubectl", "create", "namespace", namespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

			// Add helm labels and annotations to simulate legacy installation
			cmd = exec.Command("kubectl", "label", "namespace", namespace,
				"app.kubernetes.io/instance=castai-agent",
				"app.kubernetes.io/managed-by=Helm",
				"app.kubernetes.io/name=castai-agent")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to label namespace")

			cmd = exec.Command("kubectl", "annotate", "namespace", namespace,
				"meta.helm.sh/release-name=castai-agent",
				"meta.helm.sh/release-namespace=castai-agent")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to annotate namespace")

			// Install legacy agent with createNamespace=true so namespace is in manifest
			cmd = exec.Command("helm", "install", "castai-agent", "castai-helm/castai-agent",
				"--namespace", namespace,
				"--set", "createNamespace=true",
				"--set", fmt.Sprintf("apiKey=%s", apiKey),
				"--set", "provider=gke",
				"--set", "additionalEnv.GKE_CLUSTER_NAME=castware-operator-e2e",
				"--set", "additionalEnv.GKE_LOCATION=e2e",
				"--set", "additionalEnv.GKE_PROJECT_ID=e2e",
				"--set", "additionalEnv.GKE_REGION=e2e",
				"--version", "0.84.2")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to install legacy castai-agent")

			By("verifying namespace is part of the helm release manifest")
			verifyNamespaceInManifest := func(g Gomega) {
				cmd := exec.Command("helm", "get", "manifest", "castai-agent", "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get helm manifest")
				g.Expect(output).To(ContainSubstring("kind: Namespace"), "Namespace should be in helm manifest")
			}
			Eventually(verifyNamespaceInManifest).Should(Succeed())

			By("verifying at least one castai-agent pod is ready")
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

			By("storing namespace UID before operator installation")
			cmd = exec.Command("kubectl", "get", "namespace", namespace, "-o", "jsonpath={.metadata.uid}")
			namespaceUIDBefore, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get namespace UID")
			Expect(namespaceUIDBefore).NotTo(BeEmpty(), "Namespace UID should not be empty")

			By("installing operator with migrationMode=autoUpgrade to trigger agent upgrade")
			cmd = exec.Command("helm", "upgrade", "--install", "castware-operator",
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
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to install operator")

			By("verifying namespace still exists with same UID (not deleted and recreated)")
			cmd = exec.Command("kubectl", "get", "namespace", namespace, "-o", "jsonpath={.metadata.uid}")
			namespaceUIDAfter, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get namespace UID after operator install")
			Expect(namespaceUIDAfter).To(Equal(namespaceUIDBefore), "Namespace UID should be the same")

			By("verifying namespace still has helm annotations")
			cmd = exec.Command("kubectl", "get", "namespace", namespace,
				"-o", "jsonpath={.metadata.annotations.meta\\.helm\\.sh/release-name}")
			releaseAnnotation, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get namespace annotations")
			Expect(releaseAnnotation).To(Equal("castai-agent"), "Namespace should still have helm release annotation")

			By("waiting for castai-agent component to be upgraded")
			verifyAgentComponentUpgraded := func(g Gomega) {
				version, err := componentHelper.GetCurrentVersion(components.ComponentNameAgent)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get castai-agent component")
				g.Expect(version).NotTo(BeEmpty(), "Component version should be set")
				g.Expect(version).NotTo(Equal("0.84.2"), "Component should be upgraded to newer version")
			}
			Eventually(verifyAgentComponentUpgraded, 5*time.Minute).Should(Succeed())

			By("verifying castai-agent component is Available")
			err = componentHelper.VerifyStatusCondition(components.ComponentNameAgent, "Available")
			Expect(err).NotTo(HaveOccurred(), "Component should be Available after upgrade")

			By("verifying castai-agent pods are ready after upgrade")
			Eventually(verifyAgentPodReady, 5*time.Minute).Should(Succeed())

			By("verifying helm values have createNamespace=true")
			cmd = exec.Command("helm", "get", "values", "castai-agent", "-n", namespace, "-o", "json")
			valuesOutput, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get helm values")
			Expect(valuesOutput).To(ContainSubstring(`"createNamespace":true`),
				"Helm values should have createNamespace=true to prevent namespace deletion")

			By("verifying namespace is still in the helm manifest after upgrade")
			cmd = exec.Command("helm", "get", "manifest", "castai-agent", "-n", namespace)
			manifestOutput, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get helm manifest after upgrade")
			Expect(manifestOutput).To(ContainSubstring("kind: Namespace"),
				"Namespace should still be in helm manifest after upgrade")
		})

		It("should downgrade agent if CR changed when no helm labels on namespace", func() {
			By("removing helm labels from namespace")
			cmd := exec.Command("kubectl", "label", "namespace", namespace,
				"app.kubernetes.io/instance-",
				"app.kubernetes.io/managed-by-",
				"app.kubernetes.io/name-")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to remove namespace labels")

			By("removing helm annotations from namespace")
			cmd = exec.Command("kubectl", "annotate", "namespace", namespace,
				"meta.helm.sh/release-name-",
				"meta.helm.sh/release-namespace-")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to remove namespace annotations")

			downgradeVersion := "0.134.0"

			By(fmt.Sprintf("patching castai-agent component to downgrade to version %s", downgradeVersion))
			err = componentHelper.PatchVersion(components.ComponentNameAgent, downgradeVersion)
			Expect(err).NotTo(HaveOccurred(), "Failed to patch component to downgrade version")

			By(fmt.Sprintf("waiting for castai-agent to be downgraded to version %s", downgradeVersion))
			Eventually(componentHelper.VerifyVersion, 5*time.Minute).
				WithArguments(components.ComponentNameAgent, downgradeVersion).
				Should(Succeed())

			By(fmt.Sprintf("verifying castai-agent deployment has the correct version label %s", downgradeVersion))
			err = deploymentHelper.VerifyDeploymentExists("helm.sh/chart=castai-agent", downgradeVersion)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("No castai-agent deployment found with version %s", downgradeVersion))

			By("verifying castai-agent component is Available after downgrade")
			err = componentHelper.VerifyStatusCondition(components.ComponentNameAgent, "Available")
			Expect(err).NotTo(HaveOccurred(), "Component should be Available after downgrade")

			By("verifying at least one castai-agent pod is ready after cleanup")
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

			By("verifying component status conditions after downgrade")
			err = componentHelper.VerifyStatusCondition(components.ComponentNameAgent, "Available")
			Expect(err).NotTo(HaveOccurred(), "Component should be in Available status")

			By("verifying namespace still has no helm labels after downgrade")
			cmd = exec.Command("kubectl", "get", "namespace", namespace,
				"-o", "jsonpath={.metadata.labels}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get namespace labels")
			Expect(output).NotTo(ContainSubstring("app.kubernetes.io/instance"),
				"Namespace should not have app.kubernetes.io/instance label")
			Expect(output).NotTo(ContainSubstring("app.kubernetes.io/managed-by"),
				"Namespace should not have app.kubernetes.io/managed-by label")
			Expect(output).NotTo(ContainSubstring("app.kubernetes.io/name"),
				"Namespace should not have app.kubernetes.io/name label")

			By("verifying namespace still has no helm annotations after downgrade")
			cmd = exec.Command("kubectl", "get", "namespace", namespace,
				"-o", "jsonpath={.metadata.annotations}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get namespace annotations")
			Expect(output).NotTo(ContainSubstring("meta.helm.sh/release-name"),
				"Namespace should not have meta.helm.sh/release-name annotation")
			Expect(output).NotTo(ContainSubstring("meta.helm.sh/release-namespace"),
				"Namespace should not have meta.helm.sh/release-namespace annotation")
		})

		It("should onboard phase2", func() {
			By("getting phase2 script")

			scriptResp := struct {
				Script string `json:"script"`
			}{}
			// nolint: lll
			getPhase2URL := fmt.Sprintf("%s/v1/kubernetes/external-clusters/%s/credentials-script?crossRole=true&nvidiaDevicePlugin=false&installSecurityAgent=true&installAutoscalerAgent=true&installGpuMetricsExporter=false&installNetflowExporter=false&installWorkloadAutoscaler=true&installPodMutator=false&installOmni=false",
				apiURL, clusterID)
			err := apiHelper.FetchFromAPI(getPhase2URL, http.MethodGet, nil, &scriptResp)
			Expect(err).NotTo(HaveOccurred(), "Failed to get phase2 script")

			phase2Script := strings.ReplaceAll(scriptResp.Script, "PREFLIGHT_CHECKS=true", "PREFLIGHT_CHECKS=false")
			cmd := exec.Command("bash", "-c", phase2Script)
			output, _ := utils.Run(cmd)
			// Phase2 script returns an error, but it's expected because it tries to
			// run "gcloud container clusters describe", but the cluster is not running in GKE.
			// Checking successful install of spot-handler and cluster-controller is enough for this test.
			Expect(output).To(ContainSubstring("cluster-controller ready with version"), "Failed to install cluster-controller")
			Expect(output).To(ContainSubstring("spot-handler ready with version "), "Phase2 spot handler install failed")

			By("verifying spot-handler component CR exists and is ready")
			Eventually(componentHelper.VerifyVersionIsSet, 5*time.Minute).
				WithArguments(components.ComponentNameSpotHandler).
				Should(Succeed())

			By("verifying spot-handler component status is Available")
			err = componentHelper.VerifyStatusCondition(components.ComponentNameSpotHandler, "Available")
			Expect(err).NotTo(HaveOccurred(), "spot-handler component should be Available")

			By("verifying cluster-controller component CR exists and is ready")
			Eventually(componentHelper.VerifyVersionIsSet, 5*time.Minute).
				WithArguments(components.ComponentNameClusterController).
				Should(Succeed())

			By("verifying cluster-controller component status is Available")
			err = componentHelper.VerifyStatusCondition(components.ComponentNameClusterController, "Available")
			Expect(err).NotTo(HaveOccurred(), "cluster-controller component should be Available")

			By("verifying spot-handler has phase2Permissions=true from helm values")
			verifyPhase2Permissions := func(g Gomega) {
				cmd := exec.Command("helm", "get", "values", "castai-spot-handler",
					"-n", namespace,
					"-o", "json",
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get spot-handler helm values")
				g.Expect(output).To(ContainSubstring(`"phase2Permissions":true`),
					"spot-handler should have phase2Permissions enabled in helm values")
			}
			Eventually(verifyPhase2Permissions, 2*time.Minute).Should(Succeed())
		})

		It("install with legacy scripts without operator, then operator takes over with extended permissions", func() {
			By("uninstalling the operator")
			err := helmHelper.UninstallOperator()
			Expect(err).NotTo(HaveOccurred(), "Failed to uninstall helm release")

			By("verifying that CRDs don't exist anymore")
			verifyCRDsGone := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "crds", "-o", "name")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get CRDs")
				g.Expect(output).NotTo(ContainSubstring("castware.cast.ai"), "CRDs should be deleted")
			}
			Eventually(verifyCRDsGone).Should(Succeed())

			By("deleting any existing agent components")
			cmd := exec.Command("kubectl", "delete", "deployment", "castai-agent", "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)

			By("deleting any existing spot-handler components")
			cmd = exec.Command("kubectl", "delete", "daemonset", "castai-spot-handler", "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)

			By("deleting the namespace")
			err = namespaceHelper.Delete(namespace)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete namespace")

			By("getting phase1 script")
			var scriptResp string
			getScriptURL := fmt.Sprintf("%s/v1/agent.sh?provider=gke", apiURL)
			err = apiHelper.FetchFromAPI(getScriptURL, http.MethodGet, nil, &scriptResp)
			Expect(err).NotTo(HaveOccurred(), "Failed to get phase1 script")

			cmd = exec.Command("bash", "-c", scriptResp)
			output, _ := utils.Run(cmd)
			Expect(output).To(ContainSubstring("deployment.apps/castai-agent created"), "Agent not installed")
			Expect(output).To(ContainSubstring("daemonset.apps/castai-spot-handler created"), "Spot handler not installed")

			By("patching castai-agent deployment to add GKE environment variables")
			cmd = exec.Command("kubectl", "patch", "deployment", "castai-agent",
				"-n", namespace,
				"--type=strategic",
				"-p", patchAgentDeploymentJSON)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to patch castai-agent deployment")

			By("waiting for castai-agent deployment to be updated")
			verifyDeploymentUpdated := func(g Gomega) {
				cmd := exec.Command("kubectl", "rollout", "status", "deployment/castai-agent", "-n", namespace, "--timeout=60s")
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Deployment rollout failed")
			}
			Eventually(verifyDeploymentUpdated, 2*time.Minute).Should(Succeed())

			By("verifying at least one castai-agent pod is in ready state after phase1")
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

			By("getting phase2 script with operator=false")
			scriptResp2 := struct {
				Script string `json:"script"`
			}{}
			// nolint: lll
			getPhase2URL := fmt.Sprintf("%s/v1/kubernetes/external-clusters/%s/credentials-script?crossRole=true&nvidiaDevicePlugin=false&installSecurityAgent=true&installAutoscalerAgent=true&installGpuMetricsExporter=false&installNetflowExporter=false&installWorkloadAutoscaler=true&installPodMutator=false&installOmni=false&installOperator=false",
				apiURL, clusterID)
			err = apiHelper.FetchFromAPI(getPhase2URL, http.MethodGet, nil, &scriptResp2)
			Expect(err).NotTo(HaveOccurred(), "Failed to get phase2 script")

			By("modifying phase2 script to set OPERATOR_MANAGED=false")
			// Replace OPERATOR_MANAGED=true with OPERATOR_MANAGED=false if it exists
			modifiedScript := strings.ReplaceAll(scriptResp2.Script, "OPERATOR_MANAGED=true", "OPERATOR_MANAGED=false")

			By("running phase2 script")
			cmd = exec.Command("bash", "-c", modifiedScript)
			output, _ = utils.Run(cmd)
			Expect(output).To(ContainSubstring("Finished installing castai-cluster-controller"),
				"Failed to install cluster-controller")
			Expect(output).To(ContainSubstring("Finished installing castai-spot-handler"),
				"Phase2 spot handler install failed")

			By("verifying castai-agent deployment is ready")
			verifyAgentDeploymentReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", "castai-agent",
					"-n", namespace,
					"-o", "jsonpath={.status.availableReplicas}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get castai-agent deployment")
				g.Expect(output).NotTo(BeEmpty(), "castai-agent has no available replicas")
				g.Expect(output).NotTo(Equal("0"), "castai-agent available replicas should not be 0")
			}
			Eventually(verifyAgentDeploymentReady, 5*time.Minute).Should(Succeed())

			By("verifying that spot-handler daemonset is in ready state")
			verifyPodReady := func(g Gomega) {
				// Get pods with label app.kubernetes.io/name=castai-spot-handler
				cmd := exec.Command("kubectl", "get", "daemonsets",
					"-l", "app.kubernetes.io/instance=castai-spot-handler",
					"-n", namespace,
					"-o", "jsonpath={range .items[*]}{.metadata.name}{'|'}{.status.conditions[?(@.type=='Ready')].status}{'\\n'}{end}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get spot-handler daemonset")
				g.Expect(output).NotTo(BeEmpty(), "No spot-handler daemonsets found")
			}
			Eventually(verifyPodReady, 5*time.Minute).Should(Succeed())

			By("verifying operator is not installed yet")
			cmd = exec.Command("kubectl", "get", "deployment",
				"-l", "app.kubernetes.io/instance=castware-operator",
				"-n", namespace, "-o", "name")
			out, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to check for operator deployment")
			Expect(out).To(BeEmpty(), "operator deployment should not exist yet")

			By("verifying Component CRDs do not exist yet")
			cmd = exec.Command("kubectl", "get", "crd", "components.castware.cast.ai", "-o", "name")
			out, err = utils.Run(cmd)
			Expect(err).To(HaveOccurred(), "Failed to check for Component CRD")
			Expect(out).To(ContainSubstring("not found"), "Component CRD should not exist yet")

			By("installing the operator with extendedPermissions=true")
			cmd = exec.Command("helm", "upgrade", "--install", "castware-operator",
				"--namespace", namespace,
				"--set", fmt.Sprintf("image.repository=%s", imageParts[0]),
				"--set", fmt.Sprintf("image.tag=%s", imageParts[1]),
				"--set", "image.pullPolicy=IfNotPresent",
				"--set", fmt.Sprintf("apiKeySecret.apiKey=%s", apiKey),
				"--set", fmt.Sprintf("defaultCluster.api.apiUrl=%s", apiURL),
				"--set", "defaultCluster.provider=gke",
				"--set", "defaultCluster.terraform=false",
				"--set", "defaultCluster.extendedPermissions=true",
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
			Expect(err).NotTo(HaveOccurred(), "Failed to install helm release with extended permissions")

			By("waiting for castai-agent component CR to be created")
			Eventually(componentHelper.VerifyVersionIsSet, 1*time.Minute).
				WithArguments(components.ComponentNameAgent).
				Should(Succeed())

			By("verifying castai-agent component status is Available")
			err = componentHelper.VerifyStatusCondition(components.ComponentNameAgent, "Available")
			Expect(err).NotTo(HaveOccurred(), "castai-agent component should be Available")

			By("waiting for spot-handler component CR to be created")
			Eventually(componentHelper.VerifyVersionIsSet, 1*time.Minute).
				WithArguments(components.ComponentNameSpotHandler).
				Should(Succeed())

			By("verifying spot-handler component status is Available")
			err = componentHelper.VerifyStatusCondition(components.ComponentNameSpotHandler, "Available")
			Expect(err).NotTo(HaveOccurred(), "spot-handler component should be Available")
		})

		It("should offboard the operator and all phase2 components", func() {
			By("uninstalling the operator")
			err := helmHelper.UninstallOperator()
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
			cmd := exec.Command("kubectl", "get", "deployment", "-l", "app.kubernetes.io/name=castai-agent", "-n", namespace)
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
			err = namespaceHelper.Delete(namespace)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete namespace")

			By("verifying that namespace is deleted")
			Eventually(namespaceHelper.VerifyDeleted, 5*time.Minute).
				WithArguments(namespace).
				Should(Succeed())

			By("deleting cluster roles and cluster role bindings")
			err = deleteClusterRoleResourcesWithAnnotation()
			Expect(err).NotTo(HaveOccurred(), "castai cluster roles should be deleted")
		})

		It("should onboard phase2 with legacy script", func() {
			By("getting phase1 script")

			var scriptResp string
			// nolint: lll
			getScriptURL := fmt.Sprintf("%s/v1/agent.sh?provider=gke", apiURL)
			err := apiHelper.FetchFromAPI(getScriptURL, http.MethodGet, nil, &scriptResp)
			Expect(err).NotTo(HaveOccurred(), "Failed to get phase1 script")

			cmd := exec.Command("bash", "-c", scriptResp)
			output, _ := utils.Run(cmd)
			Expect(output).To(ContainSubstring("deployment.apps/castai-agent created"), "Agent not installed")
			Expect(output).To(ContainSubstring("daemonset.apps/castai-spot-handler created"), "Spot handler not installed")

			By("patching castai-agent deployment to add GKE environment variables")
			cmd = exec.Command("kubectl", "patch", "deployment", "castai-agent",
				"-n", namespace,
				"--type=strategic",
				"-p", patchAgentDeploymentJSON)
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
			Eventually(podHelper.VerifyPodsReady, 5*time.Minute).
				WithArguments("app.kubernetes.io/name", "castai-agent").
				Should(Succeed())

			// Wait for cluster onboarding before running phase2 script
			time.Sleep(time.Minute)

			By("getting phase2 script")
			phase2ScriptResp := struct {
				Script string `json:"script"`
			}{}
			// nolint: lll
			getPhase2URL := fmt.Sprintf("%s/v1/kubernetes/external-clusters/%s/credentials-script?crossRole=true&nvidiaDevicePlugin=false&installSecurityAgent=true&installAutoscalerAgent=true&installGpuMetricsExporter=false&installNetflowExporter=false&installWorkloadAutoscaler=true&installPodMutator=false&installOmni=false",
				apiURL, clusterID)
			err = apiHelper.FetchFromAPI(getPhase2URL, http.MethodGet, nil, &phase2ScriptResp)
			Expect(err).NotTo(HaveOccurred(), "Failed to get phase2 script")

			// Install phase2 as not operator managed
			phase2ScriptResp.Script = strings.ReplaceAll(phase2ScriptResp.Script, "OPERATOR_MANAGED=true", "")
			phase2ScriptResp.Script = strings.ReplaceAll(phase2ScriptResp.Script, "PREFLIGHT_CHECKS=true", "PREFLIGHT_CHECKS=false")

			cmd = exec.Command("bash", "-c", phase2ScriptResp.Script)
			output, _ = utils.Run(cmd)
			// Phase2 script returns an error, but it's expected because it tries to
			// run "gcloud container clusters describe", but the cluster is not running in GKE.
			// Checking successful install of spot-handler and cluster-controller is enough for this test.
			Expect(output).To(ContainSubstring("Finished installing castai-cluster-controller"),
				"Failed to install cluster-controller")
		})

		It("should install the operator with extended permissions and take over cluster controller", func() {
			By("installing the operator")
			cmd := exec.Command("helm", "upgrade", "--install", "castware-operator",
				"--namespace", namespace,
				"--set", fmt.Sprintf("image.repository=%s", imageParts[0]),
				"--set", fmt.Sprintf("image.tag=%s", imageParts[1]),
				"--set", "image.pullPolicy=IfNotPresent",
				"--set", fmt.Sprintf("apiKeySecret.apiKey=%s", apiKey),
				"--set", fmt.Sprintf("defaultCluster.api.apiUrl=%s", apiURL),
				"--set", "extendedPermissions=true",
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
			Eventually(componentHelper.VerifyVersionIsSet, 5*time.Minute).
				WithArguments(components.ComponentNameAgent).
				Should(Succeed())

			By("verifying castai-agent component status is Available")
			err = componentHelper.VerifyStatusCondition(components.ComponentNameAgent, "Available")
			Expect(err).NotTo(HaveOccurred(), "castai-agent component should be Available")

			By("waiting for spot-handler component to be ready")
			Eventually(componentHelper.VerifyVersionIsSet, 5*time.Minute).
				WithArguments(components.ComponentNameSpotHandler).
				Should(Succeed())

			By("verifying spot-handler component status is Available")
			err = componentHelper.VerifyStatusCondition(components.ComponentNameSpotHandler, "Available")
			Expect(err).NotTo(HaveOccurred(), "spot-handler component should be Available")

			By("waiting for cluster-controller component to be ready")
			Eventually(componentHelper.VerifyVersionIsSet, 5*time.Minute).
				WithArguments(components.ComponentNameClusterController).
				Should(Succeed())

			By("verifying cluster-controller component status is Available")
			err = componentHelper.VerifyStatusCondition(components.ComponentNameClusterController, "Available")
			Expect(err).NotTo(HaveOccurred(), "cluster-controller component should be Available")
		})

		It("should not allow to disable extended permissions once they are enabled", func() {
			cmd := exec.Command("helm", "upgrade", "--install", "castware-operator",
				"--namespace", namespace,
				"--set", fmt.Sprintf("image.repository=%s", imageParts[0]),
				"--set", fmt.Sprintf("image.tag=%s", imageParts[1]),
				"--set", "image.pullPolicy=IfNotPresent",
				"--set", "extendedPermissions=false",
				"--reuse-values",
				"--atomic",
				"--timeout", "5m",
				operatorChartPath,
			)

			_, err := utils.Run(cmd)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("job castware-operator-preflight-check failed"))
		})

		It("should detect and report operator parameter changes via helm revision tracking", func() {
			By("waiting for cluster CR to be initialized with lastReportedHelmRevision")
			var initialRevision string
			verifyInitialRevision := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "cluster", "castai",
					"-n", namespace,
					"-o", "jsonpath={.status.lastReportedHelmRevision}",
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get cluster CR")
				g.Expect(output).NotTo(BeEmpty(), "lastReportedHelmRevision should be set")
				initialRevision = output
			}
			Eventually(verifyInitialRevision, 5*time.Minute).Should(Succeed())
			By(fmt.Sprintf("initial operator helm revision: %s", initialRevision))

			By("upgrading operator helm release with extendedPermissions parameter change only")
			// Change extendedPermissions from false to true without version change
			// Use local filesystem path instead of helm repo to avoid ChartMuseum issues
			cmd := exec.Command("helm", "upgrade", "castware-operator",
				"--namespace", namespace,
				"--reuse-values",
				"--set", "extendedPermissions=true",
				operatorChartPath,
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to upgrade operator with parameter change")

			By("waiting for operator pod to be ready after parameter change")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", "app.kubernetes.io/name=castware-operator",
					"-n", namespace,
					"-o", "jsonpath={.items[0].status.conditions[?(@.type=='Ready')].status}",
				)
				status, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(status).To(Equal("True"))
			}, 2*time.Minute).Should(Succeed())

			By("waiting for helm revision to increment in cluster status")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "cluster", "castai",
					"-n", namespace,
					"-o", "jsonpath={.status.lastReportedHelmRevision}",
				)
				newRevision, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(newRevision).NotTo(Equal(initialRevision),
					"Operator helm revision should have incremented after parameter change")
			}, 1*time.Minute).Should(Succeed()) // Operator reconciles immediately

			By("verifying operator logs contain successful reporting message")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "logs",
					"-l", "app.kubernetes.io/name=castware-operator",
					"-n", namespace,
					"--tail=100",
				)
				logs, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get operator logs")
				g.Expect(logs).To(ContainSubstring("Successfully reported operator helm revision change to Mothership"),
					"Operator should log successful reporting to Mothership")
			}, 30*time.Second).Should(Succeed())

			By("test completed: operator parameter changes detected and reported via helm revision tracking")
		})

		It("should self upgrade", func() {
			Skip("Skipping self upgrade test til we figure out why it doesn't pick local image")
			// operatorComponentID := ""
			By("fetching operator component ID")
			componentList, err := apiHelper.GetClusterComponents(organizationID, clusterID)
			Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("Failed to fetch components list: %v", err))
			operatorComponent, ok := FindComponentByName(componentList, components.ComponentNameOperator)
			Expect(ok).To(BeTrue(), "Operator component not found")
			Expect(operatorComponent.ID).NotTo(BeEmpty(), "Operator component id not found")

			By("calling the run action endpoint to trigger a self upgrade")
			runActionURL := fmt.Sprintf("%s/cluster-management/v1/organizations/%s/clusters/%s/components/%s:runAction",
				apiURL, organizationID, clusterID, operatorComponent.ID)
			reqBody := map[string]interface{}{"action": "UPDATE"}
			resp := struct {
				Action struct {
					Action    string `json:"action"`
					Automated bool   `json:"automated"`
				} `json:"action"`
			}{}
			err = apiHelper.FetchFromAPI(runActionURL, http.MethodPost, reqBody, &resp)
			Expect(err).ToNot(HaveOccurred(), "Failed to run update action")
			Expect(resp.Action.Automated).To(BeTrue(), "Action should be automated")

			By("checking that self upgrade job is completed successfully")
			verifyUpgradeJobCompleted := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "jobs",
					"-n", namespace,
					"-l", "app.kubernetes.io/name=castware-operator,app.kubernetes.io/component=upgrade-job",
					"-o", "json",
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get upgrade job")

				var jobList struct {
					Items []batchv1.Job `json:"items"`
				}
				err = json.Unmarshal([]byte(output), &jobList)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to parse job list")
				g.Expect(jobList.Items).NotTo(BeEmpty(), "No upgrade job found")

				job := jobList.Items[0]
				g.Expect(job.Status.Succeeded).To(BeEquivalentTo(1), "Upgrade job has not completed successfully")
			}
			Eventually(verifyUpgradeJobCompleted, 5*time.Minute, 10*time.Second).Should(Succeed())

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

func verifyPodReady(g Gomega, label, deploymentName, namespace string) {
	// Get pods with label app.kubernetes.io/name=deploymentName
	cmd := exec.Command("kubectl", "get", "pods",
		"-l", fmt.Sprintf("%s=%s", label, deploymentName),
		"-n", namespace,
		"-o", "jsonpath={range .items[*]}{.metadata.name}{'|'}{.status.conditions[?(@.type=='Ready')].status}{'\\n'}{end}")
	output, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get %s pods", deploymentName))
	g.Expect(output).NotTo(BeEmpty(), fmt.Sprintf("No %s pods found", deploymentName))

	// Check that at least one pod has Ready=True
	lines := utils.GetNonEmptyLines(output)
	g.Expect(lines).ToNot(BeEmpty(), fmt.Sprintf("No %s pods found", deploymentName))

	foundReady := false
	for _, line := range lines {
		if podReady(line) {
			foundReady = true
			break
		}
	}
	g.Expect(foundReady).To(BeTrue(), fmt.Sprintf("No %s pods are in Ready state", deploymentName))
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}

// deleteClusterRoleResourcesWithAnnotation deletes all cluster roles and cluster role bindings
// with the specified annotation "meta.helm.sh/release-namespace=castai-agent"
func deleteClusterRoleResourcesWithAnnotation() error {
	annotation := "meta.helm.sh/release-namespace=castai-agent"

	// Delete ClusterRoles with the annotation
	// nolint: lll
	cmd := exec.Command("kubectl", "get", "clusterroles",
		"-o", "jsonpath={range .items[?(@.metadata.annotations.meta\\.helm\\.sh/release-namespace=='castai-agent')]}{.metadata.name}{'\\n'}{end}")
	output, err := utils.Run(cmd)
	if err != nil {
		return fmt.Errorf("failed to list ClusterRoles with annotation %s: %w", annotation, err)
	}

	clusterRoles := utils.GetNonEmptyLines(output)
	for _, clusterRole := range clusterRoles {
		cmd = exec.Command("kubectl", "delete", "clusterrole", clusterRole)
		_, err = utils.Run(cmd)
		if err != nil {
			return fmt.Errorf("failed to delete ClusterRole %s: %w", clusterRole, err)
		}
		fmt.Printf("Deleted ClusterRole: %s\n", clusterRole)
	}

	// Delete ClusterRoleBindings with the annotation
	// nolint: lll
	cmd = exec.Command("kubectl", "get", "clusterrolebindings",
		"-o", "jsonpath={range .items[?(@.metadata.annotations.meta\\.helm\\.sh/release-namespace=='castai-agent')]}{.metadata.name}{'\\n'}{end}")
	output, err = utils.Run(cmd)
	if err != nil {
		return fmt.Errorf("failed to list ClusterRoleBindings with annotation %s: %w", annotation, err)
	}

	clusterRoleBindings := utils.GetNonEmptyLines(output)
	for _, clusterRoleBinding := range clusterRoleBindings {
		cmd = exec.Command("kubectl", "delete", "clusterrolebinding", clusterRoleBinding)
		_, err = utils.Run(cmd)
		if err != nil {
			return fmt.Errorf("failed to delete ClusterRoleBinding %s: %w", clusterRoleBinding, err)
		}
		fmt.Printf("Deleted ClusterRoleBinding: %s\n", clusterRoleBinding)
	}

	return nil
}
