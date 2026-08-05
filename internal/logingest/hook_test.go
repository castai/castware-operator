package logingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

func newTestHook(t *testing.T, level logrus.Level) *Hook {
	t.Helper()
	// Long flush interval so tests that only exercise Fire() never trigger a
	// background flush; explicit ship()/Start() paths are tested separately.
	return NewHook(logrus.New(), "castware-operator", "v0.0.1-test", level, 100, time.Hour, 2*time.Second)
}

func entry(level logrus.Level, msg string, fields logrus.Fields) *logrus.Entry {
	e := &logrus.Entry{
		Logger:  logrus.New(),
		Data:    fields,
		Time:    time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC),
		Level:   level,
		Message: msg,
	}
	if e.Data == nil {
		e.Data = logrus.Fields{}
	}
	return e
}

// snapshot mirrors what Hook.Fire does: convert a logrus entry into the
// immutable queuedEntry the ship path consumes. Tests that exercise ship()
// directly use this to construct input without going through Fire.
func snapshot(e *logrus.Entry) *queuedEntry {
	return &queuedEntry{
		level:   e.Level,
		message: e.Message,
		time:    e.Time,
		fields:  sanitizeFields(e.Data),
	}
}

func TestFireLevelFilter(t *testing.T) {
	t.Parallel()
	// Ingest level = Warn. Info and below are dropped; Warn+ are buffered.
	h := newTestHook(t, logrus.WarnLevel)
	h.UpdateState("11111111-1111-1111-1111-111111111111", "http://localhost", "key", "gke")

	r := require.New(t)
	r.NoError(h.Fire(entry(logrus.InfoLevel, "info-msg", nil)))
	r.NoError(h.Fire(entry(logrus.DebugLevel, "debug-msg", nil)))
	r.NoError(h.Fire(entry(logrus.WarnLevel, "warn-msg", nil)))
	r.NoError(h.Fire(entry(logrus.ErrorLevel, "err-msg", nil)))

	r.Len(h.ch, 2) // warn + error only
}

func TestFireUnboundIsNoOp(t *testing.T) {
	t.Parallel()
	h := newTestHook(t, logrus.InfoLevel)
	// No UpdateState call: state is nil.
	r := require.New(t)
	r.NoError(h.Fire(entry(logrus.InfoLevel, "msg", nil)))
	r.Len(h.ch, 0)
}

func TestFireNeverBlocks(t *testing.T) {
	t.Parallel()
	// Tiny buffer; flood well past capacity. Fire must return promptly (drop).
	h := NewHook(logrus.New(), "c", "v", logrus.InfoLevel, 1, time.Hour, time.Second)
	h.UpdateState("11111111-1111-1111-1111-111111111111", "http://localhost", "key", "gke")

	r := require.New(t)
	start := time.Now()
	for i := 0; i < 1000; i++ {
		r.NoError(h.Fire(entry(logrus.InfoLevel, "msg", nil)))
	}
	elapsed := time.Since(start)
	r.Less(elapsed, time.Second, "Fire should not block when the buffer is full")
}

func TestMapLevel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   logrus.Level
		want string
	}{
		{logrus.TraceLevel, "LOG_LEVEL_DEBUG"},
		{logrus.DebugLevel, "LOG_LEVEL_DEBUG"},
		{logrus.InfoLevel, "LOG_LEVEL_INFO"},
		{logrus.WarnLevel, "LOG_LEVEL_WARNING"},
		{logrus.ErrorLevel, "LOG_LEVEL_ERROR"},
		{logrus.FatalLevel, "LOG_LEVEL_FATAL"},
		{logrus.PanicLevel, "LOG_LEVEL_FATAL"},
		{logrus.Level(99), "LOG_LEVEL_INFO"}, // unknown fallback
	}
	for _, c := range cases {
		require.Equal(t, c.want, mapLevel(c.in))
	}
}

func TestSanitizeFields(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// Reserved keys are stripped; the `error` field is preserved.
	got := sanitizeFields(logrus.Fields{
		"cluster_id":          "spoofed",
		"component":           "spoofed",
		"component_version":   "spoofed",
		"component_timestamp": "spoofed",
		"alert_group":         "spoofed",
		"error":               "boom",
		"node":                "node-abc",
		"count":               42,
	})
	r.Equal("boom", got["error"])
	r.Equal("node-abc", got["node"])
	r.Equal("42", got["count"])
	for _, k := range []string{"cluster_id", "component", "component_version", "component_timestamp", "alert_group"} {
		_, ok := got[k]
		r.False(ok, "reserved key %s should be stripped", k)
	}

	r.Nil(sanitizeFields(nil))
	r.Nil(sanitizeFields(logrus.Fields{"cluster_id": "x"})) // all reserved -> nil
}

