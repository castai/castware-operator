package v1alpha1

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sirupsen/logrus"
	"helm.sh/helm/v3/pkg/release"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	castwarev1alpha1 "github.com/castai/castware-operator/api/v1alpha1"
	castaitest "github.com/castai/castware-operator/internal/castai/test"
	"github.com/castai/castware-operator/internal/config"
	"github.com/castai/castware-operator/internal/helm"
	mock_helm "github.com/castai/castware-operator/internal/helm/mock"
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
				Migration: "",
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
		helmClient  *mock_helm.MockClient
		defaulter   ComponentCustomDefaulter
	)

	BeforeEach(func() {
		t := GinkgoT()
		cfg, _ := config.GetFromEnvironment()
		obj = newTestComponent(t)
		oldObj = newTestComponent(t)
		log := logrus.New()
		ctrl := gomock.NewController(t)
		chartLoader = mock_helm.NewMockChartLoader(ctrl)
		helmClient = mock_helm.NewMockClient(ctrl)
		validator = ComponentCustomValidator{
			client:      k8sClient,
			config:      cfg,
			chartLoader: chartLoader,
			helmClient:  helmClient,
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

		It("Should deny creation if migration is helm and helm chart is not installed", func() {
			By("simulating an invalid update scenario")
			obj.Spec.Migration = castwarev1alpha1.ComponentMigrationHelm
			obj.Spec.Component = componentName
			obj.Spec.Cluster = clusterName
			obj.SetNamespace("default")
			chartLoader.EXPECT().Load(gomock.Any(), &helm.ChartSource{
				RepoURL: "",
				Name:    "test-helm-chart",
				Version: "",
			})
			helmClient.EXPECT().GetRelease(helm.GetReleaseOptions{
				Namespace:   obj.Namespace,
				ReleaseName: obj.Spec.Component,
			}).Return(nil, fmt.Errorf("helm release not found"))

			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).Error().To(MatchError("failed to validate existing helm release: helm release not found"))
		})

		It("Should deny creation if migration is helm and helm chart is failed", func() {
			By("simulating an invalid update scenario")
			obj.Spec.Migration = castwarev1alpha1.ComponentMigrationHelm
			obj.Spec.Component = componentName
			obj.Spec.Cluster = clusterName
			obj.SetNamespace("default")
			chartLoader.EXPECT().Load(gomock.Any(), &helm.ChartSource{
				RepoURL: "",
				Name:    "test-helm-chart",
				Version: "",
			})
			helmClient.EXPECT().GetRelease(helm.GetReleaseOptions{
				Namespace:   obj.Namespace,
				ReleaseName: obj.Spec.Component,
			}).Return(&release.Release{Info: &release.Info{Status: release.StatusFailed}}, nil)

			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).Error().To(MatchError("failed to validate existing helm release: helm chart is in failed status"))
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

		It("Should admit creation if migration is helm and helm chart is valid", func() {
			By("simulating a valid creation scenario")
			obj.Spec.Migration = castwarev1alpha1.ComponentMigrationHelm
			obj.Spec.Component = componentName
			obj.Spec.Cluster = clusterName
			obj.SetNamespace("default")
			chartLoader.EXPECT().Load(gomock.Any(), &helm.ChartSource{
				RepoURL: "",
				Name:    "test-helm-chart",
				Version: "",
			})
			helmClient.EXPECT().GetRelease(helm.GetReleaseOptions{
				Namespace:   obj.Namespace,
				ReleaseName: obj.Spec.Component,
			}).Return(&release.Release{Info: &release.Info{Status: release.StatusDeployed}}, nil)

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

		It("Should deny update if migration has changed and the new value is not empty", func() {
			By("simulating an invalid update scenario")
			oldObj.Spec.Migration = ""
			obj.Spec.Migration = castwarev1alpha1.ComponentMigrationHelm
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).Error().To(MatchError("components can be migrated only during resource creation"))
		})

		It("Should admit update", func() {
			By("simulating a valid update scenario without version change")
			oldObj.Spec.Component = componentName
			oldObj.Spec.Cluster = clusterName
			oldObj.Spec.Enabled = true
			oldObj.Spec.Version = "0.0.1" // Set same version to avoid upgrade validation
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

	Context("When validating component upgrade", Ordered, func() {
		var apiServer *httptest.Server
		BeforeAll(func() {
			// Spin up a temporary API server for setup
			apiServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{}`))
			}))

			// create a dummy cluster with a valid API key secret
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-api-key-upgrade",
					Namespace: "default",
				},
				Data: map[string][]byte{
					"API_KEY": []byte("dummy-api-key"),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			cluster := &castwarev1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster-upgrade",
					Namespace: "default",
				},
				Spec: castwarev1alpha1.ClusterSpec{
					Provider:     "test",
					APIKeySecret: "test-api-key-upgrade",
					API: castwarev1alpha1.APISpec{
						APIURL: apiServer.URL, // Use temporary server URL
					},
					Cluster: &castwarev1alpha1.ClusterMetadataSpec{
						ClusterID: "12345678-1234-5678-89ab-123456789abc",
					},
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		})

		AfterAll(func() {
			if apiServer != nil {
				apiServer.Close()
			}
		})

		It("Should admit update when upgrade validation passes", func() {
			By("setting up mock server that allows upgrade")
			dummyComponent, _ := json.Marshal(castaitest.CreateComponentObject())

			// Close the old server and create a new one with proper handlers
			if apiServer != nil {
				apiServer.Close()
			}

			apiServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/me":
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{}`))
					return
				case "/cluster-management/v1/components:getByName":
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(dummyComponent)
					return
				case "/cluster-management/v1/clusters/12345678-1234-5678-89ab-123456789abc/components:validateUpgrade":
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"allowed": true, "block_reason": ""}`))
					return
				default:
					w.WriteHeader(http.StatusNotFound)
					return
				}
			}))

			// Update cluster with new API server URL
			cluster := &castwarev1alpha1.Cluster{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "test-cluster-upgrade", Namespace: "default"}, cluster)).To(Succeed())
			cluster.Spec.API.APIURL = apiServer.URL
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

			By("simulating a valid version upgrade")
			oldObj.Spec.Component = componentName
			oldObj.Spec.Cluster = "test-cluster-upgrade"
			oldObj.Spec.Version = "0.116.0"
			oldObj.SetNamespace("default")

			obj.Spec.Component = componentName
			obj.Spec.Cluster = "test-cluster-upgrade"
			obj.Spec.Version = "0.117.0"
			obj.SetNamespace("default")

			chartLoader.EXPECT().Load(gomock.Any(), &helm.ChartSource{
				RepoURL: "",
				Name:    "test-helm-chart",
				Version: "0.117.0",
			})

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(BeNil())
		})

		It("Should deny update when upgrade validation fails due to RBAC changes", func() {
			By("setting up mock server that blocks upgrade")
			dummyComponent, _ := json.Marshal(castaitest.CreateComponentObject())
			apiServer.Close()
			apiServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/me":
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{}`))
					return
				case "/cluster-management/v1/components:getByName":
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(dummyComponent)
					return
				case "/cluster-management/v1/clusters/12345678-1234-5678-89ab-123456789abc/components:validateUpgrade":
					w.WriteHeader(http.StatusOK)
					blockReason := "Component version 0.118.0 requires RBAC permission changes and was released after your operator version. Please upgrade castware-operator (current: 0.45.0 from 2025-01-10) before upgrading castai-agent"
					response := fmt.Sprintf(`{"allowed": false, "block_reason": "%s"}`, blockReason)
					_, _ = w.Write([]byte(response))
					return
				default:
					defer GinkgoRecover()
					Fail(fmt.Sprintf("Unexpected request path: %s", r.URL.Path))
				}
			}))

			// Update cluster with new API server URL
			cluster := &castwarev1alpha1.Cluster{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "test-cluster-upgrade", Namespace: "default"}, cluster)).To(Succeed())
			cluster.Spec.API.APIURL = apiServer.URL
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

			By("simulating a blocked version upgrade")
			oldObj.Spec.Component = componentName
			oldObj.Spec.Cluster = "test-cluster-upgrade"
			oldObj.Spec.Version = "0.117.0"
			oldObj.SetNamespace("default")

			obj.Spec.Component = componentName
			obj.Spec.Cluster = "test-cluster-upgrade"
			obj.Spec.Version = "0.118.0"
			obj.SetNamespace("default")

			chartLoader.EXPECT().Load(gomock.Any(), &helm.ChartSource{
				RepoURL: "",
				Name:    "test-helm-chart",
				Version: "0.118.0",
			})

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("RBAC permission changes"))
			Expect(err.Error()).To(ContainSubstring("upgrade castware-operator"))
		})

		It("Should skip upgrade validation when version does not change", func() {
			By("simulating an update without version change")
			oldObj.Spec.Component = componentName
			oldObj.Spec.Cluster = "test-cluster-upgrade"
			oldObj.Spec.Version = "0.117.0"
			oldObj.Spec.Enabled = true
			oldObj.SetNamespace("default")

			obj.Spec.Component = componentName
			obj.Spec.Cluster = "test-cluster-upgrade"
			obj.Spec.Version = "0.117.0" // Same version
			obj.Spec.Enabled = false     // Only changing enabled flag
			obj.SetNamespace("default")

			chartLoader.EXPECT().Load(gomock.Any(), &helm.ChartSource{
				RepoURL: "",
				Name:    "test-helm-chart",
				Version: "0.117.0",
			})

			// No ValidateComponentUpgrade API call should be made
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(BeNil())
		})
	})
})
