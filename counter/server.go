package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	pathChatCompletions = "/v1/chat/completions"
	pathCompletions     = "/v1/completions"
	pathModels          = "/v1/models"
	pathHealthz         = "/healthz"

	// bodyReadTimeout limits reading a request body. It is set per request and cleared
	// as soon as the body is read, so it cannot reach into the response.
	bodyReadTimeout = 60 * time.Second

	// statusClientLeft is recorded when the client left before any response was sent.
	statusClientLeft = 499
)

type server struct {
	table           atomic.Pointer[routeTable]
	state           routeState
	keys            *keyStore
	metrics         *metrics
	logger          *slog.Logger
	proxyLog        *log.Logger
	transport       http.RoundTripper
	retryAfter      string
	maxBodyBytes    int64
	bodyReadTimeout time.Duration
	statusMu        sync.Mutex
	statusCache     modelObservation
}

func newServer(
	cfg *config, keys *keyStore, m *metrics, logger *slog.Logger, upstreamHeaderTimeout time.Duration,
) (*server, error) {
	s := &server{
		state:           routeState{static: cfg.Models, d: cfg.Discovery, limiters: map[string]*limiter{}, seen: map[string]bool{}},
		keys:            keys,
		metrics:         m,
		logger:          logger,
		proxyLog:        slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		transport:       newUpstreamTransport(upstreamHeaderTimeout),
		retryAfter:      strconv.Itoa(cfg.RetryAfterSeconds),
		maxBodyBytes:    cfg.MaxBodyBytes,
		bodyReadTimeout: bodyReadTimeout,
	}
	for _, mc := range cfg.Models {
		if _, err := parseUpstream(mc.Upstream); err != nil {
			return nil, err
		}
	}
	// With discovery, configured models without a limit wait on the first list with the default.
	if err := s.applyRoutes(nil, false); err != nil {
		return nil, err
	}
	return s, nil
}

func modelsResponse(names []string, created int64) ([]byte, error) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	data := make([]model, 0, len(names))
	for _, name := range names {
		data = append(data, model{ID: name, Object: "model", Created: created, OwnedBy: "kukie"})
	}
	return json.Marshal(struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{Object: "list", Data: data})
}

// newUpstreamTransport never uses the environment's proxy settings: prompts must go
// straight to the model. Compression stays off so the usage meter can read responses.
func newUpstreamTransport(responseHeaderTimeout time.Duration) *http.Transport {
	return &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
		DisableCompression:    true,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
	}
}

// newPublicServer has no WriteTimeout because it would cut long streams, and no
// ReadTimeout because reading the body has its own limit, see bodyReadTimeout.
// IdleTimeout must stay above the idle timeout of the load balancer in front (600s in the
// chart's Ingress): a load balancer that reuses a connection this side just closed answers 502.
func newPublicServer(handler http.Handler, errorLog *log.Logger) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       660 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          errorLog,
	}
}

type apiError struct {
	status  int
	errType string
	code    string
	message string
}

var (
	errPathNotFound = apiError{http.StatusNotFound, "invalid_request_error", "not_found", "Not found."}
	errInvalidKey   = apiError{http.StatusUnauthorized, "authentication_error", "invalid_api_key",
		"The API key is missing, wrong or revoked."}
	errBadRequest = apiError{http.StatusBadRequest, "invalid_request_error", "invalid_request",
		"The request body must be a JSON object with a string \"model\" field."}
	errModelNotFound = apiError{http.StatusNotFound, "invalid_request_error", "model_not_found",
		"The model does not exist."}
	errTooLarge = apiError{http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large",
		"The request body is too large."}
	errAtLimit = apiError{http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded",
		"The model is at its concurrency limit. Retry later."}
	errUnavailable = apiError{http.StatusServiceUnavailable, "server_error", "model_unavailable",
		"The model is unavailable. Retry later."}
)

func (s *server) writeError(w http.ResponseWriter, e apiError) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	switch e.status {
	case http.StatusUnauthorized:
		h.Set("WWW-Authenticate", "Bearer")
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		h.Set("Retry-After", s.retryAfter)
	}
	w.WriteHeader(e.status)
	type body struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	}
	_ = json.NewEncoder(w).Encode(struct {
		Error body `json:"error"`
	}{body{e.message, e.errType, e.code}})
}

