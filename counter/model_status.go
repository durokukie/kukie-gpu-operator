package main

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

type modelObservation struct {
	CheckedAt   time.Time    `json:"checkedAt"`
	AgeMS       int64        `json:"ageMs"`
	ReadyModels []string     `json:"readyModels"`
	Limits      []modelLimit `json:"limits"`
}

// modelLimit is read from the current routes on every call, not from the probe cache.
type modelLimit struct {
	Model  string `json:"model"`
	Limit  int    `json:"limit"`
	Source string `json:"source"`
	Reason string `json:"reason"`
}

func currentLimits(table *routeTable) []modelLimit {
	limits := make([]modelLimit, 0, len(table.routes))
	for _, name := range slices.Sorted(maps.Keys(table.routes)) {
		r := table.routes[name]
		limits = append(limits, modelLimit{Model: name, Limit: r.limit, Source: r.source, Reason: r.reason})
	}
	return limits
}

// This authenticated endpoint probes configured targets only. It never starts
// a model or follows redirects, and does not send customer keys upstream.
func (s *server) modelStatus(w http.ResponseWriter, r *http.Request) {
	if !s.statusMu.TryLock() {
		http.Error(w, "status busy", http.StatusServiceUnavailable)
		return
	}
	defer s.statusMu.Unlock()
	table := s.table.Load()
	if time.Since(s.statusCache.CheckedAt) >= 15*time.Second {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
		defer cancel()
		names := make([]string, 0, len(table.routes))
		for name := range table.routes {
			names = append(names, name)
		}
		slices.Sort(names)
		if len(names) > 100 {
			names = names[:100]
		}
		ready := []string{}
		var mu sync.Mutex
		var workers sync.WaitGroup
		jobs := make(chan string)
		client := &http.Client{Transport: s.transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		for range min(8, len(names)) {
			workers.Go(func() {
				for name := range jobs {
					if probeModel(ctx, client, table.routes[name], name) {
						mu.Lock()
						ready = append(ready, name)
						mu.Unlock()
					}
				}
			})
		}
		for _, name := range names {
			select {
			case jobs <- name:
			case <-ctx.Done():
			}
		}
		close(jobs)
		workers.Wait()
		if ctx.Err() != nil {
			http.Error(w, "status unavailable", http.StatusServiceUnavailable)
			return
		}
		slices.Sort(ready)
		s.statusCache = modelObservation{CheckedAt: time.Now(), ReadyModels: ready}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	observation := s.statusCache
	observation.AgeMS = max(0, time.Since(observation.CheckedAt).Milliseconds())
	observation.CheckedAt = observation.CheckedAt.UTC()
	observation.Limits = currentLimits(table)
	_ = json.NewEncoder(w).Encode(observation)
}

func probeModel(parent context.Context, client *http.Client, route *modelRoute, name string) bool {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	target := *route.target
	target.Path = strings.TrimRight(target.Path, "/") + "/v1/models"
	target.RawPath = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return false
	}
	response, err := client.Do(req)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return false
	}
	b, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(b) > 64<<10 {
		return false
	}
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &result) != nil {
		return false
	}
	for _, m := range result.Data {
		if m.ID == name {
			return true
		}
	}
	return false
}
