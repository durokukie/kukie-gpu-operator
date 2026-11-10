package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testModel      = "org/test-model"
	testKey        = "kk-test-key-active"
	testRevokedKey = "kk-test-key-revoked"
	testKeyID      = "alice"
	testRevokedID  = "bob"
	waitTimeout    = 10 * time.Second
)

func hashOf(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func keyFile(t *testing.T, entries ...keyEntry) []byte {
	t.Helper()
	if entries == nil {
		entries = []keyEntry{}
	}
	data, err := json.Marshal(map[string][]keyEntry{"keys": entries})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func defaultKeyEntries() []keyEntry {
	return []keyEntry{
		{ID: testKeyID, Name: "Alice", SHA256: hashOf(testKey)},
		{ID: testRevokedID, Name: "Bob", SHA256: hashOf(testRevokedKey), Disabled: true},
	}
}

// syncBuffer lets a test read the log while handlers are still writing to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type gateway struct {
	t        *testing.T
	url      string
	srv      *server
	keysPath string
	logs     *syncBuffer
	metrics  *metrics
	client   *http.Client
}

type gatewayOption func(*server, *config)

func withLimit(n int) gatewayOption {
	return func(_ *server, cfg *config) {
		m := cfg.Models[testModel]
		m.MaxConcurrent = n
		cfg.Models[testModel] = m
	}
}

func withMaxBody(n int64) gatewayOption {
	return func(_ *server, cfg *config) { cfg.MaxBodyBytes = n }
}

// startGateway runs the handler behind a real http.Server configured like production,
// because cancellation, flushing and aborts depend on the real server.
func startGateway(t *testing.T, upstream string, opts ...gatewayOption) *gateway {
	t.Helper()
	cfg := &config{
		Models:            map[string]modelConfig{testModel: {Upstream: upstream, MaxConcurrent: 8}},
		RetryAfterSeconds: 7,
		MaxBodyBytes:      1 << 20,
	}
	for _, opt := range opts {
		opt(nil, cfg)
	}

	keysPath := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(keysPath, keyFile(t, defaultKeyEntries()...), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	m := newMetrics()
	keys, err := newKeyStore(keysPath, logger, m.keysReloadErrors)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := newServer(cfg, keys, m, logger, 0)
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewUnstartedServer(srv)
	ts.Config = newPublicServer(srv, slog.NewLogLogger(logger.Handler(), slog.LevelError))
	ts.Start()
	t.Cleanup(ts.Close)

	return &gateway{
		t: t, url: ts.URL, srv: srv, keysPath: keysPath, logs: logs, metrics: m,
		client: &http.Client{Timeout: waitTimeout},
	}
}

func chatBody() string {
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}]}`, testModel)
}

func (g *gateway) newRequest(method, path, key, body string) *http.Request {
	g.t.Helper()
	req, err := http.NewRequest(method, g.url+path, strings.NewReader(body))
	if err != nil {
		g.t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return req
}

func (g *gateway) do(req *http.Request) (*http.Response, string) {
	g.t.Helper()
	res, err := g.client.Do(req)
	if err != nil {
		g.t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		g.t.Fatalf("read body: %v", err)
	}
	return res, string(body)
}

func (g *gateway) post(body string) (*http.Response, string) {
	g.t.Helper()
	return g.do(g.newRequest(http.MethodPost, pathChatCompletions, testKey, body))
}

func (g *gateway) scrape() string {
	g.t.Helper()
	rec := httptest.NewRecorder()
	g.metrics.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

// waitMetric polls because a handler's deferred work finishes just after the client
// has seen the end of the response.
func (g *gateway) waitMetric(line string) {
	g.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for {
		text := g.scrape()
		for got := range strings.Lines(text) {
			if strings.TrimSpace(got) == line {
				return
			}
		}
		if time.Now().After(deadline) {
			name, _, _ := strings.Cut(line, "{")
			var related []string
			for got := range strings.Lines(text) {
				if strings.HasPrefix(got, name) {
					related = append(related, strings.TrimSpace(got))
				}
			}
			g.t.Fatalf("metric line %q not found; have %q", line, related)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (g *gateway) waitSlotsFree() {
	g.t.Helper()
	g.waitMetric(fmt.Sprintf(`kukie_counter_in_flight{model=%q} 0`, testModel))
	if n := g.srv.table.Load().routes[testModel].slots.held(); n != 0 {
		g.t.Fatalf("%d slots still held", n)
	}
}

func requireError(t *testing.T, res *http.Response, body string, status int, code string) {
	t.Helper()
	if res.StatusCode != status {
		t.Fatalf("status = %d, want %d, body %q", res.StatusCode, status, body)
	}
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("error body %q is not JSON: %v", body, err)
	}
	if parsed.Error.Code != code || parsed.Error.Message == "" || parsed.Error.Type == "" {
		t.Fatalf("error body = %q, want code %q with message and type", body, code)
	}
}

func waitFor(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(waitTimeout):
		t.Fatalf("timed out waiting for %s", what)
	}
}
