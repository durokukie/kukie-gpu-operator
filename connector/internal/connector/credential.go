package connector

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"
)

const renewPath = "/connector/v1/credential/renew"

// credentialStore keeps the credential in memory so a renewed one is used on the
// next connection right away. A change of the mounted file (an administrator
// issuing a new credential) still wins over the in-memory value.
type credentialStore struct {
	read    func() (string, error)
	mu      sync.Mutex
	token   string
	file    string
	gen     uint64
	changed chan struct{}
}

func newCredentialStore(read func() (string, error)) *credentialStore {
	return &credentialStore{read: read, changed: make(chan struct{})}
}

// current returns the credential, its generation and a channel closed when it changes.
func (s *credentialStore) current() (string, uint64, <-chan struct{}, error) {
	file, err := s.read()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil && file != s.file {
		s.file = file
		if file != s.token {
			s.setLocked(file)
		}
	}
	if s.token == "" {
		if err == nil {
			err = errors.New("empty credential")
		}
		return "", s.gen, s.changed, err
	}
	return s.token, s.gen, s.changed, nil
}

func (s *credentialStore) adopt(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if token != s.token {
		s.setLocked(token)
	}
}

func (s *credentialStore) setLocked(token string) {
	s.token = token
	s.gen++
	close(s.changed)
	s.changed = make(chan struct{})
}

type renewed struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (renewed) String() string { return "renewed(REDACTED)" }

type renewError struct{ status int }

func (e renewError) Error() string {
	if e.status == 0 {
		return "gateway unreachable"
	}
	return fmt.Sprintf("gateway status %d", e.status)
}

func (c *Client) renew(ctx context.Context, token string) (renewed, error) {
	var result renewed
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.renewURL, http.NoBody)
	if err != nil {
		return result, renewError{}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Kukie-Cluster", c.cfg.ClusterID)
	response, err := c.gatewayHTTP.Do(req)
	if err != nil {
		return result, renewError{}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if response.StatusCode != http.StatusOK {
		return result, renewError{response.StatusCode}
	}
	if err != nil || len(body) > 4096 || json.Unmarshal(body, &result) != nil {
		return renewed{}, renewError{response.StatusCode}
	}
	raw, err := hex.DecodeString(result.Token)
	if err != nil || len(raw) != 32 || !result.ExpiresAt.After(time.Now()) {
		return renewed{}, renewError{response.StatusCode}
	}
	return result, nil
}

// renewLoop renews at startup and then at half of the remaining lifetime. A
// renewed credential is saved to the Secret first so a restarted Pod uses it,
// and adopted in memory even if saving fails because the old one now only
// lives through the server's overlap.
func (c *Client) renewLoop(ctx context.Context, startup chan<- struct{}) {
	log := c.log
	wait := time.Duration(0)
	backoff := 30 * time.Second
	unsaved := ""
	first := true
	for {
		if wait > 0 {
			timer := time.NewTimer(wait + time.Duration(rand.Int64N(int64(wait/10)+1)))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		if ctx.Err() != nil {
			return
		}
		if unsaved != "" {
			if err := c.cfg.Persist(ctx, unsaved); err != nil {
				log.Warn("connector credential still not saved to Secret; a Pod restart after the overlap will need re-registration", "error", err.Error())
				wait, backoff = backoff, min(2*backoff, 10*time.Minute)
				continue
			}
			log.Info("connector credential saved to Secret")
			unsaved, backoff = "", 30*time.Second
		}
		token, _, _, err := c.store.current()
		var result renewed
		if err == nil {
			result, err = c.renew(ctx, token)
		}
		var failure renewError
		switch {
		case err == nil:
			if perr := c.cfg.Persist(ctx, result.Token); perr != nil {
				unsaved = result.Token
				log.Warn("connector credential renewed but not saved to Secret; will retry", "error", perr.Error())
			}
			c.store.adopt(result.Token)
			next := max(30*time.Second, time.Until(result.ExpiresAt)/2)
			log.Info("connector credential renewed", "expiresAt", result.ExpiresAt.UTC().Format(time.RFC3339), "nextRenewal", next.Round(time.Second).String())
			wait, backoff = next, 30*time.Second
			if unsaved != "" {
				wait = backoff
			}
		case errors.As(err, &failure) && failure.status == http.StatusUnauthorized:
			log.Error("connector credential rejected: it expired or was revoked; an administrator must issue a new connector credential in the dashboard and update the Secret")
			wait = 10 * time.Minute
		case errors.As(err, &failure) && failure.status == http.StatusNotFound:
			log.Warn("gateway does not support connector credential renewal yet; keeping the current credential")
			wait = 30 * time.Minute
		case errors.As(err, &failure) && failure.status == http.StatusTooManyRequests:
			log.Info("connector credential renewed moments ago; retrying later")
			wait = time.Minute
		default:
			log.Warn("connector credential renewal failed; retrying", "error", err.Error(), "retryIn", backoff.String())
			wait, backoff = backoff, min(2*backoff, 10*time.Minute)
		}
		if first {
			// Workers start only after the startup renewal is applied or has failed.
			first = false
			close(startup)
		}
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
