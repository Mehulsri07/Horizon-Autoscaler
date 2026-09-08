package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ----------------------------------------------------------------------------
// Metrics definitions
// ----------------------------------------------------------------------------

var (
	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests by endpoint and status code.",
		},
		[]string{"endpoint", "status"},
	)

	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency in seconds by endpoint.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"endpoint"},
	)
)

func init() {
	prometheus.MustRegister(httpRequestsTotal)
	prometheus.MustRegister(httpRequestDuration)
}

// ----------------------------------------------------------------------------
// Config helpers
// ----------------------------------------------------------------------------

// envInt reads an integer environment variable with a fallback default.
func envInt(key string, defaultVal int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return defaultVal
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("warning: invalid value for %s (%q), using default %d", key, raw, defaultVal)
		return defaultVal
	}
	return v
}

// ----------------------------------------------------------------------------
// Instrumented response writer — captures the status code written downstream.
// ----------------------------------------------------------------------------

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// ----------------------------------------------------------------------------
// Middleware
// ----------------------------------------------------------------------------

// metricsMiddleware wraps an http.Handler and records request count + latency
// for every route.
func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		dur := time.Since(start).Seconds()

		endpoint := r.URL.Path
		statusStr := strconv.Itoa(rec.status)

		httpRequestsTotal.WithLabelValues(endpoint, statusStr).Inc()
		httpRequestDuration.WithLabelValues(endpoint).Observe(dur)
	})
}

// ----------------------------------------------------------------------------
// Handlers
// ----------------------------------------------------------------------------

// healthzHandler returns 200 once the server is ready, 503 otherwise.
func healthzHandler(isReady *atomic.Bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isReady.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "not ready"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

// workHandler performs a configurable unit of artificial CPU-bound work.
// The work is bounded by a per-request context timeout so a misconfigured
// WORK_DURATION_MS can never hang the server indefinitely.
func workHandler(workDuration time.Duration, workTimeout time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Apply timeout; honour the shorter of the incoming request context and
		// our own cap so we don't exceed workTimeout.
		ctx, cancel := context.WithTimeout(r.Context(), workTimeout)
		defer cancel()

		// Simulate work with a timer instead of time.Sleep so we respect ctx
		// cancellation and avoid leaking goroutines.
		timer := time.NewTimer(workDuration)
		defer timer.Stop()

		select {
		case <-timer.C:
			// Work completed within budget.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status":      "ok",
				"work_done_ms": fmt.Sprintf("%d", workDuration.Milliseconds()),
			})

		case <-ctx.Done():
			// Timed out — either the client disconnected or workTimeout exceeded.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusGatewayTimeout)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "timeout",
				"error":  ctx.Err().Error(),
			})
		}
	}
}

// ----------------------------------------------------------------------------
// Server setup
// ----------------------------------------------------------------------------

// NewServer constructs an http.ServeMux wired with all routes and the
// metrics middleware, and returns the resulting http.Handler. Separating this
// from main() makes it easy to spin up a test server without a real port.
func NewServer(isReady *atomic.Bool, workDuration, workTimeout time.Duration) http.Handler {
	mux := http.NewServeMux()

	mux.Handle("/healthz", healthzHandler(isReady))
	mux.Handle("/work", workHandler(workDuration, workTimeout))
	mux.Handle("/metrics", promhttp.Handler())

	return metricsMiddleware(mux)
}

// ----------------------------------------------------------------------------
// Entry point
// ----------------------------------------------------------------------------

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	workDurationMs := envInt("WORK_DURATION_MS", 100)
	workTimeoutMs := envInt("WORK_TIMEOUT_MS", 5000)

	workDuration := time.Duration(workDurationMs) * time.Millisecond
	workTimeout := time.Duration(workTimeoutMs) * time.Millisecond

	var isReady atomic.Bool

	handler := NewServer(&isReady, workDuration, workTimeout)

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Mark ready before starting to accept connections.
	isReady.Store(true)

	log.Printf("horizon-app starting on :%s (WORK_DURATION_MS=%d, WORK_TIMEOUT_MS=%d)",
		port, workDurationMs, workTimeoutMs)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}
