package connector

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
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
	Credential        func() (string, error)
	CounterCredential func() (string, error)
	GatewayClient     *http.Client
	CounterClient     *http.Client
}

type Client struct {
	cfg                      Config
	gateway                  string
	counter                  string
	gatewayHTTP, counterHTTP *http.Client
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
	g.Scheme = "wss"
	return &Client{cfg, g.String(), strings.TrimRight(cfg.CounterURL, "/"), copyClient(cfg.GatewayClient, true), copyClient(cfg.CounterClient, false)}, nil
}

// Run maintains a fixed-size connection pool. Inference is never retried; only
// idle connection establishment/re-establishment is retried with bounded jitter.
func (c *Client) Run(ctx context.Context) {
	var workers sync.WaitGroup
	for range c.cfg.Workers {
		workers.Go(func() {
			delay := time.Second
			for ctx.Err() == nil {
				started := time.Now()
				_ = c.once(ctx)
				if ctx.Err() != nil {
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
	workers.Wait()
}

func (c *Client) once(ctx context.Context) error {
	token, err := c.cfg.Credential()
	if err != nil {
		return errSession
	}
	raw, err := hex.DecodeString(token)
	if err != nil || len(raw) != 32 {
		return errSession
	}
	dialCtx, done := context.WithTimeout(ctx, 10*time.Second)
	ws, response, err := websocket.Dial(dialCtx, c.gateway, &websocket.DialOptions{HTTPClient: c.gatewayHTTP, HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}, "X-Kukie-Cluster": []string{c.cfg.ClusterID}}, Subprotocols: []string{statusProtocol, protocol}})
	done()
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		return errSession
	}
	defer ws.CloseNow()
	if ws.Subprotocol() != protocol && ws.Subprotocol() != statusProtocol {
		return errSession
	}
	ws.SetReadLimit(2 << 20)
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var forwarding sync.Mutex
	if ws.Subprotocol() == statusProtocol {
		go c.reportModels(sessionCtx, ws, cancel, &forwarding)
	}
	incoming := make(chan message, 1)
	go func() {
		defer cancel()
		for {
			typ, b, err := ws.Read(sessionCtx)
			if err != nil {
				return
			}
			var m message
			if typ != websocket.MessageText || json.Unmarshal(b, &m) != nil || m.Type != "request" || !idPattern.MatchString(m.ID) || m.Path != "/v1/chat/completions" || len(m.Body) == 0 || len(m.Body) > maxRequest || m.TimeoutMS < 1 || m.TimeoutMS > 300000 {
				return
			}
			// At most one request may be outstanding on this connection.
			select {
			case incoming <- m:
			case <-sessionCtx.Done():
				return
			default:
				return
			}
		}
	}()
	for {
		select {
		case <-sessionCtx.Done():
			return errSession
		case m := <-incoming:
			forwarding.Lock()
			err := c.forward(sessionCtx, ws, m)
			forwarding.Unlock()
			if err != nil {
				return errSession
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
