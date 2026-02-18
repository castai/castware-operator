package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/castai/castware-operator/test/utils"
)

var (
	// projectImage is the name of the image which will be build and loaded
	// with the code source changes to be tested.
	projectImage = "cast.ai/castware-operator:e2e"
)

// TestE2E runs the end-to-end (e2e) test suite for the project. These tests execute in an isolated,
// temporary environment to validate project changes with the purposed to be used in CI jobs.
// The default setup requires Kind, builds/loads the Manager Docker image locally, and installs
// CertManager.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting castware-operator integration test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	// E2E_OPERATOR_IMAGE is set by CI on release runs to point at the pre-pushed
	// RC image (e.g. castware-operator:0.10.0-rc1). We pull it into Kind directly
	// instead of building from source, because the RC version must already be known
	// to Mothership before the tests run.
	if ciImage := os.Getenv("E2E_OPERATOR_IMAGE"); ciImage != "" {
		projectImage = ciImage
		By(fmt.Sprintf("using pre-built CI operator image: %s", projectImage))

		// E2E_OPERATOR_VERSION carries the RC version string (e.g. "0.10.0-rc1").
		// Patch Chart.yaml so helm installs exactly that version during the tests.
		if ciVersion := os.Getenv("E2E_OPERATOR_VERSION"); ciVersion != "" {
			By(fmt.Sprintf("patching Chart.yaml to version: %s", ciVersion))
			wd, _ := os.Getwd()
			chartPath := filepath.Join(wd, "..", "..", "charts", "castai-castware-operator", "Chart.yaml")
			patchChartVersion(chartPath, ciVersion)
		}

		By("pulling RC image into Kind cluster")
		err := utils.LoadImageToKindClusterWithName(projectImage)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load RC image into Kind")
		return
	}

	// Local / push / PR flow: build from source.
	By("building the manager(Operator) image")

	wd, _ := os.Getwd()
	b, err := os.ReadFile(filepath.Join(wd, "..", "..", "charts", "castai-castware-operator", "Chart.yaml"))
	Expect(err).NotTo(HaveOccurred(), "Failed to read Chart.yaml")
	chart := map[string]interface{}{}
	err = yaml.Unmarshal(b, &chart)
	Expect(err).NotTo(HaveOccurred(), "Failed to unmarshal Chart.yaml")
	appVersion := strings.Trim(chart["appVersion"].(string), " ")
	versionedImage := strings.Replace(projectImage, ":e2e", ":"+appVersion, 1)

	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", projectImage))
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager(Operator) image")

	cmd = exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", versionedImage))
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the versioned manager(Operator) image")

	// load image as cast.ai/castware-operator:e2e and as cast.ai/castware-operator:(appVersion) to test self upgrade
	By("loading the manager(Operator) image on Kind")
	err = utils.LoadImageToKindClusterWithName(projectImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager(Operator) image into Kind")
	err = utils.LoadImageToKindClusterWithName(versionedImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager(Operator) versioned image into Kind")
})

var _ = AfterSuite(func() {

})

// patchChartVersion rewrites the version and appVersion fields in a Chart.yaml file in-place
func patchChartVersion(chartPath, version string) {
	b, err := os.ReadFile(chartPath)
	ExpectWithOffset(2, err).NotTo(HaveOccurred(), "Failed to read Chart.yaml for patching")

	lines := strings.Split(string(b), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "version:") {
			lines[i] = "version: " + version
		} else if strings.HasPrefix(line, "appVersion:") {
			lines[i] = `appVersion: "` + version + `"`
		}
	}

	err = os.WriteFile(chartPath, []byte(strings.Join(lines, "\n")), 0o644)
	ExpectWithOffset(2, err).NotTo(HaveOccurred(), "Failed to write patched Chart.yaml")
}
