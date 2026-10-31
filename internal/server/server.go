// Package server is the HTTP edge: authentication, request parsing, client-format
// encoding, health endpoints and graceful drain.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/gateway"
	"github.com/SureshKhemka/llm-gateway/internal/governance"
)

type Server struct {
	Engine *gateway.Engine
	Log    *slog.Logger
	// Ready reports readiness: config loaded and ≥1 provider reachable [OBS-05].
	Ready func() (bool, string)
	// Extra routes (admin API, metrics, console) are mounted by the caller.
	Mux *http.ServeMux

	inFlight atomic.Int64
	draining atomic.Bool
}

func New(e *gateway.Engine, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{Engine: e, Log: log, Mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.Mux.HandleFunc("POST /v1/chat/completions", s.track(s.handleChat))
	s.Mux.HandleFunc("POST /v1/embeddings", s.track(s.handleEmbeddings))
	s.Mux.HandleFunc("GET /v1/models", s.handleModels)
	s.Mux.HandleFunc("POST /v1/route/explain", s.handleExplain)
	s.anthropicRoutes()
	s.Mux.HandleFunc("POST /v1/feedback", s.handleFeedback)
	s.Mux.HandleFunc("GET /v1/route/decisions/{id}", s.handleDecision)
	s.Mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	s.Mux.HandleFunc("GET /readyz", s.handleReady)
}

func (s *Server) Handler() http.Handler { return s.Mux }

// InFlight returns the number of requests currently being served.
func (s *Server) InFlight() int64 { return s.inFlight.Load() }

// track counts in-flight requests for draining and load shedding.
func (s *Server) track(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Count first, then check: Drain sets draining before it starts waiting, so any
		// request that saw draining=false is already counted.
		s.inFlight.Add(1)
		defer s.inFlight.Add(-1)
		if s.draining.Load() {
			api.WriteError(w, formatOf(r), api.NewError(api.CodeOverloaded, "gateway is shutting down"))
			return
		}
		h(w, r)
	}
}

// Drain stops accepting requests and waits for in-flight ones up to ctx [RES-10].
func (s *Server) Drain(ctx context.Context) error {
	s.draining.Store(true)
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for s.inFlight.Load() > 0 {
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	if s.draining.Load() {
		http.Error(w, "draining", http.StatusServiceUnavailable)
		return
	}
	if s.Ready != nil {
		if ok, why := s.Ready(); !ok {
			http.Error(w, why, http.StatusServiceUnavailable)
			return
		}
	}
	w.Write([]byte("ready"))
}

func formatOf(r *http.Request) api.Format {
	if strings.HasPrefix(r.URL.Path, "/v1/messages") {
		return api.FormatAnthropic
	}
	return api.FormatOpenAI
}

// bearer extracts the client credential.
func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}

func clientIP(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, _ := netip.ParseAddr(host)
	return a.Unmap()
}

// Authenticate resolves the caller [TEN-02, TEN-03]. With no keys configured the
// gateway runs in open single-tenant mode (minimal config, CFG-07).
func (s *Server) Authenticate(r *http.Request, endpoint string) (*governance.Principal, *api.Error) {
	snap := s.Engine.Snapshot()
	tok := bearer(r)
	if snap.Keys.Len() == 0 {
		return &governance.Principal{Anonymous: true, KeyPrefix: "anonymous"}, nil
	}
	if tok == "" {
		return nil, api.NewError(api.CodeUnauthorized, "missing API key")
	}
	k := snap.Keys.ByHash(governance.HashKey(tok))
	if k == nil || !k.Valid(time.Now()) || s.revoked(k.Record.ID) {
		return nil, api.NewError(api.CodeUnauthorized, "invalid API key")
	}
	p := &governance.Principal{Tenant: k.Tenant, Project: k.Project, KeyID: k.Record.ID, KeyPrefix: k.Record.Prefix, KeyName: k.Record.Name, Scopes: k.Scopes}
	if !p.EndpointAllowed(endpoint) {
		return nil, api.NewError(api.CodeForbidden, "this key may not call the %s endpoint", endpoint)
	}
	if !p.IPAllowed(clientIP(r)) {
		return nil, api.NewError(api.CodeForbidden, "client address not allowed for this key")
	}
	return p, nil
}

