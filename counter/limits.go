package main

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"math"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Where a model's limit comes from, as shown in kukie_counter_limit_source and /v1/model-status.
const (
	sourceConfig   = "config"
	sourceBaseline = "baseline"
	sourceDefault  = "default"

	reasonConfigured       = "configured"
	reasonMeasured         = "measured"
	reasonPartial          = "partial"
	reasonNoBaseline       = "no_baseline"
	reasonStaleBaseline    = "stale_baseline"
	reasonNoServedModel    = "no_servedmodel"
	reasonUpstreamMismatch = "upstream_mismatch"
	reasonNotReadYet       = "not_read_yet"

	// maxDiscoveredNames bounds the routes, and so the metric series, ServedModels can add.
	maxDiscoveredNames = 100
	// maxBaselineConcurrency keeps a nonsense status value from becoming a huge int.
	maxBaselineConcurrency = 1 << 20

	discoveryTimeout = 10 * time.Second
)

// limiter is one model server's concurrency limit. A request takes a place without
// waiting and gives it back when its response ends. The limit can change at any time:
// requests already holding a place keep it, new ones are refused until enough have ended.
type limiter struct {
	mu       sync.Mutex
	limit    int
	inFlight int
}

func (l *limiter) tryAcquire() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight >= l.limit {
		return false
	}
	l.inFlight++
	return true
}

func (l *limiter) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inFlight--
}

func (l *limiter) setLimit(n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limit = n
}

func (l *limiter) held() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inFlight
}

// modelRoute is one name a request may ask for. Routes are never changed after they are
// made; a refresh makes new ones that share the limiter of the same model server.
type modelRoute struct {
	name   string
	target *url.URL
	slots  *limiter
	limit  int
	source string
	reason string
}

// routeTable is replaced as a whole, so a request sees one consistent set of routes.
type routeTable struct {
	routes         map[string]*modelRoute
	modelsResponse []byte
}

// plannedRoute is a route before its limiter is attached.
type plannedRoute struct {
	key    string // the model server; names with the same key share one limiter
	target *url.URL
	limit  int
	source string
	reason string
}

// limitFromBaseline is floor(maxConcurrency × safetyFactor), at least 1. Without a usable
// baseline it is the conservative default.
func (d *discoveryConfig) limitFromBaseline(m servedModel) (int, string, string) {
	b := m.Baseline
	if b == nil || b.MaxConcurrency < 1 {
		return d.DefaultMaxConcurrent, sourceDefault, reasonNoBaseline
	}
	// The epsilon keeps 0.29 × 100 from landing on 28.999… and rounding down to 28.
	measured := int(math.Floor(float64(min(b.MaxConcurrency, maxBaselineConcurrency))*d.SafetyFactor + 1e-9))
	measured = max(1, measured)
	if b.Generation < m.Generation {
		// The numbers describe an older model or engine setting. They may still say the
		// server is small, so the lower of the two is kept.
		return min(d.DefaultMaxConcurrent, measured), sourceDefault, reasonStaleBaseline
	}
	if b.Partial {
		// The run stopped early, so the numbers are a lower bound: safe to use as they are.
		return measured, sourceBaseline, reasonPartial
	}
	return measured, sourceBaseline, reasonMeasured
}

// sameServer reports whether a configured upstream is the Service of a ServedModel.
func sameServer(u *url.URL, m servedModel) bool {
	if u.Scheme != "http" || (u.Path != "" && u.Path != "/") || u.Port() != fmt.Sprint(servedModelPort) {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	base := m.Name + "." + m.Namespace
	return host == base || host == base+".svc" || host == base+".svc.cluster.local"
}

// planRoutes decides every route and its limit. listed is false until the first
// successful list, so a configured model can say why it is waiting.
func planRoutes(static map[string]modelConfig, d *discoveryConfig, servedModels []servedModel, listed bool) (
	map[string]plannedRoute, []string,
) {
	var warnings []string
	byName := map[string]servedModel{}
	if d != nil {
		sorted := slices.SortedFunc(slices.Values(servedModels), func(a, b servedModel) int {
			return cmp.Compare(a.Name, b.Name)
		})
		for _, m := range sorted {
			if m.Phase == phaseStopped || m.Deleting {
				continue
			}
			for _, name := range m.ServedNames {
				if other, taken := byName[name]; taken {
					if other.Name != m.Name {
						warnings = append(warnings, fmt.Sprintf("model %q is served by both %s and %s; using %s",
							name, other.Name, m.Name, other.Name))
					}
					continue
				}
				byName[name] = m
			}
		}
	}

	plan := make(map[string]plannedRoute, len(static)+len(byName))
	for name, mc := range static {
		target, err := parseUpstream(mc.Upstream)
		if err != nil {
			continue // parseConfig has already refused it.
		}
		p := plannedRoute{key: "config/" + name, target: target}
		m, found := byName[name]
		switch {
		case mc.MaxConcurrent > 0:
			p.limit, p.source, p.reason = mc.MaxConcurrent, sourceConfig, reasonConfigured
		case !listed:
			p.limit, p.source, p.reason = d.DefaultMaxConcurrent, sourceDefault, reasonNotReadYet
		case !found:
			p.limit, p.source, p.reason = d.DefaultMaxConcurrent, sourceDefault, reasonNoServedModel
		case !sameServer(target, m):
			// The baseline was measured on another server than the one this name goes to.
			p.limit, p.source, p.reason = d.DefaultMaxConcurrent, sourceDefault, reasonUpstreamMismatch
		default:
			p.limit, p.source, p.reason = d.limitFromBaseline(m)
		}
		if found && !sameServer(target, m) {
			warnings = append(warnings, fmt.Sprintf("model %q: the configured upstream wins over ServedModel %s", name, m.Name))
		}
		plan[name] = p
	}

	discovered := 0
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		if _, configured := plan[name]; configured {
			continue
		}
		if discovered == maxDiscoveredNames {
			warnings = append(warnings, fmt.Sprintf("more than %d discovered model names; the rest are ignored", maxDiscoveredNames))
			break
		}
		discovered++
		m := byName[name]
		p := plannedRoute{key: "servedmodel/" + m.Namespace + "/" + m.Name, target: m.upstream()}
		p.limit, p.source, p.reason = d.limitFromBaseline(m)
		plan[name] = p
	}
	return plan, warnings
}

