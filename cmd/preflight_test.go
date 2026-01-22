package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"

	"github.com/castai/castware-operator/internal/castai"
	"github.com/castai/castware-operator/internal/config"
)

const indexYamlPath = "/index.yaml"

func TestCheckHTTPClient(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.ErrorLevel) // Reduce noise in tests

	cfg := &config.Config{
		RequestTimeout: 10 * time.Second,
	}

	t.Run(
		"success - component found", func(t *testing.T) {
			// Create a test server that returns a valid component
			server := httptest.NewServer(
				http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/cluster-management/v1/components:getByName" &&
							r.URL.Query().Get("name") == "castware-operator" {
							component := &castai.Component{
								Id:            "test-id",
								Name:          "castware-operator",
								LatestVersion: "1.0.0",
							}
							w.Header().Set("Content-Type", "application/json")
							err := json.NewEncoder(w).Encode(component)
							assert.NoError(t, err)
							return
						}
						http.NotFound(w, r)
					},
				),
			)
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			err := checkHTTPClient(ctx, log, cfg, server.URL, "test-api-key")
			assert.NoError(t, err)
		},
	)

	t.Run(
		"failure - component not found (404)", func(t *testing.T) {
			// Create a test server that returns 404
			server := httptest.NewServer(
				http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusNotFound)
						err := json.NewEncoder(w).Encode(map[string]string{"message": "component not found"})
						assert.NoError(t, err)
					},
				),
			)
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			err := checkHTTPClient(ctx, log, cfg, server.URL, "test-api-key")
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "Cannot connect to CAST AI API")
		},
	)

	t.Run(
		"failure - invalid component response (empty ID)", func(t *testing.T) {
			// Create a test server that returns a component with empty ID
			server := httptest.NewServer(
				http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) {
						component := &castai.Component{
							Id:            "", // Empty ID
							Name:          "castware-operator",
							LatestVersion: "1.0.0",
						}
						w.Header().Set("Content-Type", "application/json")
						err := json.NewEncoder(w).Encode(component)
						assert.NoError(t, err)
					},
				),
			)
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			err := checkHTTPClient(ctx, log, cfg, server.URL, "test-api-key")
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "Invalid API response")
		},
	)

	t.Run(
		"failure - unauthorized (401)", func(t *testing.T) {
			// Create a test server that returns 401
			server := httptest.NewServer(
				http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusUnauthorized)
						err := json.NewEncoder(w).Encode(map[string]string{"message": "unauthorized"})
						assert.NoError(t, err)
					},
				),
			)
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			err := checkHTTPClient(ctx, log, cfg, server.URL, "invalid-api-key")
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "Cannot connect to CAST AI API")
		},
	)

	t.Run(
		"failure - server unreachable", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			// Use an unreachable address
			err := checkHTTPClient(ctx, log, cfg, "http://localhost:1", "test-api-key")
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "Cannot connect to CAST AI API")
		},
	)

	t.Run(
		"failure - context timeout", func(t *testing.T) {
			// Create a test server that delays response
			server := httptest.NewServer(
				http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) {
						time.Sleep(100 * time.Millisecond) // Just long enough to trigger timeout
						w.WriteHeader(http.StatusOK)
					},
				),
			)
			defer server.Close()

			// Use a very short timeout to force a timeout
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()

			cfg := &config.Config{
				RequestTimeout: 10 * time.Millisecond,
			}

			err := checkHTTPClient(ctx, log, cfg, server.URL, "test-api-key")
			assert.Error(t, err)
		},
	)
}

