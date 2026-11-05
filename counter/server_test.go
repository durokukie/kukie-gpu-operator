package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const usageChunk = `data: {"id":"c","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}`

func jsonUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func sseHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

func writeChunk(w http.ResponseWriter, s string) {
	_, _ = io.WriteString(w, s)
	w.(http.Flusher).Flush()
}

func TestStreamChunkReachesClientBeforeUpstreamContinues(t *testing.T) {
	received := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseHeaders(w)
		writeChunk(w, "data: {\"n\":1}\n\n")
		// The next chunk is withheld until the client has the first. A buffering
		// gateway would deadlock here and fail on the client's timeout.
		select {
		case <-received:
		case <-r.Context().Done():
			return
		}
		writeChunk(w, "data: {\"n\":2}\n\ndata: [DONE]\n\n")
	}))
	defer ts.Close()
	g := startGateway(t, ts.URL)

	res, err := g.client.Do(g.newRequest(http.MethodPost, pathChatCompletions, testKey, chatBody()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	reader := bufio.NewReader(res.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != "data: {\"n\":1}\n" {
		t.Fatalf("first line = %q, err %v", line, err)
	}
	close(received)
	rest, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(rest), "[DONE]") {
		t.Fatalf("rest = %q, err %v", rest, err)
	}
}

func TestStreamChunksArriveAsTheyAreSent(t *testing.T) {
	const gap = 150 * time.Millisecond
	const chunks = 4
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseHeaders(w)
		for i := range chunks {
			if i > 0 {
				time.Sleep(gap)
			}
			writeChunk(w, fmt.Sprintf("data: {\"n\":%d}\n\n", i))
		}
	}))
	defer ts.Close()
	g := startGateway(t, ts.URL)

	res, err := g.client.Do(g.newRequest(http.MethodPost, pathChatCompletions, testKey, chatBody()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if got := res.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q", got)
	}
	var arrivals []time.Time
	scanner := bufio.NewScanner(res.Body)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data:") {
			arrivals = append(arrivals, time.Now())
		}
	}
	if len(arrivals) != chunks {
		t.Fatalf("got %d chunks, want %d", len(arrivals), chunks)
	}
	// A lower bound only, so a slow machine cannot fail it. Buffered chunks would all
	// arrive together.
	for i := 1; i < chunks; i++ {
		if d := arrivals[i].Sub(arrivals[i-1]); d < gap/2 {
			t.Fatalf("chunk %d arrived %v after the previous one, want about %v", i, d, gap)
		}
	}
}

func TestStreamUsageIsCounted(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseHeaders(w)
		writeChunk(w, "data: {\"choices\":[{\"delta\":{\"content\":\"the \\\"usage\\\" word\"}}],\"usage\":null}\n\n")
		// Split inside the line: the meter has to join it across reads.
		writeChunk(w, usageChunk[:20])
		time.Sleep(20 * time.Millisecond)
		writeChunk(w, usageChunk[20:]+"\n\n")
		writeChunk(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()
	g := startGateway(t, ts.URL)

	res, _ := g.post(chatBody())
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	g.waitMetric(fmt.Sprintf(`kukie_counter_tokens_total{key=%q,model=%q,type="prompt"} 11`, testKeyID, testModel))
	g.waitMetric(fmt.Sprintf(`kukie_counter_tokens_total{key=%q,model=%q,type="completion"} 22`, testKeyID, testModel))
	g.waitMetric(fmt.Sprintf(`kukie_counter_usage_missing_total{model=%q} 0`, testModel))
}

func TestStreamWithoutUsageIsCountedMissing(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseHeaders(w)
		writeChunk(w, "data: {\"n\":1}\n\ndata: [DONE]\n\n")
	}))
	defer ts.Close()
	g := startGateway(t, ts.URL)

	g.post(chatBody())
	g.waitMetric(fmt.Sprintf(`kukie_counter_usage_missing_total{model=%q} 1`, testModel))
	if text := g.scrape(); strings.Contains(text, "kukie_counter_tokens_total{") {
		t.Fatalf("tokens counted without usage:\n%s", text)
	}
}

