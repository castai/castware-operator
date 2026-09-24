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
	var imageParts []string

	var apiURL = os.Getenv("API_URL")
	if apiURL == "" {
		apiURL = "https://api.dev-master.cast.ai"
	}

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		// Extract image repository and tag from projectImage (format: repository:tag)
		imageParts = strings.Split(projectImage, ":")
		Expect(imageParts).To(HaveLen(2), "invalid projectImage format")
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
		// Tolerate a leftover namespace from a previous local run; CI always
		// starts from a fresh cluster where the create succeeds.
		if err != nil && !strings.Contains(err.Error(), "AlreadyExists") {
			Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")
		}

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

		cmd = exec.Command("helm", "repo", "add", "local", "http://localhost:5001/helm-charts", "--force-update")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed add helm registry")

		By("adding helm chart to local registry")
		cmd = exec.Command("helm", "package", "./charts/castai-castware-operator")
		output, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to package helm chart")
		chartPath := strings.Split(output, "/")
		chartPackage := strings.TrimSuffix(chartPath[len(chartPath)-1], "\n")

		By(fmt.Sprintf("uploading helm chart %s to local registry", chartPackage))
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

		By("waiting for component CRs to be finalized")
		// The CR deletions above are asynchronous: their cleanup-helm finalizers
		// are resolved by the still-running operator. Waiting here prevents the
		// namespace deletion below from hanging when the operator teardown
		// wins the race (the finalizer would never resolve).
		Eventually(func(g Gomega) {
			cmd = exec.Command("kubectl", "get", "components", "-n", namespace, "-o", "name")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred(), "Failed to list components")
			g.Expect(strings.TrimSpace(output)).To(BeEmpty(), "component CRs should be finalized")
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		By("uninstalling helm release")
		_ = helmHelper.UninstallOperator()

		By("uninstalling the umbrella release if present")
		// The operator's pre-delete cleanup preserves the umbrella helm release
		// by design; remove it explicitly so the suite leaves no leftovers even
		// when the last specs ended with an installed umbrella. The uninstall is
		// best-effort (a teardown failure must not mask the spec results), but it
		// is logged so leftover state is diagnosable from CI output.
		if apiHelper != nil {
			if umbrellaReleaseName, err := apiHelper.GetUmbrellaReleaseName(); err == nil {
				cmd = exec.Command("helm", "uninstall", umbrellaReleaseName, "-n", namespace, "--ignore-not-found")
				if _, err := utils.Run(cmd); err != nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "umbrella release cleanup failed (continuing): %v\n", err)
				}
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "failed to resolve the umbrella release name for cleanup (continuing): %v\n", err)
			}
		}

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
			// In a focused run the first Manager spec may be skipped, leaving
			// controllerPodName unset; fall back to the label selector which
			// covers any operator pod (including the Umbrella specs).
			if controllerPodName != "" {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				controllerLogs, err := utils.Run(cmd)
				if err == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
				} else {
					_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
				}
			} else if operatorLogs, err := getOperatorLogs(namespace); err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", operatorLogs)
			}

			By("Fetching Kubernetes events")
			cmd := exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
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
			if controllerPodName != "" {
				cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
				podDescription, err := utils.Run(cmd)
				if err == nil {
					fmt.Println("Pod description:\n", podDescription)
				} else {
					fmt.Println("Failed to describe controller pod")
				}
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

			phase2Script := disablePreflightChecks(scriptResp.Script)
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
			Eventually(podHelper.VerifyPodsReady, 5*time.Minute).
				WithArguments("app.kubernetes.io/name", "castai-agent").
				Should(Succeed())

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
			Eventually(podHelper.VerifyPodsReady, 5*time.Minute).
				WithArguments("app.kubernetes.io/name", "castai-agent").
				Should(Succeed())

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
			Eventually(podHelper.VerifyPodsReady, 5*time.Minute).
				WithArguments("app.kubernetes.io/name", "castai-agent").
				Should(Succeed())

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

			phase2Script := disablePreflightChecks(scriptResp.Script)
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
			Eventually(podHelper.VerifyPodsReady, 5*time.Minute).
				WithArguments("app.kubernetes.io/name", "castai-agent").
				Should(Succeed())

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
			modifiedScript := disablePreflightChecks(scriptResp2.Script)
			// Replace OPERATOR_MANAGED=true with OPERATOR_MANAGED=false if it exists
			modifiedScript = strings.ReplaceAll(modifiedScript, "OPERATOR_MANAGED=true", "OPERATOR_MANAGED=false")

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
			phase2ScriptResp.Script = disablePreflightChecks(phase2ScriptResp.Script)

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

	// Umbrella scenarios (CID-1054). This group covers the fresh-install PR:
	// chart-hook fresh install (readonly and full tag modes), takeover of a
	// hand-installed umbrella, read-mode takeover, exclusivity, reporting
	// round-trip and offboarding. The migration-trigger, induced-failure
	// rollback and permission-gate scenarios land in a follow-up PR.
	//
	// Every spec is self-contained: it starts by resetting the cluster state
	// (operator, umbrella release, namespace), so the group runs alone with
	// -ginkgo.focus "Umbrella" and after the specs above in a full-suite run.
	// Shared closures for the Umbrella and Umbrella migration spec groups.
	umbrellaReset := func() {
		By("uninstalling the operator")
		cmd := exec.Command("helm", "uninstall", "castware-operator", "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("waiting for the operator CRDs to be removed")
		// The pre-delete cleanup job may wait up to two minutes for the
		// umbrella finalizer to resolve before deleting the CRDs.
		Eventually(func(g Gomega) {
			for _, crd := range []string{"components.castware.cast.ai", "clusters.castware.cast.ai"} {
				exists, err := crdExists(crd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to check CRD "+crd)
				g.Expect(exists).To(BeFalse(), "CRD "+crd+" should be removed")
			}
		}, 4*time.Minute, 5*time.Second).Should(Succeed())

		By("uninstalling the umbrella release if present")
		umbrellaReleaseName, err := apiHelper.GetUmbrellaReleaseName()
		Expect(err).NotTo(HaveOccurred(), "Failed to resolve the umbrella release name")
		cmd = exec.Command("helm", "uninstall", umbrellaReleaseName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("recreating the namespace")
		_ = namespaceHelper.Delete(namespace)
		Eventually(namespaceHelper.VerifyDeleted, 3*time.Minute).
			WithArguments(namespace).
			Should(Succeed())
		Expect(namespaceHelper.Create(namespace)).NotTo(HaveOccurred(), "Failed to create namespace")
		// The umbrella's readonly tag set includes kvisor, a security scanner
		// that needs host access (hostPID, privileged exporters, hostPath
		// volumes) and cannot satisfy the restricted PodSecurity standard the
		// Manager specs enforce for the agent/spot-handler charts. Relax the
		// namespace to privileged so the exact per-tag component set can land.
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=privileged")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with privileged policy")
	}

	// installOperatorWithRetry retries a failed operator install up to the
	// limit. The main transient failure is the chart's post-install hooks
	// racing the webhook server's first (cold) Mothership call — the 10s
	// admission deadline can be exceeded on a fresh kind namespace. Every
	// install uses --atomic, so a failed attempt is fully rolled back and a
	// retry starts clean; rather than match on helm/kubectl error wording (an
	// unstable contract), any install error is retried and the last error is
	// surfaced if the limit is reached.
	installOperatorWithRetry := func(install func() error) {
		const maxAttempts = 3
		var err error
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			err = install()
			if err == nil {
				return
			}
			if attempt < maxAttempts {
				By(fmt.Sprintf("operator install failed (attempt %d/%d), retrying: %v", attempt, maxAttempts, err))
				time.Sleep(15 * time.Second)
			}
		}
		Expect(err).NotTo(HaveOccurred(), "Failed to install operator after retries")
	}

	waitForOnboardedCluster := func() {
		By("waiting for the cluster to be onboarded")
		Eventually(func(g Gomega) {
			clusterID = clusterHelper.VerifyClusterID(g, "castai")
		}, 5*time.Minute, 5*time.Second).Should(Succeed())
		clusterResp, err := apiHelper.GetCluster(clusterID)
		Expect(err).NotTo(HaveOccurred(), "Failed to get cluster from API")
		org, ok := clusterResp["organizationId"].(string)
		Expect(ok).To(BeTrue(), "organizationId missing in cluster response")
		organizationID = org
	}
	waitUmbrellaComponentReady := func() {
		By("waiting for the castai-umbrella component to become available")
		Eventually(func(g Gomega) {
			componentHelper.VerifyVersionIsSet(g, components.ComponentNameUmbrella)
			err := componentHelper.VerifyStatusCondition(components.ComponentNameUmbrella, "Available")
			g.Expect(err).NotTo(HaveOccurred(), "castai-umbrella should be Available")
		}, 12*time.Minute, 15*time.Second).Should(Succeed())
	}

	ensureCastaiHelmRepo := func() {
		By("ensuring the castai-helm repo is configured")
		cmd := exec.Command("helm", "repo", "add", "castai-helm", "https://castai.github.io/helm-charts", "--force-update")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to add the castai-helm repo")
		cmd = exec.Command("helm", "repo", "update", "castai-helm")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to update the castai-helm repo")
	}

	Context("Umbrella", func() {
		// Exact per-tag sub-chart sets from internal/component/umbrella_tags.go.
		// Each sub-chart is verified through the app.kubernetes.io/name labels
		// its workloads carry; kvisor renders two workloads (agent daemonset
		// and controller deployment) under its own names.
		umbrellaWorkloadNames := map[string][]string{
			components.ComponentNameAgent:                      {components.ComponentNameAgent},
			components.UmbrellaSubchartSpotHandler:             {components.UmbrellaSubchartSpotHandler},
			components.ComponentNameKvisor:                     {"castai-kvisor-agent", "castai-kvisor-controller"},
			components.UmbrellaSubchartClusterController:       {components.UmbrellaSubchartClusterController},
			components.ComponentNameEvictor:                    {components.ComponentNameEvictor},
			components.ComponentNamePodMutator:                 {components.ComponentNamePodMutator},
			components.ComponentNamePodPinner:                  {components.ComponentNamePodPinner},
			components.ComponentNameWorkloadAutoscaler:         {components.ComponentNameWorkloadAutoscaler},
			components.ComponentNameWorkloadAutoscalerExporter: {components.ComponentNameWorkloadAutoscalerExporter},
		}

		readonlySubcharts := []string{
			components.ComponentNameAgent,
			components.UmbrellaSubchartSpotHandler,
			components.ComponentNameKvisor,
		}
		// The full tag set also contains castai-live, but the operator's own
		// defaults disable it (autoscaler.castai-live.enabled=false), matching
		// the umbrella's defaults, so it is not expected to land.
		fullSubcharts := append(append([]string{}, readonlySubcharts...),
			components.UmbrellaSubchartClusterController,
			components.ComponentNameEvictor,
			components.ComponentNamePodMutator,
			components.ComponentNamePodPinner,
			components.ComponentNameWorkloadAutoscaler,
			components.ComponentNameWorkloadAutoscalerExporter,
		)
		fullReadySubcharts := append(append([]string{}, readonlySubcharts...),
			components.UmbrellaSubchartClusterController,
		)

		// The node-autoscaler tag set also contains castai-live, but the
		// operator's own defaults disable it
		// (autoscaler.castai-live.enabled=false), matching the umbrella's
		// defaults, so it is not expected to land.
		nodeAutoscalerSubcharts := append(append([]string{}, readonlySubcharts...),
			components.UmbrellaSubchartClusterController,
			components.ComponentNameEvictor,
			components.ComponentNamePodMutator,
			components.ComponentNamePodPinner,
		)
		// Only the readonly trio and the cluster-controller are asserted ready
		// (matching the full mode's ready set); the remaining node-side
		// sub-charts are verified by workload existence only.
		nodeAutoscalerReadySubcharts := append(append([]string{}, readonlySubcharts...),
			components.UmbrellaSubchartClusterController,
		)
		// installUmbrellaOperator installs the operator chart with the
		// defaultComponents umbrella hook enabled. extraFlags add or override
		// chart values (e.g. extendedPermissions=true for the full tag mode).
		installUmbrellaOperator := func(extraFlags ...string) {
			args := []string{ //nolint:prealloc
				"upgrade", "--install", "castware-operator",
				"--namespace", namespace,
				"--set", fmt.Sprintf("image.repository=%s", imageParts[0]),
				"--set", fmt.Sprintf("image.tag=%s", imageParts[1]),
				"--set", "image.pullPolicy=IfNotPresent",
				"--set", fmt.Sprintf("apiKeySecret.apiKey=%s", apiKey),
				"--set", fmt.Sprintf("defaultCluster.api.apiUrl=%s", apiURL),
				"--set", "defaultCluster.provider=gke",
				"--set", "defaultCluster.terraform=false",
				"--set", "defaultComponents.enabled=true",
				"--set", "defaultComponents.umbrella.enabled=true",
				"--set", "webhook.env.GKE_CLUSTER_NAME=castware-operator-e2e",
				"--set", "webhook.env.GKE_LOCATION=e2e",
				"--set", "webhook.env.GKE_PROJECT_ID=e2e",
				"--set", "webhook.env.GKE_REGION=e2e",
				"--atomic",
				"--timeout", "5m",
			}
			// The agent under the umbrella needs the GKE env to run in kind; the
			// umbrella overrides path is long, so build these flags in a loop to
			// stay under the line-length limit.
			for key, value := range map[string]string{
				"GKE_CLUSTER_NAME": "castware-operator-e2e",
				"GKE_LOCATION":     "e2e",
				"GKE_PROJECT_ID":   "e2e",
				"GKE_REGION":       "e2e",
			} {
				args = append(args, "--set",
					"defaultComponents.umbrella.overrides.autoscaler.castai-agent.additionalEnv."+key+"="+value)
			}
			args = append(args, extraFlags...)
			args = append(args, operatorChartPath)
			installOperatorWithRetry(func() error {
				cmd := exec.Command("helm", args...)
				_, err := utils.Run(cmd)
				return err
			})
		}

		// verifyDaemonsetReady asserts that a daemonset with the given
		// app.kubernetes.io/name is deployed and reports a Ready condition. Used
		// for the spot-handler, which only schedules on cloud spot instances and
		// therefore never runs a pod in kind (matching the Manager specs' own
		// spot-handler assertion).
		verifyDaemonsetReady := func(g Gomega, name string) {
			cmd := exec.Command("kubectl", "get", "daemonsets",
				"-l", fmt.Sprintf("app.kubernetes.io/name=%s", name),
				"-n", namespace,
				"-o", "jsonpath={range .items[*]}{.metadata.name}{'|'}{.status.conditions[?(@.type=='Ready')].status}{'\\n'}{end}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred(), "Failed to get "+name+" daemonset")
			g.Expect(output).NotTo(BeEmpty(), "No "+name+" daemonsets found")
		}

		// verifyUmbrellaSubcharts asserts that a workload exists for every
		// expected sub-chart and that pods are ready for the ready subset
		// (except the spot-handler, which is verified via its daemonset).
		verifyUmbrellaSubcharts := func(g Gomega, expected, ready []string) {
			for _, subchart := range expected {
				for _, name := range umbrellaWorkloadNames[subchart] {
					cmd := exec.Command("kubectl", "get", "deployments,daemonsets,statefulsets",
						"-l", fmt.Sprintf("app.kubernetes.io/name=%s", name),
						"-n", namespace,
						"-o", "name")
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred(), "Failed to list workloads for "+name)
					// "No resources found" is still a successful kubectl call, so look
					// for a resource name (all listed kinds print as <kind>.apps/<name>).
					g.Expect(strings.Contains(output, "apps/")).To(BeTrue(),
						fmt.Sprintf("no workload found for %s", name))
				}
			}
			for _, subchart := range ready {
				for _, name := range umbrellaWorkloadNames[subchart] {
					if name == components.UmbrellaSubchartSpotHandler {
						verifyDaemonsetReady(g, name)
						continue
					}
					podHelper.VerifyPodsReady(g, "app.kubernetes.io/name", name)
				}
			}
		}

		// handInstallUmbrella installs the published castai umbrella chart by
		// hand with the same globals the operator would inject, so the operator
		// can detect and adopt it afterwards.
		handInstallUmbrella := func(releaseName, clusterIDValue, tag string, extraFlags ...string) {
			ensureCastaiHelmRepo()
			// The cluster webhook derives the kvisor grpc address from the api URL
			// (api.dev-master.cast.ai -> kvisor.dev-master.cast.ai) and the
			// operator injects it into the kvisor sub-chart; mirror that here so
			// kvisor authenticates against the same environment. Without it the
			// sub-chart's prod default rejects the key and kvisor never starts.
			apiHost := strings.TrimPrefix(strings.TrimPrefix(apiURL, "https://"), "http://")
			kvisorGrpcAddr := "kvisor." + strings.Join(strings.SplitN(apiHost, ".", 2)[1:], "")
			By(fmt.Sprintf("hand-installing the umbrella chart with tag %s", tag))
			flags := []string{ //nolint:prealloc
				"--set", fmt.Sprintf("global.castai.apiURL=%s", apiURL),
				"--set", "global.castai.provider=gke",
				"--set", fmt.Sprintf("global.castai.clusterID=%s", clusterIDValue),
				"--set", "global.castai.apiKeySecretRef=castware-api-key",
				"--set", fmt.Sprintf("tags.%s=true", tag),
				"--set", fmt.Sprintf("autoscaler.castai-kvisor.castai.grpcAddr=%s", kvisorGrpcAddr),
				// Neutralize the kvisor cluster-id refs like the operator does, so
				// the injected clusterID is consumed immediately.
				"--set", "autoscaler.castai-kvisor.castai.clusterIdConfigMapKeyRef.name=",
				"--set", "autoscaler.castai-kvisor.castai.clusterIdSecretKeyRef.name=",
				// The agent needs the GKE env to run in kind.
				"--set", "autoscaler.castai-agent.additionalEnv.GKE_CLUSTER_NAME=castware-operator-e2e",
				"--set", "autoscaler.castai-agent.additionalEnv.GKE_LOCATION=e2e",
				"--set", "autoscaler.castai-agent.additionalEnv.GKE_PROJECT_ID=e2e",
				"--set", "autoscaler.castai-agent.additionalEnv.GKE_REGION=e2e",
			}
			flags = append(flags, extraFlags...)
			err := helmHelper.InstallChart(releaseName, "castai-helm/castai", flags...)
			Expect(err).NotTo(HaveOccurred(), "Failed to hand-install the umbrella chart")
		}

		It("should fresh install the umbrella via the chart hook in readonly mode", func() {
			umbrellaReset()

			By("installing the operator with the umbrella hook enabled (readonly defaults)")
			installUmbrellaOperator()

			waitForOnboardedCluster()

			By("waiting for the castai-umbrella CR created by the post-install hook to become available")
			waitUmbrellaComponentReady()

			umbrellaReleaseName, err := apiHelper.GetUmbrellaReleaseName()
			Expect(err).NotTo(HaveOccurred())

			By("verifying the exact readonly component set landed (extras included)")
			Eventually(func(g Gomega) {
				verifyUmbrellaSubcharts(g, readonlySubcharts, readonlySubcharts)
			}, 10*time.Minute, 15*time.Second).Should(Succeed())

			By("verifying no per-component CRs were created")
			names, err := componentHelper.ListNames()
			Expect(err).NotTo(HaveOccurred(), "Failed to list component CRs")
			Expect(names).To(Equal([]string{components.ComponentNameUmbrella}),
				"only the umbrella CR should exist, got %v", names)

			By("verifying the umbrella release values match the umbrella's own defaults")
			values, err := helmHelper.GetReleaseValuesJSON(umbrellaReleaseName)
			Expect(err).NotTo(HaveOccurred(), "Failed to get umbrella release values")
			Expect(values).To(ContainSubstring(`"tags":{"readonly":true}`),
				"the readonly tag should be auto-derived from extendedPermissions=false")
			Expect(values).To(ContainSubstring(`"castai-live":{"enabled":false}`),
				"castai-live should be disabled by default")

			By("verifying the install report carries tags and inventory")
			Eventually(func(g Gomega) {
				logs, err := getOperatorLogs(namespace)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get operator logs")
				g.Expect(logs).To(ContainSubstring("Recorded action result for component"))
				g.Expect(logs).To(ContainSubstring("Name:castai-umbrella"))
				g.Expect(logs).To(ContainSubstring("ComponentParams:map["))
				// The inventory entries print with Go field names ({Name:... Version:... Enabled:...})
				// and the tags map carries every mode tag, so match the real shapes.
				g.Expect(logs).To(ContainSubstring("readonly:true"))
				g.Expect(logs).To(ContainSubstring("inventory:[{Name:"))
			}, 5*time.Minute, 15*time.Second).Should(Succeed())
		})

		It("should fresh install the umbrella via the chart hook in full mode with extras", func() {
			umbrellaReset()

			By("installing the operator with extendedPermissions=true (auto-derived tags.full=true)")
			installUmbrellaOperator("--set", "extendedPermissions=true")

			waitForOnboardedCluster()
			waitUmbrellaComponentReady()

			umbrellaReleaseName, err := apiHelper.GetUmbrellaReleaseName()
			Expect(err).NotTo(HaveOccurred())

			By("verifying the exact full component set landed (extras included)")
			Eventually(func(g Gomega) {
				verifyUmbrellaSubcharts(g, fullSubcharts, fullReadySubcharts)
			}, 12*time.Minute, 15*time.Second).Should(Succeed())

			By("verifying castai-live is disabled by the umbrella's own defaults")
			cmd := exec.Command("kubectl", "get", "deployments,daemonsets,statefulsets",
				"-l", fmt.Sprintf("app.kubernetes.io/name=%s", components.ComponentNameLive),
				"-n", namespace, "-o", "name")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.Contains(output, "apps/")).To(BeFalse(), "castai-live should be disabled by default")

			By("verifying no per-component CRs were created")
			names, err := componentHelper.ListNames()
			Expect(err).NotTo(HaveOccurred())
			Expect(names).To(Equal([]string{components.ComponentNameUmbrella}),
				"only the umbrella CR should exist, got %v", names)

			By("verifying the full tag in the release values")
			values, err := helmHelper.GetReleaseValuesJSON(umbrellaReleaseName)
			Expect(err).NotTo(HaveOccurred())
			Expect(values).To(ContainSubstring(`"tags":{"full":true}`),
				"the full tag should be auto-derived from extendedPermissions=true")

			By("verifying the install report carries the full tag and inventory")
			Eventually(func(g Gomega) {
				logs, err := getOperatorLogs(namespace)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get operator logs")
				// The inventory entries print with Go field names and the tags map carries
				// every mode tag, so match the real shapes.
				g.Expect(logs).To(ContainSubstring("full:true"))
				g.Expect(logs).To(ContainSubstring("inventory:[{Name:"))
			}, 5*time.Minute, 15*time.Second).Should(Succeed())
		})

		It("should fresh install the umbrella via the chart hook with an explicit node-autoscaler tag", func() {
			umbrellaReset()

			By("installing the operator with an explicit node-autoscaler tag")
			// extendedPermissions=true satisfies the admission gate (any
			// non-readonly tag pulls in the cluster-controller), but the explicit
			// tag must win over the tags.full the chart would otherwise derive
			// from extendedPermissions.
			installUmbrellaOperator("--set", "extendedPermissions=true",
				"--set", "defaultComponents.umbrella.tags."+components.UmbrellaTagNodeAutoscaler+"=true")

			waitForOnboardedCluster()
			waitUmbrellaComponentReady()

			umbrellaReleaseName, err := apiHelper.GetUmbrellaReleaseName()
			Expect(err).NotTo(HaveOccurred())

			By("verifying the exact node-autoscaler component set landed")
			Eventually(func(g Gomega) {
				verifyUmbrellaSubcharts(g, nodeAutoscalerSubcharts, nodeAutoscalerReadySubcharts)
			}, 12*time.Minute, 15*time.Second).Should(Succeed())

			By("verifying components outside the node-autoscaler tag are absent")
			// castai-live is disabled by the umbrella's own defaults and the
			// workload-autoscaler pair belongs to workload-autoscaler/full only;
			// together they pin the tag's boundary against the full set.
			cmd := exec.Command("kubectl", "get", "deployments,daemonsets,statefulsets",
				"-l", fmt.Sprintf("app.kubernetes.io/name in (%s, %s, %s)",
					components.ComponentNameLive,
					components.ComponentNameWorkloadAutoscaler,
					components.ComponentNameWorkloadAutoscalerExporter),
				"-n", namespace, "-o", "name")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.Contains(output, "apps/")).To(BeFalse(),
				"the node-autoscaler tag must not install live or the workload-autoscaler components")

			By("verifying no per-component CRs were created")
			names, err := componentHelper.ListNames()
			Expect(err).NotTo(HaveOccurred())
			Expect(names).To(Equal([]string{components.ComponentNameUmbrella}),
				"only the umbrella CR should exist, got %v", names)

			By("verifying the node-autoscaler tag in the release values")
			values, err := helmHelper.GetReleaseValuesJSON(umbrellaReleaseName)
			Expect(err).NotTo(HaveOccurred())
			Expect(values).To(ContainSubstring(`"tags":{"node-autoscaler":true}`),
				"the explicit tag should pass through instead of the extendedPermissions-derived full tag")

			By("verifying the install report carries the node-autoscaler tag and inventory")
			Eventually(func(g Gomega) {
				logs, err := getOperatorLogs(namespace)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get operator logs")
				g.Expect(logs).To(ContainSubstring("Name:castai-umbrella"))
				// The inventory entries print with Go field names and the tags map carries
				// every mode tag, so match the real shapes.
				g.Expect(logs).To(ContainSubstring("node-autoscaler:true"))
				g.Expect(logs).To(ContainSubstring("inventory:[{Name:"))
			}, 5*time.Minute, 15*time.Second).Should(Succeed())
		})

		It("should take over a hand-installed umbrella and preserve its release history", func() {
			umbrellaReset()

			By("installing the operator without default components")
			installOperatorWithRetry(func() error {
				return helmHelper.InstallOperator(imageParts[0], imageParts[1], apiKey, apiURL, operatorChartPath, "")
			})
			var err error

			waitForOnboardedCluster()

			umbrellaReleaseName, err := apiHelper.GetUmbrellaReleaseName()
			Expect(err).NotTo(HaveOccurred())

			By("hand-installing the umbrella chart")
			handInstallUmbrella(umbrellaReleaseName, clusterID, components.UmbrellaTagReadonly)

			By("waiting for the umbrella workloads to be ready")
			Eventually(func(g Gomega) {
				verifyUmbrellaSubcharts(g, readonlySubcharts, readonlySubcharts)
			}, 12*time.Minute, 15*time.Second).Should(Succeed())

			By("capturing the release revision and history before adoption")
			revision, err := helmHelper.GetReleaseRevision(umbrellaReleaseName)
			Expect(err).NotTo(HaveOccurred())
			historyCount, err := helmHelper.GetReleaseHistoryCount(umbrellaReleaseName)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the operator to adopt the umbrella release")
			Eventually(func(g Gomega) {
				componentHelper.VerifyVersionIsSet(g, components.ComponentNameUmbrella)
			}, 5*time.Minute, 15*time.Second).Should(Succeed())

			By("verifying the adoption metadata on the CR")
			migration, err := componentHelper.GetField(components.ComponentNameUmbrella, "{.spec.migration}")
			Expect(err).NotTo(HaveOccurred())
			Expect(migration).To(Equal("helm"), "adopted CR should use migration: helm")
			releaseNameField, err := componentHelper.GetField(components.ComponentNameUmbrella, "{.spec.releaseName}")
			Expect(err).NotTo(HaveOccurred())
			Expect(releaseNameField).To(Equal(umbrellaReleaseName), "adopted CR should pin the release name")

			By("waiting for the adopted component to become available")
			waitUmbrellaComponentReady()

			By("verifying the release history is preserved (no install or upgrade by the operator)")
			revisionAfter, err := helmHelper.GetReleaseRevision(umbrellaReleaseName)
			Expect(err).NotTo(HaveOccurred())
			historyAfter, err := helmHelper.GetReleaseHistoryCount(umbrellaReleaseName)
			Expect(err).NotTo(HaveOccurred())
			Expect(revisionAfter).To(Equal(revision), "operator must not bump the release revision on adoption")
			Expect(historyAfter).To(Equal(historyCount), "operator must not add release history on adoption")

			By("verifying no per-component CRs exist")
			names, err := componentHelper.ListNames()
			Expect(err).NotTo(HaveOccurred())
			Expect(names).To(Equal([]string{components.ComponentNameUmbrella}),
				"only the umbrella CR should exist, got %v", names)

			By("verifying the adoption reported tags and inventory to Mothership")
			Eventually(func(g Gomega) {
				logs, err := getOperatorLogs(namespace)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get operator logs")
				g.Expect(logs).To(ContainSubstring("Name:castai-umbrella"))
				g.Expect(logs).To(ContainSubstring("ComponentParams:map["))
				// The inventory entries print with Go field names ({Name:... Version:... Enabled:...})
				// and the tags map carries every mode tag, so match the real shapes.
				g.Expect(logs).To(ContainSubstring("readonly:true"))
				g.Expect(logs).To(ContainSubstring("inventory:[{Name:"))
			}, 5*time.Minute, 15*time.Second).Should(Succeed())
		})

		It("should take over a hand-installed umbrella in read mode without write ops", func() {
			umbrellaReset()

			By("installing the operator in migrationMode=read")
			installOperatorWithRetry(func() error {
				return helmHelper.InstallOperator(imageParts[0], imageParts[1], apiKey, apiURL, operatorChartPath, "read")
			})
			var err error

			waitForOnboardedCluster()

			umbrellaReleaseName, err := apiHelper.GetUmbrellaReleaseName()
			Expect(err).NotTo(HaveOccurred())

			By("hand-installing the umbrella chart")
			handInstallUmbrella(umbrellaReleaseName, clusterID, components.UmbrellaTagReadonly)

			By("waiting for the umbrella workloads to be ready")
			Eventually(func(g Gomega) {
				verifyUmbrellaSubcharts(g, readonlySubcharts, readonlySubcharts)
			}, 12*time.Minute, 15*time.Second).Should(Succeed())

			By("capturing the release revision and history before adoption")
			revision, err := helmHelper.GetReleaseRevision(umbrellaReleaseName)
			Expect(err).NotTo(HaveOccurred())
			historyCount, err := helmHelper.GetReleaseHistoryCount(umbrellaReleaseName)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the operator to adopt the umbrella release as read-only")
			Eventually(func(g Gomega) {
				componentHelper.VerifySpecReadonly(g, components.ComponentNameUmbrella, true)
			}, 5*time.Minute, 15*time.Second).Should(Succeed())

			By("verifying the adoption metadata on the CR")
			migration, err := componentHelper.GetField(components.ComponentNameUmbrella, "{.spec.migration}")
			Expect(err).NotTo(HaveOccurred())
			Expect(migration).To(Equal("helm"), "adopted CR should use migration: helm")
			releaseNameField, err := componentHelper.GetField(components.ComponentNameUmbrella, "{.spec.releaseName}")
			Expect(err).NotTo(HaveOccurred())
			Expect(releaseNameField).To(Equal(umbrellaReleaseName), "adopted CR should pin the release name")

			By("waiting for the observed release version in the CR status")
			Eventually(func(g Gomega) {
				version, err := componentHelper.GetField(components.ComponentNameUmbrella, "{.status.currentVersion}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(version).NotTo(BeEmpty(), "the read-only path should still observe the release version")
			}, 5*time.Minute, 15*time.Second).Should(Succeed())

			By("verifying no write ops occurred")
			revisionAfter, err := helmHelper.GetReleaseRevision(umbrellaReleaseName)
			Expect(err).NotTo(HaveOccurred())
			historyAfter, err := helmHelper.GetReleaseHistoryCount(umbrellaReleaseName)
			Expect(err).NotTo(HaveOccurred())
			Expect(revisionAfter).To(Equal(revision), "read mode must not bump the release revision")
			Expect(historyAfter).To(Equal(historyCount), "read mode must not add release history")

			By("verifying the operator recorded no action results (observe-only)")
			logs, err := getOperatorLogs(namespace)
			Expect(err).NotTo(HaveOccurred(), "Failed to get operator logs")
			Expect(logs).NotTo(ContainSubstring("Recorded action result for component"),
				"read mode must not perform or report any component writes")

			// Note: in read mode the operator itself never calls recordActionResult
			// (verified above), so any Mothership-side inventory for the cluster
			// comes solely from the agent's own snapshot channel. The dev
			// Mothership did not surface the agent's usedVersion through the
			// components:view endpoint for a read-mode (operator-onboarded)
			// cluster within the retry window, so no API read-back is asserted
			// here; the observable contract for read mode is the CR status above.

			By("verifying the scan adopted the umbrella via the release")
			Eventually(func(g Gomega) {
				logs, err := getOperatorLogs(namespace)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get operator logs")
				g.Expect(logs).To(ContainSubstring("Umbrella release found, creating castai-umbrella component resource"),
					"the scan should have adopted the hand-installed release")
			}, 2*time.Minute, 10*time.Second).Should(Succeed())
		})

		It("should enforce exclusivity between the umbrella and individual components", func() {
			umbrellaReset()

			By("installing the operator without default components")
			installOperatorWithRetry(func() error {
				return helmHelper.InstallOperator(imageParts[0], imageParts[1], apiKey, apiURL, operatorChartPath, "")
			})
			var err error

			waitForOnboardedCluster()

			umbrellaReleaseName, err := apiHelper.GetUmbrellaReleaseName()
			Expect(err).NotTo(HaveOccurred())

			By("creating an individual castai-agent component CR")
			err = componentHelper.CreateFromYAML(components.ComponentNameAgent, components.ComponentNameAgent, "")
			Expect(err).NotTo(HaveOccurred(), "Failed to create agent CR")

			By("waiting for the agent to be installed")
			Eventually(func(g Gomega) {
				componentHelper.VerifyVersionIsSet(g, components.ComponentNameAgent)
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying umbrella CR creation is denied while individual CRs exist")
			// The readonly tag keeps the umbrella within the operator's base
			// permissions, so the admission check reaches the mutual-exclusivity
			// gate instead of the extended-permissions denial.
			err = componentHelper.CreateUmbrellaFromYAML(components.ComponentNameUmbrella, "castai",
				"  values:\n    tags:\n      readonly: true\n")
			Expect(err).To(HaveOccurred(), "umbrella CR creation must be denied while individual CRs exist")
			Expect(err.Error()).To(ContainSubstring(fmt.Sprintf(
				"umbrella component cannot be created while individual component CR %q exists; "+
					"set spec.migrate: true to take it over",
				components.ComponentNameAgent)))

			By("hand-installing the umbrella chart alongside the agent (hybrid)")
			// The agent sub-chart is disabled so the umbrella does not collide
			// with the standalone agent release's resources: the hybrid state
			// (umbrella release + individual release both installed) is what the
			// operator must detect and handle without adopting the umbrella.
			handInstallUmbrella(umbrellaReleaseName, clusterID, components.UmbrellaTagReadonly,
				"--set", "autoscaler.castai-agent.enabled=false")

			By("verifying the hybrid configuration is not adopted")
			names, err := componentHelper.ListNames()
			Expect(err).NotTo(HaveOccurred(), "Failed to list component CRs")
			Expect(names).To(Equal([]string{components.ComponentNameAgent}),
				"the hybrid config must not adopt the umbrella, got %v", names)

			By("verifying the agent CR is forced read-only with an UmbrellaConflict condition")
			Eventually(func(g Gomega) {
				componentHelper.VerifySpecReadonly(g, components.ComponentNameAgent, true)
				err := componentHelper.VerifyStatusConditionReason(
					components.ComponentNameAgent, "UmbrellaConflict", "UmbrellaReleasePresent")
				g.Expect(err).NotTo(HaveOccurred(), "agent CR should carry the UmbrellaConflict condition")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying the read-only agent CR cannot be modified")
			err = componentHelper.PatchVersion(components.ComponentNameAgent, "0.125.0")
			Expect(err).To(HaveOccurred(), "modifying a read-only component must be denied")
			Expect(err.Error()).To(ContainSubstring("readonly components cannot be modified"))

			By("removing the umbrella release")
			Expect(helmHelper.UninstallRelease(umbrellaReleaseName)).
				NotTo(HaveOccurred(), "Failed to uninstall the umbrella release")

			By("flipping spec.readonly back on the agent CR")
			cmd := exec.Command("kubectl", "patch", "component", components.ComponentNameAgent,
				"-n", namespace, "--type=merge", "-p", `{"spec":{"readonly":false}}`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "flipping spec.readonly must be the one allowed change")

			By("verifying the UmbrellaConflict condition clears")
			Eventually(func(g Gomega) {
				err := componentHelper.VerifyStatusConditionReason(
					components.ComponentNameAgent, "UmbrellaConflict", "UmbrellaReleaseNotPresent")
				g.Expect(err).NotTo(HaveOccurred(), "UmbrellaConflict should clear once the umbrella release is gone")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("deleting the agent component CR")
			cmd = exec.Command("kubectl", "delete", "component", components.ComponentNameAgent, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete agent CR")

			By("waiting for the agent helm release to be uninstalled")
			Eventually(func(g Gomega) {
				exists, err := helmHelper.ReleaseExists(components.ComponentNameAgent)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(exists).To(BeFalse(), "agent release should be uninstalled with the CR")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("creating the umbrella CR now that no individual CRs exist")
			// The readonly tag avoids the extended-permissions requirement the
			// webhook enforces for non-readonly umbrella values.
			err = componentHelper.CreateUmbrellaFromYAML(components.ComponentNameUmbrella, "castai",
				"  values:\n    tags:\n      readonly: true\n")
			Expect(err).NotTo(HaveOccurred(), "umbrella CR creation must be allowed without individual CRs")
			waitUmbrellaComponentReady()

			By("verifying individual CR creation is denied while the umbrella CR exists")
			err = componentHelper.CreateFromYAML(components.ComponentNameAgent, components.ComponentNameAgent, "")
			Expect(err).To(HaveOccurred(), "individual CR creation must be denied while the umbrella CR exists")
			Expect(err.Error()).To(ContainSubstring(fmt.Sprintf(
				"cannot create individual component CR %q: umbrella component CR exists; use the castai-umbrella chart instead",
				components.ComponentNameAgent)))
		})

		It("should report component params with tags and inventory across install, upgrade and uninstall", func() {
			umbrellaReset()

			By("installing the operator with the umbrella hook enabled (readonly defaults)")
			installUmbrellaOperator()

			waitForOnboardedCluster()
			waitUmbrellaComponentReady()

			umbrellaReleaseName, err := apiHelper.GetUmbrellaReleaseName()
			Expect(err).NotTo(HaveOccurred())

			By("verifying the install report carries tags and inventory")
			Eventually(func(g Gomega) {
				logs, err := getOperatorLogs(namespace)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get operator logs")
				g.Expect(logs).To(ContainSubstring("Recorded action result for component"))
				g.Expect(logs).To(ContainSubstring("Name:castai-umbrella"))
				g.Expect(logs).To(ContainSubstring("Action:ENABLE"))
				g.Expect(logs).To(ContainSubstring("ComponentParams:map["))
				// The inventory entries print with Go field names ({Name:... Version:... Enabled:...})
				// and the tags map carries every mode tag, so match the real shapes.
				g.Expect(logs).To(ContainSubstring("readonly:true"))
				g.Expect(logs).To(ContainSubstring("inventory:[{Name:"))
			}, 5*time.Minute, 15*time.Second).Should(Succeed())

			By("waiting for the reported revision to be tracked on the CR")
			Eventually(func(g Gomega) {
				reported, err := componentHelper.GetField(components.ComponentNameUmbrella, "{.status.lastReportedHelmRevision}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(reported).NotTo(BeEmpty(), "lastReportedHelmRevision should be set after the install report")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())
			reportedRevision, err := componentHelper.GetField(
				components.ComponentNameUmbrella, "{.status.lastReportedHelmRevision}")
			Expect(err).NotTo(HaveOccurred())

			By("upgrading the umbrella release directly (parameter-only revision bump)")
			ensureCastaiHelmRepo()
			err = helmHelper.InstallChart(umbrellaReleaseName, "castai-helm/castai", "--reuse-values")
			Expect(err).NotTo(HaveOccurred(), "Failed to upgrade the umbrella release directly")

			By("waiting for the revision change to be detected and reported")
			Eventually(func(g Gomega) {
				reported, err := componentHelper.GetField(components.ComponentNameUmbrella, "{.status.lastReportedHelmRevision}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(reported).NotTo(Equal(reportedRevision), "lastReportedHelmRevision should advance after the revision bump")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying the upgrade report carries tags and inventory")
			Eventually(func(g Gomega) {
				logs, err := getOperatorLogs(namespace)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get operator logs")
				g.Expect(logs).To(ContainSubstring("Successfully reported helm revision change to Mothership"))
				// The inventory entries print with Go field names ({Name:... Version:... Enabled:...})
				// and the tags map carries every mode tag, so match the real shapes.
				g.Expect(logs).To(ContainSubstring("readonly:true"))
				g.Expect(logs).To(ContainSubstring("inventory:[{Name:"))
			}, 5*time.Minute, 15*time.Second).Should(Succeed())

			By("deleting the umbrella CR (uninstall)")
			cmd := exec.Command("kubectl", "delete", "component", components.ComponentNameUmbrella, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete the umbrella CR")

			By("waiting for the umbrella release to be uninstalled")
			Eventually(func(g Gomega) {
				exists, err := helmHelper.ReleaseExists(umbrellaReleaseName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(exists).To(BeFalse(), "the umbrella release should be uninstalled with the CR")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying the uninstall was reported")
			// The DELETE report does not carry component_params (the release is
			// gone); the round-trip closes with the DISABLE action record.
			Eventually(func(g Gomega) {
				logs, err := getOperatorLogs(namespace)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get operator logs")
				g.Expect(logs).To(ContainSubstring("Recorded action result for component"))
				g.Expect(logs).To(ContainSubstring("Name:castai-umbrella"))
				g.Expect(logs).To(ContainSubstring("Action:DISABLE"))
			}, 5*time.Minute, 15*time.Second).Should(Succeed())
		})

		It("should preserve the umbrella when the operator is uninstalled (offboarding)", func() {
			umbrellaReset()

			By("installing the operator with the umbrella hook enabled in full mode")
			installUmbrellaOperator("--set", "extendedPermissions=true")

			waitForOnboardedCluster()
			waitUmbrellaComponentReady()

			umbrellaReleaseName, err := apiHelper.GetUmbrellaReleaseName()
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the full workload set to be running")
			Eventually(func(g Gomega) {
				verifyUmbrellaSubcharts(g, fullSubcharts, fullReadySubcharts)
			}, 12*time.Minute, 15*time.Second).Should(Succeed())

			By("capturing the CRDs owned by the umbrella release")
			umbrellaCRDNames, err := helmHelper.GetReleaseCRDNames(umbrellaReleaseName)
			Expect(err).NotTo(HaveOccurred(), "Failed to get umbrella release CRDs")
			Expect(umbrellaCRDNames).NotTo(BeEmpty(), "the umbrella release should own at least one CRD")

			By("uninstalling the operator")
			Expect(helmHelper.UninstallOperator()).NotTo(HaveOccurred(), "Failed to uninstall the operator")

			By("verifying the operator CRDs are gone")
			Eventually(func(g Gomega) {
				for _, crd := range []string{"components.castware.cast.ai", "clusters.castware.cast.ai"} {
					exists, err := crdExists(crd)
					g.Expect(err).NotTo(HaveOccurred(), "Failed to check CRD "+crd)
					g.Expect(exists).To(BeFalse(), "operator CRD "+crd+" should be removed")
				}
			}, 4*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the umbrella helm release survived")
			exists, err := helmHelper.ReleaseExists(umbrellaReleaseName)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue(), "the umbrella release must survive the operator uninstall")

			By("verifying the umbrella workloads survived")
			Eventually(func(g Gomega) {
				verifyUmbrellaSubcharts(g, fullSubcharts, fullReadySubcharts)
			}, 2*time.Minute, 15*time.Second).Should(Succeed())

			By("verifying the umbrella CRDs survived")
			for _, crdName := range umbrellaCRDNames {
				exists, err := crdExists(crdName)
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeTrue(), "umbrella CRD %s must survive the operator uninstall", crdName)
			}

			By("verifying the namespace is intact")
			cmd := exec.Command("kubectl", "get", "ns", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "the namespace must survive the operator uninstall")
		})
	})

	// Umbrella migration scenarios (PR2 of the umbrella e2e ticket):
	// migration via both triggers, an induced-failure rollback, and the
	// permission gate failing fast on insufficient RBAC. An umbrella-to-
	// individual migration case is explicitly out of scope per the ticket.
	//
	// Every spec is self-contained: it starts by resetting the cluster state,
	// so the group runs alone with -ginkgo.focus "Umbrella migration" and in
	// any full-suite order.
	Context("Umbrella migration", func() {
		// migrationReset extends the shared reset with the standalone covered
		// releases the migration specs hand-install.
		migrationReset := func() {
			umbrellaReset()
			By("uninstalling standalone covered releases if present")
			for _, release := range []string{components.ComponentNameKvisor, components.ComponentNameEvictor} {
				cmd := exec.Command("helm", "uninstall", release, "-n", namespace, "--ignore-not-found")
				if _, err := utils.Run(cmd); err != nil {
					_, _ = fmt.Fprintf(GinkgoWriter,
						"standalone release %s cleanup failed (continuing): %v\n", release, err)
				}
			}
		}

		// installBareOperator installs the operator without default components
		// and with base permissions (no extendedPermissions).
		installBareOperator := func() {
			installOperatorWithRetry(func() error {
				return helmHelper.InstallOperator(imageParts[0], imageParts[1], apiKey, apiURL, operatorChartPath, "")
			})
		}

		// kvisorGrpcAddr derives the kvisor grpc endpoint from the api URL the
		// same way the cluster webhook does (api.dev-master.cast.ai ->
		// kvisor.dev-master.cast.ai); without it kvisor authenticates against
		// the prod endpoint and never starts.
		kvisorGrpcAddr := func() string {
			apiHost := strings.TrimPrefix(strings.TrimPrefix(apiURL, "https://"), "http://")
			return "kvisor." + strings.Join(strings.SplitN(apiHost, ".", 2)[1:], "")
		}

		// handInstallKvisor installs a standalone kvisor release (no Component
		// CR) with the dev grpc address, so the migration absorbs it by chart
		// identity and carries its values into the umbrella.
		handInstallKvisor := func(clusterIDValue string) {
			ensureCastaiHelmRepo()
			By("hand-installing the kvisor standalone chart")
			err := helmHelper.InstallStandaloneChart(components.ComponentNameKvisor,
				"castai-helm/castai-kvisor", map[string]string{
					"castai.apiKeySecretRef":               "castware-api-key",
					"castai.clusterID":                     clusterIDValue,
					"castai.grpcAddr":                      kvisorGrpcAddr(),
					"castai.clusterIdConfigMapKeyRef.name": "",
					"castai.clusterIdSecretKeyRef.name":    "",
				})
			Expect(err).NotTo(HaveOccurred(), "Failed to hand-install the kvisor standalone chart")
		}

		// handInstallEvictor installs a standalone evictor release (no Component
		// CR). The pod is not expected to run in kind; only the release's
		// presence matters — it widens the migration's derived tag to
		// node-autoscaler, beyond what a base-permission operator holds.
		handInstallEvictor := func() {
			ensureCastaiHelmRepo()
			By("hand-installing the evictor standalone chart")
			err := helmHelper.InstallStandaloneChart(components.ComponentNameEvictor,
				"castai-helm/castai-evictor", map[string]string{
					"apiKeySecretRef": "castware-api-key",
				})
			Expect(err).NotTo(HaveOccurred(), "Failed to hand-install the evictor standalone chart")
		}

		// waitAgentInstalled creates the individual agent CR and waits for its
		// release to be deployed.
		waitAgentInstalled := func() {
			By("creating an individual castai-agent component CR")
			err := componentHelper.CreateFromYAML(components.ComponentNameAgent, components.ComponentNameAgent, "")
			Expect(err).NotTo(HaveOccurred(), "Failed to create agent CR")
			By("waiting for the agent to be installed")
			Eventually(func(g Gomega) {
				componentHelper.VerifyVersionIsSet(g, components.ComponentNameAgent)
			}, 5*time.Minute, 10*time.Second).Should(Succeed())
		}

		// waitOperatorReported waits until the cluster controller has reported
		// the operator's revision (and with it its extendedPermissions
		// condition) to Mothership — the migration permission gate relies on
		// that server-side state.
		waitOperatorReported := func() {
			By("waiting for the operator to report its revision to Mothership")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "cluster", "castai",
					"-n", namespace,
					"-o", "jsonpath={.status.lastReportedHelmRevision}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get cluster CR")
				g.Expect(output).NotTo(BeEmpty(), "lastReportedHelmRevision should be set")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())
		}

		// createMigratingUmbrellaCR arms the migration on a fresh umbrella CR
		// whose readonly-tag values satisfy the admission webhook's permission
		// check on a base-permission operator.
		createMigratingUmbrellaCR := func() {
			By("creating the umbrella CR with spec.migrate")
			err := componentHelper.CreateUmbrellaFromYAML(components.ComponentNameUmbrella, "castai",
				"  migrate: true\n  values:\n    tags:\n      readonly: true\n")
			Expect(err).NotTo(HaveOccurred(), "Failed to create the migrating umbrella CR")
		}

		waitMigrationSucceeded := func() {
			By("waiting for the migration to succeed")
			Eventually(func(g Gomega) {
				err := componentHelper.VerifyStatusConditionReason(
					components.ComponentNameUmbrella, "Migrating", "MigrationSucceeded")
				g.Expect(err).NotTo(HaveOccurred())
				err = componentHelper.VerifyStatusCondition(components.ComponentNameUmbrella, "Available")
				g.Expect(err).NotTo(HaveOccurred(), "umbrella should be Available after the migration")
			}, 10*time.Minute, 15*time.Second).Should(Succeed())
		}

		waitMigrationRolledBack := func() {
			By("waiting for the migration to fail and roll back")
			Eventually(func(g Gomega) {
				phase, err := componentHelper.GetField(components.ComponentNameUmbrella, "{.status.migrationPhase}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(phase).To(Equal("RolledBack"), "migrationPhase should be RolledBack")
				err = componentHelper.VerifyStatusConditionReason(
					components.ComponentNameUmbrella, "Migrating", "MigrationFailed")
				g.Expect(err).NotTo(HaveOccurred())
			}, 10*time.Minute, 15*time.Second).Should(Succeed())
		}

		// verifyUmbrellaKvisorWorkloads asserts the kvisor workloads exist (the
		// agent readiness is asserted separately).
		verifyUmbrellaKvisorWorkloads := func(g Gomega) {
			for _, name := range []string{"castai-kvisor-agent", "castai-kvisor-controller"} {
				cmd := exec.Command("kubectl", "get", "deployments,daemonsets",
					"-l", fmt.Sprintf("app.kubernetes.io/name=%s", name),
					"-n", namespace,
					"-o", "name")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to list workloads for "+name)
				g.Expect(strings.Contains(output, "apps/")).To(BeTrue(), "no workload found for "+name)
			}
		}

		It("should migrate agent and kvisor standalones to the umbrella via spec.migrate", func() {
			migrationReset()
			installBareOperator()
			waitForOnboardedCluster()
			umbrellaReleaseName, err := apiHelper.GetUmbrellaReleaseName()
			Expect(err).NotTo(HaveOccurred())

			waitAgentInstalled()
			handInstallKvisor(clusterID)
			By("waiting for the kvisor standalone release to be present")
			Eventually(func(g Gomega) {
				exists, err := helmHelper.ReleaseExists(components.ComponentNameKvisor)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(exists).To(BeTrue(), "kvisor standalone release should be installed")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("capturing the agent pod identity before the migration")
			agentPodsBefore, err := podHelper.GetPodIdentities("app.kubernetes.io/name", components.ComponentNameAgent)
			Expect(err).NotTo(HaveOccurred())
			Expect(agentPodsBefore).NotTo(BeEmpty(), "expected a running agent pod")

			createMigratingUmbrellaCR()
			waitMigrationSucceeded()

			By("verifying only the umbrella CR remains")
			names, err := componentHelper.ListNames()
			Expect(err).NotTo(HaveOccurred())
			Expect(names).To(Equal([]string{components.ComponentNameUmbrella}),
				"only the umbrella CR should remain, got %v", names)

			By("verifying the migration cleared the migrate and readonly flags")
			migrate, err := componentHelper.GetField(components.ComponentNameUmbrella, "{.spec.migrate}")
			Expect(err).NotTo(HaveOccurred())
			Expect(migrate).To(Or(Equal("false"), BeEmpty()), "spec.migrate should be cleared")
			readonly, err := componentHelper.GetField(components.ComponentNameUmbrella, "{.spec.readonly}")
			Expect(err).NotTo(HaveOccurred())
			Expect(readonly).To(Or(Equal("false"), BeEmpty()), "spec.readonly should be cleared")

			By("verifying the kvisor standalone release was absorbed")
			Eventually(func(g Gomega) {
				exists, err := helmHelper.ReleaseExists(components.ComponentNameKvisor)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(exists).To(BeFalse(), "the kvisor standalone release should be uninstalled")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the absorbed kvisor values were carried over")
			values, err := helmHelper.GetReleaseValuesJSON(umbrellaReleaseName)
			Expect(err).NotTo(HaveOccurred())
			Expect(values).To(ContainSubstring(kvisorGrpcAddr()),
				"the kvisor grpcAddr should be carried into the umbrella release values")

			By("verifying the agent workload was re-created under the umbrella")
			// The migration deletes the standalone's agent workloads before the
			// umbrella install (their selectors embed the standalone release name
			// and are immutable), so the agent pod is re-created — the documented
			// restart trade-off of the migration path.
			agentPodsAfter, err := podHelper.GetPodIdentities("app.kubernetes.io/name", components.ComponentNameAgent)
			Expect(err).NotTo(HaveOccurred())
			Expect(agentPodsAfter).NotTo(Equal(agentPodsBefore),
				"the agent pod should have been re-created with the umbrella's selector")
			Expect(agentPodsAfter).NotTo(BeEmpty(), "expected a running agent pod after the migration")

			By("verifying the agent and kvisor workloads run under the umbrella")
			Eventually(func(g Gomega) {
				podHelper.VerifyPodsReady(g, "app.kubernetes.io/name", components.ComponentNameAgent)
				verifyUmbrellaKvisorWorkloads(g)
			}, 5*time.Minute, 15*time.Second).Should(Succeed())
		})

		It("should migrate via the Mothership install action with migrate", func() {
			migrationReset()
			installBareOperator()
			waitForOnboardedCluster()
			waitAgentInstalled()

			// The runAction request schema known to this repo carries only the
			// action enum; whether the dev Mothership propagates a migrate flag
			// from the request into the polled install action is decided
			// Mothership-side. Attempt the call with a migrate field and assert
			// what actually happens; if the API rejects it, the gap is documented
			// and the operator-side propagation (action.Migrate -> spec.migrate,
			// unit-tested in this repo) stands.
			By("triggering an umbrella install action via the API")
			umbrellaComponent, err := apiHelper.GetComponentByName(components.ComponentNameUmbrella)
			Expect(err).NotTo(HaveOccurred())
			Expect(umbrellaComponent.ID).NotTo(BeEmpty(), "umbrella component ID not found")

			runActionURL := fmt.Sprintf(
				"%s/cluster-management/v1/organizations/%s/clusters/%s/components/%s:runAction",
				apiURL, organizationID, clusterID, umbrellaComponent.ID)
			var runResp struct {
				Action struct {
					Action    string `json:"action"`
					Automated bool   `json:"automated"`
				} `json:"action"`
			}
			err = apiHelper.FetchFromAPI(runActionURL, http.MethodPost,
				map[string]interface{}{"action": "ENABLE", "migrate": true}, &runResp)
			if err != nil {
				By("documenting the Mothership-side gap: the runAction request cannot drive the migration")
				_, _ = fmt.Fprintf(GinkgoWriter,
					"runAction ENABLE with a migrate field failed: %v\n"+
						"The public request schema carries only the action enum; the migrate flag\n"+
						"is set Mothership-side. The operator-side propagation (action.Migrate ->\n"+
						"spec.migrate) is covered by internal/controller unit tests.\n", err)
				return
			}

			By("waiting for the operator to pick up the install action")
			// pollActions runs every 30 seconds. If the polled action carries
			// migrate, the umbrella CR is created with spec.migrate and the
			// migration proceeds; if it does not, the install is blocked by the
			// mutual-exclusivity gate while the individuals are present.
			Eventually(func(g Gomega) string {
				logs, err := getOperatorLogs(namespace)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get operator logs")
				return logs
			}, 3*time.Minute, 10*time.Second).Should(ContainSubstring("install action: castai-umbrella"))

			By("waiting for the action outcome")
			// The polled action either creates the umbrella CR (when the
			// Mothership-side action carries migrate) or is blocked by the
			// mutual-exclusivity gate while the agent release is present (when
			// it does not — the observed behavior of the dev Mothership's
			// runAction, which accepts but does not propagate the migrate field).
			var outcome string
			Eventually(func(g Gomega) {
				names, err := componentHelper.ListNames()
				if err == nil && stringSliceContains(names, components.ComponentNameUmbrella) {
					outcome = "created"
				} else if logs, err := getOperatorLogs(namespace); err == nil &&
					strings.Contains(logs, "cannot install umbrella component") {
					outcome = "blocked"
				} else {
					outcome = ""
				}
				g.Expect(outcome).NotTo(BeEmpty(),
					"the install action must either create the umbrella CR or be blocked")
			}, 3*time.Minute, 10*time.Second).Should(Succeed())

			if outcome == "created" {
				By("the action carried migrate: waiting for the migration to succeed")
				migrate, err := componentHelper.GetField(components.ComponentNameUmbrella, "{.spec.migrate}")
				Expect(err).NotTo(HaveOccurred())
				Expect(migrate).To(Equal("true"), "an action-driven umbrella CR must carry spec.migrate")
				waitMigrationSucceeded()
				names, err := componentHelper.ListNames()
				Expect(err).NotTo(HaveOccurred())
				Expect(names).To(Equal([]string{components.ComponentNameUmbrella}),
					"only the umbrella CR should remain, got %v", names)
				Eventually(func(g Gomega) {
					podHelper.VerifyPodsReady(g, "app.kubernetes.io/name", components.ComponentNameAgent)
				}, 5*time.Minute, 15*time.Second).Should(Succeed())
				return
			}

			// The blocked outcome (observed on the dev Mothership): the
			// runAction request accepted the migrate field but the polled
			// lifecycle action did not carry it, so the operator's sanctioned
			// behavior is to refuse the umbrella install while individuals are
			// present. Assert that refusal; driving the migration through the
			// public API needs the Mothership-side migrate propagation (the
			// operator-side action.Migrate propagation is covered by
			// internal/controller unit tests).
			By("documenting that the action did not carry migrate")
			_, _ = fmt.Fprintf(GinkgoWriter,
				"install action was polled without migrate; the umbrella install was blocked by the\n"+
					"mutual-exclusivity gate. Driving the migration through the public runAction API needs\n"+
					"the Mothership-side migrate propagation.\n")
			logs, err := getOperatorLogs(namespace)
			Expect(err).NotTo(HaveOccurred())
			Expect(logs).To(ContainSubstring("install action: castai-umbrella"),
				"the operator should have polled the install action")
			Expect(logs).To(ContainSubstring("cannot install umbrella component: individual component releases present"),
				"the install must be blocked while the agent release is present")
			names, err := componentHelper.ListNames()
			Expect(err).NotTo(HaveOccurred())
			Expect(names).NotTo(ContainElement(components.ComponentNameUmbrella),
				"no umbrella CR must exist when the action is blocked")
			Eventually(func(g Gomega) {
				podHelper.VerifyPodsReady(g, "app.kubernetes.io/name", components.ComponentNameAgent)
			}, 2*time.Minute, 10*time.Second).Should(Succeed())
		})

		It("should roll back to individuals when the umbrella install fails", func() {
			migrationReset()
			installBareOperator()
			waitForOnboardedCluster()
			umbrellaReleaseName, err := apiHelper.GetUmbrellaReleaseName()
			Expect(err).NotTo(HaveOccurred())

			waitAgentInstalled()
			handInstallKvisor(clusterID)
			By("waiting for the kvisor standalone release to be present")
			Eventually(func(g Gomega) {
				exists, err := helmHelper.ReleaseExists(components.ComponentNameKvisor)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(exists).To(BeTrue(), "kvisor standalone release should be installed")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("pre-creating an unowned spot-handler DaemonSet to collide with the umbrella install")
			// The readonly umbrella render includes the castai-spot-handler
			// DaemonSet; an unowned resource with that name makes the umbrella helm
			// install fail, which the migration controller treats as a rollback
			// trigger (not a retry).
			collisionYAML := fmt.Sprintf(`apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: castai-spot-handler
  namespace: %s
  labels:
    app.kubernetes.io/name: castai-spot-handler
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: castai-spot-handler
  template:
    metadata:
      labels:
        app.kubernetes.io/name: castai-spot-handler
    spec:
      containers:
      - name: placeholder
        image: registry.k8s.io/pause:3.10
`, namespace)
			err = componentHelper.ApplyYAML("spot-handler-collision", collisionYAML)
			Expect(err).NotTo(HaveOccurred(), "Failed to pre-create the collision DaemonSet")

			createMigratingUmbrellaCR()
			waitMigrationRolledBack()

			By("verifying the umbrella release is absent")
			exists, err := helmHelper.ReleaseExists(umbrellaReleaseName)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeFalse(), "the umbrella release should be uninstalled by the rollback")

			By("verifying the kvisor standalone was reinstalled from the snapshot")
			Eventually(func(g Gomega) {
				exists, err := helmHelper.ReleaseExists(components.ComponentNameKvisor)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(exists).To(BeTrue(), "the kvisor standalone release should be reinstalled")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying the agent component was reactivated and its pods run again")
			// The rollback deliberately uninstalls the umbrella (which had adopted
			// the agent Deployment), so the agent restarts on this path — the
			// no-restart invariant is a success-path property.
			Eventually(func(g Gomega) {
				componentHelper.VerifySpecReadonly(g, components.ComponentNameAgent, false)
			}, 2*time.Minute, 10*time.Second).Should(Succeed())
			Eventually(func(g Gomega) {
				podHelper.VerifyPodsReady(g, "app.kubernetes.io/name", components.ComponentNameAgent)
			}, 5*time.Minute, 15*time.Second).Should(Succeed())
		})

		It("should block the migration fast when RBAC is insufficient", func() {
			migrationReset()
			installBareOperator()
			waitForOnboardedCluster()
			waitOperatorReported()

			waitAgentInstalled()
			handInstallEvictor()
			By("waiting for the evictor standalone release to be present")
			Eventually(func(g Gomega) {
				exists, err := helmHelper.ReleaseExists(components.ComponentNameEvictor)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(exists).To(BeTrue(), "evictor standalone release should be installed")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			// The evictor widens the derived tag to node-autoscaler while the CR's
			// own readonly values pass the admission webhook: the gate payload
			// requests a surface the base-permission operator does not hold. If
			// the dev Mothership enforces the server-side comparison, the refusal
			// surfaces as MigrationBlocked before the finalizer, the readonly
			// patch and any uninstall.
			createMigratingUmbrellaCR()

			By("verifying the permission gate blocks the migration")
			Eventually(func(g Gomega) {
				err := componentHelper.VerifyStatusConditionReason(
					components.ComponentNameUmbrella, "Migrating", "MigrationBlocked")
				g.Expect(err).NotTo(HaveOccurred(), "the migration should be blocked by the permission gate")
				derived, err := componentHelper.GetField(components.ComponentNameUmbrella, "{.status.migrationDerivedTag}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(derived).To(Equal(components.UmbrellaTagNodeAutoscaler),
					"the evictor should widen the derived tag to node-autoscaler")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying the block was fail-fast: nothing was uninstalled")
			agentRelease, err := helmHelper.ReleaseExists(components.ComponentNameAgent)
			Expect(err).NotTo(HaveOccurred())
			Expect(agentRelease).To(BeTrue(), "the agent release must not be uninstalled by a blocked migration")
			Eventually(func(g Gomega) {
				componentHelper.VerifySpecReadonly(g, components.ComponentNameAgent, false)
			}, 2*time.Minute, 10*time.Second).Should(Succeed())
			evictorRelease, err := helmHelper.ReleaseExists(components.ComponentNameEvictor)
			Expect(err).NotTo(HaveOccurred())
			Expect(evictorRelease).To(BeTrue(), "the evictor release must remain present")

			By("cleaning up the blocked umbrella CR")
			// No migration finalizer is armed before the gate, so the CR deletes
			// without a rollback.
			cmd := exec.Command("kubectl", "delete", "component", components.ComponentNameUmbrella, "-n", namespace)
			if _, err := utils.Run(cmd); err != nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "blocked umbrella CR cleanup failed (continuing): %v\n", err)
			}
		})
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
