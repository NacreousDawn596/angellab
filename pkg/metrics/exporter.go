// Package metrics implements a Prometheus-compatible HTTP metrics endpoint
// for the AngelLab daemon.
//
// Zero external dependencies — the Prometheus text exposition format
// (version 0.0.4) is simple enough to write by hand.  This keeps the
// binary small and avoids pulling in the full prometheus/client_golang tree.
//
// Endpoint: GET /metrics  (default: :9101/metrics)
//
// Exposed metrics:
//
//	angellab_angel_state{angel_id,angel_type,conn_state}      gauge 0/1
//	angellab_angel_restarts_total{angel_id,angel_type}        counter
//	angellab_angel_rss_bytes{angel_id,angel_type}             gauge
//	angellab_angel_cpu_percent{angel_id,angel_type}           gauge
//	angellab_angel_fd_count{angel_id,angel_type}              gauge
//	angellab_angel_goroutines{angel_id,angel_type}            gauge
//	angellab_angel_uptime_seconds{angel_id,angel_type}        gauge
//	angellab_events_total{angel_id,angel_type,severity}       counter
//	angellab_lab_uptime_seconds                               gauge
//	angellab_lab_angel_count                                  gauge
//
// The endpoint is served on a separate goroutine and never touches the
// angel registry directly — it reads a snapshot from MetricsSnapshot
// which is updated by the Supervisor on every heartbeat.
package metrics

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Snapshot types (populated by Supervisor/Server on each heartbeat)
// ---------------------------------------------------------------------------

// AngelMetrics is a point-in-time snapshot of one angel's metrics.
// The Supervisor updates this after every HeartbeatPayload.
type AngelMetrics struct {
	ID           string
	AngelType    string
	State        string  // ACTIVE, TRAINING, UNSTABLE, etc.
	ConnState    string  // ACTIVE, RECOVERING, LOST, etc.
	RestartCount int
	RSSBytes     uint64
	CPUPercent   float64
	FDCount      int
	Goroutines   int
	UptimeSecs   int64
	UpdatedAt    time.Time
}

// EventCounter tracks emitted events per angel+severity.
type EventCounter struct {
	AngelID   string
	AngelType string
	Severity  string // INFO, WARN, CRIT
	Total     int64
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// Registry holds the current metrics snapshot and serves /metrics.
// It is designed for high-frequency writes (every heartbeat, ~10s) and
// low-frequency reads (Prometheus scrape, ~15s).
type Registry struct {
	mu sync.RWMutex

	angels map[string]*AngelMetrics // angel_id → metrics
	events map[string]*EventCounter // "angel_id:severity" → counter

	labStartedAt time.Time
}

// NewRegistry creates an empty Registry with the daemon start time.
func NewRegistry(startedAt time.Time) *Registry {
	return &Registry{
		angels:       make(map[string]*AngelMetrics),
		events:       make(map[string]*EventCounter),
		labStartedAt: startedAt,
	}
}

// UpdateAngel upserts the metrics for one angel.
// Called by the Supervisor after every successful heartbeat decode.
func (r *Registry) UpdateAngel(m *AngelMetrics) {
	r.mu.Lock()
	r.angels[m.ID] = m
	r.mu.Unlock()
}

// RemoveAngel removes an angel's metrics (called on TERMINATED).
func (r *Registry) RemoveAngel(id string) {
	r.mu.Lock()
	delete(r.angels, id)
	r.mu.Unlock()
}

// IncrementEvent records one emitted event.
func (r *Registry) IncrementEvent(angelID, angelType, severity string) {
	key := angelID + ":" + severity
	r.mu.Lock()
	if _, ok := r.events[key]; !ok {
		r.events[key] = &EventCounter{
			AngelID:   angelID,
			AngelType: angelType,
			Severity:  severity,
		}
	}
	r.events[key].Total++
	r.mu.Unlock()
}

// ---------------------------------------------------------------------------
// HTTP handler
// ---------------------------------------------------------------------------

// Handler returns an http.HandlerFunc that serves the Prometheus text format.
func (r *Registry) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)

		if req.Method == http.MethodHead {
			return
		}

		r.mu.RLock()
		angels := make([]*AngelMetrics, 0, len(r.angels))
		for _, a := range r.angels {
			angels = append(angels, a)
		}
		events := make([]*EventCounter, 0, len(r.events))
		for _, e := range r.events {
			events = append(events, e)
		}
		labStartedAt := r.labStartedAt
		r.mu.RUnlock()

		var b strings.Builder
		now := time.Now()

		// angellab_lab_uptime_seconds
		writeHelp(&b, "angellab_lab_uptime_seconds", "gauge",
			"Seconds since the Lab daemon started")
		writeLine(&b, "angellab_lab_uptime_seconds", nil,
			fmt.Sprintf("%.1f", now.Sub(labStartedAt).Seconds()))

		// angellab_lab_angel_count
		writeHelp(&b, "angellab_lab_angel_count", "gauge",
			"Number of registered angel processes")
		writeLine(&b, "angellab_lab_angel_count", nil,
			fmt.Sprintf("%d", len(angels)))

		// Per-angel metrics
		writeHelp(&b, "angellab_angel_state", "gauge",
			"1 if the angel is in the given state, 0 otherwise")
		for _, a := range angels {
			for _, state := range []string{"ACTIVE", "TRAINING", "UNSTABLE", "CONTAINED", "TERMINATED"} {
				v := "0"
				if a.State == state {
					v = "1"
				}
				writeLine(&b, "angellab_angel_state",
					map[string]string{
						"angel_id":   a.ID,
						"angel_type": a.AngelType,
						"state":      state,
					}, v)
			}
		}

		writeHelp(&b, "angellab_angel_restarts_total", "counter",
			"Total number of restarts for this angel")
		for _, a := range angels {
			writeLine(&b, "angellab_angel_restarts_total",
				map[string]string{"angel_id": a.ID, "angel_type": a.AngelType},
				fmt.Sprintf("%d", a.RestartCount))
		}

		writeHelp(&b, "angellab_angel_rss_bytes", "gauge",
			"Resident set size of the angel process in bytes")
		for _, a := range angels {
			writeLine(&b, "angellab_angel_rss_bytes",
				map[string]string{"angel_id": a.ID, "angel_type": a.AngelType},
				fmt.Sprintf("%d", a.RSSBytes))
		}

		writeHelp(&b, "angellab_angel_cpu_percent", "gauge",
			"CPU utilisation of the angel process (0–100)")
		for _, a := range angels {
			writeLine(&b, "angellab_angel_cpu_percent",
				map[string]string{"angel_id": a.ID, "angel_type": a.AngelType},
				fmt.Sprintf("%.2f", a.CPUPercent))
		}

		writeHelp(&b, "angellab_angel_fd_count", "gauge",
			"Open file descriptor count of the angel process")
		for _, a := range angels {
			writeLine(&b, "angellab_angel_fd_count",
				map[string]string{"angel_id": a.ID, "angel_type": a.AngelType},
				fmt.Sprintf("%d", a.FDCount))
		}

		writeHelp(&b, "angellab_angel_goroutines", "gauge",
			"Number of goroutines in the angel process")
		for _, a := range angels {
			writeLine(&b, "angellab_angel_goroutines",
				map[string]string{"angel_id": a.ID, "angel_type": a.AngelType},
				fmt.Sprintf("%d", a.Goroutines))
		}

		writeHelp(&b, "angellab_angel_uptime_seconds", "gauge",
			"Seconds since the angel process last registered")
		for _, a := range angels {
			writeLine(&b, "angellab_angel_uptime_seconds",
				map[string]string{"angel_id": a.ID, "angel_type": a.AngelType},
				fmt.Sprintf("%d", a.UptimeSecs))
		}

		// Event counters
		writeHelp(&b, "angellab_events_total", "counter",
			"Total events emitted by each angel, by severity")
		for _, e := range events {
			writeLine(&b, "angellab_events_total",
				map[string]string{
					"angel_id":   e.AngelID,
					"angel_type": e.AngelType,
					"severity":   e.Severity,
				},
				fmt.Sprintf("%d", e.Total))
		}

		fmt.Fprint(w, b.String())
	}
}

