package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbeRequiresActualModelAndDoesNotFollowRedirect(t *testing.T) {
	for _, scenario := range []struct {
		name, body string
		status     int
		ready      bool
	}{
		{"ready", `{"data":[{"id":"m"}]}`, 200, true},
		{"different", `{"data":[{"id":"other"}]}`, 200, false},
		{"unavailable", `{}`, 503, false},
		{"invalid", `not json`, 200, false},
		{"oversized", strings.Repeat("x", 65537), 200, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "" {
					t.Error("unexpected probe")
				}
				w.WriteHeader(scenario.status)
				_, _ = io.WriteString(w, scenario.body)
			}))
			defer ts.Close()
			target, _ := url.Parse(ts.URL)
			if got := probeModel(context.Background(), ts.Client(), &modelRoute{target: target}, "m"); got != scenario.ready {
				t.Fatalf("ready=%v", got)
			}
		})
	}
	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("followed redirect") }))
	defer targetServer.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, targetServer.URL, 302) }))
	defer redirect.Close()
	target, _ := url.Parse(redirect.URL)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if probeModel(context.Background(), client, &modelRoute{target: target}, "m") {
		t.Fatal("redirect accepted")
	}
}
func TestProbeCancellation(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer ts.Close()
	target, _ := url.Parse(ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if probeModel(ctx, ts.Client(), &modelRoute{target: target}, "m") {
		t.Fatal("canceled probe ready")
	}
}

func TestStatusEndpointIsAuthenticatedAndCached(t *testing.T) {
	var probes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		fmt.Fprintf(w, `{"data":[{"id":%q}]}`, testModel)
	}))
	defer upstream.Close()
	gateway := startGateway(t, upstream.URL)
	for _, token := range []string{"", testKey, testKey} {
		response, err := gateway.client.Do(gateway.newRequest(http.MethodGet, "/v1/model-status", token, ""))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if token == "" {
			if response.StatusCode != 401 {
				t.Fatal(response.StatusCode)
			}
		} else if response.StatusCode != 200 || !strings.Contains(string(body), testModel) {
			t.Fatal(response.StatusCode, string(body))
		}
	}
	if probes.Load() != 1 {
		t.Fatal("probe cache was not used")
	}
	gateway.srv.statusMu.Lock()
	response, err := gateway.client.Do(gateway.newRequest(http.MethodGet, "/v1/model-status", testKey, ""))
	gateway.srv.statusMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 503 {
		t.Fatal("busy refresh should not queue")
	}
}

func TestCanceledCallerCannotPoisonObservationCache(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, `{"data":[{"id":%q}]}`, testModel) }))
	defer upstream.Close()
	gateway := startGateway(t, upstream.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodGet, "/v1/model-status", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	gateway.srv.modelStatus(response, request)
	if response.Code != 200 || len(gateway.srv.statusCache.ReadyModels) != 1 {
		t.Fatal("caller cancellation poisoned cache")
	}
}
func TestIncompleteProbeDoesNotOverwritePreviousObservation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer upstream.Close()
	gateway := startGateway(t, upstream.URL)
	route := gateway.srv.routes[testModel]
	for i := 0; i < 9; i++ {
		gateway.srv.routes[fmt.Sprint(i)] = route
	}
	previous := modelObservation{CheckedAt: time.Now().Add(-16 * time.Second), ReadyModels: []string{testModel}}
	gateway.srv.statusCache = previous
	response := httptest.NewRecorder()
	gateway.srv.modelStatus(response, httptest.NewRequest(http.MethodGet, "/v1/model-status", nil))
	if response.Code != 503 || !gateway.srv.statusCache.CheckedAt.Equal(previous.CheckedAt) {
		t.Fatal("incomplete probe replaced cache")
	}
}