func TestJSONResponseMetricsAndPassThrough(t *testing.T) {
	g := startGateway(t, jsonUpstream(t).URL)

	res, body := g.post(chatBody())
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `"prompt_tokens":3`) {
		t.Fatalf("status %d body %q", res.StatusCode, body)
	}
	g.waitMetric(fmt.Sprintf(`kukie_counter_requests_total{code="200",key=%q,model=%q} 1`, testKeyID, testModel))
	g.waitMetric(fmt.Sprintf(`kukie_counter_tokens_total{key=%q,model=%q,type="prompt"} 3`, testKeyID, testModel))
	g.waitMetric(fmt.Sprintf(`kukie_counter_tokens_total{key=%q,model=%q,type="completion"} 4`, testKeyID, testModel))
	g.waitMetric(fmt.Sprintf(`kukie_counter_first_byte_seconds_count{model=%q} 1`, testModel))
	g.waitMetric(fmt.Sprintf(`kukie_counter_request_seconds_count{model=%q} 1`, testModel))
	g.waitMetric(fmt.Sprintf(`kukie_counter_request_body_bytes_count{model=%q} 1`, testModel))
	g.waitMetric(fmt.Sprintf(`kukie_counter_request_body_bytes_sum{model=%q} %d`, testModel, len(chatBody())))
	g.waitMetric(fmt.Sprintf(`kukie_counter_limit{model=%q} 8`, testModel))
	g.waitSlotsFree()
}

func TestUnauthorized(t *testing.T) {
	var upstreamCalls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { upstreamCalls.Add(1) }))
	defer ts.Close()
	g := startGateway(t, ts.URL)

	for name, set := range map[string]func(*http.Request){
		"missing":   func(*http.Request) {},
		"wrong":     func(r *http.Request) { r.Header.Set("Authorization", "Bearer kk-wrong") },
		"revoked":   func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testRevokedKey) },
		"no scheme": func(r *http.Request) { r.Header.Set("Authorization", testKey) },
		"basic":     func(r *http.Request) { r.SetBasicAuth("x", testKey) },
		"empty":     func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") },
	} {
		t.Run(name, func(t *testing.T) {
			req := g.newRequest(http.MethodPost, pathChatCompletions, "", chatBody())
			set(req)
			res, body := g.do(req)
			requireError(t, res, body, http.StatusUnauthorized, "invalid_api_key")
		})
	}
	if n := upstreamCalls.Load(); n != 0 {
		t.Fatalf("upstream called %d times", n)
	}
	g.waitMetric(`kukie_counter_requests_total{code="401",key="unknown",model="unknown"} 5`)
	g.waitMetric(fmt.Sprintf(`kukie_counter_requests_total{code="401",key=%q,model="unknown"} 1`, testRevokedID))
}

func TestPathsOutsideTheAllowListAre404(t *testing.T) {
	var upstreamCalls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { upstreamCalls.Add(1) }))
	defer ts.Close()
	g := startGateway(t, ts.URL)

	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/metrics"},
		{http.MethodGet, "/health"},
		{http.MethodPost, "/tokenize"},
		{http.MethodPost, "/v1/embeddings"},
		{http.MethodGet, pathChatCompletions},
		{http.MethodPost, pathModels},
		{http.MethodPost, pathChatCompletions + "/"},
		{http.MethodPost, "/v1/chat%2Fcompletions"},
		{http.MethodPost, "/v1/../metrics"},
		{http.MethodPost, pathHealthz},
	} {
		res, body := g.do(g.newRequest(c.method, c.path, testKey, chatBody()))
		requireError(t, res, body, http.StatusNotFound, "not_found")
	}
	if n := upstreamCalls.Load(); n != 0 {
		t.Fatalf("upstream called %d times", n)
	}
	// A request's path must never become a label.
	if text := g.scrape(); strings.Contains(text, "tokenize") || strings.Contains(text, "embeddings") {
		t.Fatalf("path leaked into metrics:\n%s", text)
	}
}

