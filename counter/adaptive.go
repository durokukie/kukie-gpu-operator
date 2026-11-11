package main

import (
	"bytes"
	"context"
	"errors"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Adaptive limit (DURO-212). The counter times every streamed response it forwards: the time to
// the first token (TTFT) and the gaps between token events (ITL). Per model server it keeps a
// short window of these and, every interval, decides what the limit should be: one more when
// both tail latencies are well inside the SLA and the limit was really used, 30% less when one
// is over the SLA. In observe mode the decision is only published as metrics and logs; in on
// mode it is the limit in force. Brakes (brake.go) refuse new requests for a moment while the
// model server queues or its KV cache is nearly full, without moving the limit.

const (
	adaptiveOff     = "off"
	adaptiveObserve = "observe"
	adaptiveOn      = "on"

	defaultAdaptiveSLATTFTMillis   = 1000
	defaultAdaptiveSLAITLMillis    = 50
	defaultAdaptiveIntervalSeconds = 5
	defaultAdaptiveWindowSeconds   = 30
	defaultAdaptiveMaxLimit        = 256
	defaultTTFTMaxPromptTokens     = 4096

	// adaptiveDecrease is the factor applied when a tail latency is over the SLA.
	adaptiveDecrease = 0.7
	// adaptiveHeadroom: raise only when both tail latencies are at most this share of the SLA.
	// Between it and the SLA the limit holds, so it does not flap around the edge.
	adaptiveHeadroom = 0.8
	// adaptiveFill: raise only when requests in flight reached this share of the limit.
	adaptiveFill = 0.9
	// adaptiveCooldownTicks is how many intervals after a decrease the limit may not rise.
	adaptiveCooldownTicks = 3
	// A decision needs this many finished streamed requests started under the current limit,
	// and for ITL this many token gaps among them.
	adaptiveMinRequests = 10
	adaptiveMinGaps     = 200
	// adaptiveMinTTFTRequests is higher: TTFT has one sample per request, and with ten of them
	// the p95 is the slowest one. On kukie-gpu (2026-11-11) the first request after the model
	// sat idle took 1.5 s while the p95 was 0.18 s, and cut the limit by 30% on its own. With
	// twenty, the p95 is over the SLA only when at least two requests are.
	adaptiveMinTTFTRequests = 20
	// adaptiveRaiseSlowTTFT: with fewer than adaptiveMinTTFTRequests TTFT samples, this many of
	// them above the headroom keep the limit from rising. On kukie-gpu (2026-11-11) the first
	// ten seconds of a burst had TTFT p95 1.0 s from under twenty samples, and the limit rose on
	// ITL alone, which dropped those samples (DURO-214). Two matches the p95 rule above: with
	// twenty samples the p95 is over a line only when at least two are.
	adaptiveRaiseSlowTTFT = 2
	// minGapsPerRequest: a request with fewer gaps gives no ITL sample of its own.
	minGapsPerRequest = 5
	// maxGapsPerRequest bounds the gaps kept for one request's p95: the latest ones.
	maxGapsPerRequest = 1024
	// maxWindowSamples bounds one server's window.
	maxWindowSamples = 4096
	// bytesPerPromptToken estimates prompt tokens from the body size when usage is absent.
	bytesPerPromptToken = 4

	// Decision reasons, as in kukie_counter_adaptive_decisions_total.
	adaptiveIncrease      = "increase"
	adaptiveDecreaseITL   = "decrease_itl"
	adaptiveDecreaseTTFT  = "decrease_ttft"
	adaptiveHold          = "hold"
	adaptiveNotFull       = "not_full"
	adaptiveCooling       = "cooling"
	adaptiveAtMax         = "at_max"
	adaptiveTooFewSamples = "too_few_samples"
	// adaptiveStarted is the change reason right after a reset: the limit is the start value.
	adaptiveStarted = "start"
)

var adaptiveReasons = []string{
	adaptiveIncrease, adaptiveDecreaseITL, adaptiveDecreaseTTFT, adaptiveHold, adaptiveNotFull,
	adaptiveCooling, adaptiveAtMax, adaptiveTooFewSamples,
}

type adaptiveConfig struct {
	Mode                string  `json:"mode"`
	SLATTFTMillis       float64 `json:"slaTtftMs"`
	SLAITLMillis        float64 `json:"slaItlMs"`
	IntervalSeconds     int     `json:"intervalSeconds"`
	WindowSeconds       int     `json:"windowSeconds"`
	MaxLimit            int     `json:"maxLimit"`
	TTFTMaxPromptTokens int     `json:"ttftMaxPromptTokens"`
	// Brakes, brake.go. Read every BrakeIntervalSeconds; brake while vllm:num_requests_waiting
	// was above 0 BrakeWaitingScrapes reads in a row, or while KV cache usage is BrakeKVRatio or more.
	BrakeIntervalSeconds int     `json:"brakeIntervalSeconds"`
	BrakeWaitingScrapes  int     `json:"brakeWaitingScrapes"`
	BrakeKVRatio         float64 `json:"brakeKvRatio"`
}

func (a *adaptiveConfig) validate() error {
	switch a.Mode {
	case "":
		a.Mode = adaptiveOff
	case adaptiveOff, adaptiveObserve, adaptiveOn:
	default:
		return errors.New("config: discovery.adaptive.mode must be off, observe or on")
	}
	for _, v := range []float64{a.SLATTFTMillis, a.SLAITLMillis} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return errors.New("config: discovery.adaptive.slaTtftMs and slaItlMs must be positive numbers")
		}
	}
	if a.IntervalSeconds < 0 || a.WindowSeconds < 0 || a.MaxLimit < 0 || a.TTFTMaxPromptTokens < 0 ||
		a.BrakeIntervalSeconds < 0 || a.BrakeWaitingScrapes < 0 {
		return errors.New("config: discovery.adaptive values must not be negative")
	}
	a.SLATTFTMillis = cmpOr(a.SLATTFTMillis, defaultAdaptiveSLATTFTMillis)
	a.SLAITLMillis = cmpOr(a.SLAITLMillis, defaultAdaptiveSLAITLMillis)
	a.IntervalSeconds = cmpOr(a.IntervalSeconds, defaultAdaptiveIntervalSeconds)
	a.WindowSeconds = cmpOr(a.WindowSeconds, defaultAdaptiveWindowSeconds)
	a.MaxLimit = cmpOr(a.MaxLimit, defaultAdaptiveMaxLimit)
	a.TTFTMaxPromptTokens = cmpOr(a.TTFTMaxPromptTokens, defaultTTFTMaxPromptTokens)
	a.BrakeIntervalSeconds = cmpOr(a.BrakeIntervalSeconds, defaultBrakeIntervalSeconds)
	a.BrakeWaitingScrapes = cmpOr(a.BrakeWaitingScrapes, defaultBrakeWaitingScrapes)
	a.BrakeKVRatio = cmpOr(a.BrakeKVRatio, defaultBrakeKVRatio)
	if math.IsNaN(a.BrakeKVRatio) || a.BrakeKVRatio <= 0 || a.BrakeKVRatio > 1 {
		return errors.New("config: discovery.adaptive.brakeKvRatio must be above 0 and at most 1")
	}
	if a.WindowSeconds < a.IntervalSeconds {
		return errors.New("config: discovery.adaptive.windowSeconds must be at least intervalSeconds")
	}
	return nil
}

