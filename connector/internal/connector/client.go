package connector

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

var errSession = errors.New("connector session interrupted")
var idPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
var clusterPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,63}$`)

type Config struct {
	GatewayURL string
	ClusterID  string
	CounterURL string
	Workers    int
	// Callbacks reread mounted credentials on reconnect; values must never be logged.
	Credential func() (string, error)
	// Persist saves a renewed credential (the connector's own Secret). Without
	// it the connector does not renew, because a renewed credential kept only in
	// memory would be lost on restart after the old one stopped working.
	// It replaces the given previous value only.
	Persist           func(ctx context.Context, previous, next string) error
	Logger            *slog.Logger
	CounterCredential func() (string, error)
	GatewayClient     *http.Client
	CounterClient     *http.Client
}

type Client struct {
	cfg                      Config
	gateway                  string
	counter                  string
	gatewayHTTP, counterHTTP *http.Client
	statusMu                 sync.Mutex
	statusCached             modelObservation
	statusPolledAt           time.Time
	store                    *credentialStore
	renewURL                 string
	log                      *slog.Logger
	sessions                 atomic.Int64
	logMu                    sync.Mutex
	logged                   map[string]time.Time
	retryBase                time.Duration
}

func New(cfg Config) (*Client, error) {
	g, err := url.Parse(cfg.GatewayURL)
	if err != nil || g.Scheme != "https" || g.Host == "" || g.User != nil || g.Path != "/connector/v1/connect" || g.RawQuery != "" || g.Fragment != "" {
		return nil, errors.New("invalid gateway origin")
	}
	u, err := url.Parse(cfg.CounterURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || strings.TrimRight(u.Path, "/") != "/v1" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid counter origin")
	}
	if !clusterPattern.MatchString(cfg.ClusterID) || cfg.Workers < 1 || cfg.Workers > 8 || cfg.Credential == nil || cfg.CounterCredential == nil {
		return nil, errors.New("invalid connector configuration")
	}
	copyClient := func(c *http.Client, proxy bool) *http.Client {
		if c == nil {
			tr := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
			if proxy {
				tr.Proxy = http.ProxyFromEnvironment
			}
			c = &http.Client{Transport: tr}
		}
		copied := *c
		copied.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		return &copied
	}
	renew := *g
	renew.Path = renewPath
	g.Scheme = "wss"
	logger := cfg.Logger
	if logger == nil {
		logger = discardLogger()
	}
	c := &Client{cfg: cfg, gateway: g.String(), counter: strings.TrimRight(cfg.CounterURL, "/"), gatewayHTTP: copyClient(cfg.GatewayClient, true), counterHTTP: copyClient(cfg.CounterClient, false),
		renewURL: renew.String(), log: logger, logged: map[string]time.Time{}, retryBase: 30 * time.Second}
	c.store = newCredentialStore(func() (string, error) { return c.cfg.Credential() })
	return c, nil
}

// Run maintains a fixed-size connection pool. Inference is never retried; only
// idle connection establishment/re-establishment is retried with bounded jitter.
// When the credential changes, each worker opens its replacement first and the
// old connection finishes its work until the gateway retires it.
func (c *Client) Run(ctx context.Context) {
	var workers sync.WaitGroup
	if c.cfg.Persist != nil {
		startup := make(chan struct{})
		workers.Go(func() { c.renewLoop(ctx, startup) })
		select {
		case <-startup:
		case <-time.After(20 * time.Second):
		case <-ctx.Done():
		}
	} else {
		c.log.Warn("connector credential auto-renewal is off: no Secret to save it to; the credential must be replaced before it expires")
	}
	var spawn func()
	spawn = func() {
		workers.Go(func() {
			delay := time.Second
			for ctx.Err() == nil {
				started := time.Now()
				replaced, _ := c.once(ctx, spawn)
				if replaced || ctx.Err() != nil {
					return
				}
				if time.Since(started) > 30*time.Second {
					delay = time.Second
				}
				timer := time.NewTimer(delay + time.Duration(rand.Int64N(int64(delay/2)+1)))
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				delay = min(30*time.Second, delay*2)
			}
		})
	}
	for range c.cfg.Workers {
		spawn()
	}
	workers.Wait()
}

// limited logs a repeating failure at most once per five minutes.
func (c *Client) limited(key string, write func()) {
	c.logMu.Lock()
	last, seen := c.logged[key]
	due := !seen || time.Since(last) >= 5*time.Minute
	if due {
		c.logged[key] = time.Now()
	}
	c.logMu.Unlock()
	if due {
		write()
	}
}

func (c *Client) once(ctx context.Context, spawn func()) (bool, error) {
	token, gen, changed, err := c.store.current()
	if err != nil {
		c.limited("credential-file", func() { c.log.Error("cannot read connector credential file") })
		return false, errSession
	}
	raw, err := hex.DecodeString(token)
	if err != nil || len(raw) != 32 {
		c.limited("credential-format", func() { c.log.Error("connector credential has an invalid format") })
		return false, errSession
	}
	dialCtx, done := context.WithTimeout(ctx, 10*time.Second)
	ws, response, err := websocket.Dial(dialCtx, c.gateway, &websocket.DialOptions{HTTPClient: c.gatewayHTTP, HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}, "X-Kukie-Cluster": []string{c.cfg.ClusterID}}, Subprotocols: []string{statusProtocol, protocol}})
	done()
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
			if response.Body != nil {
				response.Body.Close()
			}
		}
		switch {
		case ctx.Err() != nil:
		case status == http.StatusUnauthorized:
			c.limited("connect-401", func() {
				c.log.Error("gateway rejected the connector credential (401): it expired or was revoked; an administrator must issue a new connector credential in the dashboard and update the Secret")
			})
		case status != 0:
			c.limited(fmt.Sprintf("connect-%d", status), func() { c.log.Warn("gateway refused the connection", "status", status) })
		default:
			c.limited("connect-network", func() { c.log.Warn("cannot reach the gateway; retrying") })
		}
		return false, errSession
	}
	defer ws.CloseNow()
	if ws.Subprotocol() != protocol && ws.Subprotocol() != statusProtocol {
		return false, errSession
	}
	c.logMu.Lock()
	clear(c.logged)
	c.logMu.Unlock()
	session := c.sessions.Add(1)
	c.log.Info("gateway session connected", "session", session, "protocol", ws.Subprotocol(), "credentialGeneration", gen)
	ws.SetReadLimit(2 << 20)
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var forwarding sync.Mutex
	if ws.Subprotocol() == statusProtocol {
		go c.reportModels(sessionCtx, ws, cancel, &forwarding)
	}
	incoming := make(chan message, 1)
	var readErr atomic.Value
	go func() {
		defer cancel()
		for {
			typ, b, err := ws.Read(sessionCtx)
			if err != nil {
				if status := websocket.CloseStatus(err); status != -1 {
					readErr.Store(fmt.Sprintf("gateway closed with status %d", status))
				}
				return
			}
			var m message
			if typ != websocket.MessageText || json.Unmarshal(b, &m) != nil || m.Type != "request" || !idPattern.MatchString(m.ID) || m.Path != "/v1/chat/completions" || len(m.Body) == 0 || len(m.Body) > maxRequest || m.TimeoutMS < 1 || m.TimeoutMS > 300000 {
				readErr.Store("protocol violation")
				return
			}
			// At most one request may be outstanding on this connection.
			select {
			case incoming <- m:
			case <-sessionCtx.Done():
				return
			default:
				readErr.Store("protocol violation")
				return
			}
		}
	}()
	replaced := false
	ended := func() (bool, error) {
		<-sessionCtx.Done()
		reason, _ := readErr.Load().(string)
		if ctx.Err() != nil {
			reason = "shutting down"
		} else if reason == "" {
			reason = "connection lost"
		}
		c.log.Info("gateway session disconnected", "session", session, "reason", reason, "replaced", replaced)
		return replaced, errSession
	}
	for {
		select {
		case <-sessionCtx.Done():
			return ended()
		case <-changed:
			// Open the replacement with the new credential before this one goes away.
			changed = nil
			replaced = true
			spawn()
		case m := <-incoming:
			forwarding.Lock()
			err := c.forward(sessionCtx, ws, m)
			forwarding.Unlock()
			if err != nil {
				cancel()
				return ended()
			}
		}
	}
}

func (c *Client) forward(ctx context.Context, ws *websocket.Conn, m message) error {
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(m.TimeoutMS)*time.Millisecond)
	defer cancel()
	token, err := c.cfg.CounterCredential()
	if err != nil || token == "" || len(token) > 512 || strings.ContainsAny(token, " \t\r\n") {
		return errSession
	}
	request, err := http.NewRequestWithContext(callCtx, "POST", c.counter+"/chat/completions", bytes.NewReader(m.Body))
	if err != nil {
		return errSession
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	response, err := c.counterHTTP.Do(request)
	if err != nil {
		return errSession
	}
	defer response.Body.Close()
	ct := response.Header.Get("Content-Type")
	if len(ct) > 128 {
		return errSession
	}
	header, _ := json.Marshal(message{Type: "response", ID: m.ID, Status: response.StatusCode, ContentType: ct})
	if err = ws.Write(callCtx, websocket.MessageText, header); err != nil {
		return errSession
	}
	buf := make([]byte, chunkSize)
	var total int64
	for {
		n, readErr := response.Body.Read(buf)
		if n > 0 {
			if int64(n) > maxResponse-total {
				return errSession
			}
			total += int64(n)
			if err = ws.Write(callCtx, websocket.MessageBinary, buf[:n]); err != nil {
				return errSession
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return errSession
		}
	}
	end, _ := json.Marshal(message{Type: "end", ID: m.ID})
	if ws.Write(callCtx, websocket.MessageText, end) != nil {
		return errSession
	}
	return nil
}
