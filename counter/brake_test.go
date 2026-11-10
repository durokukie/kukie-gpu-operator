package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestColdTTFTCannotCutAlone(t *testing.T) {
	cfg := testAdaptive()
	now := time.Unix(5000, 0)
	run := func(fast, slow int) adaptiveDecision {
		a := &adaptiveState{}
		a.reset(16, "k", false)
		fill(a, now, fast, 0.011, 0.18)
		fill(a, now, slow, 0.011, 1.5) // the first request after the model sat idle
		return a.tick(now, cfg, 4)
	}
	// As on kukie-gpu: one cold request among a dozen. Too few TTFTs to judge, ITL alone holds.
	if d := run(11, 1); d.reason == adaptiveDecreaseTTFT {
		t.Fatalf("one cold request among 12 cut the limit: %+v", d)
	}
	// Twenty TTFTs with one cold: the p95 is the 19th, which is fast.
	if d := run(19, 1); d.reason == adaptiveDecreaseTTFT {
		t.Fatalf("one cold request among 20 cut the limit: %+v", d)
	}
	// Two slow among twenty is a real tail.
	if d := run(18, 2); d.reason != adaptiveDecreaseTTFT || d.limit != 11 {
		t.Fatalf("two slow among 20 = %+v, want decrease_ttft to 11", d)
	}
}

func TestParseVLLMMetrics(t *testing.T) {
	page := `# HELP vllm:num_requests_waiting Number of requests waiting.
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{engine="0",model_name="openai/gpt-oss-20b"} 2.0
vllm:num_requests_waiting{engine="1",model_name="openai/gpt-oss-20b"} 1.0
vllm:num_requests_waiting_by_reason{reason="capacity"} 40.0
vllm:kv_cache_usage_perc{engine="0",model_name="openai/gpt-oss-20b"} 0.42
vllm:kv_cache_usage_perc{engine="1",model_name="openai/gpt-oss-20b"} 0.91
vllm:num_requests_running{engine="0"} 12.0
`
	load, err := parseVLLMMetrics(strings.NewReader(page))
	if err != nil || load.waiting != 3 || load.kv != 0.91 {
		t.Fatalf("load = %+v, %v; want waiting 3 (summed), kv 0.91 (highest)", load, err)
	}
	load, err = parseVLLMMetrics(strings.NewReader("vllm:gpu_cache_usage_perc 0.5\n"))
	if err != nil || load.kv != 0.5 {
		t.Fatalf("older vLLM name: %+v, %v", load, err)
	}
	if _, err := parseVLLMMetrics(strings.NewReader("go_goroutines 7\n")); err == nil {
		t.Fatal("a page without vLLM metrics must be an error, so it never brakes")
	}
}

func TestBrakeDecide(t *testing.T) {
	cfg := testAdaptive()
	var w brakeWatch
	steps := []struct {
		load upstreamLoad
		want string
	}{
		{upstreamLoad{waiting: 3}, ""}, // one read with a queue: a burst arriving, wait
		{upstreamLoad{waiting: 1}, brakeWaiting},
		{upstreamLoad{waiting: 2}, brakeWaiting},
		{upstreamLoad{}, ""},
		{upstreamLoad{kv: 0.95}, brakeKV}, // KV is braked on the first read
		{upstreamLoad{kv: 0.5}, ""},
	}
	for i, st := range steps {
		if got := w.decide(st.load, cfg); got != st.want {
			t.Fatalf("step %d %+v: brake %q, want %q", i, st.load, got, st.want)
		}
	}
}

// fakeVLLM answers chat requests and serves a /metrics page the test can change.
type fakeVLLM struct {
	*httptest.Server
	mu   sync.Mutex
	page string
	code int
}

func newFakeVLLM(t *testing.T) *fakeVLLM {
	f := &fakeVLLM{code: http.StatusOK, page: "vllm:num_requests_waiting 0\nvllm:kv_cache_usage_perc 0.1\n"}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			f.mu.Lock()
			code, page := f.code, f.page
			f.mu.Unlock()
			w.WriteHeader(code)
			_, _ = io.WriteString(w, page)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeVLLM) set(code int, page string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.code, f.page = code, page
}

// startModeGateway is startDiscoveryGateway with adaptive in the given mode from the start, as
// in production: limiters made by the first routes already carry a controller.
func startModeGateway(t *testing.T, vllm, mode string, lister *fakeLister, static map[string]modelConfig) *gateway {
	t.Helper()
	a := &adaptiveConfig{Mode: mode}
	if err := a.validate(); err != nil {
		t.Fatal(err)
	}
	g := startGateway(t, "http://unused.invalid", func(_ *server, cfg *config) {
		cfg.Models = static
		cfg.Discovery = testDiscovery()
		cfg.Discovery.Adaptive = a
	})
	addr := strings.TrimPrefix(vllm, "http://")
	transport := newUpstreamTransport(0)
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	g.srv.transport = transport
	if err := g.srv.refreshRoutes(context.Background(), lister); err != nil {
		t.Fatal(err)
	}
	return g
}