func cmpOr[T int | float64](v, fallback T) T {
	if v == 0 {
		return fallback
	}
	return v
}

func (a *adaptiveConfig) on() bool {
	return a != nil && a.Mode != adaptiveOff
}

// enforced reports whether the controller's limit and the brakes are the ones in force.
func (a *adaptiveConfig) enforced() bool {
	return a != nil && a.Mode == adaptiveOn
}

// tokenMarkers start a JSON string that holds generated text in OpenAI-compatible stream
// chunks: chat content, reasoning, tool call arguments and completions text. A marker counts
// only when the string is not empty, so vLLM's first chunk with the role and "content":"" is
// not taken for the first token. Quotes inside a string are escaped, so generated text cannot
// fake a marker.
var tokenMarkers = [][]byte{
	[]byte(`"content":"`), []byte(`"content": "`),
	[]byte(`"reasoning_content":"`), []byte(`"reasoning_content": "`),
	[]byte(`"reasoning":"`), []byte(`"reasoning": "`),
	[]byte(`"arguments":"`), []byte(`"arguments": "`),
	[]byte(`"text":"`), []byte(`"text": "`),
}

// markerTail is how many bytes of a read are kept to find a marker split across two reads:
// the longest marker plus the first byte of its string.
var markerTail = slices.MaxFunc(tokenMarkers, func(a, b []byte) int { return len(a) - len(b) })

