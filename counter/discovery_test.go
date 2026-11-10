package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const testNamespace = "llm"

// fakeLister stands in for the API server in gateway tests. TestKubeLister covers the
// real client against a fake API server.
type fakeLister struct {
	mu     sync.Mutex
	models []servedModel
	err    error
}

func (f *fakeLister) set(models ...servedModel) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.models, f.err = models, nil
}

func (f *fakeLister) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeLister) list(context.Context) ([]servedModel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.models), f.err
}

func sm(name string, generation int64, b *servedModelBaseline, served ...string) servedModel {
	if len(served) == 0 {
		served = []string{testModel}
	}
	return servedModel{Name: name, Namespace: testNamespace, Generation: generation, ServedNames: served,
		Phase: "Ready", Baseline: b}
}

func measured(maxConcurrency, generation int64) *servedModelBaseline {
	return &servedModelBaseline{MaxConcurrency: maxConcurrency, Generation: generation}
}

func testDiscovery() *discoveryConfig {
	return &discoveryConfig{Namespace: testNamespace, SafetyFactor: 1, DefaultMaxConcurrent: 3, IntervalSeconds: 30}
}

// startDiscoveryGateway runs a gateway with discovery on. Every upstream, including the
// <name>.llm.svc:8000 of discovered models, is dialled to the fake vLLM.
func startDiscoveryGateway(t *testing.T, vllm string, static map[string]modelConfig, lister *fakeLister) *gateway {
	t.Helper()
	g := startGateway(t, "http://unused.invalid", func(_ *server, cfg *config) {
		cfg.Models = static
		cfg.Discovery = testDiscovery()
	})
	addr := strings.TrimPrefix(vllm, "http://")
	transport := newUpstreamTransport(0)
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	g.srv.transport = transport
	if lister != nil {
		if err := g.srv.refreshRoutes(context.Background(), lister); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

func (g *gateway) route(name string) *modelRoute {
	return g.srv.table.Load().routes[name]
}

func (g *gateway) requireLimit(name string, limit int, source, reason string) {
	g.t.Helper()
	r := g.route(name)
	if r == nil {
		g.t.Fatalf("no route for %q", name)
	}
	if r.limit != limit || r.source != source || r.reason != reason {
		g.t.Fatalf("%s: limit %d %s %s, want %d %s %s", name, r.limit, r.source, r.reason, limit, source, reason)
	}
	g.waitMetric(fmt.Sprintf(`kukie_counter_limit{model=%q} %d`, name, limit))
	g.waitMetric(fmt.Sprintf(`kukie_counter_limit_source{model=%q,reason=%q,source=%q} 1`, name, reason, source))
}

// holdingUpstream answers with a stream that stays open until release is called.
type holdingUpstream struct {
	*httptest.Server
	started chan struct{}
	release func()
	hosts   chan string
}

func newHoldingUpstream(t *testing.T) *holdingUpstream {
	finish := make(chan struct{})
	h := &holdingUpstream{started: make(chan struct{}, 64), hosts: make(chan string, 64)}
	h.release = sync.OnceFunc(func() { close(finish) })
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.hosts <- r.Host
		sseHeaders(w)
		writeChunk(w, "data: {\"n\":1}\n\n")
		h.started <- struct{}{}
		<-finish
		writeChunk(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(h.Close)
	t.Cleanup(h.release) // Runs before Close.
	return h
}

// startStreams starts n requests that hold their place until the upstream releases them.
func (g *gateway) startStreams(h *holdingUpstream, model string, n int) *sync.WaitGroup {
	g.t.Helper()
	var running sync.WaitGroup
	body := fmt.Sprintf(`{"model":%q,"stream":true}`, model)
	for range n {
		running.Go(func() {
			res, err := g.client.Do(g.newRequest(http.MethodPost, pathChatCompletions, testKey, body))
			if err != nil {
				g.t.Error(err)
				return
			}
			defer func() { _ = res.Body.Close() }()
			if res.StatusCode != http.StatusOK {
				g.t.Errorf("held stream status %d", res.StatusCode)
			}
			_, _ = io.Copy(io.Discard, res.Body)
		})
	}
	for range n {
		select {
		case <-h.started:
		case <-time.After(waitTimeout):
			g.t.Fatal("upstream did not start")
		}
	}
	return &running
}

func (g *gateway) postModel(model string) (*http.Response, string) {
	g.t.Helper()
	return g.post(fmt.Sprintf(`{"model":%q}`, model))
}

func TestLimitFromBaseline(t *testing.T) {
	d := &discoveryConfig{SafetyFactor: 0.8, DefaultMaxConcurrent: 4}
	for _, c := range []struct {
		name           string
		model          servedModel
		limit          int
		source, reason string
	}{
		{"measured, the 2026-11-04 L40S run", sm("a", 1, measured(32, 1)), 25, sourceBaseline, reasonMeasured},
		{"no baseline", sm("a", 1, nil), 4, sourceDefault, reasonNoBaseline},
		{"zero baseline", sm("a", 1, measured(0, 1)), 4, sourceDefault, reasonNoBaseline},
		{"never below one", sm("a", 1, measured(1, 1)), 1, sourceBaseline, reasonMeasured},
		{"partial is a lower bound", sm("a", 1, &servedModelBaseline{MaxConcurrency: 10, Partial: true, Generation: 1}),
			8, sourceBaseline, reasonPartial},
		{"spec changed after the run", sm("a", 2, measured(32, 1)), 4, sourceDefault, reasonStaleBaseline},
		{"stale but smaller than the default", sm("a", 2, measured(2, 1)), 1, sourceDefault, reasonStaleBaseline},
		{"newer generation than the object is not stale", sm("a", 1, measured(10, 2)), 8, sourceBaseline, reasonMeasured},
	} {
		t.Run(c.name, func(t *testing.T) {
			limit, source, reason := d.limitFromBaseline(c.model)
			if limit != c.limit || source != c.source || reason != c.reason {
				t.Fatalf("got %d %s %s, want %d %s %s", limit, source, reason, c.limit, c.source, c.reason)
			}
		})
	}
	// Floating point: 0.29 × 100 is 28.999… in binary.
	if limit, _, _ := (&discoveryConfig{SafetyFactor: 0.29}).limitFromBaseline(sm("a", 1, measured(100, 1))); limit != 29 {
		t.Fatalf("0.29 × 100 = %d", limit)
	}
}

func TestServedNames(t *testing.T) {
	for _, c := range []struct {
		args []string
		want []string
	}{
		{nil, []string{"org/m"}},
		{[]string{"--max-model-len", "8192"}, []string{"org/m"}},
		{[]string{"--served-model-name", "a", "b", "--enforce-eager"}, []string{"a", "b"}},
		{[]string{"--served-model-name=a"}, []string{"a"}},
		{[]string{"--served_model_name", "a"}, []string{"a"}},
		{[]string{"--served-model-name", "a", "--served-model-name", "b"}, []string{"b"}},
		{[]string{"--served-model-name"}, []string{"org/m"}},
		{[]string{"--served-model-name", "unknown", "ok"}, []string{"ok"}},
	} {
		if got := servedNames("org/m", c.args); !slices.Equal(got, c.want) {
			t.Errorf("servedNames(%q) = %q, want %q", c.args, got, c.want)
		}
	}
	if got := servedNames("bad name", nil); len(got) != 0 {
		t.Errorf("a name with a space was kept: %q", got)
	}
}

func TestDiscoveryConfig(t *testing.T) {
	cfg, err := parseConfig([]byte(`{"discovery":{"namespace":"llm"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if d := cfg.Discovery; d.SafetyFactor != 0.8 || d.DefaultMaxConcurrent != 4 || d.IntervalSeconds != 30 {
		t.Fatalf("defaults = %+v", d)
	}
	cfg, err = parseConfig([]byte(`{"models":{"m":{"upstream":"http://m:8000"}},"discovery":{"namespace":"llm"}}`))
	if err != nil || cfg.Models["m"].MaxConcurrent != 0 {
		t.Fatalf("a model without maxConcurrent under discovery: %v", err)
	}
	for name, raw := range map[string]string{
		"no models and no discovery":         `{"models":{}}`,
		"no maxConcurrent without discovery": `{"models":{"m":{"upstream":"http://m:8000"}}}`,
		"negative maxConcurrent":             `{"models":{"m":{"upstream":"http://m:8000","maxConcurrent":-1}},"discovery":{"namespace":"llm"}}`,
		"no namespace":                       `{"discovery":{}}`,
		"bad namespace":                      `{"discovery":{"namespace":"LLM"}}`,
		"factor above one":                   `{"discovery":{"namespace":"llm","safetyFactor":1.5}}`,
		"negative factor":                    `{"discovery":{"namespace":"llm","safetyFactor":-0.5}}`,
		"negative default":                   `{"discovery":{"namespace":"llm","defaultMaxConcurrent":-1}}`,
		"negative interval":                  `{"discovery":{"namespace":"llm","intervalSeconds":-1}}`,
		"unknown field":                      `{"discovery":{"namespace":"llm","watch":true}}`,
	} {
		if _, err := parseConfig([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestStaticConfigStaysAsItWas(t *testing.T) {
	g := startGateway(t, "http://model.invalid:8000", withLimit(5))
	g.requireLimit(testModel, 5, sourceConfig, reasonConfigured)
	g.waitMetric(`kukie_counter_discovery_last_success_timestamp_seconds 0`)
}

func TestBaselineSetsTheLimitAndIsEnforced(t *testing.T) {
	h := newHoldingUpstream(t)
	lister := &fakeLister{}
	lister.set(sm("gpt", 1, measured(2, 1)))
	g := startDiscoveryGateway(t, h.URL, nil, lister)
	g.requireLimit(testModel, 2, sourceBaseline, reasonMeasured)

	running := g.startStreams(h, testModel, 2)
	if host := <-h.hosts; host != "gpt.llm.svc:8000" {
		t.Fatalf("upstream host = %q", host)
	}
	res, body := g.postModel(testModel)
	requireError(t, res, body, http.StatusTooManyRequests, "rate_limit_exceeded")
	h.release()
	running.Wait()
	g.waitSlotsFree()

	// /v1/models comes from the ServedModels too.
	res, body = g.do(g.newRequest(http.MethodGet, pathModels, testKey, ""))
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `"id":"`+testModel+`"`) {
		t.Fatalf("models %d %s", res.StatusCode, body)
	}
}

func TestDefaultsWithoutAUsableBaseline(t *testing.T) {
	lister := &fakeLister{}
	lister.set(
		sm("none", 1, nil, "no-baseline"),
		sm("stale", 3, measured(40, 2), "stale"),
		sm("partial", 1, &servedModelBaseline{MaxConcurrency: 6, Partial: true, Generation: 1}, "partial"),
		servedModel{Name: "stopped", Namespace: testNamespace, ServedNames: []string{"stopped"}, Phase: phaseStopped},
		servedModel{Name: "going", Namespace: testNamespace, ServedNames: []string{"going"}, Deleting: true},
	)
	g := startDiscoveryGateway(t, "http://127.0.0.1:1", nil, lister)
	g.requireLimit("no-baseline", 3, sourceDefault, reasonNoBaseline)
	g.requireLimit("stale", 3, sourceDefault, reasonStaleBaseline)
	g.requireLimit("partial", 6, sourceBaseline, reasonPartial)
	for _, name := range []string{"stopped", "going"} {
		if g.route(name) != nil {
			t.Fatalf("%s has a route", name)
		}
	}
}

func TestConfiguredMaxConcurrentWins(t *testing.T) {
	lister := &fakeLister{}
	lister.set(sm("gpt", 1, measured(2, 1)))
	static := map[string]modelConfig{testModel: {Upstream: "http://gpt.llm.svc.cluster.local:8000", MaxConcurrent: 9}}
	g := startDiscoveryGateway(t, "http://127.0.0.1:1", static, lister)
	g.requireLimit(testModel, 9, sourceConfig, reasonConfigured)
}

func TestConfiguredUpstreamTakesTheBaselineOnlyFromItsOwnServer(t *testing.T) {
	lister := &fakeLister{}
	lister.set(sm("gpt", 1, measured(7, 1)), sm("other", 1, measured(7, 1), "elsewhere"))
	static := map[string]modelConfig{
		testModel:   {Upstream: "http://gpt.llm.svc:8000/"},
		"elsewhere": {Upstream: "http://some-other-host:8000"},
		"missing":   {Upstream: "http://missing.llm.svc:8000"},
	}
	g := startDiscoveryGateway(t, "http://127.0.0.1:1", static, lister)
	g.requireLimit(testModel, 7, sourceBaseline, reasonMeasured)
	g.requireLimit("elsewhere", 3, sourceDefault, reasonUpstreamMismatch)
	if got := g.route("elsewhere").target.Host; got != "some-other-host:8000" {
		t.Fatalf("configured upstream lost: %s", got)
	}
	g.requireLimit("missing", 3, sourceDefault, reasonNoServedModel)
	if !strings.Contains(g.logs.String(), "the configured upstream wins") {
		t.Fatal("conflict not logged")
	}
}

func TestBeforeTheFirstListConfiguredModelsUseTheDefault(t *testing.T) {
	static := map[string]modelConfig{testModel: {Upstream: "http://gpt.llm.svc:8000"}}
	g := startDiscoveryGateway(t, "http://127.0.0.1:1", static, nil)
	g.requireLimit(testModel, 3, sourceDefault, reasonNotReadYet)
}

func TestLimitChangesWithoutCuttingRunningRequests(t *testing.T) {
	h := newHoldingUpstream(t)
	lister := &fakeLister{}
	lister.set(sm("gpt", 1, measured(3, 1)))
	g := startDiscoveryGateway(t, h.URL, nil, lister)
	running := g.startStreams(h, testModel, 3)

	// Lowered below what is running: the three keep going, new ones are refused.
	lister.set(sm("gpt", 1, measured(1, 1)))
	if err := g.srv.refreshRoutes(context.Background(), lister); err != nil {
		t.Fatal(err)
	}
	g.requireLimit(testModel, 1, sourceBaseline, reasonMeasured)
	g.waitMetric(fmt.Sprintf(`kukie_counter_in_flight{model=%q} 3`, testModel))
	res, body := g.postModel(testModel)
	requireError(t, res, body, http.StatusTooManyRequests, "rate_limit_exceeded")

	// Raised above what is running: a new one gets in at once.
	lister.set(sm("gpt", 1, measured(4, 1)))
	if err := g.srv.refreshRoutes(context.Background(), lister); err != nil {
		t.Fatal(err)
	}
	more := g.startStreams(h, testModel, 1)
	h.release()
	running.Wait()
	more.Wait()
	g.waitSlotsFree()
}

func TestRemovedModelLetsRunningRequestsFinish(t *testing.T) {
	h := newHoldingUpstream(t)
	lister := &fakeLister{}
	lister.set(sm("gpt", 1, measured(2, 1)))
	g := startDiscoveryGateway(t, h.URL, nil, lister)
	held := g.route(testModel).slots
	running := g.startStreams(h, testModel, 1)

	lister.set()
	if err := g.srv.refreshRoutes(context.Background(), lister); err != nil {
		t.Fatal(err)
	}
	res, body := g.postModel(testModel)
	requireError(t, res, body, http.StatusNotFound, "model_not_found")
	if strings.Contains(g.scrape(), `kukie_counter_limit{model="`+testModel) {
		t.Fatal("limit series of a removed model is still there")
	}

	// Back before the running request ended: it still counts against the limit.
	lister.set(sm("gpt", 1, measured(1, 1)))
	if err := g.srv.refreshRoutes(context.Background(), lister); err != nil {
		t.Fatal(err)
	}
	if g.route(testModel).slots != held {
		t.Fatal("a new limiter was made while requests were running")
	}
	res, body = g.postModel(testModel)
	requireError(t, res, body, http.StatusTooManyRequests, "rate_limit_exceeded")
	// The gauge must not be reset by the route coming back, or the end of the request
	// would take it below zero.
	g.waitMetric(fmt.Sprintf(`kukie_counter_in_flight{model=%q} 1`, testModel))
	h.release()
	running.Wait()
	if held.held() != 0 {
		t.Fatal("place not returned")
	}
	g.waitMetric(fmt.Sprintf(`kukie_counter_in_flight{model=%q} 0`, testModel))
}

func TestConfiguredNameAndDiscoveredAliasShareOneLimit(t *testing.T) {
	h := newHoldingUpstream(t)
	lister := &fakeLister{}
	lister.set(sm("m", 1, measured(2, 1), testModel, "alias"))
	static := map[string]modelConfig{testModel: {Upstream: "http://m.llm.svc:8000"}}
	g := startDiscoveryGateway(t, h.URL, static, lister)
	if g.route(testModel).slots != g.route("alias").slots {
		t.Fatal("one server has two limiters")
	}
	g.requireLimit(testModel, 2, sourceBaseline, reasonMeasured)
	g.requireLimit("alias", 2, sourceBaseline, reasonMeasured)
	running := g.startStreams(h, testModel, 1)
	more := g.startStreams(h, "alias", 1)
	res, body := g.postModel("alias")
	requireError(t, res, body, http.StatusTooManyRequests, "rate_limit_exceeded")
	h.release()
	running.Wait()
	more.Wait()

	// A limit a person set on one name holds for the whole server.
	static[testModel] = modelConfig{Upstream: "http://m.llm.svc:8000", MaxConcurrent: 1}
	if err := g.srv.refreshRoutes(context.Background(), lister); err != nil {
		t.Fatal(err)
	}
	g.requireLimit("alias", 1, sourceConfig, reasonConfigured)
}

func TestListFailureKeepsTheLimits(t *testing.T) {
	lister := &fakeLister{}
	lister.set(sm("gpt", 1, measured(5, 1)))
	g := startDiscoveryGateway(t, "http://127.0.0.1:1", nil, lister)
	lister.fail(errors.New("HTTP 403"))
	if err := g.srv.refreshRoutes(context.Background(), lister); err == nil {
		t.Fatal("no error")
	}
	g.requireLimit(testModel, 5, sourceBaseline, reasonMeasured)
	g.waitMetric(`kukie_counter_discovery_errors_total 1`)
}

func TestSharedAndDuplicateNames(t *testing.T) {
	lister := &fakeLister{}
	lister.set(sm("b", 1, measured(9, 1), "dup"), sm("a", 1, measured(4, 1), "alias-1", "alias-2", "dup"))
	g := startDiscoveryGateway(t, "http://127.0.0.1:1", nil, lister)
	if g.route("alias-1").slots != g.route("alias-2").slots {
		t.Fatal("names of one server do not share its limit")
	}
	// The first ServedModel by name wins a name both serve.
	if g.route("dup").slots != g.route("alias-1").slots {
		t.Fatal("duplicate name went to the wrong ServedModel")
	}
	if !strings.Contains(g.logs.String(), `served by both a and b`) {
		t.Fatal("duplicate not logged")
	}
}

func TestModelStatusShowsLimits(t *testing.T) {
	vllm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"data":[{"id":%q}]}`, testModel)
	}))
	defer vllm.Close()
	lister := &fakeLister{}
	lister.set(sm("gpt", 1, measured(32, 1)))
	g := startDiscoveryGateway(t, vllm.URL, nil, lister)
	res, body := g.do(g.newRequest(http.MethodGet, "/v1/model-status", testKey, ""))
	var got modelObservation
	if err := json.Unmarshal([]byte(body), &got); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	want := []modelLimit{{Model: testModel, Limit: 32, Source: sourceBaseline, Reason: reasonMeasured}}
	if !slices.Equal(got.Limits, want) || !slices.Equal(got.ReadyModels, []string{testModel}) {
		t.Fatalf("status = %+v", got)
	}

	// Limits are live, not part of the 15-second probe cache.
	lister.set(sm("gpt", 2, measured(32, 1)))
	if err := g.srv.refreshRoutes(context.Background(), lister); err != nil {
		t.Fatal(err)
	}
	_, body = g.do(g.newRequest(http.MethodGet, "/v1/model-status", testKey, ""))
	if !strings.Contains(body, `"limit":3,"source":"default","reason":"stale_baseline"`) {
		t.Fatalf("status after the change = %s", body)
	}
}

func TestKubeLister(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("sa-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	list := `{"metadata":{},"items":[
	  {"metadata":{"name":"gpt","namespace":"llm","generation":2},
	   "spec":{"source":{"huggingface":"openai/gpt-oss-20b"},"engine":{"args":["--max-model-len","8192"]}},
	   "status":{"phase":"Ready","baseline":{"maxConcurrency":32,"maxWaiting":4,"maxKvPercent":10,
	     "measuredAt":"2026-11-04T10:00:00Z","generation":2}}},
	  {"metadata":{"name":"Bad_Name","namespace":"llm"},"spec":{"source":{"huggingface":"x/y"}}},
	  {"metadata":{"name":"new","namespace":"llm","generation":1,"deletionTimestamp":"2026-11-10T00:00:00Z"},
	   "spec":{"source":{"huggingface":"x/y"}},"status":{}}]}`
	var mu sync.Mutex
	status, body := http.StatusOK, list
	answer := func(code int, text string) {
		mu.Lock()
		defer mu.Unlock()
		status, body = code, text
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/apis/serving.kukie.io/v1alpha1/namespaces/llm/servedmodels" || r.Method != http.MethodGet {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sa-token" {
			t.Error("token not sent")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	defer api.Close()
	lister := newKubeLister(api.URL, "llm", api.Client(), tokenPath)

	models, err := lister.list(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []servedModel{
		{Name: "gpt", Namespace: "llm", Generation: 2, ServedNames: []string{"openai/gpt-oss-20b"}, Phase: "Ready",
			Baseline: &servedModelBaseline{MaxConcurrency: 32, Generation: 2}},
		{Name: "new", Namespace: "llm", Generation: 1, ServedNames: []string{"x/y"}, Deleting: true},
	}
	if len(models) != 2 || fmt.Sprint(models[0].Baseline) != fmt.Sprint(want[0].Baseline) ||
		!slices.Equal(models[0].ServedNames, want[0].ServedNames) || models[1].Name != "new" || !models[1].Deleting {
		t.Fatalf("models = %+v", models)
	}

	answer(http.StatusForbidden, `{"message":"forbidden secret detail"}`)
	if _, err := lister.list(context.Background()); err == nil || strings.Contains(err.Error(), "secret detail") {
		t.Fatalf("403: %v", err)
	}
	answer(http.StatusOK, `{"metadata":{"continue":"x"},"items":[]}`)
	if _, err := lister.list(context.Background()); err == nil {
		t.Fatal("a paginated answer was taken as complete")
	}
	answer(http.StatusOK, strings.Repeat(" ", maxListBytes+1))
	if _, err := lister.list(context.Background()); err == nil {
		t.Fatal("oversized answer accepted")
	}

	redirectTarget := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("followed a redirect with the token")
	}))
	defer redirectTarget.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusFound)
	}))
	defer redirect.Close()
	if _, err := newKubeLister(redirect.URL, "llm", redirect.Client(), tokenPath).list(context.Background()); err == nil {
		t.Fatal("redirect accepted")
	}
}

func TestDiscoveryNamesOnly(t *testing.T) {
	// More names than the bound: the rest are left out rather than becoming metric series.
	models := make([]servedModel, 0, maxDiscoveredNames+5)
	for i := range maxDiscoveredNames + 5 {
		models = append(models, sm(fmt.Sprintf("m%03d", i), 1, nil, fmt.Sprintf("name-%03d", i)))
	}
	lister := &fakeLister{}
	lister.set(models...)
	g := startDiscoveryGateway(t, "http://127.0.0.1:1", nil, lister)
	if n := len(g.srv.table.Load().routes); n != maxDiscoveredNames {
		t.Fatalf("%d routes", n)
	}
}

func TestConfiguredModelKeepsItsCountWhenTheListChanges(t *testing.T) {
	h := newHoldingUpstream(t)
	static := map[string]modelConfig{testModel: {Upstream: "http://m.llm.svc:8000", MaxConcurrent: 2}}
	g := startDiscoveryGateway(t, h.URL, static, nil)
	running := g.startStreams(h, testModel, 2)

	// The first list succeeds while both streams run: the count must carry on.
	lister := &fakeLister{}
	lister.set(sm("m", 1, measured(8, 1), testModel, "alias"))
	if err := g.srv.refreshRoutes(context.Background(), lister); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{testModel, "alias"} {
		res, body := g.postModel(name)
		requireError(t, res, body, http.StatusTooManyRequests, "rate_limit_exceeded")
	}

	// And the other way: the ServedModel stops.
	lister.set(servedModel{Name: "m", Namespace: testNamespace, ServedNames: []string{testModel}, Phase: phaseStopped})
	if err := g.srv.refreshRoutes(context.Background(), lister); err != nil {
		t.Fatal(err)
	}
	res, body := g.postModel(testModel)
	requireError(t, res, body, http.StatusTooManyRequests, "rate_limit_exceeded")
	h.release()
	running.Wait()
	g.waitSlotsFree()
}

func TestServiceOf(t *testing.T) {
	for raw, want := range map[string]string{
		"http://m.llm.svc:8000":                "m/llm",
		"http://m.llm:8000/":                   "m/llm",
		"http://M.LLM.svc.cluster.local.:8000": "m/llm",
		"http://m.llm.svc:8001":                "",
		"https://m.llm.svc:8000":               "",
		"http://m.llm.svc:8000/v1":             "",
		"http://m:8000":                        "",
		"http://m.llm.example.com:8000":        "",
		"http://10.0.0.1:8000":                 "",
	} {
		u, err := parseUpstream(raw)
		if err != nil {
			t.Fatal(err)
		}
		name, ns, ok := serviceOf(u)
		got := ""
		if ok {
			got = name + "/" + ns
		}
		if got != want {
			t.Errorf("serviceOf(%s) = %q, want %q", raw, got, want)
		}
	}
}