// routeState is what the refresher owns. Requests only ever read the table.
type routeState struct {
	mu       sync.Mutex
	static   map[string]modelConfig
	d        *discoveryConfig
	limiters map[string]*limiter
	summary  string
}

// applyRoutes builds and publishes a new table. It runs at start and after every list.
func (s *server) applyRoutes(servedModels []servedModel, listed bool) error {
	st := &s.state
	st.mu.Lock()
	defer st.mu.Unlock()

	plan, warnings := planRoutes(st.static, st.d, servedModels, listed)
	routes := make(map[string]*modelRoute, len(plan))
	used := map[string]bool{}
	for name, p := range plan {
		lim := st.limiters[p.key]
		if lim == nil {
			lim = &limiter{}
			st.limiters[p.key] = lim
		}
		used[p.key] = true
		routes[name] = &modelRoute{name: name, target: p.target, slots: lim, limit: p.limit, source: p.source, reason: p.reason}
	}
	// Names of one server share a limiter, so its limit is set once from any of them; they
	// all have the same plan because it comes from the same ServedModel.
	for _, r := range routes {
		r.slots.setLimit(r.limit)
	}
	for key, lim := range st.limiters {
		// A limiter with requests still running is kept, so a server that comes back
		// before they end does not start counting from zero.
		if !used[key] && lim.held() == 0 {
			delete(st.limiters, key)
		}
	}

	body, err := modelsResponse(slices.Sorted(maps.Keys(routes)), time.Now().Unix())
	if err != nil {
		return err
	}
	old := s.table.Load()
	s.table.Store(&routeTable{routes: routes, modelsResponse: body})

	for name, r := range routes {
		if old == nil || old.routes[name] == nil {
			s.metrics.initModel(name)
		}
		s.metrics.limit.WithLabelValues(name).Set(float64(r.limit))
		s.metrics.limitSource.DeletePartialMatch(prometheus.Labels{"model": name})
		s.metrics.limitSource.WithLabelValues(name, r.source, r.reason).Set(1)
	}
	if old != nil {
		for name := range old.routes {
			if routes[name] == nil {
				s.metrics.limit.DeleteLabelValues(name)
				s.metrics.limitSource.DeletePartialMatch(prometheus.Labels{"model": name})
			}
		}
	}

	// Logged only when something changed, so a steady state does not repeat itself.
	summary := routeSummary(routes) + strings.Join(warnings, "\n")
	if summary != st.summary {
		st.summary = summary
		for _, w := range warnings {
			s.logger.Warn("discovery: " + w)
		}
		for _, name := range slices.Sorted(maps.Keys(routes)) {
			r := routes[name]
			s.logger.Info("model limit", "model", name, "limit", r.limit, "source", r.source, "reason", r.reason)
		}
	}
	return nil
}

func routeSummary(routes map[string]*modelRoute) string {
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(routes)) {
		r := routes[name]
		fmt.Fprintf(&b, "%s %s %d %s %s\n", name, r.target, r.limit, r.source, r.reason)
	}
	return b.String()
}

// refreshRoutes lists ServedModels once. On failure the routes stay as they are: a short
// API outage must not drop models or change limits.
func (s *server) refreshRoutes(ctx context.Context, lister servedModelLister) error {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	servedModels, err := lister.list(ctx)
	if err != nil {
		s.metrics.discoveryErrors.Inc()
		s.logger.Warn("discovery: list ServedModels failed, keeping the current limits", "error", err)
		return err
	}
	if err := s.applyRoutes(servedModels, true); err != nil {
		return err
	}
	s.metrics.discoveryLastSuccess.SetToCurrentTime()
	return nil
}

// watchServedModels refreshes every interval until ctx ends.
func (s *server) watchServedModels(ctx context.Context, lister servedModelLister, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.refreshRoutes(ctx, lister)
		}
	}
}