func TestHealthzNeedsNoKey(t *testing.T) {
	g := startGateway(t, "http://127.0.0.1:1")
	res, _ := g.do(g.newRequest(http.MethodGet, pathHealthz, "", ""))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
}

func TestBodyErrors(t *testing.T) {
	g := startGateway(t, jsonUpstream(t).URL, withMaxBody(256))

	for name, body := range map[string]string{
		"not json":    `hello`,
		"empty":       ``,
		"array":       `[1]`,
		"null":        `null`,
		"no model":    `{"messages":[]}`,
		"number":      `{"model":3}`,
		"null model":  `{"model":null}`,
		"empty model": `{"model":""}`,
		"other case":  fmt.Sprintf(`{"MODEL":%q}`, testModel),
		"trailing":    chatBody() + `x`,
	} {
		t.Run(name, func(t *testing.T) {
			res, got := g.post(body)
			requireError(t, res, got, http.StatusBadRequest, "invalid_request")
		})
	}

	t.Run("unknown model is not echoed or labelled", func(t *testing.T) {
		const name = "attacker-chosen-model"
		res, got := g.post(fmt.Sprintf(`{"model":%q}`, name))
		requireError(t, res, got, http.StatusNotFound, "model_not_found")
		if strings.Contains(got, name) {
			t.Fatalf("response echoes the model: %q", got)
		}
		g.waitMetric(fmt.Sprintf(`kukie_counter_requests_total{code="404",key=%q,model="unknown"} 1`, testKeyID))
		if strings.Contains(g.scrape(), name) || strings.Contains(g.logs.String(), name) {
			t.Fatal("requested model reached metrics or logs")
		}
	})

	big := fmt.Sprintf(`{"model":%q,"pad":%q}`, testModel, strings.Repeat("x", 512))
	t.Run("too large with content length", func(t *testing.T) {
		res, got := g.post(big)
		requireError(t, res, got, http.StatusRequestEntityTooLarge, "request_too_large")
	})
	t.Run("too large when chunked", func(t *testing.T) {
		req := g.newRequest(http.MethodPost, pathChatCompletions, testKey, "")
		req.Body = io.NopCloser(strings.NewReader(big))
		req.ContentLength = -1
		res, got := g.do(req)
		requireError(t, res, got, http.StatusRequestEntityTooLarge, "request_too_large")
	})
}