func TestShipBodyURLHeaders(t *testing.T) {
	t.Parallel()

	var (
		gotMethod string
		gotPath   string
		gotKey    string
		gotCT     string
		body      ingestLogsRequest
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-API-Key")
		gotCT = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	h := newTestHook(t, logrus.InfoLevel)
	h.UpdateState("11111111-1111-1111-1111-111111111111", srv.URL, "secret-key", "gke")

	h.ship([]*queuedEntry{
		snapshot(entry(logrus.InfoLevel, "rebalance completed", logrus.Fields{"nodes_removed": "2"})),
		snapshot(entry(logrus.ErrorLevel, "failed to drain", logrus.Fields{"node": "node-abc"})),
	})

	r := require.New(t)
	r.Equal(http.MethodPost, gotMethod)
	r.Equal("/v1/clusters/11111111-1111-1111-1111-111111111111/components/castware-operator/logs", gotPath)
	r.Equal("secret-key", gotKey)
	r.Equal("application/json", gotCT)

	r.Equal("v0.0.1-test", body.Version)
	r.Len(body.Entries, 2)
	r.Equal("LOG_LEVEL_INFO", body.Entries[0].Level)
	r.Equal("rebalance completed", body.Entries[0].Message)
	r.Equal("2", body.Entries[0].Fields["nodes_removed"])
	r.Equal("gke", body.Entries[0].Fields["provider"], "provider should be injected into each shipped entry")
	r.Equal("LOG_LEVEL_ERROR", body.Entries[1].Level)
	r.Equal("failed to drain", body.Entries[1].Message)
	r.Equal("gke", body.Entries[1].Fields["provider"])
}

func TestShipHandlesServerError(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"cluster not found"}`))
	}))
	t.Cleanup(srv.Close)

	// Capture the diagnostics logger output so we can assert the server's error
	// body is surfaced (rather than discarded as before).
	var buf bytes.Buffer
	diag := logrus.New()
	diag.SetOutput(&buf)
	diag.SetLevel(logrus.WarnLevel)
	diag.SetFormatter(&logrus.TextFormatter{DisableTimestamp: true})

	h := NewHook(diag, "castware-operator", "v1", logrus.InfoLevel, 100, time.Hour, 2*time.Second)
	h.UpdateState("11111111-1111-1111-1111-111111111111", srv.URL, "key", "gke")

	h.ship([]*queuedEntry{snapshot(entry(logrus.InfoLevel, "msg", nil))})

	out := buf.String()
	r.Contains(out, "status=500")
	r.Contains(out, "cluster not found", "server error body should be surfaced in the warn line")
}

func TestShipHandlesEmptyErrorBody(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	diag := logrus.New()
	diag.SetOutput(&buf)
	diag.SetLevel(logrus.WarnLevel)
	diag.SetFormatter(&logrus.TextFormatter{DisableTimestamp: true})

	h := NewHook(diag, "castware-operator", "v1", logrus.InfoLevel, 100, time.Hour, 2*time.Second)
	h.UpdateState("11111111-1111-1111-1111-111111111111", srv.URL, "key", "gke")

	h.ship([]*queuedEntry{snapshot(entry(logrus.InfoLevel, "msg", nil))})

	out := buf.String()
	r.Contains(out, "status=401")
	r.Contains(out, "empty body", "empty body should be reported as such, not silently dropped")
}

func TestStartFlushesOnBatchSize(t *testing.T) {
	t.Parallel()

	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b ingestLogsRequest
		_ = json.Unmarshal(raw, &b)
		count.Add(int64(len(b.Entries)))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	// batchSize 2, long flush interval so only the size trigger fires.
	h := NewHook(logrus.New(), "c", "v", logrus.InfoLevel, 2, time.Hour, 2*time.Second)
	h.UpdateState("11111111-1111-1111-1111-111111111111", srv.URL, "key", "gke")

	ctx, cancel := contextWithCancel()
	t.Cleanup(cancel)
	go func() { _ = h.Start(ctx) }()

	r := require.New(t)
	r.NoError(h.Fire(entry(logrus.InfoLevel, "m1", nil)))
	r.NoError(h.Fire(entry(logrus.InfoLevel, "m2", nil))) // triggers flush

	require.Eventually(t, func() bool { return count.Load() == 2 }, time.Second, 10*time.Millisecond)
	cancel()
	<-h.done
}

func TestStartFlushesOnInterval(t *testing.T) {
	t.Parallel()

	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b ingestLogsRequest
		_ = json.Unmarshal(raw, &b)
		count.Add(int64(len(b.Entries)))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	// Large batchSize, short interval so the ticker triggers.
	h := NewHook(logrus.New(), "c", "v", logrus.InfoLevel, 1000, 20*time.Millisecond, 2*time.Second)
	h.UpdateState("11111111-1111-1111-1111-111111111111", srv.URL, "key", "gke")

	ctx, cancel := contextWithCancel()
	t.Cleanup(cancel)
	go func() { _ = h.Start(ctx) }()

	r := require.New(t)
	r.NoError(h.Fire(entry(logrus.InfoLevel, "m1", nil)))

	require.Eventually(t, func() bool { return count.Load() == 1 }, time.Second, 10*time.Millisecond)
	cancel()
	<-h.done
}

func TestStartFlushesOnStop(t *testing.T) {
	t.Parallel()

	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b ingestLogsRequest
		_ = json.Unmarshal(raw, &b)
		count.Add(int64(len(b.Entries)))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	h := NewHook(logrus.New(), "c", "v", logrus.InfoLevel, 1000, time.Hour, 2*time.Second)
	h.UpdateState("11111111-1111-1111-1111-111111111111", srv.URL, "key", "gke")

	ctx, cancel := contextWithCancel()
	t.Cleanup(cancel)
	go func() { _ = h.Start(ctx) }()

	r := require.New(t)
	r.NoError(h.Fire(entry(logrus.InfoLevel, "m1", nil)))

	h.Stop() // drain + flush
	r.Eventually(func() bool { return count.Load() == 1 }, time.Second, 10*time.Millisecond)
}

func TestUpdateStateRequiresClusterID(t *testing.T) {
	t.Parallel()
	h := newTestHook(t, logrus.InfoLevel)
	h.UpdateState("", "http://localhost", "key", "")
	r := require.New(t)
	r.Nil(h.state.Load(), "empty clusterID must not enable shipping")

	h.UpdateState("11111111-1111-1111-1111-111111111111", "http://localhost", "key", "gke")
	st := h.state.Load()
	r.NotNil(st)
	r.Equal("11111111-1111-1111-1111-111111111111", st.clusterID)
	r.Equal("gke", st.provider)
}

// TestNewHookEmptyVersionFallsBack ensures an empty version (e.g. a build with
// an unset version ldflag) never produces a server-rejectable payload: the
// IngestLogs endpoint requires min_len: 1 on the version field.
func TestNewHookEmptyVersionFallsBack(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		var b ingestLogsRequest
		_ = json.Unmarshal(raw, &b)
		r.NotEmpty(b.Version, "version must never be empty in the payload")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	h := NewHook(logrus.New(), "castware-operator", "", logrus.InfoLevel, 100, time.Hour, 2*time.Second)
	h.UpdateState("11111111-1111-1111-1111-111111111111", srv.URL, "key", "gke")
	h.ship([]*queuedEntry{snapshot(entry(logrus.InfoLevel, "msg", nil))})
}

// TestFireEntrySurvivesPoolReuse drives a real *logrus.Logger (which pools the
// base entry via sync.Pool) concurrently with the hook attached, then verifies
// every shipped entry's message and fields are intact — i.e. the entry passed to
// Fire is not recycled/mutated before the ship goroutine reads it. This guards
// against the class of bug where a hook retains the raw *logrus.Entry pointer.
func TestFireEntrySurvivesPoolReuse(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	var mu sync.Mutex
	got := make(map[string]string) // message -> "req_id" field value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		var b ingestLogsRequest
		if err := json.Unmarshal(raw, &b); err == nil {
			mu.Lock()
			for _, e := range b.Entries {
				got[e.Message] = e.Fields["req_id"]
			}
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	// Real logger that uses the entry pool, with the hook attached.
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	logger.SetLevel(logrus.InfoLevel)
	h := NewHook(logger, "castware-operator", "v1", logrus.InfoLevel, 50, 10*time.Millisecond, 2*time.Second)
	h.UpdateState("11111111-1111-1111-1111-111111111111", srv.URL, "key", "gke")
	logger.AddHook(h)

	ctx, cancel := contextWithCancel()
	t.Cleanup(cancel)
	go func() { _ = h.Start(ctx) }()

	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("req-%d", i)
			// Each goroutine logs with a distinct field; if the entry were
			// pool-reused, fields/messages would cross-contaminate.
			logger.WithField("req_id", id).Info(id)
		}(i)
	}
	wg.Wait()

	// Flush pending batches.
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-h.done

	mu.Lock()
	defer mu.Unlock()
	// The buffer can drop entries under burst load (drop-on-full by design), so
	// we don't assert completeness. What we DO assert — and what the pool-reuse
	// bug would violate — is that every shipped entry's field matches its own
	// message, i.e. no cross-contamination from a recycled *logrus.Entry.
	r.NotEmpty(got, "some entries must have been shipped")
	var mismatches []string
	for msg, field := range got {
		if msg != field {
			mismatches = append(mismatches, fmt.Sprintf("msg=%q field=%q", msg, field))
		}
	}
	r.Empty(mismatches, "no shipped entry may have a field from a different entry (pool-reuse corruption): %v", mismatches)
}