func (g *gateway) modelStatusLimit(name string) modelLimit {
	g.t.Helper()
	res, body := g.do(g.newRequest(http.MethodGet, "/v1/model-status", testKey, ""))
	if res.StatusCode != http.StatusOK {
		g.t.Fatalf("model-status %d %s", res.StatusCode, body)
	}
	var st struct {
		Limits []modelLimit `json:"limits"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		g.t.Fatal(err)
	}
	for _, l := range st.Limits {
		if l.Model == name {
			return l
		}
	}
	g.t.Fatalf("no limit for %s in %s", name, body)
	return modelLimit{}
}

func TestOnModeOwnsTheLimit(t *testing.T) {
	vllm := newFakeVLLM(t)
	lister := &fakeLister{}
	lister.set(sm("gpt", 1, measured(16, 1)))
	g := startModeGateway(t, vllm.URL, adaptiveOn, lister, nil)
	// The controller starts from the measured 16 itself, not 16 × safetyFactor.
	g.requireLimit(testModel, 16, sourceAdaptive, adaptiveStarted)
	slots := g.route(testModel).slots

	// Fast and full: the controller raises, and that is now the limit in force.
	fill(slots.adaptive, time.Now(), 12, 0.010, 0.05)
	slots.mu.Lock()
	slots.peak = 16
	slots.mu.Unlock()
	g.srv.adaptiveTick(time.Now())
	if limit, source, reason := slots.snapshot(); limit != 17 || source != sourceAdaptive || reason != adaptiveIncrease {
		t.Fatalf("after increase: %d %s %s", limit, source, reason)
	}
	g.waitMetric(fmt.Sprintf(`kukie_counter_limit{model=%q} 17`, testModel))
	g.waitMetric(fmt.Sprintf(`kukie_counter_limit_source{model=%q,reason=%q,source=%q} 1`, testModel, adaptiveIncrease, sourceAdaptive))
	if l := g.modelStatusLimit(testModel); l.Limit != 17 || l.Source != sourceAdaptive {
		t.Fatalf("model-status = %+v, want 17 adaptive", l)
	}

	// The 30 s refresh with the same baseline must not put 16 back.
	if err := g.srv.refreshRoutes(context.Background(), lister); err != nil {
		t.Fatal(err)
	}
	g.requireLimit(testModel, 17, sourceAdaptive, adaptiveIncrease)

	// Too slow: the controller cuts, and new requests see the lower limit.
	fill(slots.adaptive, time.Now(), 12, 0.080, 0.05)
	g.srv.adaptiveTick(time.Now())
	if limit, _, reason := slots.snapshot(); limit != 11 || reason != adaptiveDecreaseITL {
		t.Fatalf("after decrease: %d %s", limit, reason)
	}

	// Measured again: the controller starts over from the new baseline.
	lister.set(sm("gpt", 1, measured(24, 1)))
	if err := g.srv.refreshRoutes(context.Background(), lister); err != nil {
		t.Fatal(err)
	}
	g.requireLimit(testModel, 24, sourceAdaptive, adaptiveStarted)
}

func TestOnModeLeavesAConfiguredLimitAlone(t *testing.T) {
	vllm := newFakeVLLM(t)
	lister := &fakeLister{}
	lister.set(sm("gpt", 1, measured(16, 1)))
	static := map[string]modelConfig{testModel: {Upstream: "http://gpt.llm.svc:8000", MaxConcurrent: 5}}
	g := startModeGateway(t, vllm.URL, adaptiveOn, lister, static)
	g.requireLimit(testModel, 5, sourceConfig, reasonConfigured)
	slots := g.route(testModel).slots
	fill(slots.adaptive, time.Now(), 12, 0.010, 0.05)
	g.srv.adaptiveTick(time.Now())
	if limit, source, _ := slots.snapshot(); limit != 5 || source != sourceConfig {
		t.Fatalf("a configured limit moved: %d %s", limit, source)
	}
}

func TestBrakeRefusesOnlyInOnMode(t *testing.T) {
	queued := "vllm:num_requests_waiting{engine=\"0\"} 4\nvllm:kv_cache_usage_perc{engine=\"0\"} 0.2\n"
	for _, mode := range []string{adaptiveObserve, adaptiveOn} {
		t.Run(mode, func(t *testing.T) {
			vllm := newFakeVLLM(t)
			lister := &fakeLister{}
			lister.set(sm("gpt", 1, measured(16, 1)))
			g := startModeGateway(t, vllm.URL, mode, lister, nil)

			vllm.set(http.StatusOK, queued)
			g.srv.brakeTick(context.Background())
			g.srv.brakeTick(context.Background()) // the second read in a row with a queue
			g.waitMetric(fmt.Sprintf(`kukie_counter_brake_active{model=%q,reason="waiting"} 1`, testModel))
			g.waitMetric(fmt.Sprintf(`kukie_counter_upstream_waiting{model=%q} 4`, testModel))

			res, body := g.postModel(testModel)
			if mode == adaptiveObserve {
				if res.StatusCode != http.StatusOK {
					t.Fatalf("observe refused a request: %d %s", res.StatusCode, body)
				}
				return
			}
			requireError(t, res, body, http.StatusTooManyRequests, "rate_limit_exceeded")
			g.waitMetric(fmt.Sprintf(`kukie_counter_rejected_total{model=%q,reason="brake"} 1`, testModel))
			// The brake never moved the limit.
			if limit, _, _ := g.route(testModel).slots.snapshot(); limit != 16 {
				t.Fatalf("the brake moved the limit to %d", limit)
			}

			// The queue drains: the next read releases the brake.
			vllm.set(http.StatusOK, "vllm:num_requests_waiting 0\nvllm:kv_cache_usage_perc 0.2\n")
			g.srv.brakeTick(context.Background())
			if res, body := g.postModel(testModel); res.StatusCode != http.StatusOK {
				t.Fatalf("after the queue drained: %d %s", res.StatusCode, body)
			}

			// KV nearly full brakes on the first read; a failed read releases it and never brakes.
			vllm.set(http.StatusOK, "vllm:num_requests_waiting 0\nvllm:kv_cache_usage_perc 0.95\n")
			g.srv.brakeTick(context.Background())
			if g.route(testModel).slots.brakeReason() != brakeKV {
				t.Fatal("KV at 0.95 did not brake")
			}
			vllm.set(http.StatusInternalServerError, "")
			g.srv.brakeTick(context.Background())
			if r := g.route(testModel).slots.brakeReason(); r != "" {
				t.Fatalf("a failed read left the brake %q", r)
			}
			g.waitMetric(fmt.Sprintf(`kukie_counter_upstream_metrics_errors_total{model=%q} 1`, testModel))
		})
	}
}

func TestBrakeReadsOnlyOvenServers(t *testing.T) {
	vllm := newFakeVLLM(t)
	vllm.set(http.StatusOK, "vllm:num_requests_waiting 9\n")
	lister := &fakeLister{}
	// A configured upstream that is not an oven Service: its /metrics may not be vLLM's.
	static := map[string]modelConfig{"other": {Upstream: "http://10.0.0.5:8000"}}
	g := startModeGateway(t, vllm.URL, adaptiveOn, lister, static)
	g.srv.brakeTick(context.Background())
	g.srv.brakeTick(context.Background())
	if r := g.route("other").slots.brakeReason(); r != "" {
		t.Fatalf("a non-oven upstream was braked: %q", r)
	}
}

// TestConfiguredLimitKeepsItsBrakeSeries: a person-set limit has no controller series, but the
// brake still applies to it, so its brake series must survive the controller's tick.
func TestConfiguredLimitKeepsItsBrakeSeries(t *testing.T) {
	vllm := newFakeVLLM(t)
	lister := &fakeLister{}
	lister.set(sm("gpt", 1, measured(16, 1)))
	static := map[string]modelConfig{testModel: {Upstream: "http://gpt.llm.svc:8000", MaxConcurrent: 5}}
	g := startModeGateway(t, vllm.URL, adaptiveOn, lister, static)

	vllm.set(http.StatusInternalServerError, "")
	g.srv.brakeTick(context.Background())
	g.srv.adaptiveTick(time.Now())
	g.srv.brakeTick(context.Background())
	g.srv.adaptiveTick(time.Now())
	// A counter that was deleted in between would read 1, not 2.
	g.waitMetric(fmt.Sprintf(`kukie_counter_upstream_metrics_errors_total{model=%q} 2`, testModel))
	g.waitMetric(fmt.Sprintf(`kukie_counter_brake_active{model=%q,reason="waiting"} 0`, testModel))
	if strings.Contains(g.scrape(), "kukie_counter_adaptive_limit{") {
		t.Fatal("a configured limit has controller series")
	}
}

// TestBrakeReadFinishingAfterTheModelLeftWritesNothing: a refresh removes the model while its
// /metrics read is still in flight. The late result must not bring its series back.
func TestBrakeReadFinishingAfterTheModelLeftWritesNothing(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	vllm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-release
		_, _ = io.WriteString(w, "vllm:num_requests_waiting 5\nvllm:kv_cache_usage_perc 0.99\n")
	}))
	t.Cleanup(vllm.Close)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	lister := &fakeLister{}
	lister.set(sm("gpt", 1, measured(16, 1)))
	g := startModeGateway(t, vllm.URL, adaptiveOn, lister, nil)

	done := make(chan struct{})
	go func() {
		g.srv.brakeTick(context.Background())
		close(done)
	}()
	waitFor(t, "the brake read to start", entered)
	lister.set() // the ServedModel is gone
	if err := g.srv.refreshRoutes(context.Background(), lister); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitFor(t, "the brake tick to end", done)
	for _, name := range []string{"kukie_counter_brake_active{", "kukie_counter_upstream_kv_ratio{", "kukie_counter_upstream_waiting{"} {
		if strings.Contains(g.scrape(), name) {
			t.Fatalf("%s came back for a removed model:\n%s", name, g.scrape())
		}
	}
}