func TestBodyAndHeadersForwardedAsRequired(t *testing.T) {
	type seen struct {
		method, uri, host string
		header            http.Header
		body              []byte
		contentLength     int64
	}
	got := make(chan seen, 2)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- seen{r.Method, r.RequestURI, r.Host, r.Header.Clone(), body, r.ContentLength}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer ts.Close()
	g := startGateway(t, ts.URL)

	// Odd spacing, key order, duplicate and unknown fields must all survive byte for byte.
	original := fmt.Sprintf("{ \"z\" : 1,\n\t\"model\":%q, \"stream\":true, \"z\":2, \"note\":\"\\u00e9\" }\n", testModel)

	for name, chunked := range map[string]bool{"content length": false, "chunked": true} {
		t.Run(name, func(t *testing.T) {
			req := g.newRequest(http.MethodPost, pathChatCompletions+"?secret=1", testKey, original)
			if chunked {
				req.Body = io.NopCloser(strings.NewReader(original))
				req.ContentLength = -1
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Cookie", "session=abc")
			req.Header.Set("X-Forwarded-For", "203.0.113.9")
			req.Header.Set("X-Forwarded-Host", "public.example")
			req.Header.Set("X-Forwarded-Proto", "https")
			req.Header.Set("X-Forwarded-Port", "443")
			req.Header.Set("X-Real-Ip", "203.0.113.9")
			req.Header.Set("Forwarded", "for=203.0.113.9")
			req.Header.Set("Accept-Encoding", "gzip")
			req.Header.Set("Connection", "X-Hop, Upgrade")
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("X-Hop", "1")
			req.Header.Set("Keep-Alive", "timeout=5")
			req.Header.Set("Proxy-Authorization", "Basic abc")
			req.Header.Set("X-Client-Header", "kept")
			res, _ := g.do(req)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", res.StatusCode)
			}

			s := <-got
			if !bytes.Equal(s.body, []byte(original)) {
				t.Fatalf("body changed:\n got %q\nwant %q", s.body, original)
			}
			if s.contentLength != int64(len(original)) {
				t.Fatalf("upstream Content-Length = %d, want %d", s.contentLength, len(original))
			}
			if s.uri != pathChatCompletions {
				t.Fatalf("upstream URI = %q", s.uri)
			}
			if s.host != strings.TrimPrefix(ts.URL, "http://") {
				t.Fatalf("upstream Host = %q", s.host)
			}
			for _, name := range []string{
				"Authorization", "Cookie", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
				"X-Forwarded-Port", "X-Real-Ip", "Forwarded", "Accept-Encoding", "Connection", "Upgrade",
				"X-Hop", "Keep-Alive", "Proxy-Authorization",
			} {
				if v := s.header.Values(name); len(v) != 0 {
					t.Errorf("%s reached the upstream: %q", name, v)
				}
			}
			if s.header.Get("X-Client-Header") != "kept" || s.header.Get("Content-Type") != "application/json" {
				t.Errorf("ordinary headers were dropped: %v", s.header)
			}
		})
	}
}

func TestLimitRejectsAndSlotReturnsAtEndOfBody(t *testing.T) {
	const limit = 2
	headersSent := make(chan struct{}, limit)
	finish := make(chan struct{})
	release := sync.OnceFunc(func() { close(finish) })
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseHeaders(w)
		writeChunk(w, "data: {\"n\":1}\n\n")
		headersSent <- struct{}{}
		<-finish
		writeChunk(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()
	// Runs before ts.Close, which would otherwise wait forever if the test fails early.
	defer release()
	g := startGateway(t, ts.URL, withLimit(limit))

	var running sync.WaitGroup
	for range limit {
		running.Go(func() {
			res, err := g.client.Do(g.newRequest(http.MethodPost, pathChatCompletions, testKey, chatBody()))
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = res.Body.Close() }()
			_, _ = io.Copy(io.Discard, res.Body)
		})
	}
	for range limit {
		select {
		case <-headersSent:
		case <-time.After(waitTimeout):
			t.Fatal("upstream did not start")
		}
	}

	// Headers are out but the bodies are not finished, so the slots must still be held.
	g.waitMetric(fmt.Sprintf(`kukie_counter_in_flight{model=%q} %d`, testModel, limit))
	res, body := g.post(chatBody())
	requireError(t, res, body, http.StatusTooManyRequests, "rate_limit_exceeded")
	if got := res.Header.Get("Retry-After"); got != "7" {
		t.Fatalf("Retry-After = %q", got)
	}
	g.waitMetric(fmt.Sprintf(`kukie_counter_rejected_total{model=%q,reason="limit"} 1`, testModel))

	release()
	running.Wait()
	g.waitSlotsFree()

	if res, _ := g.post(chatBody()); res.StatusCode != http.StatusOK {
		t.Fatalf("status after slots returned = %d", res.StatusCode)
	}
}

