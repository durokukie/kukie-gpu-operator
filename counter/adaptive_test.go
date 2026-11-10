package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAdaptiveConfig(t *testing.T) {
	cfg, err := parseConfig([]byte(`{"discovery":{"namespace":"llm","adaptive":{"mode":"observe"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Discovery.Adaptive
	if a.SLATTFTMillis != 1000 || a.SLAITLMillis != 50 || a.IntervalSeconds != 5 || a.WindowSeconds != 30 ||
		a.MaxLimit != 256 || a.TTFTMaxPromptTokens != 4096 {
		t.Fatalf("defaults = %+v", a)
	}
	cfg, err = parseConfig([]byte(`{"discovery":{"namespace":"llm","adaptive":{}}}`))
	if err != nil || cfg.Discovery.Adaptive.on() {
		t.Fatalf("an empty block must mean off: %v", err)
	}
	for name, raw := range map[string]string{
		"on is not here yet": `{"mode":"on"}`,
		"unknown mode":       `{"mode":"auto"}`,
		"negative sla":       `{"mode":"observe","slaItlMs":-1}`,
		"negative window":    `{"mode":"observe","windowSeconds":-1}`,
		"window below tick":  `{"mode":"observe","intervalSeconds":10,"windowSeconds":5}`,
		"unknown field":      `{"mode":"observe","decrease":0.5}`,
	} {
		raw = `{"discovery":{"namespace":"llm","adaptive":` + raw + `}}`
		if _, err := parseConfig([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTokenMarkers(t *testing.T) {
	for _, c := range []struct {
		chunk string
		want  bool
	}{
		{`data: {"choices":[{"delta":{"role":"assistant","content":""}}]}`, false},
		{`data: {"choices":[{"delta":{"content":"Hi"}}]}`, true},
		{`data: {"choices":[{"delta":{"reasoning_content":"so"}}]}`, true},
		{`data: {"choices":[{"delta":{"reasoning": "so"}}]}`, true},
		{`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{"}}]}}]}`, true},
		{`data: {"choices":[{"text":"x","index":0}]}`, true},
		{`data: {"choices":[{"text":"","finish_reason":"stop"}]}`, false},
		// Generated text cannot fake a marker: its quotes are escaped.
		{`data: {"choices":[{"delta":{"content":""}}],"x":"say \"content\":\"a\""}`, false},
		{`data: {"choices":[],"usage":{"prompt_tokens":1}}`, false},
		{`data: [DONE]`, false},
	} {
		timer := newStreamTimer(time.Now(), nil)
		if got := timer.scan([]byte(c.chunk)); got != c.want {
			t.Errorf("%s: %v, want %v", c.chunk, got, c.want)
		}
	}
}

func TestTokenMarkerSplitAcrossReads(t *testing.T) {
	chunk := `data: {"choices":[{"delta":{"content":"Hi"}}]}` + "\n\n"
	for cut := 1; cut < len(chunk); cut++ {
		timer := newStreamTimer(time.Now(), nil)
		first, second := timer.scan([]byte(chunk[:cut])), timer.scan([]byte(chunk[cut:]))
		if first == second {
			t.Fatalf("cut at %d: found in first %v, second %v; want exactly one", cut, first, second)
		}
	}
	// Byte by byte: still one token, never two.
	timer := newStreamTimer(time.Now(), nil)
	found := 0
	for i := range len(chunk) {
		if timer.scan([]byte{chunk[i]}) {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("byte by byte found %d tokens", found)
	}
}

// TestStreamTimerLeavesOutClientTime drives the timer with made-up clock readings: the
// time between reads, spent writing to a slow client, must count neither in TTFT nor in ITL.
func TestStreamTimerLeavesOutClientTime(t *testing.T) {
	start := time.Unix(1000, 0)
	at := func(ms int) time.Time { return start.Add(time.Duration(ms) * time.Millisecond) }
	token := []byte(`data: {"choices":[{"delta":{"content":"a"}}]}` + "\n\n")
	role := []byte(`data: {"choices":[{"delta":{"role":"assistant","content":""}}]}` + "\n\n")
	timer := newStreamTimer(start, nil)

	timer.beforeRead(at(0))
	timer.afterRead(at(100), role) // 100 ms waiting on the model
	timer.beforeRead(at(400))      // 300 ms writing to the client
	timer.afterRead(at(500), token)
	if timer.ttft != 200*time.Millisecond {
		t.Fatalf("ttft = %v, want 200ms", timer.ttft)
	}
	timer.beforeRead(at(2500)) // a client that stalls for 2 s
	timer.afterRead(at(2520), token)
	timer.beforeRead(at(2520))
	timer.afterRead(at(2530), nil) // a read with no token, part of the next gap
	timer.beforeRead(at(2530))
	timer.afterRead(at(2560), token)
	if len(timer.gaps) != 2 || !near(timer.gaps[0], 0.020) || !near(timer.gaps[1], 0.040) {
		t.Fatalf("gaps = %v, want [0.020 0.040]", timer.gaps)
	}
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

func testAdaptive() *adaptiveConfig {
	a := &adaptiveConfig{Mode: adaptiveObserve}
	if err := a.validate(); err != nil {
		panic(err)
	}
	return a
}

// fill adds n requests started under the current limit, each with the given ITL p95 and TTFT.
func fill(a *adaptiveState, now time.Time, n int, itl, ttft float64) {
	gen := a.generation()
	for range n {
		a.add(requestSample{end: now, gen: gen, ttft: ttft, itlP95: itl, gaps: 50})
	}
}

func TestAdaptiveDecisions(t *testing.T) {
	cfg := testAdaptive()
	now := time.Unix(5000, 0)
	newState := func(limit int) *adaptiveState {
		a := &adaptiveState{}
		a.reset(limit, "k", false)
		return a
	}
	for _, c := range []struct {
		name      string
		limit     int
		n         int
		itl, ttft float64
		peak      int
		reason    string
		want      int
	}{
		{"too few requests", 10, 9, 0.010, 0.1, 10, adaptiveTooFewSamples, 10},
		{"fast and full rises by one", 10, 12, 0.020, 0.2, 9, adaptiveIncrease, 11},
		{"fast but not full holds", 10, 12, 0.020, 0.2, 8, adaptiveNotFull, 10},
		{"ITL over the SLA cuts by 30%", 20, 12, 0.051, 0.2, 20, adaptiveDecreaseITL, 14},
		{"TTFT over the SLA cuts by 30%", 20, 12, 0.020, 1.2, 20, adaptiveDecreaseTTFT, 14},
		{"between 80% and 100% of the SLA holds", 10, 12, 0.045, 0.2, 10, adaptiveHold, 10},
		{"TTFT near the SLA holds", 10, 12, 0.020, 0.9, 10, adaptiveHold, 10},
		{"never below one", 1, 12, 0.200, 0.2, 1, adaptiveDecreaseITL, 1},
		{"never above the maximum", 256, 12, 0.010, 0.1, 256, adaptiveAtMax, 256},
		{"TTFT alone can cut", 10, 12, -1, 5, 10, adaptiveDecreaseTTFT, 7},
		{"TTFT alone cannot raise", 10, 12, -1, 0.1, 10, adaptiveHold, 10},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newState(c.limit)
			fill(a, now, c.n, c.itl, c.ttft)
			d := a.tick(now, cfg, c.peak)
			if d.reason != c.reason || d.limit != c.want {
				t.Fatalf("got %s %d, want %s %d", d.reason, d.limit, c.reason, c.want)
			}
		})
	}
}

func TestAdaptiveMedianIgnoresOneSlowRequest(t *testing.T) {
	a := &adaptiveState{}
	a.reset(10, "k", false)
	now := time.Unix(5000, 0)
	fill(a, now, 11, 0.020, 0.2)
	fill(a, now, 1, 5.0, 0.2) // one client stuck for seconds
	if d := a.tick(now, testAdaptive(), 10); d.reason != adaptiveIncrease {
		t.Fatalf("one slow request decided %s", d.reason)
	}
}

func TestAdaptiveDecreaseDoesNotCascade(t *testing.T) {
	cfg := testAdaptive()
	a := &adaptiveState{}
	a.reset(20, "k", false)
	now := time.Unix(5000, 0)
	slowGen := a.generation()
	fill(a, now, 12, 0.080, 0.2)
	if d := a.tick(now, cfg, 20); d.limit != 14 {
		t.Fatalf("first decrease = %+v", d)
	}
	// Slow requests started under the old limit finish after the decrease. They must not
	// cut again: only requests started under 14 may judge 14.
	for range 12 {
		a.add(requestSample{end: now.Add(time.Second), gen: slowGen, ttft: 0.2, itlP95: 0.080, gaps: 50})
	}
	if d := a.tick(now.Add(5*time.Second), cfg, 14); d.reason != adaptiveTooFewSamples || d.limit != 14 {
		t.Fatalf("after the decrease = %+v, want too few samples at 14", d)
	}
	// Fast and full under the new limit, but still cooling down from the cut.
	fill(a, now.Add(6*time.Second), 12, 0.020, 0.2)
	if d := a.tick(now.Add(10*time.Second), cfg, 14); d.reason != adaptiveCooling {
		t.Fatalf("right after a cut = %+v, want cooling", d)
	}
	if d := a.tick(now.Add(15*time.Second), cfg, 14); d.reason != adaptiveCooling {
		t.Fatalf("third tick after a cut = %+v, want cooling", d)
	}
	if d := a.tick(now.Add(20*time.Second), cfg, 14); d.reason != adaptiveIncrease || d.limit != 15 {
		t.Fatalf("after cooling = %+v, want 15", d)
	}
}

func TestAdaptiveWindowDropsOldSamples(t *testing.T) {
	a := &adaptiveState{}
	a.reset(10, "k", false)
	now := time.Unix(5000, 0)
	fill(a, now, 12, 0.080, 0.2)
	if d := a.tick(now.Add(31*time.Second), testAdaptive(), 10); d.reason != adaptiveTooFewSamples {
		t.Fatalf("samples older than the window decided %s", d.reason)
	}
}

func TestAdaptiveResetOnlyWhenTheStartChanges(t *testing.T) {
	cfg := testAdaptive()
	a := &adaptiveState{}
	a.reset(10, "baseline/measured/10", false)
	now := time.Unix(5000, 0)
	fill(a, now, 12, 0.020, 0.2)
	a.tick(now, cfg, 10)
	a.reset(10, "baseline/measured/10", false) // the 30 s refresh: same baseline
	if a.limit != 11 {
		t.Fatalf("a refresh with the same baseline reset the learned limit to %d", a.limit)
	}
	a.reset(24, "baseline/measured/24", false) // measured again
	if a.limit != 24 {
		t.Fatalf("a new baseline left the limit at %d", a.limit)
	}
	a.reset(9, "config/configured/9", true)
	if d := a.tick(now, cfg, 9); !d.fixed {
		t.Fatalf("a configured limit was decided on: %+v", d)
	}
}

// startAdaptiveGateway is startDiscoveryGateway with the controller in observe mode.
func startAdaptiveGateway(t *testing.T, vllm string, lister *fakeLister) *gateway {
	t.Helper()
	g := startDiscoveryGateway(t, vllm, nil, nil)
	g.srv.state.d.Adaptive = testAdaptive()
	if err := g.srv.refreshRoutes(context.Background(), lister); err != nil {
		t.Fatal(err)
	}
	return g
}

func streamBody() string {
	return fmt.Sprintf(`{"model":%q,"stream":true,"messages":[{"role":"user","content":"hello"}]}`, testModel)
}

// tokenStream answers like vLLM: a role chunk, tokens every gap, then [DONE].
func tokenStream(t *testing.T, tokens int, gap time.Duration) *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseHeaders(w)
		writeChunk(w, `data: {"choices":[{"delta":{"role":"assistant","content":""}}]}`+"\n\n")
		for range tokens {
			time.Sleep(gap)
			writeChunk(w, `data: {"choices":[{"delta":{"content":"a"}}]}`+"\n\n")
		}
		writeChunk(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestStreamsAreTimedAndObservedWithoutChangingTheLimit(t *testing.T) {
	vllm := tokenStream(t, 8, 10*time.Millisecond)
	lister := &fakeLister{}
	lister.set(sm("gpt", 1, measured(4, 1)))
	g := startAdaptiveGateway(t, vllm.URL, lister)
	g.requireLimit(testModel, 4, sourceBaseline, reasonMeasured)

	for range adaptiveMinRequests {
		res, _ := g.do(g.newRequest(http.MethodPost, pathChatCompletions, testKey, streamBody()))
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status %d", res.StatusCode)
		}
	}
	g.waitMetric(fmt.Sprintf(`kukie_counter_ttft_seconds_count{model=%q} %d`, testModel, adaptiveMinRequests))
	g.waitMetric(fmt.Sprintf(`kukie_counter_itl_seconds_count{model=%q} %d`, testModel, adaptiveMinRequests*7))
	g.waitMetric(fmt.Sprintf(`kukie_counter_prefill_in_flight{model=%q} 0`, testModel))
	// A non-streamed request is neither timed nor counted as a prefill.
	g.post(chatBody())
	g.waitSlotsFree()

	a := g.route(testModel).slots.adaptive
	deadline := time.Now().Add(waitTimeout)
	for {
		a.mu.Lock()
		n := len(a.samples)
		a.mu.Unlock()
		if n == adaptiveMinRequests {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d samples in the window", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The window has 10 TTFTs but only 70 gaps, under adaptiveMinGaps. TTFT alone may cut but
	// never raise, so the limit holds.
	g.srv.adaptiveTick(time.Now())
	g.waitMetric(fmt.Sprintf(`kukie_counter_adaptive_limit{mode="observe",model=%q} 4`, testModel))
	g.waitMetric(fmt.Sprintf(`kukie_counter_adaptive_decisions_total{model=%q,reason=%q} 1`, testModel, adaptiveHold))
	g.waitMetric(fmt.Sprintf(`kukie_counter_window_itl_p95_seconds{model=%q} -1`, testModel))

	// Enough fast samples and a full limit: observe decides 5 but enforces nothing.
	fill(a, time.Now(), 12, 0.010, 0.05)
	slots := g.route(testModel).slots
	slots.mu.Lock()
	slots.peak = 4
	slots.mu.Unlock()
	g.srv.adaptiveTick(time.Now())
	g.waitMetric(fmt.Sprintf(`kukie_counter_adaptive_limit{mode="observe",model=%q} 5`, testModel))
	g.requireLimit(testModel, 4, sourceBaseline, reasonMeasured)
	slots.mu.Lock()
	enforced := slots.limit
	slots.mu.Unlock()
	if enforced != 4 {
		t.Fatalf("observe changed the enforced limit to %d", enforced)
	}
	if !strings.Contains(g.logs.String(), `"msg":"adaptive limit decided"`) {
		t.Fatalf("no log for the decision:\n%s", g.logs.String())
	}

	// The 30 s refresh with the same baseline keeps what was learned.
	if err := g.srv.refreshRoutes(context.Background(), lister); err != nil {
		t.Fatal(err)
	}
	if a.limit != 5 {
		t.Fatalf("a refresh reset the learned limit to %d", a.limit)
	}
}

func TestBaselineStartsTheControllerWithoutTheSafetyFactor(t *testing.T) {
	lister := &fakeLister{}
	lister.set(sm("gpt", 1, measured(16, 1)), sm("new", 1, nil, "new"))
	g := startDiscoveryGateway(t, "http://127.0.0.1:1", nil, nil)
	g.srv.state.d.SafetyFactor = 0.8
	g.srv.state.d.Adaptive = testAdaptive()
	if err := g.srv.refreshRoutes(context.Background(), lister); err != nil {
		t.Fatal(err)
	}
	g.requireLimit(testModel, 12, sourceBaseline, reasonMeasured)
	if got := g.route(testModel).slots.adaptive.limit; got != 16 {
		t.Fatalf("start = %d, want the measured 16", got)
	}
	if got := g.route("new").slots.adaptive.limit; got != 3 {
		t.Fatalf("start without a baseline = %d, want the default 3", got)
	}
}

func TestAdaptiveOffMakesNoController(t *testing.T) {
	vllm := tokenStream(t, 3, time.Millisecond)
	lister := &fakeLister{}
	lister.set(sm("gpt", 1, measured(4, 1)))
	g := startDiscoveryGateway(t, vllm.URL, nil, lister)
	if g.route(testModel).slots.adaptive != nil {
		t.Fatal("a controller without adaptive")
	}
	// Timing still works: it costs little and helps read the model's latency.
	g.do(g.newRequest(http.MethodPost, pathChatCompletions, testKey, streamBody()))
	g.waitMetric(fmt.Sprintf(`kukie_counter_ttft_seconds_count{model=%q} 1`, testModel))
	if strings.Contains(g.scrape(), "kukie_counter_adaptive_limit{") {
		t.Fatal("adaptive series without adaptive")
	}
}
