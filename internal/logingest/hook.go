// Package logingest ships the operator's structured logs to the CAST AI
// ComponentsAPI.IngestLogs endpoint
// (POST /v1/clusters/{cluster_id}/components/{component}/logs) via a logrus hook.
//
// The hook is non-blocking: entries are enqueued to a buffered channel and a
// background goroutine batches and POSTs them. A full channel drops entries
// rather than blocking the caller, so log shipping can never stall the control
// loop. Failed POSTs are best-effort (a single warn line to stdout); the server
// provides no retry/durability, matching the established caller pattern.
package logingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
)

// StateUpdater receives the cluster identity needed to ship logs once the
// cluster has been registered with the mothership. Until UpdateState is called
// with a non-empty clusterID, the hook buffers nothing and ships nothing.
// provider is the cluster's cloud provider, added as a field to every shipped
// entry to match other CAST AI components. Implementations must be safe for
// concurrent use.
type StateUpdater interface {
	UpdateState(clusterID, apiURL, apiKey, provider string)
}

// reservedFields are stripped from each entry's fields before shipping, then
// re-derived server-side. Mirrors the components service's removeReadOnlyFields
// (see ingest-logs-endpoints.md): prevents a sender from spoofing identity.
var reservedFields = map[string]struct{}{
	"cluster_id":          {},
	"alert_group":         {},
	"component":           {},
	"component_version":   {},
	"component_timestamp": {},
}

// ingestState is the ship-to target plus cluster-derived context injected into
// every shipped entry. Stored atomically so Fire() (hot path) never takes a
// lock.
type ingestState struct {
	clusterID string
	apiURL    string
	apiKey    string
	provider  string
}