func TestClientDisconnectCancelsUpstreamAndReturnsSlot(t *testing.T) {
	for name, afterHeaders := range map[string]bool{"during stream": true, "before any response": false} {
		t.Run(name, func(t *testing.T) {
			started := make(chan struct{})
			cancelled := make(chan struct{})
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if afterHeaders {
					sseHeaders(w)
					writeChunk(w, "data: {\"n\":1}\n\n")
				}
				close(started)
				<-r.Context().Done()
				close(cancelled)
			}))
			defer ts.Close()
			g := startGateway(t, ts.URL, withLimit(1))

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			firstChunk := make(chan struct{})
			errs := make(chan error, 1)
			go func() {
				req := g.newRequest(http.MethodPost, pathChatCompletions, testKey, chatBody()).WithContext(ctx)
				res, err := g.client.Do(req)
				if err == nil {
					// Only now has the gateway certainly sent the response status.
					_, _ = bufio.NewReader(res.Body).ReadString('\n')
					close(firstChunk)
					_, err = io.Copy(io.Discard, res.Body)
					_ = res.Body.Close()
				}
				errs <- err
			}()

			waitFor(t, "upstream start", started)
			if afterHeaders {
				waitFor(t, "first chunk", firstChunk)
			}
			cancel()
			waitFor(t, "upstream cancellation", cancelled)
			<-errs
			g.waitSlotsFree()

			if afterHeaders {
				g.waitMetric(fmt.Sprintf(`kukie_counter_requests_total{code="200",key=%q,model=%q} 1`, testKeyID, testModel))
			} else {
				g.waitMetric(fmt.Sprintf(`kukie_counter_requests_total{code="499",key=%q,model=%q} 1`, testKeyID, testModel))
				// The client left. That says nothing about the model.
				g.waitMetric(fmt.Sprintf(`kukie_counter_rejected_total{model=%q,reason="unavailable"} 0`, testModel))
			}
			g.waitMetric(fmt.Sprintf(`kukie_counter_upstream_broken_total{model=%q} 0`, testModel))
		})
	}
}

func TestConnectionFailureIs503(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close() // Nothing listens here now.

	g := startGateway(t, "http://"+addr, withLimit(1))
	res, body := g.post(chatBody())
	requireError(t, res, body, http.StatusServiceUnavailable, "model_unavailable")
	if got := res.Header.Get("Retry-After"); got != "7" {
		t.Fatalf("Retry-After = %q", got)
	}
	if strings.Contains(body, addr) {
		t.Fatalf("response exposes the upstream address: %q", body)
	}
	g.waitMetric(fmt.Sprintf(`kukie_counter_rejected_total{model=%q,reason="unavailable"} 1`, testModel))
	g.waitSlotsFree()
}

func TestUpstreamDropsConnectionBeforeResponseIs503(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		panic(http.ErrAbortHandler)
	}))
	defer ts.Close()
	g := startGateway(t, ts.URL, withLimit(1))

	res, body := g.post(chatBody())
	requireError(t, res, body, http.StatusServiceUnavailable, "model_unavailable")
	g.waitSlotsFree()
}

