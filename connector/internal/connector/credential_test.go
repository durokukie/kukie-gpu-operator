package connector

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestCredentialStorePrefersRenewedValueUntilTheFileChanges(t *testing.T) {
	file := strings.Repeat("a", 64)
	s := newCredentialStore(func() (string, error) { return file, nil })
	token, gen, changed, err := s.current()
	if err != nil || token != file || gen != 1 {
		t.Fatal(token, gen, err)
	}
	s.adopt(strings.Repeat("b", 64))
	select {
	case <-changed:
	default:
		t.Fatal("renewal did not signal workers")
	}
	if token, gen, _, _ = s.current(); token != strings.Repeat("b", 64) || gen != 2 {
		t.Fatal("unchanged file overrode the renewed credential")
	}
	file = strings.Repeat("b", 64) // The Secret update reached the mount.
	if token, gen, _, _ = s.current(); token != file || gen != 2 {
		t.Fatal("saved credential counted as a new one")
	}
	file = strings.Repeat("c", 64) // An administrator replaced the Secret.
	if token, gen, _, _ = s.current(); token != file || gen != 3 {
		t.Fatal("administrator credential ignored")
	}
	broken := newCredentialStore(func() (string, error) { return "", errors.New("missing") })
	if _, _, _, err = broken.current(); err == nil {
		t.Fatal("missing credential accepted")
	}
}

// renewGateway accepts sessions and renew calls like the real gateway, keeping
// each token's sessions so the test can see which credential they used.
type renewGateway struct {
	mu       sync.Mutex
	valid    map[string]bool
	sessions map[string]int
	next     []string
	renews   atomic.Int32
	status   int
	conns    map[string][]*websocket.Conn
}

func (g *renewGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	g.mu.Lock()
	ok := g.valid[token] && r.Header.Get("X-Kukie-Cluster") == "a"
	g.mu.Unlock()
	if !ok {
		w.WriteHeader(401)
		return
	}
	if r.URL.Path == renewPath {
		g.renews.Add(1)
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.status != 0 {
			w.WriteHeader(g.status)
			return
		}
		next := g.next[0]
		g.next = g.next[1:]
		g.valid[next] = true
		json.NewEncoder(w).Encode(map[string]any{"token": next, "expiresAt": time.Now().Add(time.Hour)})
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{protocol}})
	if err != nil {
		return
	}
	g.mu.Lock()
	g.sessions[token]++
	g.conns[token] = append(g.conns[token], c)
	g.mu.Unlock()
	c.Read(context.Background())
}

func (g *renewGateway) count(token string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sessions[token]
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(what)
}

