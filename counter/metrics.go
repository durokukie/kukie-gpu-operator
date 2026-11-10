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
	reasonBrake       = "brake"

	tokenPrompt     = "prompt"
	tokenCompletion = "completion"
)

// metrics labels only ever carry configured model names, key ids from the key file
// and fixed values, never anything read from a request.
type metrics struct {
	registry *prometheus.Registry

	inFlight         *prometheus.GaugeVec
	limit            *prometheus.GaugeVec
	limitSource      *prometheus.GaugeVec
	requests         *prometheus.CounterVec
	rejected         *prometheus.CounterVec
	firstByte        *prometheus.HistogramVec
	requestSeconds   *prometheus.HistogramVec
	bodyBytes        *prometheus.HistogramVec
	tokens           *prometheus.CounterVec
	usageMissing     *prometheus.CounterVec
	upstreamBroken   *prometheus.CounterVec
	keysReloadErrors prometheus.Counter

	ttft              *prometheus.HistogramVec
	itl               *prometheus.HistogramVec
	prefillInFlight   *prometheus.GaugeVec
	adaptiveLimit     *prometheus.GaugeVec
	adaptiveDecisions *prometheus.CounterVec
	windowITL         *prometheus.GaugeVec
	windowTTFT        *prometheus.GaugeVec

	upstreamWaiting       *prometheus.GaugeVec
	upstreamKV            *prometheus.GaugeVec
	upstreamMetricsErrors *prometheus.CounterVec
	brakeActive           *prometheus.GaugeVec

	discoveryErrors      prometheus.Counter
	discoveryLastSuccess prometheus.Gauge
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
			Help: "Current concurrency limit. kukie_counter_limit_source says where it comes from.",
		}, []string{"model"}),
		limitSource: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kukie_counter_limit_source",
			Help: "Always 1, on the series naming where the current limit comes from: source config, baseline or default, and why.",
		}, []string{"model", "source", "reason"}),
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
		ttft: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name: "kukie_counter_ttft_seconds",
			Help: "Streamed requests: seconds from forwarding to the first chunk holding generated text, " +
				"less the time spent writing to the client.",
			Buckets: []float64{.05, .1, .25, .5, .75, 1, 1.5, 2, 3, 5, 10, 30},
		}, []string{"model"}),
		itl: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name: "kukie_counter_itl_seconds",
			Help: "Streamed requests: seconds waited on the model between two reads holding generated text. " +
				"Time spent writing to the client is left out.",
			Buckets: []float64{.005, .01, .02, .03, .04, .05, .075, .1, .15, .25, .5, 1},
		}, []string{"model"}),
		prefillInFlight: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kukie_counter_prefill_in_flight",
			Help: "Streamed requests forwarded that have not received their first token yet.",
		}, []string{"model"}),
		adaptiveLimit: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kukie_counter_adaptive_limit",
			Help: "The limit the adaptive controller decided. In observe mode it is not enforced; " +
				"kukie_counter_limit is. Absent when a person set maxConcurrent.",
		}, []string{"model", "mode"}),
		adaptiveDecisions: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "kukie_counter_adaptive_decisions_total",
			Help: "Adaptive controller decisions by reason, one per model per interval.",
		}, []string{"model", "reason"}),
		windowITL: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kukie_counter_window_itl_p95_seconds",
			Help: "Median of per-request ITL p95 in the controller's window. -1 when there are too few samples.",
		}, []string{"model"}),
		windowTTFT: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kukie_counter_window_ttft_p95_seconds",
			Help: "TTFT p95 in the controller's window, long prompts left out. -1 when there are too few samples.",
		}, []string{"model"}),
		upstreamWaiting: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kukie_counter_upstream_waiting",
			Help: "vllm:num_requests_waiting at the last brake read of the model server.",
		}, []string{"model"}),
		upstreamKV: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kukie_counter_upstream_kv_ratio",
			Help: "vLLM KV cache usage (0 to 1) at the last brake read of the model server.",
		}, []string{"model"}),
		upstreamMetricsErrors: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "kukie_counter_upstream_metrics_errors_total",
			Help: "Failed reads of the model server's /metrics. A failed read never brakes.",
		}, []string{"model"}),
		brakeActive: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kukie_counter_brake_active",
			Help: "1 on the brake that would be in force: waiting or kv. In observe mode nothing is refused.",
		}, []string{"model", "reason"}),
		keysReloadErrors: factory.NewCounter(prometheus.CounterOpts{
			Name: "kukie_counter_keys_reload_errors_total",
			Help: "Failed key file reloads. The previous keys stay in use.",
		}),
		discoveryErrors: factory.NewCounter(prometheus.CounterOpts{
			Name: "kukie_counter_discovery_errors_total",
			Help: "Failed ServedModel lists. The previous routes and limits stay in use.",
		}),
		discoveryLastSuccess: factory.NewGauge(prometheus.GaugeOpts{
			Name: "kukie_counter_discovery_last_success_timestamp_seconds",
			Help: "Unix time of the last successful ServedModel list. 0 means none yet or discovery is off.",
		}),
	}
}

// initModel creates a model's series up front, so dashboards and alerts see zeros
// rather than nothing before the first request.
// The limit series are set by applyRoutes.
func (m *metrics) initModel(model string) {
	m.inFlight.WithLabelValues(model).Set(0)
	m.usageMissing.WithLabelValues(model).Add(0)
	m.upstreamBroken.WithLabelValues(model).Add(0)
	for _, reason := range []string{reasonLimit, reasonUnavailable, reasonBrake} {
		m.rejected.WithLabelValues(model, reason).Add(0)
	}
	m.prefillInFlight.WithLabelValues(model).Set(0)
}

// deleteAdaptive removes a model's controller series, when its route goes away or a
// person sets its limit.
func (m *metrics) deleteAdaptive(model string) {
	m.adaptiveLimit.DeletePartialMatch(prometheus.Labels{"model": model})
	m.adaptiveDecisions.DeletePartialMatch(prometheus.Labels{"model": model})
	m.windowITL.DeleteLabelValues(model)
	m.windowTTFT.DeleteLabelValues(model)
	m.upstreamWaiting.DeleteLabelValues(model)
	m.upstreamKV.DeleteLabelValues(model)
	m.upstreamMetricsErrors.DeleteLabelValues(model)
	m.brakeActive.DeletePartialMatch(prometheus.Labels{"model": model})
}

func (m *metrics) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}))
	return mux
}