func TestUpstreamCutMidStreamAbortsClientAndReturnsSlot(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseHeaders(w)
		writeChunk(w, "data: {\"n\":1}\n\n")
		panic(http.ErrAbortHandler)
	}))
	defer ts.Close()
	g := startGateway(t, ts.URL, withLimit(1))

	res, err := g.client.Do(g.newRequest(http.MethodPost, pathChatCompletions, testKey, chatBody()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	got, err := io.ReadAll(res.Body)
	// The status is already out, so a broken connection is the only honest signal.
	if err == nil {
		t.Fatalf("client saw a clean end of stream: %q", got)
	}
	if !strings.Contains(string(got), `"n":1`) {
		t.Fatalf("first chunk lost: %q", got)
	}
	g.waitSlotsFree()
	g.waitMetric(fmt.Sprintf(`kukie_counter_upstream_broken_total{model=%q} 1`, testModel))
	g.waitMetric(fmt.Sprintf(`kukie_counter_rejected_total{model=%q,reason="unavailable"} 0`, testModel))
	g.waitMetric(fmt.Sprintf(`kukie_counter_usage_missing_total{model=%q} 1`, testModel))
}

func TestUpstreamErrorStatusPassesThroughAndReturnsSlot(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"too long"}}`)
	}))
	defer ts.Close()
	g := startGateway(t, ts.URL, withLimit(1))

	res, body := g.post(chatBody())
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(body, "too long") {
		t.Fatalf("status %d body %q", res.StatusCode, body)
	}
	g.waitSlotsFree()
	g.waitMetric(fmt.Sprintf(`kukie_counter_requests_total{code="400",key=%q,model=%q} 1`, testKeyID, testModel))
	// An error response has no usage by nature, so it is not "missing".
	g.waitMetric(fmt.Sprintf(`kukie_counter_usage_missing_total{model=%q} 0`, testModel))
}

type panicTransport struct{}

func (panicTransport) RoundTrip(*http.Request) (*http.Response, error) {
	panic("boom")
}

func TestPanicReturnsSlot(t *testing.T) {
	g := startGateway(t, "http://127.0.0.1:1", withLimit(1))
	g.srv.transport = panicTransport{}

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected the panic to propagate")
			}
		}()
		req := g.newRequest(http.MethodPost, pathChatCompletions, testKey, chatBody())
		g.srv.ServeHTTP(httptest.NewRecorder(), req)
	}()
	g.waitSlotsFree()
	g.waitMetric(fmt.Sprintf(`kukie_counter_requests_total{code="500",key=%q,model=%q} 1`, testKeyID, testModel))
}

// Run with -race. Slots must never be over-issued or leaked, whatever the mix of outcomes.
func TestConcurrentLoadNeverExceedsLimit(t *testing.T) {
	const (
		limit   = 4
		clients = 16
		rounds  = 25
	)
	var current, peak atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		n := current.Add(1)
		defer current.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		sseHeaders(w)
		writeChunk(w, "data: {\"n\":1}\n\n")
		select {
		case <-time.After(time.Millisecond):
		case <-r.Context().Done():
			return
		}
		writeChunk(w, usageChunk+"\n\n")
	}))
	defer ts.Close()
	g := startGateway(t, ts.URL, withLimit(limit))

	var ok, limited, abandoned atomic.Int32
	var wg sync.WaitGroup
	for c := range clients {
		wg.Go(func() {
			for r := range rounds {
				ctx, cancel := context.WithCancel(context.Background())
				req := g.newRequest(http.MethodPost, pathChatCompletions, testKey, chatBody()).WithContext(ctx)
				res, err := g.client.Do(req)
				if err != nil {
					cancel()
					t.Error(err)
					return
				}
				switch {
				case res.StatusCode == http.StatusTooManyRequests:
					limited.Add(1)
				case (c+r)%5 == 0:
					cancel() // Leave mid-stream.
					abandoned.Add(1)
				default:
					ok.Add(1)
				}
				_, _ = io.Copy(io.Discard, res.Body)
				_ = res.Body.Close()
				cancel()
			}
		})
	}
	wg.Wait()

	g.waitSlotsFree()
	if p := peak.Load(); p > limit {
		t.Fatalf("upstream saw %d concurrent requests, limit %d", p, limit)
	}
	if total := ok.Load() + limited.Load() + abandoned.Load(); total != clients*rounds {
		t.Fatalf("accounted for %d of %d requests", total, clients*rounds)
	}
	if limited.Load() == 0 {
		t.Fatal("the limit was never reached, so this test proved nothing")
	}
}

func TestModelsListComesFromConfig(t *testing.T) {
	var upstreamCalls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { upstreamCalls.Add(1) }))
	defer ts.Close()
	g := startGateway(t, ts.URL)

	res, body := g.do(g.newRequest(http.MethodGet, pathModels, testKey, ""))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	var parsed struct {
		Object string `json:"object"`
		Data   []struct {
			ID     string `json:"id"`
			Object string `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Object != "list" || len(parsed.Data) != 1 || parsed.Data[0].ID != testModel || parsed.Data[0].Object != "model" {
		t.Fatalf("models = %q", body)
	}
	if upstreamCalls.Load() != 0 {
		t.Fatal("/v1/models reached the upstream")
	}

	res, body = g.do(g.newRequest(http.MethodGet, pathModels, "", ""))
	requireError(t, res, body, http.StatusUnauthorized, "invalid_api_key")
}