// streamTimer times one streamed response as it passes through upstreamBody.Read. Only the
// time spent waiting on the upstream counts: time spent writing to a slow client is left
// out, so a client that reads slowly cannot make the model look slow. It keeps no content
// beyond the last few bytes needed to find a marker split across reads. Not safe for
// concurrent use; Read is called from one goroutine.
type streamTimer struct {
	start      time.Time
	lastReturn time.Time
	outside    time.Duration // time between reads, spent writing to the client
	sinceToken time.Duration // upstream wait since the last read that held a token
	ttft       time.Duration // 0 until the first token
	gaps       []float64     // seconds, the latest maxGapsPerRequest
	gapCount   int
	tail       []byte
	scratch    []byte
	itl        prometheus.Observer
}

func newStreamTimer(start time.Time, itl prometheus.Observer) *streamTimer {
	return &streamTimer{start: start, itl: itl, tail: make([]byte, 0, len(markerTail)+1)}
}

// beforeRead and afterRead wrap one upstream read.
func (t *streamTimer) beforeRead(now time.Time) {
	if !t.lastReturn.IsZero() {
		t.outside += now.Sub(t.lastReturn)
	}
	t.lastReturn = now
}

func (t *streamTimer) afterRead(now time.Time, p []byte) {
	wait := now.Sub(t.lastReturn)
	t.lastReturn = now
	token := t.scan(p)
	switch {
	case !token:
		t.sinceToken += wait
	case t.ttft == 0:
		t.ttft = max(now.Sub(t.start)-t.outside, time.Nanosecond)
		t.sinceToken = 0
	default:
		gap := (t.sinceToken + wait).Seconds()
		t.sinceToken = 0
		t.gapCount++
		if len(t.gaps) == maxGapsPerRequest {
			copy(t.gaps, t.gaps[1:])
			t.gaps = t.gaps[:maxGapsPerRequest-1]
		}
		t.gaps = append(t.gaps, gap)
		if t.itl != nil {
			t.itl.Observe(gap)
		}
	}
}

// scan reports whether p holds the start of a non-empty text string, including one whose
// marker began in the previous read.
func (t *streamTimer) scan(p []byte) bool {
	found := false
	if len(t.tail) > 0 && len(p) > 0 {
		// Only matches that start in the tail and end in p: those wholly in p are found below.
		t.scratch = append(append(t.scratch[:0], t.tail...), p[:min(len(p), len(markerTail))]...)
		found = hasMarker(t.scratch, len(t.tail))
	}
	if !found {
		found = hasMarker(p, 0)
	}
	if len(p) >= cap(t.tail) {
		t.tail = append(t.tail[:0], p[len(p)-cap(t.tail):]...)
	} else {
		keep := min(len(t.tail), cap(t.tail)-len(p))
		t.tail = append(t.tail[:0], t.tail[len(t.tail)-keep:]...)
		t.tail = append(t.tail, p...)
	}
	return found
}

// hasMarker looks for a marker followed by a byte other than a closing quote. With split > 0
// a match must start before split and its following byte must be at or after it.
func hasMarker(b []byte, split int) bool {
	for _, m := range tokenMarkers {
		rest, offset := b, 0
		for {
			i := bytes.Index(rest, m)
			if i < 0 {
				break
			}
			start := offset + i
			next := start + len(m)
			if split > 0 && start >= split {
				break
			}
			if next < len(b) && b[next] != '"' && (split == 0 || next >= split) {
				return true
			}
			offset = start + 1
			rest = b[offset:]
		}
	}
	return false
}

// requestSample is what one finished streamed request leaves in the window. Negative
// values mean "no sample".
type requestSample struct {
	end    time.Time
	gen    uint64
	ttft   float64 // seconds
	itlP95 float64 // seconds, the p95 of this request's gaps
	gaps   int
}

// sample turns the timer into a window sample. withTTFT is false for long prompts, whose
// first token is slow because of their length, not because of the load.
func (t *streamTimer) sample(end time.Time, gen uint64, withTTFT bool) requestSample {
	s := requestSample{end: end, gen: gen, ttft: -1, itlP95: -1, gaps: t.gapCount}
	if t.ttft > 0 && withTTFT {
		s.ttft = t.ttft.Seconds()
	}
	if len(t.gaps) >= minGapsPerRequest {
		s.itlP95 = percentile(slices.Clone(t.gaps), 0.95)
	}
	return s
}

