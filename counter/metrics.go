package main

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	reasonLimit       = "limit"
	reasonUnavailable = "unavailable"

	tokenPrompt     = "prompt"
	tokenCompletion = "completion"
)

// metrics labels only ever carry configured model names, key ids from the key file
// and fixed values, never anything read from a request.
type metrics struct {
	registry *prometheus.Registry

	inFlight         *prometheus.GaugeVec
	limit            *prometheus.GaugeVec
	requests         *prometheus.CounterVec
	rejected         *prometheus.CounterVec
	firstByte        *prometheus.HistogramVec
	requestSeconds   *prometheus.HistogramVec
	bodyBytes        *prometheus.HistogramVec
	tokens           *prometheus.CounterVec
	usageMissing     *prometheus.CounterVec
	upstreamBroken   *prometheus.CounterVec
	keysReloadErrors prometheus.Counter
}

func newMetrics() *metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	factory := promauto.With(reg)
	return &metrics{
		registry: reg,
		inFlight: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kukie_counter_in_flight",
			Help: "Requests holding a concurrency slot.",
		}, []string{"model"}),
		limit: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kukie_counter_limit",
			Help: "Configured concurrency limit.",
		}, []string{"model"}),
		requests: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "kukie_counter_requests_total",
			Help: "Requests by HTTP status. 499 means the client left before any response was sent.",
		}, []string{"model", "key", "code"}),
		rejected: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "kukie_counter_rejected_total",
			Help: "Requests refused because the model was at its limit or could not be reached.",
		}, []string{"model", "reason"}),
		firstByte: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kukie_counter_first_byte_seconds",
			Help:    "Seconds from forwarding the request to the first response body byte (2xx only). Not the first token.",
			Buckets: []float64{.01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}, []string{"model"}),
		requestSeconds: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kukie_counter_request_seconds",
			Help:    "Seconds from forwarding the request to the end of the upstream response.",
			Buckets: []float64{.1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1200},
		}, []string{"model"}),
		bodyBytes: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kukie_counter_request_body_bytes",
			Help:    "Request body size for requests naming a configured model.",
			Buckets: prometheus.ExponentialBuckets(1<<10, 4, 9),
		}, []string{"model"}),
		tokens: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "kukie_counter_tokens_total",
			Help: "Tokens from the usage field of responses.",
		}, []string{"model", "key", "type"}),
		usageMissing: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "kukie_counter_usage_missing_total",
			Help: "2xx responses that ended without a usage field.",
		}, []string{"model"}),
		upstreamBroken: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "kukie_counter_upstream_broken_total",
			Help: "Responses cut off by the upstream after the status had already been sent to the client.",
		}, []string{"model"}),
		keysReloadErrors: factory.NewCounter(prometheus.CounterOpts{
			Name: "kukie_counter_keys_reload_errors_total",
			Help: "Failed key file reloads. The previous keys stay in use.",
		}),
	}
}

// initModel creates a model's series up front, so dashboards and alerts see zeros
// rather than nothing before the first request.
func (m *metrics) initModel(model string, limit int) {
	m.limit.WithLabelValues(model).Set(float64(limit))
	m.inFlight.WithLabelValues(model).Set(0)
	m.usageMissing.WithLabelValues(model).Add(0)
	m.upstreamBroken.WithLabelValues(model).Add(0)
	for _, reason := range []string{reasonLimit, reasonUnavailable} {
		m.rejected.WithLabelValues(model, reason).Add(0)
	}
}

func (m *metrics) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}))
	return mux
}
