package connector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCounterObservationChecksFreshnessAndUsesServiceKey(t *testing.T) {
	for _, age := range []time.Duration{0, -time.Hour, time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			counter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/model-status" || r.Header.Get("Authorization") != "Bearer fixture-service-key" {
					t.Error("wrong status request")
				}
				_ = json.NewEncoder(w).Encode(modelObservation{CheckedAt: time.Now().Add(age), ReadyModels: []string{"ready"}})
			}))
			defer counter.Close()
			gateway := httptest.NewTLSServer(http.NotFoundHandler())
			defer gateway.Close()
			result := fixture(t, gateway, counter).observeModels(context.Background())
			if age == 0 && len(result.ReadyModels) != 1 {
				t.Fatal("fresh observation missing")
			}
			if age != 0 && !result.CheckedAt.IsZero() {
				t.Fatal("stale/future observation accepted")
			}
		})
	}
}
