package e2e

import (
	"fmt"
	"os"
	"os/exec"
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
	By("building the manager(Operator) image")
	// TODO: better way to locate file
	b, err := os.ReadFile("../../charts/castai-castware-operator/Chart.yaml")
	Expect(err).NotTo(HaveOccurred(), "Failed to read Chart.yaml")
	chart := map[string]interface{}{}
	err = yaml.Unmarshal(b, &chart)
	Expect(err).NotTo(HaveOccurred(), "Failed to unmarshal Chart.yaml")
	appVersion := strings.Trim(chart["appVersion"].(string), " ")
	versionedImage := strings.Replace(projectImage, ":e2e", ":"+appVersion, 1)
	// projectImage = fmt.Sprintf("cast.ai/castware-operator:%s", version)
	fmt.Println("APP VERSION: ", appVersion)
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", projectImage))
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager(Operator) image")

	cmd = exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", versionedImage))
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager(Operator) image")

	// built and available before running the tests. Also, remove the following block.
	// TODO: image is loaded as cast.ai/castware-operator:e2e
	// load it as cast.ai/castware-operator:(latest_version) too to test self upgrade
	By("loading the manager(Operator) image on Kind")
	err = utils.LoadImageToKindClusterWithName(projectImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager(Operator) image into Kind")
	err = utils.LoadImageToKindClusterWithName(versionedImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager(Operator) versioned image into Kind")
})

var _ = AfterSuite(func() {

})
