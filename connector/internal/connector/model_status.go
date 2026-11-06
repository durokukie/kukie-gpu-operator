package connector

import (
	"context"
	"encoding/json"
	"github.com/coder/websocket"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type modelObservation struct {
	AgeMS       *int64    `json:"ageMs"`
	Type        string    `json:"type"`
	CheckedAt   time.Time `json:"checkedAt"`
	ReadyModels []string  `json:"readyModels"`
}

func (c *Client) reportModels(ctx context.Context, ws *websocket.Conn, cancel context.CancelFunc, forwarding *sync.Mutex) {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		observation := c.observeModels(ctx)
		observation.Type = "model_status"
		b, _ := json.Marshal(observation)
		if forwarding.TryLock() {
			writeCtx, done := context.WithTimeout(ctx, 5*time.Second)
			err := ws.Write(writeCtx, websocket.MessageText, b)
			done()
			forwarding.Unlock()
			if err != nil {
				cancel()
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
func (c *Client) observeModels(parent context.Context) modelObservation {
	unknown := modelObservation{ReadyModels: []string{}}
	started := time.Now()
	ctx, cancel := context.WithTimeout(parent, 4*time.Second)
	defer cancel()
	token, err := c.cfg.CounterCredential()
	if err != nil || token == "" || len(token) > 512 || strings.ContainsAny(token, " \t\r\n") {
		return unknown
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.counter+"/model-status", nil)
	if err != nil {
		return unknown
	}
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := c.counterHTTP.Do(req)
	if err != nil {
		return unknown
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return unknown
	}
	b, err := io.ReadAll(io.LimitReader(response.Body, (32<<10)+1))
	if err != nil || len(b) > 32<<10 {
		return unknown
	}
	var result modelObservation
	if json.Unmarshal(b, &result) != nil || len(result.ReadyModels) > 100 {
		return unknown
	}
	for _, m := range result.ReadyModels {
		if len(m) == 0 || len(m) > 200 {
			return unknown
		}
	}
	if result.AgeMS == nil || *result.AgeMS < 0 || *result.AgeMS > 45000 {
		return unknown
	}
	age := *result.AgeMS + time.Since(started).Milliseconds()
	if age > 45000 {
		return unknown
	}
	result.AgeMS = &age
	return result
}