// logEntry mirrors the IngestLogs LogEntry shape (ingest-logs-endpoints.md).
type logEntry struct {
	Level   string            `json:"level"`
	Message string            `json:"message"`
	Time    time.Time         `json:"time"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// ingestLogsRequest is the HTTP body for POST .../components/{component}/logs.
// The body maps to the proto's `logs` field (grpc-gateway `body: "logs"`), so it
// is the ComponentLogs message itself — flat {version, entries} — NOT wrapped in
// {"logs": ...}. This matches the reference callers (workload-autoscaler,
// logging/components); the "logs" wrapper in the spec's HTTP example is
// misleading.
type ingestLogsRequest struct {
	Version string     `json:"version"`
	Entries []logEntry `json:"entries"`
}

// Hook is a logrus.Hook that ships logs to the IngestLogs endpoint.
// It is safe for concurrent use.
type Hook struct {
	component  string
	version    string
	level      logrus.Level
	batchSize  int
	flushTick  time.Duration
	reqTimeout time.Duration
	httpClient *http.Client
	log        logrus.FieldLogger

	state atomic.Pointer[ingestState]
	ch    chan *logrus.Entry
	stop  chan struct{}
	done  chan struct{}
}

// NewHook returns a hook that ships logs at or above the given level. It does
// not start shipping until Start is called and UpdateState provides a clusterID.
//
// The diag logger receives the hook's own diagnostics (e.g. a failed POST). It
// MUST be a logger that does not have this hook attached, otherwise ship-failure
// warnings would re-enter the hook and form a feedback loop. Callers pass a
// plain (non-instrumented) logger for this.
func NewHook(diag logrus.FieldLogger, component, version string, level logrus.Level, batchSize int, flushInterval, reqTimeout time.Duration) *Hook {
	// The IngestLogs endpoint requires a non-empty version (min_len: 1). Fall back
	// to the component name if the caller passed an empty string (e.g. a build
	// with an unset version ldflag) so a misconfiguration never causes 400s.
	if version == "" {
		version = component
	}
	return &Hook{
		component:  component,
		version:    version,
		level:      level,
		batchSize:  batchSize,
		flushTick:  flushInterval,
		reqTimeout: reqTimeout,
		httpClient: &http.Client{Timeout: reqTimeout},
		log:        diag,
		ch:         make(chan *logrus.Entry, batchSize*2+1),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
}

// Levels returns all levels; the logger's own level gate already drops
// sub-level entries before Fire is called. Fire applies a second (ingest)
// level filter.
func (h *Hook) Levels() []logrus.Level {
	return logrus.AllLevels
}

// Fire is called by logrus on every emitted entry. It never blocks: if the
// buffer is full the entry is dropped.
func (h *Hook) Fire(entry *logrus.Entry) error {
	// Cheapest check first: level filter. Lower numeric value == higher severity,
	// so ship entries at least as severe as the configured ingest level.
	if entry.Level > h.level {
		return nil
	}
	// No cluster context yet: cluster not registered. Drop silently.
	if h.state.Load() == nil {
		return nil
	}
	select {
	case h.ch <- entry:
	default:
		// Buffer full: drop rather than block the caller.
	}
	return nil
}

// UpdateState sets the ship-to target and the cluster's provider. Once
// clusterID is non-empty the hook begins shipping queued entries. Idempotent and
// safe to call repeatedly.
func (h *Hook) UpdateState(clusterID, apiURL, apiKey, provider string) {
	if clusterID == "" {
		return
	}
	h.state.Store(&ingestState{
		clusterID: clusterID,
		apiURL:    apiURL,
		apiKey:    apiKey,
		provider:  provider,
	})
}

// Start runs the drain/batch goroutine until ctx is cancelled or Stop is
// called. It blocks until shutdown is complete, satisfying manager.Runnable.
func (h *Hook) Start(ctx context.Context) error {
	defer close(h.done)

	ticker := time.NewTicker(h.flushTick)
	defer ticker.Stop()

	var batch []*logrus.Entry

	flush := func() {
		if len(batch) == 0 {
			return
		}
		h.ship(batch)
		batch = batch[:0]
	}

	for {
		select {
		case <-ctx.Done():
			// Drain anything left in the channel, then flush the partial batch.
			for {
				select {
				case e := <-h.ch:
					batch = append(batch, e)
				default:
					flush()
					return nil
				}
			}
		case <-h.stop:
			for {
				select {
				case e := <-h.ch:
					batch = append(batch, e)
				default:
					flush()
					return nil
				}
			}
		case e := <-h.ch:
			batch = append(batch, e)
			if len(batch) >= h.batchSize {
				flush()
			}
		case <-ticker.C:
			// Bounded latency at low volume: flush whatever has accumulated.
			flush()
		}
	}
}

// Stop signals the drain goroutine to shut down and waits for it. Safe to call
// at most once.
func (h *Hook) Stop() {
	select {
	case <-h.stop:
		// Already stopped.
	default:
		close(h.stop)
	}
	<-h.done
}

// ship POSTs a batch of entries to the IngestLogs endpoint. Best-effort: on
// failure it logs a single warn line to stdout and drops the batch.
func (h *Hook) ship(entries []*logrus.Entry) {
	state := h.state.Load()
	if state == nil || state.clusterID == "" {
		return
	}

	body := ingestLogsRequest{
		Version: h.version,
		Entries: make([]logEntry, 0, len(entries)),
	}
	for _, e := range entries {
		fields := sanitizeFields(e.Data)
		// Inject cluster-derived context so every shipped entry carries the
		// same identity fields other CAST AI components emit (e.g. provider=gke).
		// version is expected on the logger already (WithField at startup).
		if state.provider != "" {
			if fields == nil {
				fields = make(map[string]string, 1)
			}
			fields["provider"] = state.provider
		}
		body.Entries = append(body.Entries, logEntry{
			Level:   mapLevel(e.Level),
			Message: e.Message,
			Time:    e.Time,
			Fields:  fields,
		})
	}

	payload, err := json.Marshal(body)
	if err != nil {
		h.log.WithError(err).Warn("Failed to marshal log ingest batch")
		return
	}

	url := fmt.Sprintf("%s/v1/clusters/%s/components/%s/logs", state.apiURL, state.clusterID, h.component)
	// Use a fresh context rather than the manager ctx: during shutdown the drain
	// flush runs after ctx is already cancelled, and we still want it to ship.
	reqCtx, cancel := context.WithTimeout(context.Background(), h.reqTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		h.log.WithError(err).Warn("Failed to build log ingest request")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", state.apiKey) //nolint:gosec // API key header, not a credential leak.

	resp, err := h.httpClient.Do(req)
	if err != nil {
		h.log.WithError(err).Warn("Failed to ship log ingest batch")
		return
	}
	// Read the body so we can surface the server's error message on non-2xx,
	// and to allow connection reuse. Cap at a small size to avoid unbounded reads.
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()

	if resp.StatusCode >= 400 {
		// Surface the server's explanation (field violations, validation errors,
		// auth failures, etc.) so the rejection is actionable rather than a bare
		// status code. Trimmed to keep the warn line readable.
		trim := strings.TrimSpace(string(respBody))
		if trim == "" {
			h.log.WithField("status", resp.StatusCode).Warn("Log ingest batch rejected with empty body")
		} else {
			h.log.WithFields(logrus.Fields{
				"status": resp.StatusCode,
				"body":   trim,
			}).Warn("Log ingest batch rejected")
		}
	}
}

// mapLevel converts a logrus level to the IngestLogs LogLevel enum name.
// Unknown/panic levels map to FATAL (panics are the log we most want upstream).
func mapLevel(l logrus.Level) string {
	switch l {
	case logrus.TraceLevel, logrus.DebugLevel:
		return "LOG_LEVEL_DEBUG"
	case logrus.InfoLevel:
		return "LOG_LEVEL_INFO"
	case logrus.WarnLevel:
		return "LOG_LEVEL_WARNING"
	case logrus.ErrorLevel:
		return "LOG_LEVEL_ERROR"
	case logrus.FatalLevel, logrus.PanicLevel:
		return "LOG_LEVEL_FATAL"
	default:
		return "LOG_LEVEL_INFO"
	}
}

// sanitizeFields converts logrus fields to string map and strips reserved keys
// that the server re-derives (removeReadOnlyFields, ingest-logs-endpoints.md).
func sanitizeFields(data logrus.Fields) map[string]string {
	if len(data) == 0 {
		return nil
	}
	out := make(map[string]string, len(data))
	for k, v := range data {
		if _, reserved := reservedFields[k]; reserved {
			continue
		}
		out[k] = fmt.Sprint(v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