func TestCheckHelmAvailability(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.ErrorLevel) // Reduce noise in tests

	t.Run(
		"failure - chart not found in index", func(t *testing.T) {
			// Create a test server that returns an index without castai-agent
			server := httptest.NewServer(
				http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == indexYamlPath {
							// Return empty index
							w.Header().Set("Content-Type", "application/x-yaml")
							_, err := w.Write(
								[]byte(`apiVersion: v1
entries: {}
generated: "2024-01-01T00:00:00Z"
`),
							)
							assert.NoError(t, err)
							return
						}
						http.NotFound(w, r)
					},
				),
			)
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			err := checkHelmAvailability(ctx, log, server.URL)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "castai-agent chart not found")
		},
	)

	t.Run(
		"failure - helm repo unreachable", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			// Use an unreachable address
			err := checkHelmAvailability(ctx, log, "http://localhost:1")
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "helm repository")
		},
	)

	t.Run(
		"failure - invalid helm repo URL", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			err := checkHelmAvailability(ctx, log, "not-a-valid-url")
			assert.Error(t, err)
			// Could be either initialization error or download error
			assert.True(
				t, strings.Contains(err.Error(), "helm repository"),
			)
		},
	)

	t.Run(
		"success - chart pulled successfully", func(t *testing.T) {
			// Create a test server that returns a valid index and chart archive
			var serverURL string
			server := httptest.NewServer(
				http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == indexYamlPath {
							// Return index with castai-agent
							w.Header().Set("Content-Type", "application/x-yaml")
							indexYAML := "apiVersion: v1\n" +
								"entries:\n" +
								"  castai-agent:\n" +
								"    - name: castai-agent\n" +
								"      version: 1.0.0\n" +
								"      urls:\n" +
								"        - " + serverURL + "/castai-agent-1.0.0.tgz\n" +
								"generated: \"2024-01-01T00:00:00Z\"\n"
							_, err := w.Write([]byte(indexYAML))
							assert.NoError(t, err)
							return
						}
						if r.URL.Path == "/castai-agent-1.0.0.tgz" {
							// Return a valid helm chart archive
							w.Header().Set("Content-Type", "application/gzip")
							_, err := w.Write(createMinimalHelmChart(t, "castai-agent", "1.0.0"))
							assert.NoError(t, err)

							return
						}
						http.NotFound(w, r)
					},
				),
			)
			serverURL = server.URL
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			err := checkHelmAvailability(ctx, log, server.URL)
			assert.NoError(t, err)
		},
	)

	t.Run(
		"failure - chart archive not accessible", func(t *testing.T) {
			// Create a test server that returns index but 404 for chart archive
			var serverURL string
			server := httptest.NewServer(
				http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == indexYamlPath {
							// Return index with castai-agent
							w.Header().Set("Content-Type", "application/x-yaml")
							indexYAML := "apiVersion: v1\n" +
								"entries:\n" +
								"  castai-agent:\n" +
								"    - name: castai-agent\n" +
								"      version: 1.0.0\n" +
								"      urls:\n" +
								"        - " + serverURL + "/castai-agent-1.0.0.tgz\n" +
								"generated: \"2024-01-01T00:00:00Z\"\n"
							_, err := w.Write([]byte(indexYAML))
							assert.NoError(t, err)

							return
						}
						// Return 404 for chart archive
						http.NotFound(w, r)
					},
				),
			)
			serverURL = server.URL
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			err := checkHelmAvailability(ctx, log, server.URL)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "Cannot download Helm chart")
		},
	)

	t.Run(
		"failure - empty chart entries", func(t *testing.T) {
			// Create a test server that returns an index with castai-agent but no versions
			server := httptest.NewServer(
				http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == indexYamlPath {
							// Return index with empty castai-agent entries
							w.Header().Set("Content-Type", "application/x-yaml")
							indexYAML := "apiVersion: v1\n" +
								"entries:\n" +
								"  castai-agent: []\n" +
								"generated: \"2024-01-01T00:00:00Z\"\n"
							_, err := w.Write([]byte(indexYAML))
							assert.NoError(t, err)

							return
						}
						http.NotFound(w, r)
					},
				),
			)
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			err := checkHelmAvailability(ctx, log, server.URL)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "castai-agent chart not found")
		},
	)
}

// createMinimalHelmChart creates a minimal valid helm chart tar.gz archive for testing.
func createMinimalHelmChart(t *testing.T, chartName, version string) []byte {
	t.Helper()

	var buf bytes.Buffer
	gzipWriter := gzip.NewWriter(&buf)
	tarWriter := tar.NewWriter(gzipWriter)

	// Create Chart.yaml content
	chartYAML := fmt.Sprintf(
		`apiVersion: v2
name: %s
version: %s
description: Test chart for preflight checks
type: application
`, chartName, version,
	)

	// Add Chart.yaml to tar
	chartYAMLHeader := &tar.Header{
		Name: chartName + "/Chart.yaml",
		Mode: 0644,
		Size: int64(len(chartYAML)),
	}
	if err := tarWriter.WriteHeader(chartYAMLHeader); err != nil {
		t.Fatalf("failed to write chart.yaml header: %v", err)
	}
	if _, err := tarWriter.Write([]byte(chartYAML)); err != nil {
		t.Fatalf("failed to write chart.yaml content: %v", err)
	}

	// Close tar and gzip writers
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("failed to close tar writer: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("failed to close gzip writer: %v", err)
	}

	return buf.Bytes()
}