// percentile sorts v and returns the nearest-rank value.
func percentile(v []float64, q float64) float64 {
	slices.Sort(v)
	i := int(math.Ceil(q*float64(len(v)))) - 1
	return v[max(0, min(i, len(v)-1))]
}

// adaptiveState is the controller of one model server, shared by its names like the limiter.
type adaptiveState struct {
	mu       sync.Mutex
	limit    int
	startKey string
	fixed    bool // a person set maxConcurrent: nothing to decide
	gen      uint64
	cooldown int
	peaks    []peakSample // per tick, since the last change, within the window
	samples  []requestSample
	last     adaptiveDecision
	// changeReason is why the limit last moved: adaptiveStarted after a reset, else a decision
	// reason. It is the reason shown with source "adaptive", so it changes only with the limit.
	changeReason string
}

// peakSample is the most requests in flight during one interval, ending at at.
type peakSample struct {
	at time.Time
	n  int
}

type adaptiveDecision struct {
	fixed   bool
	from    int
	limit   int
	reason  string
	changed bool
	itlP95  float64 // -1 when there were too few samples
	ttftP95 float64
}

// reset starts over from a new start value. Changing the key (the baseline was measured
// again, the spec changed, a person set or removed maxConcurrent) drops what was learned.
func (a *adaptiveState) reset(start int, key string, fixed bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if key == a.startKey && fixed == a.fixed {
		return
	}
	a.startKey, a.fixed, a.limit = key, fixed, start
	a.cooldown, a.peaks = 0, nil
	a.changeReason = adaptiveStarted
	a.gen++
	a.last = adaptiveDecision{limit: start, reason: adaptiveTooFewSamples, itlP95: -1, ttftP95: -1}
}

// current returns the controller's limit and why it last moved.
func (a *adaptiveState) current() (int, string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.limit, a.changeReason, a.fixed
}

func (a *adaptiveState) generation() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gen
}

func (a *adaptiveState) add(s requestSample) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s.gen != a.gen {
		return // started under another limit: it says nothing about this one
	}
	if len(a.samples) == maxWindowSamples {
		a.samples = slices.Delete(a.samples, 0, 1)
	}
	a.samples = append(a.samples, s)
}