func TestRenewsAtStartupSavesSecretAndReplacesSessionsWithoutDroppingOldOnes(t *testing.T) {
	first, second := strings.Repeat("a", 64), strings.Repeat("b", 64)
	g := &renewGateway{valid: map[string]bool{first: true}, sessions: map[string]int{}, conns: map[string][]*websocket.Conn{}, next: []string{second}}
	gateway := httptest.NewTLSServer(g)
	defer gateway.Close()
	counter := httptest.NewServer(http.NotFoundHandler())
	defer counter.Close()
	logs := &syncBuffer{}
	var saved, file atomic.Value
	file.Store(first)
	c, err := New(Config{GatewayURL: gateway.URL + "/connector/v1/connect", ClusterID: "a", CounterURL: counter.URL + "/v1", Workers: 2,
		Credential: func() (string, error) { return file.Load().(string), nil }, CounterCredential: func() (string, error) { return "fixture-service-key", nil }, GatewayClient: gateway.Client(),
		Persist: func(_ context.Context, previous, token string) error {
			if previous != first {
				t.Error("saved over an unexpected previous value")
			}
			saved.Store(token)
			return nil
		}, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	eventually(t, "workers did not connect with the renewed credential", func() bool { return g.count(second) == 2 })
	if g.count(first) != 0 || saved.Load() != second {
		t.Fatal("startup renewal was not applied before connecting or not saved")
	}
	// A later rotation (an administrator writes a new Secret) opens replacements first.
	third := strings.Repeat("c", 64)
	g.mu.Lock()
	g.valid[third] = true
	g.mu.Unlock()
	file.Store(third)
	c.store.current()
	eventually(t, "replacement sessions not opened", func() bool { return g.count(third) == 2 })
	g.mu.Lock()
	old := append([]*websocket.Conn{}, g.conns[second]...)
	g.mu.Unlock()
	for _, conn := range old {
		conn.Close(websocket.StatusNormalClosure, "credential rotated")
	}
	time.Sleep(300 * time.Millisecond)
	if g.count(second) != 2 || g.count(third) != 2 {
		t.Fatal("retired sessions reconnected instead of handing over", g.count(second), g.count(third))
	}
	cancel()
	<-done
	out := logs.String()
	for _, token := range []string{first, second, third} {
		if strings.Contains(out, token) {
			t.Fatal("credential leaked into logs")
		}
	}
	for _, want := range []string{"connector credential renewed", "gateway session connected", "gateway session disconnected", "status 1000"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing log %q in %s", want, out)
		}
	}
}

func TestRejectedAndUnsupportedRenewalAreLoggedAndKeepTheCredential(t *testing.T) {
	first := strings.Repeat("a", 64)
	for status, want := range map[int]string{401: "an administrator must issue a new connector credential", 404: "does not support connector credential renewal", 503: "renewal failed"} {
		g := &renewGateway{valid: map[string]bool{first: true}, sessions: map[string]int{}, conns: map[string][]*websocket.Conn{}, status: status}
		gateway := httptest.NewTLSServer(g)
		logs := &syncBuffer{}
		c, _ := New(Config{GatewayURL: gateway.URL + "/connector/v1/connect", ClusterID: "a", CounterURL: "http://127.0.0.1:1/v1", Workers: 1,
			Credential: func() (string, error) { return first, nil }, CounterCredential: func() (string, error) { return "k", nil }, GatewayClient: gateway.Client(),
			Persist: func(context.Context, string, string) error { t.Error("persisted without renewal"); return nil }, Logger: slog.New(slog.NewTextHandler(logs, nil))})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { c.Run(ctx); close(done) }()
		eventually(t, "did not connect with the existing credential", func() bool { return g.count(first) == 1 })
		cancel()
		<-done
		gateway.Close()
		if !strings.Contains(logs.String(), want) || strings.Contains(logs.String(), first) {
			t.Fatalf("status %d: %s", status, logs.String())
		}
	}
}

func TestExpiredCredentialConnectionIsLoggedOnce(t *testing.T) {
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer gateway.Close()
	logs := &syncBuffer{}
	c, _ := New(Config{GatewayURL: gateway.URL + "/connector/v1/connect", ClusterID: "a", CounterURL: "http://127.0.0.1:1/v1", Workers: 1,
		Credential: func() (string, error) { return strings.Repeat("a", 64), nil }, CounterCredential: func() (string, error) { return "k", nil }, GatewayClient: gateway.Client(), Logger: slog.New(slog.NewTextHandler(logs, nil))})
	for range 3 {
		c.once(context.Background(), func() {})
	}
	out := logs.String()
	if strings.Count(out, "gateway rejected the connector credential (401)") != 1 || strings.Contains(out, strings.Repeat("a", 64)) {
		t.Fatal(out)
	}
}

func TestSecretWriterUpdatesOnlyItsKeyWithResourceVersion(t *testing.T) {
	var puts atomic.Int32
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/kukie-gpu/secrets/kukie-connector" || r.Header.Get("Authorization") != "Bearer fixture-sa" {
			t.Error("unexpected Kubernetes request", r.URL.Path)
			w.WriteHeader(403)
			return
		}
		switch r.Method {
		case "GET":
			io.WriteString(w, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"kukie-connector","resourceVersion":"7"},"type":"Opaque","data":{"credential":"b2xk","counter-key":"a2VlcA=="}}`)
		case "PUT":
			var secret struct {
				Metadata map[string]any    `json:"metadata"`
				Data     map[string]string `json:"data"`
			}
			json.NewDecoder(r.Body).Decode(&secret)
			if puts.Add(1) == 1 {
				w.WriteHeader(409)
				return
			}
			want := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("d", 64)))
			if secret.Metadata["resourceVersion"] != "7" || secret.Data["credential"] != want || secret.Data["counter-key"] != "a2VlcA==" {
				t.Error("Secret update lost fields or resourceVersion")
			}
			io.WriteString(w, `{}`)
		}
	}))
	defer api.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("fixture-sa\n"), 0600)
	writer, err := NewSecretWriter(api.URL, "kukie-gpu", "kukie-connector", "credential", tokenFile, api.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err = writer.Write(context.Background(), "old", strings.Repeat("d", 64)); err != nil || puts.Load() != 2 {
		t.Fatal("conflict was not retried", err)
	}
	// The stored value is not the one this renewal replaces: an administrator changed it.
	if err = writer.Write(context.Background(), strings.Repeat("e", 64), strings.Repeat("d", 64)); !errors.Is(err, ErrSecretChanged) || puts.Load() != 2 {
		t.Fatal("overwrote a credential someone else stored", err)
	}
	denied := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer denied.Close()
	writer, _ = NewSecretWriter(denied.URL, "kukie-gpu", "kukie-connector", "credential", tokenFile, denied.Client())
	if err = writer.Write(context.Background(), "old", strings.Repeat("d", 64)); err == nil || strings.Contains(err.Error(), strings.Repeat("d", 64)) || !strings.Contains(err.Error(), "403") {
		t.Fatal(err)
	}
	if _, err = NewSecretWriter("http://insecure", "kukie-gpu", "kukie-connector", "credential", tokenFile, api.Client()); err == nil {
		t.Fatal("insecure Kubernetes API accepted")
	}
}

func TestSavingADeferredCredentialKeepsTheRenewalSchedule(t *testing.T) {
	first, second := strings.Repeat("a", 64), strings.Repeat("b", 64)
	g := &renewGateway{valid: map[string]bool{first: true}, sessions: map[string]int{}, conns: map[string][]*websocket.Conn{}, next: []string{second, strings.Repeat("c", 64)}}
	gateway := httptest.NewTLSServer(g)
	defer gateway.Close()
	var attempts atomic.Int32
	c, _ := New(Config{GatewayURL: gateway.URL + "/connector/v1/connect", ClusterID: "a", CounterURL: "http://127.0.0.1:1/v1", Workers: 1,
		Credential: func() (string, error) { return first, nil }, CounterCredential: func() (string, error) { return "k", nil }, GatewayClient: gateway.Client(),
		Persist: func(context.Context, string, string) error {
			if attempts.Add(1) == 1 {
				return errors.New("kubernetes API status 403")
			}
			return nil
		}})
	c.retryBase = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	eventually(t, "deferred credential not saved", func() bool { return attempts.Load() >= 2 })
	time.Sleep(500 * time.Millisecond)
	cancel()
	<-done
	if g.renews.Load() != 1 || attempts.Load() != 2 {
		t.Fatal("renewed again right after saving the deferred credential", g.renews.Load(), attempts.Load())
	}
}

func TestDeferredCredentialIsDroppedWhenTheSecretWasReplaced(t *testing.T) {
	first, second, admin := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	g := &renewGateway{valid: map[string]bool{first: true, admin: true}, sessions: map[string]int{}, conns: map[string][]*websocket.Conn{}, next: []string{second}}
	gateway := httptest.NewTLSServer(g)
	defer gateway.Close()
	var file atomic.Value
	file.Store(first)
	var writes atomic.Int32
	c, _ := New(Config{GatewayURL: gateway.URL + "/connector/v1/connect", ClusterID: "a", CounterURL: "http://127.0.0.1:1/v1", Workers: 1,
		Credential: func() (string, error) { return file.Load().(string), nil }, CounterCredential: func() (string, error) { return "k", nil }, GatewayClient: gateway.Client(),
		Persist: func(_ context.Context, previous, next string) error {
			writes.Add(1)
			if writes.Load() == 1 {
				return errors.New("kubernetes API status 403")
			}
			t.Error("retried saving after the Secret was replaced")
			return nil
		}})
	c.retryBase = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	eventually(t, "first save not attempted", func() bool { return writes.Load() == 1 })
	file.Store(admin) // An administrator issued a new credential and the mount changed.
	eventually(t, "administrator credential not used", func() bool { return g.count(admin) >= 1 })
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
}