func TestLogsAndMetricsHoldNoKeysOrContent(t *testing.T) {
	const secretPrompt = "the-secret-prompt-text"
	const secretOutput = "the-secret-model-output"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseHeaders(w)
		writeChunk(w, fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", secretOutput))
		panic(http.ErrAbortHandler) // Exercise the error logging too.
	}))
	defer ts.Close()
	g := startGateway(t, ts.URL)

	body := fmt.Sprintf(`{"model":%q,"messages":[{"content":%q}]}`, testModel, secretPrompt)
	req := g.newRequest(http.MethodPost, pathChatCompletions, testKey, body)
	if res, err := g.client.Do(req); err == nil {
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}
	g.do(g.newRequest(http.MethodPost, pathChatCompletions, testRevokedKey, body))
	g.do(g.newRequest(http.MethodPost, pathChatCompletions, "kk-wrong-key-value", body))
	g.do(g.newRequest(http.MethodPost, pathChatCompletions, testKey, `not json `+secretPrompt))
	g.waitMetric(`kukie_counter_requests_total{code="401",key="unknown",model="unknown"} 1`)
	g.waitMetric(fmt.Sprintf(`kukie_counter_upstream_broken_total{model=%q} 1`, testModel))
	g.waitMetric(fmt.Sprintf(`kukie_counter_requests_total{code="400",key=%q,model="unknown"} 1`, testKeyID))

	logs := g.logs.String()
	output := logs + g.scrape()
	for _, secret := range []string{
		testKey, testRevokedKey, "kk-wrong-key-value", hashOf(testKey), hashOf(testRevokedKey),
		secretPrompt, secretOutput, "Bearer", pathChatCompletions,
	} {
		if strings.Contains(output, secret) {
			t.Errorf("%q found in logs or metrics:\n%s", secret, logs)
		}
	}

	// The access log carries exactly the agreed fields.
	var found bool
	for line := range strings.Lines(logs) {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line %q is not JSON", line)
		}
		if entry["msg"] != "request" {
			continue
		}
		found = true
		for _, field := range []string{"time", "key", "model", "status", "duration_seconds", "body_bytes"} {
			if _, ok := entry[field]; !ok {
				t.Errorf("access log misses %q: %q", field, line)
			}
		}
		if len(entry) != 8 { // The six above plus slog's level and msg.
			t.Errorf("access log has extra fields: %q", line)
		}
	}
	if !found {
		t.Fatal("no access log lines")
	}
}

func TestServerTimeoutsDoNotCutStreams(t *testing.T) {
	cfg := newPublicServer(http.NotFoundHandler(), nil)
	if cfg.ReadTimeout != 0 || cfg.WriteTimeout != 0 {
		t.Fatalf("ReadTimeout %v, WriteTimeout %v must be 0", cfg.ReadTimeout, cfg.WriteTimeout)
	}
	if cfg.ReadHeaderTimeout <= 0 {
		t.Fatal("ReadHeaderTimeout must be set")
	}

	const streamFor = 400 * time.Millisecond
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseHeaders(w)
		writeChunk(w, "data: {\"n\":1}\n\n")
		time.Sleep(streamFor)
		writeChunk(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	g := startGateway(t, ts.URL)
	// A stream may outlive both the header timeout and the body deadline.
	g.srv.bodyReadTimeout = 50 * time.Millisecond

	res, body := g.post(chatBody())
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "[DONE]") {
		t.Fatalf("stream was cut: status %d body %q", res.StatusCode, body)
	}
}