// tick makes one decision. peak is the most requests in flight since the last tick.
func (a *adaptiveState) tick(now time.Time, cfg *adaptiveConfig, peak int) adaptiveDecision {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fixed {
		return adaptiveDecision{fixed: true}
	}
	cutoff := now.Add(-time.Duration(cfg.WindowSeconds) * time.Second)
	a.samples = slices.DeleteFunc(a.samples, func(s requestSample) bool { return s.end.Before(cutoff) || s.gen != a.gen })
	// The most in flight within the window, like the samples: a burst minutes ago must not
	// count as "the limit was used" now.
	a.peaks = slices.DeleteFunc(a.peaks, func(p peakSample) bool { return p.at.Before(cutoff) })
	a.peaks = append(a.peaks, peakSample{at: now, n: peak})
	windowPeak := 0
	for _, p := range a.peaks {
		windowPeak = max(windowPeak, p.n)
	}
	cooling := a.cooldown > 0
	if cooling {
		a.cooldown--
	}

	var itl, ttft []float64
	gaps, slowTTFT := 0, 0
	for _, s := range a.samples {
		if s.itlP95 >= 0 {
			itl = append(itl, s.itlP95)
			gaps += s.gaps
		}
		if s.ttft >= 0 {
			ttft = append(ttft, s.ttft)
			if s.ttft > cfg.SLATTFTMillis/1000*adaptiveHeadroom {
				slowTTFT++
			}
		}
	}
	d := adaptiveDecision{from: a.limit, limit: a.limit, itlP95: -1, ttftP95: -1}
	enoughITL := len(itl) >= adaptiveMinRequests && gaps >= adaptiveMinGaps
	enoughTTFT := len(ttft) >= adaptiveMinTTFTRequests
	if enoughITL {
		// The median of per-request p95s: one request cannot move it on its own.
		d.itlP95 = percentile(itl, 0.5)
	}
	if enoughTTFT {
		d.ttftP95 = percentile(ttft, 0.95)
	}
	slaITL, slaTTFT := cfg.SLAITLMillis/1000, cfg.SLATTFTMillis/1000

	switch {
	case !enoughITL && !enoughTTFT:
		d.reason = adaptiveTooFewSamples
	case enoughITL && d.itlP95 > slaITL:
		d.reason = adaptiveDecreaseITL
	case enoughTTFT && d.ttftP95 > slaTTFT:
		d.reason = adaptiveDecreaseTTFT
	case !enoughITL || d.itlP95 > slaITL*adaptiveHeadroom || (enoughTTFT && d.ttftP95 > slaTTFT*adaptiveHeadroom),
		!enoughTTFT && slowTTFT >= adaptiveRaiseSlowTTFT:
		// Too few TTFT samples to cut on, but enough slow ones not to raise: keep this
		// generation's samples so the next ticks can still decide on them.
		d.reason = adaptiveHold
	case cooling:
		d.reason = adaptiveCooling
	case float64(windowPeak) < adaptiveFill*float64(a.limit):
		d.reason = adaptiveNotFull
	case a.limit >= cfg.MaxLimit:
		d.reason = adaptiveAtMax
	default:
		d.reason = adaptiveIncrease
	}

	switch d.reason {
	case adaptiveDecreaseITL, adaptiveDecreaseTTFT:
		d.limit = max(1, int(math.Floor(float64(a.limit)*adaptiveDecrease)))
		a.cooldown = adaptiveCooldownTicks
	case adaptiveIncrease:
		d.limit = a.limit + 1
	}
	if d.reason == adaptiveDecreaseITL || d.reason == adaptiveDecreaseTTFT || d.limit != a.limit {
		// Samples taken under the old limit must not decide again: after a decrease they
		// would only repeat it. Only requests started from now on count.
		d.changed = d.limit != a.limit
		if d.changed {
			a.changeReason = d.reason
		}
		a.limit = d.limit
		a.gen++
		a.samples = a.samples[:0]
		a.peaks = a.peaks[:0]
	}
	a.last = d
	return d
}

// runAdaptive decides every interval until ctx ends.
func (s *server) runAdaptive(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(s.state.d.Adaptive.IntervalSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.adaptiveTick(now)
		}
	}
}

// adaptiveTick makes one decision per model server and publishes it under each of its names.
// It holds the route state lock so a refresh cannot remove a name's series while they are set.
func (s *server) adaptiveTick(now time.Time) {
	st := &s.state
	st.mu.Lock()
	defer st.mu.Unlock()
	cfg := st.d.Adaptive
	names := map[*limiter][]string{}
	for name, r := range s.table.Load().routes {
		if r.slots.adaptive != nil {
			names[r.slots] = append(names[r.slots], name)
		}
	}
	enforced := cfg.enforced()
	for lim, list := range names {
		slices.Sort(list)
		d := lim.adaptive.tick(now, cfg, lim.takePeak())
		var changeReason string
		if enforced && !d.fixed && d.changed {
			_, changeReason, _ = lim.adaptive.current()
			lim.set(d.limit, sourceAdaptive, changeReason)
		}
		for _, name := range list {
			if d.fixed {
				s.metrics.deleteController(name)
				continue
			}
			s.metrics.adaptiveLimit.WithLabelValues(name, cfg.Mode).Set(float64(d.limit))
			s.metrics.adaptiveDecisions.WithLabelValues(name, d.reason).Inc()
			s.metrics.windowITL.WithLabelValues(name).Set(d.itlP95)
			s.metrics.windowTTFT.WithLabelValues(name).Set(d.ttftP95)
			if changeReason != "" {
				s.metrics.limit.WithLabelValues(name).Set(float64(d.limit))
				s.metrics.limitSource.DeletePartialMatch(prometheus.Labels{"model": name})
				s.metrics.limitSource.WithLabelValues(name, sourceAdaptive, changeReason).Set(1)
			}
		}
		if d.changed {
			s.logger.Info("adaptive limit decided", "models", list, "mode", cfg.Mode, "enforced", enforced,
				"from", d.from, "limit", d.limit, "reason", d.reason,
				"itl_p95_seconds", d.itlP95, "ttft_p95_seconds", d.ttftP95)
		}
	}
}