// requestInfo is everything the access log and request counter may see.
type requestInfo struct {
	start     time.Time
	key       string
	model     string
	bodyBytes int
	stream    bool
}

// ServeHTTP checks the path, the key and the body in that order, then forwards.
// The escaped path is matched so that an encoded variant cannot pass as an allowed one.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Covers every path, before any check. A response sent without reading the body makes
	// net/http drain the rest of it first, so without this deadline a client with no key
	// could hold a connection open by declaring a body and never sending it.
	// Errors mean the writer has no deadlines, as in handler tests. There is nothing to undo.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(s.bodyReadTimeout))

	path := r.URL.EscapedPath()
	if path == pathHealthz && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		w.WriteHeader(http.StatusOK)
		return
	}

	rec := &responseRecorder{ResponseWriter: w}
	info := &requestInfo{start: time.Now(), key: labelUnknown, model: labelUnknown}
	defer s.finishRequest(rec, r, info)

	if !routeAllowed(r.Method, path) {
		s.writeError(rec, errPathNotFound)
		return
	}

	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" {
		s.writeError(rec, errInvalidKey)
		return
	}
	id, active := s.keys.lookup(token)
	if id != "" {
		info.key = id
	}
	if !active {
		s.writeError(rec, errInvalidKey)
		return
	}

	if path == "/v1/model-status" {
		s.modelStatus(rec, r)
		return
	}
	table := s.table.Load()
	if path == pathModels {
		rec.Header().Set("Content-Type", "application/json")
		_, _ = rec.Write(table.modelsResponse)
		return
	}

	body, err := s.readBody(w, rec, r)
	info.bodyBytes = len(body)
	if err != nil {
		// Without this the server would try to drain the rest of the body before replying,
		// which a client that stopped sending could hold open.
		rec.Header().Set("Connection", "close")
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeError(rec, errTooLarge)
		} else {
			s.writeError(rec, errBadRequest)
		}
		return
	}
	modelName, stream, ok := requestedModel(body)
	info.stream = stream
	if !ok {
		s.writeError(rec, errBadRequest)
		return
	}
	route, ok := table.routes[modelName]
	if !ok {
		s.writeError(rec, errModelNotFound)
		return
	}
	info.model = route.name
	s.metrics.bodyBytes.WithLabelValues(route.name).Observe(float64(len(body)))

	if !route.slots.tryAcquire() {
		s.metrics.rejected.WithLabelValues(route.name, reasonLimit).Inc()
		s.writeError(rec, errAtLimit)
		return
	}
	var gen uint64
	if route.slots.adaptive != nil {
		gen = route.slots.adaptive.generation()
	}
	inFlight := s.metrics.inFlight.WithLabelValues(route.name)
	inFlight.Inc()
	// ReverseProxy.ServeHTTP returns only once the response is over, whether it finished,
	// the client left or the upstream broke. It also panics with http.ErrAbortHandler in
	// that last case. A defer here covers all of them.
	defer func() {
		inFlight.Dec()
		route.slots.release()
	}()

	s.forward(rec, r, route, info, body, gen)
}

func routeAllowed(method, path string) bool {
	switch path {
	case pathChatCompletions, pathCompletions:
		return method == http.MethodPost
	case pathModels, "/v1/model-status":
		return method == http.MethodGet
	}
	return false
}

