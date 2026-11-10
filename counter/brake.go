package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Brakes (DURO-212). The controller moves the limit every few seconds from tail latency; a burst
// of requests arriving together is faster than that. So every brake interval the counter reads
// the model server's own /metrics and, while the server queues requests or its KV cache is
// nearly full, refuses new requests to that server with 429. A brake never moves the limit: on
// kukie-gpu the queue grew at every burst even when the SLA held, and cutting the limit on it
// would lower it for no reason. A read that fails never brakes, so the counter cannot shut a
// model off because its metrics are unreachable.
//
// Only servers the oven made (upstream http://<name>.<ns>.svc:8000) are read: their /metrics is
// vLLM's. In observe mode the brake is computed and published but nothing is refused.

const (
	defaultBrakeIntervalSeconds = 2
	defaultBrakeWaitingScrapes  = 2
	defaultBrakeKVRatio         = 0.9

	brakeWaiting = "waiting"
	brakeKV      = "kv"

	// maxUpstreamMetricsBytes bounds one /metrics read. vLLM's is a few hundred KiB at most.
	maxUpstreamMetricsBytes = 4 << 20
)

var brakeReasons = []string{brakeWaiting, brakeKV}

// brakeWatch is what the brake loop keeps per server between reads.
type brakeWatch struct {
	waitingStreak int
	last          string // the brake decided last time, enforced or not
}

// upstreamLoad is what the brake reads from a vLLM /metrics page.
type upstreamLoad struct {
	waiting float64
	kv      float64 // 0..1
}

var errNoVLLMMetrics = errors.New("no vLLM queue or KV cache metrics in the response")

// parseVLLMMetrics sums vllm:num_requests_waiting over engines and takes the highest KV cache
// usage. Older vLLM names the latter gpu_cache_usage_perc; both are ratios despite the name.
func parseVLLMMetrics(r io.Reader) (upstreamLoad, error) {
	var load upstreamLoad
	seenWaiting, seenKV := false, false
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "vllm:") {
			continue
		}
		name, rest, _ := strings.Cut(line, "{")
		if i := strings.LastIndexByte(rest, '}'); i >= 0 {
			rest = rest[i+1:]
		} else {
			name, rest, _ = strings.Cut(line, " ")
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		switch strings.TrimSpace(name) {
		case "vllm:num_requests_waiting":
			load.waiting += v
			seenWaiting = true
		case "vllm:kv_cache_usage_perc", "vllm:gpu_cache_usage_perc":
			load.kv = max(load.kv, v)
			seenKV = true
		}
	}
	if err := sc.Err(); err != nil {
		return upstreamLoad{}, err
	}
	if !seenWaiting && !seenKV {
		return upstreamLoad{}, errNoVLLMMetrics
	}
	return load, nil
}

// decide updates the streak and returns the brake that should be in force.
func (w *brakeWatch) decide(load upstreamLoad, cfg *adaptiveConfig) string {
	if load.waiting > 0 {
		w.waitingStreak++
	} else {
		w.waitingStreak = 0
	}
	switch {
	case w.waitingStreak >= cfg.BrakeWaitingScrapes:
		return brakeWaiting
	case load.kv >= cfg.BrakeKVRatio:
		return brakeKV
	}
	return ""
}

func (s *server) readUpstreamLoad(ctx context.Context, url string) (upstreamLoad, error) {
	timeout := time.Duration(s.state.d.Adaptive.BrakeIntervalSeconds) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return upstreamLoad{}, err
	}
	res, err := (&http.Client{Transport: s.transport}).Do(req)
	if err != nil {
		return upstreamLoad{}, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return upstreamLoad{}, errors.New("upstream /metrics status " + strconv.Itoa(res.StatusCode))
	}
	return parseVLLMMetrics(io.LimitReader(res.Body, maxUpstreamMetricsBytes))
}

// runBrakes reads every oven server's load each brake interval until ctx ends.
func (s *server) runBrakes(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(s.state.d.Adaptive.BrakeIntervalSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.brakeTick(ctx)
		}
	}
}

// brakeTick reads all servers at once, so one slow server cannot delay the others' brakes.
func (s *server) brakeTick(ctx context.Context) {
	cfg := s.state.d.Adaptive
	type target struct {
		url   string
		names []string
	}
	targets := map[*limiter]*target{}
	for name, r := range s.table.Load().routes {
		if r.slots.adaptive == nil {
			continue
		}
		if _, _, ok := serviceOf(r.target); !ok {
			continue
		}
		t := targets[r.slots]
		if t == nil {
			u := *r.target
			u.Path = "/metrics"
			t = &target{url: u.String()}
			targets[r.slots] = t
		}
		t.names = append(t.names, name)
	}
	var wg sync.WaitGroup
	for lim, t := range targets {
		wg.Go(func() {
			load, err := s.readUpstreamLoad(ctx, t.url)
			brake := ""
			prev := lim.brakeWatch.last
			if err != nil {
				lim.brakeWatch.waitingStreak = 0
				for _, name := range t.names {
					s.metrics.upstreamMetricsErrors.WithLabelValues(name).Inc()
				}
			} else {
				brake = lim.brakeWatch.decide(load, cfg)
				for _, name := range t.names {
					s.metrics.upstreamWaiting.WithLabelValues(name).Set(load.waiting)
					s.metrics.upstreamKV.WithLabelValues(name).Set(load.kv)
				}
			}
			for _, name := range t.names {
				for _, reason := range brakeReasons {
					v := 0.0
					if reason == brake {
						v = 1
					}
					s.metrics.brakeActive.WithLabelValues(name, reason).Set(v)
				}
			}
			lim.brakeWatch.last = brake
			if cfg.enforced() {
				lim.setBrake(brake)
			}
			if brake != prev {
				s.logger.Info("brake", "models", t.names, "brake", brake, "enforced", cfg.enforced(),
					"waiting", load.waiting, "kv_ratio", load.kv)
			}
		})
	}
	wg.Wait()
}