func TestStalledBodyGetsAnAnswerWithoutHoldingASlot(t *testing.T) {
	for name, c := range map[string]struct {
		contentLength int
		status        int
	}{
		"within the limit": {100, http.StatusBadRequest},
		"over the limit":   {4 << 20, http.StatusRequestEntityTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			g := startGateway(t, jsonUpstream(t).URL, withLimit(1))
			g.srv.bodyReadTimeout = 50 * time.Millisecond

			conn, err := net.Dial("tcp", strings.TrimPrefix(g.url, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			// Declares a body and never sends it.
			_, err = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n\r\n",
				pathChatCompletions, testKey, c.contentLength)
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(waitTimeout))
			res, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("no response to a stalled body: %v", err)
			}
			defer func() { _ = res.Body.Close() }()
			if res.StatusCode != c.status {
				t.Fatalf("status = %d, want %d", res.StatusCode, c.status)
			}
			if n := len(g.srv.routes[testModel].slots); n != 0 {
				t.Fatalf("%d slots held while reading the body", n)
			}
		})
	}
}

func TestResponseRecorderReachesTheRealWriter(t *testing.T) {
	rec := &responseRecorder{ResponseWriter: httptest.NewRecorder()}
	if err := http.NewResponseController(rec).Flush(); err != nil {
		t.Fatalf("Flush through the recorder: %v", err)
	}
	rec.WriteHeader(http.StatusEarlyHints)
	if rec.status != 0 {
		t.Fatalf("1xx recorded as the status: %d", rec.status)
	}
}

func TestUpstreamBodyTreatsEOFAsClean(t *testing.T) {
	b := &upstreamBody{ReadCloser: io.NopCloser(strings.NewReader("x")), ctx: context.Background()}
	if _, err := io.ReadAll(b); err != nil || b.broken {
		t.Fatalf("err %v broken %v", err, b.broken)
	}
	failing := &upstreamBody{ReadCloser: io.NopCloser(iotestErrReader{}), ctx: context.Background()}
	if _, err := io.ReadAll(failing); err == nil || !failing.broken {
		t.Fatalf("err %v broken %v", err, failing.broken)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	leftClient := &upstreamBody{ReadCloser: io.NopCloser(iotestErrReader{}), ctx: ctx}
	if _, err := io.ReadAll(leftClient); err == nil || leftClient.broken {
		t.Fatalf("a client leaving was blamed on the upstream: err %v broken %v", err, leftClient.broken)
	}
}

type iotestErrReader struct{}

func (iotestErrReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestServeEndToEnd(t *testing.T) {
	upstream := jsonUpstream(t)
	dir := t.TempDir()
	configPath := dir + "/config.json"
	keysPath := dir + "/keys.json"
	writeFile(t, configPath, fmt.Sprintf(`{"models":{%q:{"upstream":%q,"maxConcurrent":1}}}`, testModel, upstream.URL))
	writeFile(t, keysPath, string(keyFile(t, defaultKeyEntries()...)))

	apiAddr, metricsAddr := freeAddr(t), freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, serveOptions{
			configPath: configPath, keysPath: keysPath, addr: apiAddr, metricsAddr: metricsAddr,
			reloadInterval: time.Hour, shutdownTimeout: time.Second,
		}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	}()

	client := &http.Client{Timeout: waitTimeout}
	var res *http.Response
	var err error
	for deadline := time.Now().Add(waitTimeout); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if res, err = client.Get("http://" + apiAddr + pathHealthz); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("server did not start: %v", err)
	}
	_ = res.Body.Close()

	res, err = client.Get("http://" + metricsAddr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	text, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if !strings.Contains(string(text), "kukie_counter_limit") {
		t.Fatalf("metrics port output:\n%s", text)
	}
	// The API port does not serve metrics.
	res, err = client.Get("http://" + apiAddr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("API port /metrics status = %d", res.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("serve did not stop")
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}
