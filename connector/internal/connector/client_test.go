package connector

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func fixture(t *testing.T, gateway, counter *httptest.Server) *Client {
	t.Helper()
	c, err := New(Config{GatewayURL: gateway.URL + "/connector/v1/connect", ClusterID: "a", CounterURL: counter.URL + "/v1", Workers: 1, Credential: func() (string, error) { return strings.Repeat("a", 64), nil }, CounterCredential: func() (string, error) { return "fixture-service-key", nil }, GatewayClient: gateway.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func sendRequest(ws *websocket.Conn) {
	b, _ := json.Marshal(message{Type: "request", ID: strings.Repeat("a", 32), Path: "/v1/chat/completions", Body: []byte(`{"model":"shared"}`), TimeoutMS: 3000})
	ws.Write(context.Background(), websocket.MessageText, b)
}

func TestForwardsOnlyToConfiguredCounterWithServiceCredential(t *testing.T) {
	counter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer fixture-service-key" || r.Header.Get("Cookie") != "" {
			t.Error("incorrect request mapping")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer counter.Close()
	observed := make(chan string, 1)
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Kukie-Cluster") != "a" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("a", 64) {
			t.Error("incorrect connector authentication")
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{protocol}})
		if err != nil {
			return
		}
		defer c.CloseNow()
		sendRequest(c)
		_, b, err := c.Read(context.Background())
		var head message
		json.Unmarshal(b, &head)
		if err != nil || head.Status != 200 {
			t.Error("missing response header")
		}
		var body strings.Builder
		for {
			typ, b, err := c.Read(context.Background())
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				body.Write(b)
			} else {
				var m message
				json.Unmarshal(b, &m)
				if m.Type == "end" {
					observed <- body.String()
					return
				}
			}
		}
	}))
	defer gateway.Close()
	c := fixture(t, gateway, counter)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c.once(ctx)
	select {
	case body := <-observed:
		if body != "data: [DONE]\n\n" {
			t.Fatal(body)
		}
	default:
		t.Fatal("response did not arrive")
	}
}

func TestGatewayDisconnectCancelsCounterBeforeHeaders(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	counter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
			close(stopped)
		case <-time.After(5 * time.Second):
		}
	}))
	defer counter.Close()
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{protocol}})
		if err != nil {
			return
		}
		sendRequest(c)
		<-started
		c.CloseNow()
	}))
	defer gateway.Close()
	c := fixture(t, gateway, counter)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c.once(ctx)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("counter not cancelled")
	}
}

func TestInvalidRouteNeverCallsCounter(t *testing.T) {
	var calls atomic.Int32
	counter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer counter.Close()
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{protocol}})
		if err != nil {
			return
		}
		defer c.CloseNow()
		b, _ := json.Marshal(message{Type: "request", ID: strings.Repeat("a", 32), Path: "https://other.example", Body: []byte(`{}`), TimeoutMS: 1000})
		c.Write(context.Background(), websocket.MessageText, b)
		c.Read(context.Background())
	}))
	defer gateway.Close()
	c := fixture(t, gateway, counter)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c.once(ctx)
	if calls.Load() != 0 {
		t.Fatal("untrusted route called counter")
	}
}

func TestReconnectRereadsCredential(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	var attempts atomic.Int32
	counter := httptest.NewServer(http.NotFoundHandler())
	defer counter.Close()
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{protocol}})
		if err != nil {
			return
		}
		defer c.CloseNow()
		if attempts.Load() >= 2 {
			cancel()
		}
	}))
	defer gateway.Close()
	c := fixture(t, gateway, counter)
	c.cfg.Credential = func() (string, error) {
		n := attempts.Add(1)
		if n == 1 {
			return strings.Repeat("a", 64), nil
		}
		return strings.Repeat("b", 64), nil
	}
	c.Run(ctx)
	if attempts.Load() < 2 {
		t.Fatal("did not reconnect")
	}
}

func TestRejectsInsecureGatewayAndUntrustedCertificate(t *testing.T) {
	counter := httptest.NewServer(http.NotFoundHandler())
	defer counter.Close()
	gateway := httptest.NewTLSServer(http.NotFoundHandler())
	defer gateway.Close()
	c := fixture(t, gateway, counter)
	cfg := c.cfg
	cfg.GatewayURL = "http://example.com/connector/v1/connect"
	if _, err := New(cfg); err == nil {
		t.Fatal("insecure gateway accepted")
	}
	cfg = c.cfg
	cfg.GatewayClient = nil
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if c.once(ctx) == nil {
		t.Fatal("untrusted certificate accepted")
	}
}
