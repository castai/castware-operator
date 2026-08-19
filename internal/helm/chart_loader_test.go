package helm

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/cli"
)

// validIndexYAML is a minimal valid Helm index.yaml used across tests.
const validIndexYAML = `apiVersion: v1
generated: "0001-01-01T00:00:00Z"
entries:
  test-chart:
    - name: test-chart
      version: 1.0.0
      urls:
        - https://example.com/test-chart-1.0.0.tgz
`

func newTestLoader(t *testing.T) *remoteChartLoader {
	t.Helper()
	log := logrus.New()
	log.SetLevel(logrus.DebugLevel)
	return &remoteChartLoader{
		log: log,
		envSettings: &cli.EnvSettings{
			RepositoryCache: t.TempDir(),
		},
	}
}

func TestDownloadHelmIndex(t *testing.T) {
	t.Parallel()
	t.Run("valid response", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		cl := newTestLoader(t)

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/yaml")
			_, _ = w.Write([]byte(validIndexYAML))
		}))
		defer srv.Close()

		index, err := cl.downloadHelmIndex(srv.URL)

		r.NoError(err)
		r.NotNil(index)

		versions, ok := index.Entries["test-chart"]
		r.True(ok, "expected entry for test-chart")
		r.Len(versions, 1)

		cv := versions[0]
		r.Equal("test-chart", cv.Name)
		r.Equal("1.0.0", cv.Version)
		r.Len(cv.URLs, 1)
		r.Equal("https://example.com/test-chart-1.0.0.tgz", cv.URLs[0])
	})

	t.Run("empty response", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		cl := newTestLoader(t)

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/yaml")
			_, _ = w.Write(nil)
		}))
		defer srv.Close()

		index, err := cl.downloadHelmIndex(srv.URL)

		r.Error(err)
		r.Nil(index)
		r.True(strings.Contains(err.Error(), "empty"), "expected error to mention 'empty', got: %v", err)
	})

	t.Run("non-OK status", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		cl := newTestLoader(t)

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer srv.Close()

		index, err := cl.downloadHelmIndex(srv.URL)

		r.Error(err)
		r.Nil(index)
		r.True(strings.Contains(err.Error(), "502"), "expected error to mention '502', got: %v", err)
	})

	t.Run("concurrent access", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		cl := newTestLoader(t)

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/yaml")
			_, _ = w.Write([]byte(validIndexYAML))
		}))
		defer srv.Close()

		const goroutines = 16
		var wg sync.WaitGroup
		wg.Add(goroutines)
		errs := make(chan error, goroutines)

		for range goroutines {
			go func() {
				defer wg.Done()
				index, err := cl.downloadHelmIndex(srv.URL)
				if err != nil {
					errs <- err
					return
				}
				if index == nil {
					errs <- errors.New("nil index returned")
					return
				}
				if _, ok := index.Entries["test-chart"]; !ok {
					errs <- errors.New("test-chart entry missing from index")
					return
				}
				errs <- nil
			}()
		}

		wg.Wait()
		close(errs)

		var failures []error
		for err := range errs {
			if err != nil {
				failures = append(failures, err)
			}
		}
		r.Empty(failures, "%d/%d goroutines failed: %v", len(failures), goroutines, failures)
	})
}