// Shed rejects a request when the gateway itself is saturated, lower-priority keys
// first [RES-09]: priority 0 keys are shed at 80% of max_in_flight, priority 1 at 90%,
// higher priorities only at 100%.
func (s *Server) Shed(p *governance.Principal, hintPriority *int) *api.Error {
	max := s.Engine.Snapshot().Config.Server.MaxInFlight
	if max <= 0 {
		return nil
	}
	prio := p.Scopes.Priority
	if hintPriority != nil && *hintPriority < prio {
		prio = *hintPriority // hints may only lower priority [RTE-10]
	}
	frac := 1.0
	switch {
	case prio <= 0:
		frac = 0.8
	case prio == 1:
		frac = 0.9
	}
	if float64(s.inFlight.Load()) > frac*float64(max) {
		e := api.NewError(api.CodeOverloaded, "gateway is saturated; retry shortly")
		e.RetryAfter = time.Second
		return e
	}
	return nil
}

// revoked checks the fast-path revocation set, which every instance receives over the
// shared counters' pub/sub within milliseconds [TEN-02].
func (s *Server) revoked(id string) bool { return s.Engine.Gov.IsRevoked(id) }

func (s *Server) newRC(r *http.Request, endpoint string, f api.Format) *gateway.RequestContext {
	rid := r.Header.Get("X-Request-Id")
	if rid == "" || len(rid) > 128 {
		rid = gateway.NewRequestID()
	}
	return &gateway.RequestContext{ID: rid, Start: time.Now(), Endpoint: endpoint, Format: f, ClientIP: clientIP(r)}
}

func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, *api.Error) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, api.NewError(api.CodeInvalidRequest, "request body exceeds %d bytes", limit)
		}
		return nil, api.NewError(api.CodeInvalidRequest, "could not read body")
	}
	return b, nil
}

// ParseHints reads routing hints from x-gw-* headers and body metadata.gateway [API-08].
// Headers win over body fields.
func ParseHints(h http.Header, meta map[string]any) (gateway.Hints, *api.Error) {
	var hints gateway.Hints
	if meta != nil {
		if g, ok := meta["gateway"]; ok {
			b, _ := json.Marshal(g)
			if err := json.Unmarshal(b, &hints); err != nil {
				return hints, api.NewError(api.CodeInvalidRequest, "metadata.gateway: %v", err)
			}
		}
	}
	float := func(name string, dst **float64) *api.Error {
		if v := h.Get(name); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f < 0 {
				return api.NewError(api.CodeInvalidRequest, "header %s must be a non-negative number", name)
			}
			*dst = &f
		}
		return nil
	}
	integer := func(name string, dst **int) *api.Error {
		if v := h.Get(name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return api.NewError(api.CodeInvalidRequest, "header %s must be a non-negative integer", name)
			}
			*dst = &n
		}
		return nil
	}
	list := func(name string, dst *[]string) {
		if v := h.Get(name); v != "" {
			var out []string
			for _, p := range strings.Split(v, ",") {
				if p = strings.TrimSpace(p); p != "" {
					out = append(out, p)
				}
			}
			*dst = out
		}
	}
	str := func(name string, dst *string) {
		if v := h.Get(name); v != "" {
			*dst = v
		}
	}
	for _, e := range []*api.Error{
		float("X-Gw-Max-Cost-Usd", &hints.MaxCostUSD),
		float("X-Gw-Latency-Target-Ms", &hints.LatencyTargetMs),
		integer("X-Gw-Min-Tier", &hints.MinTier),
		integer("X-Gw-Timeout-Ms", &hints.TimeoutMs),
		integer("X-Gw-Ttft-Timeout-Ms", &hints.TTFTTimeoutMs),
		integer("X-Gw-Priority", &hints.Priority),
	} {
		if e != nil {
			return hints, e
		}
	}
	list("X-Gw-Capabilities", &hints.Capabilities)
	list("X-Gw-Allow-Providers", &hints.AllowProviders)
	list("X-Gw-Deny-Providers", &hints.DenyProviders)
	list("X-Gw-Tags", &hints.Tags)
	str("X-Gw-Cache", &hints.Cache)
	str("X-Gw-Session-Id", &hints.Session)
	str("X-Gw-End-User", &hints.EndUser)
	str("X-Gw-Sensitivity", &hints.Sensitivity)
	str("Idempotency-Key", &hints.IdempotencyKey)
	switch hints.Cache {
	case "", "bypass", "refresh", "no-store":
	default:
		return hints, api.NewError(api.CodeInvalidRequest, "cache hint must be bypass, refresh or no-store")
	}
	return hints, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, f api.Format, m *gateway.Meta, err error) {
	var ge *api.Error
	if !errors.As(err, &ge) {
		ge = api.NewError(api.CodeInternal, "internal error")
	}
	if m != nil {
		m.Headers(w.Header())
	}
	api.WriteError(w, f, ge)
}