func bearerToken(header string) string {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// readBody reads up to the configured limit, under the deadline set in ServeHTTP. It is
// cleared only after a complete read; after a failed one it stays, so nothing can wait on
// the rest of the body.
func (s *server) readBody(raw http.ResponseWriter, rec *responseRecorder, r *http.Request) ([]byte, error) {
	if r.ContentLength > s.maxBodyBytes {
		return nil, &http.MaxBytesError{Limit: s.maxBodyBytes}
	}
	// raw, not rec: MaxBytesReader tells the server to close the connection through
	// an unexported method that a wrapper would hide.
	body, err := io.ReadAll(http.MaxBytesReader(raw, r.Body, s.maxBodyBytes))
	if err != nil {
		return body, err
	}
	_ = http.NewResponseController(rec).SetReadDeadline(time.Time{})
	return body, nil
}

// requestedModel reads only the top-level "model" and "stream" fields. A map is used
// instead of a struct because encoding/json matches struct fields case-insensitively:
// {"MODEL":...} could otherwise select a route while the upstream acts on a different "model".
// stream is only a hint for timing, so a value that is not true counts as false.
func requestedModel(body []byte) (name string, stream bool, ok bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return "", false, false
	}
	raw, ok := fields["model"]
	if !ok {
		return "", false, false
	}
	if err := json.Unmarshal(raw, &name); err != nil || name == "" {
		return "", false, false
	}
	_ = json.Unmarshal(fields["stream"], &stream)
	return name, stream, true
}

// forward sends the unchanged body to the model's upstream and copies the response
// back, flushing after every write. Cancelling r's context, which the server does when
// the client leaves, cancels the upstream request.
func (s *server) forward(
	rec *responseRecorder, r *http.Request, route *modelRoute, info *requestInfo, body []byte, gen uint64,
) {
	start := time.Now()
	var upstream *upstreamBody
	prefill := s.startPrefill(route.name, info.stream)

	defer func() {
		// Also runs while http.ErrAbortHandler unwinds.
		prefill.done()
		if upstream == nil {
			return
		}
		s.metrics.requestSeconds.WithLabelValues(route.name).Observe(time.Since(start).Seconds())
		if rec.status >= 200 && rec.status < 300 && !rec.firstWrite.IsZero() {
			s.metrics.firstByte.WithLabelValues(route.name).Observe(rec.firstWrite.Sub(start).Seconds())
		}
		if upstream.broken {
			s.metrics.upstreamBroken.WithLabelValues(route.name).Inc()
			s.logger.Warn("upstream cut the response off", "key", info.key, "model", route.name)
		}
		var usage *usageCounts
		if upstream.meter != nil {
			usage = upstream.meter.finish()
			s.recordUsage(route.name, info.key, usage)
		}
		if upstream.timer != nil && !upstream.broken {
			s.recordTiming(route, info, upstream.timer, usage, gen)
		}
	}()

	proxy := &httputil.ReverseProxy{
		Transport:     s.transport,
		FlushInterval: -1,
		ErrorLog:      s.proxyLog,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(route.target)
			pr.Out.URL.RawQuery = ""
			for _, name := range []string{
				"Authorization", "Cookie",
				// ReverseProxy puts these back for protocol upgrades. No allowed route needs one.
				"Connection", "Upgrade",
				// The body is already fully read, and the response must stay readable by the meter.
				"Expect", "Accept-Encoding",
				// Rewrite mode removes the common X-Forwarded-* only. Ingresses add more.
				"X-Real-Ip",
			} {
				pr.Out.Header.Del(name)
			}
			for name := range pr.Out.Header {
				if strings.HasPrefix(name, "X-Forwarded-") {
					pr.Out.Header.Del(name)
				}
			}
			pr.Out.Body = io.NopCloser(bytes.NewReader(body))
			// GetBody lets the transport resend on a stale idle connection.
			pr.Out.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			}
			pr.Out.ContentLength = int64(len(body))
			pr.Out.TransferEncoding = nil
		},
		ModifyResponse: func(res *http.Response) error {
			upstream = &upstreamBody{ReadCloser: res.Body, ctx: r.Context(), prefill: prefill}
			if res.StatusCode >= 200 && res.StatusCode < 300 {
				upstream.meter = newUsageMeter(res.Header)
				if info.stream && upstream.meter.mode == usageModeSSE {
					upstream.timer = newStreamTimer(start, s.metrics.itl.WithLabelValues(route.name))
				}
			}
			if upstream.timer == nil {
				// No first token to wait for: an error status or a body that is not a stream.
				prefill.done()
			}
			res.Body = upstream
			return nil
		},
		// Reached only before any response byte was sent, so a clean error is still possible.
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			if req.Context().Err() != nil {
				return // The client left. finishRequest records it.
			}
			s.metrics.rejected.WithLabelValues(route.name, reasonUnavailable).Inc()
			s.logger.Warn("upstream request failed", "key", info.key, "model", route.name, "error", err)
			s.writeError(w, errUnavailable)
		},
	}
	proxy.ServeHTTP(rec, r)
}

