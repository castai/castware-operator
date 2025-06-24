package v1alpha1

import (
	"context"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"testing"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
)

func TestValidateCreate(t *testing.T) {

	t.Run("should return error when object is not castwarev1alpha1.Cluster", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		sut := &ClusterCustomValidator{}

		_, err := sut.ValidateCreate(ctx, &appsv1.Deployment{})
		r.Error(err)
	})
	t.Run("should return error when provider is not specified", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		sut := &ClusterCustomValidator{}

		_, err := sut.ValidateCreate(ctx, &castwarev1alpha1.Cluster{})
		r.Error(err)
	})
}
func TestValidateUpdate(t *testing.T) {

	t.Run("should return error when provider is not specified", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		sut := &ClusterCustomValidator{}
		oldObj := &castwarev1alpha1.Cluster{}
		oldObj.Spec.Provider = "test"
		newObj := &castwarev1alpha1.Cluster{}

		_, err := sut.ValidateUpdate(ctx, oldObj, newObj)
		r.Error(err)
	})
	t.Run("should return warning when provider is updated", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		sut := &ClusterCustomValidator{}
		oldObj := &castwarev1alpha1.Cluster{}
		oldObj.Spec.Provider = "test"
		newObj := &castwarev1alpha1.Cluster{}
		newObj.Spec.Provider = "test2"

		warnings, err := sut.ValidateUpdate(ctx, oldObj, newObj)
		r.NoError(err)
		r.Len(warnings, 1)
		r.Equal("provider value was updated", string(warnings[0]))
	})
}

func TestDefault(t *testing.T) {

	t.Run("should set GrpcURL if empty", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		sut := &ClusterCustomDefaulter{}
		obj := &castwarev1alpha1.Cluster{}
		obj.Spec.API.APIURL = "https://api.cast.ai"

		err := sut.Default(ctx, obj)
		r.NoError(err)
		r.Equal("grpc.cast.ai", obj.Spec.API.GrpcURL)
	})
	t.Run("should set KvisorGrpcURL if empty", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		sut := &ClusterCustomDefaulter{}
		obj := &castwarev1alpha1.Cluster{}
		obj.Spec.API.APIURL = "https://api.cast.ai"

		err := sut.Default(ctx, obj)
		r.NoError(err)
		r.Equal("kvisor.cast.ai", obj.Spec.API.KvisorGrpcURL)
	})
}

var _ = Describe("Cluster Webhook", func() {
	var (
		obj       *castwarev1alpha1.Cluster
		oldObj    *castwarev1alpha1.Cluster
		validator ClusterCustomValidator
		defaulter ClusterCustomDefaulter
	)

	BeforeEach(func() {
		obj = &castwarev1alpha1.Cluster{}
		oldObj = &castwarev1alpha1.Cluster{}
		validator = ClusterCustomValidator{}
		Expect(validator).NotTo(BeNil(), "Expected validator to be initialized")
		defaulter = ClusterCustomDefaulter{}
		Expect(defaulter).NotTo(BeNil(), "Expected defaulter to be initialized")
		Expect(oldObj).NotTo(BeNil(), "Expected oldObj to be initialized")
		Expect(obj).NotTo(BeNil(), "Expected obj to be initialized")
		// TODO (user): Add any setup logic common to all tests
	})

	AfterEach(func() {
		// TODO (user): Add any teardown logic common to all tests
	})

	Context("When creating Cluster under Defaulting Webhook", func() {
		// TODO (user): Add logic for defaulting webhooks
		// Example:
		// It("Should apply defaults when a required field is empty", func() {
		//     By("simulating a scenario where defaults should be applied")
		//     obj.SomeFieldWithDefault = ""
		//     By("calling the Default method to apply defaults")
		//     defaulter.Default(ctx, obj)
		//     By("checking that the default values are set")
		//     Expect(obj.SomeFieldWithDefault).To(Equal("default_value"))
		// })
	})

	Context("When creating or updating Cluster under Validating Webhook", func() {
		// TODO (user): Add logic for validating webhooks
		// Example:
		// It("Should deny creation if a required field is missing", func() {
		//     By("simulating an invalid creation scenario")
		//     obj.SomeRequiredField = ""
		//     Expect(validator.ValidateCreate(ctx, obj)).Error().To(HaveOccurred())
		// })
		//
		// It("Should admit creation if all required fields are present", func() {
		//     By("simulating an invalid creation scenario")
		//     obj.SomeRequiredField = "valid_value"
		//     Expect(validator.ValidateCreate(ctx, obj)).To(BeNil())
		// })
		//
		// It("Should validate updates correctly", func() {
		//     By("simulating a valid update scenario")
		//     oldObj.SomeRequiredField = "updated_value"
		//     obj.SomeRequiredField = "updated_value"
		//     Expect(validator.ValidateUpdate(ctx, oldObj, obj)).To(BeNil())
		// })
	})

})
