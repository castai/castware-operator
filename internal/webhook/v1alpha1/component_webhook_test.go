package v1alpha1

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/castai/castware-operator/internal/helm"
	mock_helm "github.com/castai/castware-operator/internal/helm/mock"
	"github.com/golang/mock/gomock"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/sirupsen/logrus"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	castaitest "github.com/castai/castware-operator/internal/castai/test"
	"github.com/castai/castware-operator/internal/config"
)

var _ = Describe("Component Webhook", func() {
	const (
		componentName = "castai-agent"
		clusterName   = "castai"
	)
	newTestComponent := func(t GinkgoTInterface) *castwarev1alpha1.Component {
		t.Helper()
		return &castwarev1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      componentName,
				Namespace: "test-namespace",
			},
			Spec: castwarev1alpha1.ComponentSpec{
				Component: componentName,
				Cluster:   clusterName,
				Enabled:   true,
				Version:   "",
				Values: &v1.JSON{
					Raw: []byte(`{"key": "value"}`),
				},
				Migration: "helm",
				Readonly:  false,
			},
			Status: castwarev1alpha1.ComponentStatus{},
		}
	}

	var (
		obj         *castwarev1alpha1.Component
		oldObj      *castwarev1alpha1.Component
		validator   ComponentCustomValidator
		chartLoader *mock_helm.MockChartLoader
		defaulter   ComponentCustomDefaulter
	)

	BeforeEach(func() {
		t := GinkgoT()
		cfg, _ := config.GetFromEnvironment()
		obj = newTestComponent(t)
		oldObj = newTestComponent(t)
		log := logrus.New()
		chartLoader = mock_helm.NewMockChartLoader(gomock.NewController(t))
		validator = ComponentCustomValidator{
			client:      k8sClient,
			config:      cfg,
			chartLoader: chartLoader,
			log:         log,
		}
		Expect(validator).NotTo(BeNil(), "Expected validator to be initialized")
		defaulter = ComponentCustomDefaulter{log: logrus.New()}
		Expect(defaulter).NotTo(BeNil(), "Expected defaulter to be initialized")
		Expect(oldObj).NotTo(BeNil(), "Expected oldObj to be initialized")
		Expect(obj).NotTo(BeNil(), "Expected obj to be initialized")
	})

	AfterEach(func() {
		// TODO (user): Add any teardown logic common to all tests
	})

	Context("When creating Component under Defaulting Webhook", func() {
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

	Context("When creating or updating Component under Validating Webhook", Ordered, func() {
		var apiServer *httptest.Server
		BeforeAll(func() {
			// spin up dummy CAST.AI server
			dummyUser, _ := json.Marshal(castaitest.CreateUserObject())
			dummyComponent, _ := json.Marshal(castaitest.CreateComponentObject())
			apiServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/me":
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(dummyUser)
					return
				case "/cluster-management/v1/components:getByName":
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(dummyComponent)
					return
				default:
					defer GinkgoRecover()
					Fail(fmt.Sprintf("Unexpected request path: %s", r.URL.Path))
				}
			}))

			// create a dummy cluster with a valid API key secret
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-api-key",
					Namespace: "default",
				},
				Data: map[string][]byte{
					"API_KEY": []byte("dummy-api-key"),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			cluster := &castwarev1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName,
					Namespace: "default",
				},
				Spec: castwarev1alpha1.ClusterSpec{
					Provider:     "test",
					APIKeySecret: "test-api-key",
					API: castwarev1alpha1.APISpec{
						APIURL: apiServer.URL,
					},
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		})

		AfterAll(func() {
			apiServer.Close()
		})

		It("Should deny creation if component is not supported by config", func() {
			By("simulating an invalid creation scenario")
			obj.Spec.Component = "invalid"
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).Error().To(MatchError("component 'invalid' is not supported"))
		})

		It("Should deny creation if cluster does not exist", func() {
			By("simulating an invalid creation scenario")
			obj.Spec.Component = componentName
			obj.Spec.Cluster = "invalid"
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).Error().To(HaveOccurred())
			Expect(err).Error().To(MatchError("cluster 'invalid' does not exist"))
		})

		It("Should admit creation", func() {
			By("simulating a valid creation scenario")
			obj.Spec.Component = componentName
			obj.Spec.Cluster = clusterName
			obj.SetNamespace("default")
			chartLoader.EXPECT().Load(gomock.Any(), &helm.ChartSource{
				RepoURL: "",
				Name:    "test-helm-chart",
				Version: "",
			})
			Expect(validator.ValidateCreate(ctx, obj)).To(BeNil())
		})

		It("Should deny update if component name has changed", func() {
			By("simulating an invalid update scenario")
			oldObj.Spec.Component = componentName
			obj.Spec.Component = "changed"
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).Error().To(MatchError("component name cannot be modified"))
		})

		It("Should deny update if component cluster CRD has changed", func() {
			By("simulating an invalid update scenario")
			oldObj.Spec.Cluster = clusterName
			obj.Spec.Cluster = "changed"
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).Error().To(MatchError("referenced cluster CRD cannot be modified"))
		})

		It("Should deny update if the component is readonly and was already readonly", func() {
			By("simulating an invalid update scenario")
			oldObj.Spec.Component = componentName
			oldObj.Spec.Readonly = true
			obj.Spec.Readonly = true
			obj.Spec.Version = "0.0.3"
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).Error().To(MatchError(errComponentReadonly))
		})

		It("Should deny update if the component is readonly and was not readonly before", func() {
			By("simulating an invalid update scenario")
			oldObj.Spec.Component = componentName
			obj.Spec.Readonly = true
			obj.Spec.Version = "0.0.3"
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).Error().To(MatchError(errComponentReadonly))
		})

		It("Should admit update", func() {
			By("simulating a valid update scenario")
			oldObj.Spec.Component = componentName
			oldObj.Spec.Cluster = clusterName
			oldObj.Spec.Enabled = true
			oldObj.SetNamespace("default")

			obj.Spec.Component = componentName
			obj.Spec.Cluster = clusterName
			obj.Spec.Enabled = true
			obj.Spec.Version = "0.0.1"
			obj.SetNamespace("default")

			chartLoader.EXPECT().Load(gomock.Any(), &helm.ChartSource{
				RepoURL: "",
				Name:    "test-helm-chart",
				Version: "0.0.1",
			})

			Expect(validator.ValidateUpdate(ctx, oldObj, obj)).To(BeNil())
		})
	})
})