func (s *server) recordUsage(model, key string, usage *usageCounts) {
	if usage == nil {
		s.metrics.usageMissing.WithLabelValues(model).Inc()
		return
	}
	s.metrics.tokens.WithLabelValues(model, key, tokenPrompt).Add(float64(usage.PromptTokens))
	s.metrics.tokens.WithLabelValues(model, key, tokenCompletion).Add(float64(usage.CompletionTokens))
}

// finishRequest writes the access log, which holds no path and no body content.
func (s *server) finishRequest(rec *responseRecorder, r *http.Request, info *requestInfo) {
	status := rec.status
	if status == 0 {
		status = http.StatusInternalServerError
		if r.Context().Err() != nil {
			status = statusClientLeft
		}
	}
	s.metrics.requests.WithLabelValues(info.model, info.key, strconv.Itoa(status)).Inc()
	s.logger.Info("request",
		"key", info.key,
		"model", info.model,
		"status", status,
		"duration_seconds", time.Since(info.start).Seconds(),
		"body_bytes", info.bodyBytes,
	)
}

// recordTiming publishes one streamed request's TTFT and leaves its sample in the window.
func (s *server) recordTiming(route *modelRoute, info *requestInfo, t *streamTimer, usage *usageCounts, gen uint64) {
	if t.ttft > 0 {
		s.metrics.ttft.WithLabelValues(route.name).Observe(t.ttft.Seconds())
	}
	a := route.slots.adaptive
	if a == nil {
		return
	}
	// A long prompt has a slow first token whatever the load. usage comes only when the client
	// asked for it, so the body size stands in for the prompt length otherwise.
	promptTokens := uint64(info.bodyBytes / bytesPerPromptToken)
	if usage != nil {
		promptTokens = usage.PromptTokens
	}
	withTTFT := promptTokens <= uint64(s.state.d.Adaptive.TTFTMaxPromptTokens)
	a.add(t.sample(time.Now(), gen, withTTFT))
}

// prefillTracker counts a streamed request in kukie_counter_prefill_in_flight until its first
// token arrives or the response ends, whichever comes first.
type prefillTracker struct {
	gauge prometheus.Gauge
	once  sync.Once
}

func (s *server) startPrefill(model string, stream bool) *prefillTracker {
	if !stream {
		return nil
	}
	p := &prefillTracker{gauge: s.metrics.prefillInFlight.WithLabelValues(model)}
	p.gauge.Inc()
	return p
}

func (p *prefillTracker) done() {
	if p != nil {
		p.once.Do(p.gauge.Dec)
	}
}

// upstreamBody passes the upstream response through, showing it to the meter and the
// timer and noting a failure that is the upstream's rather than the client's.
type upstreamBody struct {
	io.ReadCloser
	ctx     context.Context
	meter   *usageMeter
	timer   *streamTimer
	prefill *prefillTracker
	broken  bool
}

func (b *upstreamBody) Read(p []byte) (int, error) {
	if b.timer != nil {
		b.timer.beforeRead(time.Now())
	}
	n, err := b.ReadCloser.Read(p)
	if b.timer != nil {
		b.timer.afterRead(time.Now(), p[:n])
		if b.timer.ttft > 0 {
			b.prefill.done()
		}
	}
	if n > 0 && b.meter != nil {
		b.meter.write(p[:n])
	}
	if err != nil && !errors.Is(err, io.EOF) && b.ctx.Err() == nil {
		b.broken = true
	}
	return n, err
}

// responseRecorder records the status and the first body write. Unwrap lets
// http.ResponseController reach the real writer for Flush and deadlines.
type responseRecorder struct {
	http.ResponseWriter
	status     int
	firstWrite time.Time
}

func (r *responseRecorder) WriteHeader(code int) {
	// ReverseProxy forwards 1xx responses through here; the final status comes later.
	if r.status == 0 && code >= 200 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if r.firstWrite.IsZero() {
		r.firstWrite = time.Now()
	}
	return r.ResponseWriter.Write(p)
}

func (r *responseRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