// ---------------------------------------------------------------------------
// HTTP server
// ---------------------------------------------------------------------------

// Server wraps the Registry with an HTTP listener.
type Server struct {
	Registry *Registry
	addr     string
	server   *http.Server
}

// NewServer creates a metrics Server bound to addr (e.g. ":9101").
func NewServer(addr string, startedAt time.Time) *Server {
	reg := NewRegistry(startedAt)
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", reg.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	return &Server{
		Registry: reg,
		addr:     addr,
		server: &http.Server{
			Handler:      mux,
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
	}
}

// ListenAndServe starts the HTTP server in the background.
// Returns the actual bound address (useful when addr is ":0" in tests).
func (s *Server) ListenAndServe() (string, error) {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return "", fmt.Errorf("metrics: listen %s: %w", s.addr, err)
	}
	s.server.Addr = ln.Addr().String()
	go func() { _ = s.server.Serve(ln) }()
	return s.server.Addr, nil
}

// Close shuts down the metrics HTTP server.
func (s *Server) Close() error {
	return s.server.Close()
}

// ---------------------------------------------------------------------------
// Text format helpers
// ---------------------------------------------------------------------------

// writeHelp emits a HELP + TYPE comment block.
func writeHelp(b *strings.Builder, name, typ, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

// writeLine emits one metric sample line.
// labels is a map of label name → value; nil = no labels.
func writeLine(b *strings.Builder, name string, labels map[string]string, value string) {
	if len(labels) == 0 {
		fmt.Fprintf(b, "%s %s\n", name, value)
		return
	}
	// Stable label ordering for deterministic output.
	order := []string{
		"angel_id", "angel_type", "state", "conn_state", "severity",
	}
	var parts []string
	for _, k := range order {
		if v, ok := labels[k]; ok {
			parts = append(parts, fmt.Sprintf(`%s=%q`, k, v))
		}
	}
	// Any remaining labels not in the ordered list.
	for k, v := range labels {
		found := false
		for _, o := range order {
			if k == o {
				found = true
				break
			}
		}
		if !found {
			parts = append(parts, fmt.Sprintf(`%s=%q`, k, v))
		}
	}
	fmt.Fprintf(b, "%s{%s} %s\n", name, strings.Join(parts, ","), value)
}
