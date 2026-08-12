package intake

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gitlab.orac.local/agentic/gonk-project/pkg/rig"
)

// DefaultAdminWaitTimeout bounds POST /admin/reconcile?wait=true when
// ServerConfig.AdminWaitTimeout is left at its zero value.
const DefaultAdminWaitTimeout = 30 * time.Second

// ReconcileSummary is what `?wait=true` returns. It is a TEST/OPERATOR
// surface, so it says what happened, not merely that something happened.
type ReconcileSummary struct {
	StartedAt   time.Time      `json:"started_at"`
	FinishedAt  time.Time      `json:"finished_at"`
	Projects    int            `json:"projects"` // memberships seen
	States      map[string]int `json:"states"`   // state -> count, same vocabulary as gonk_intake_projects
	MeterPushes int            `json:"meter_pushes"`
	Dispatched  int            `json:"dispatched"`
	Errors      []string       `json:"errors,omitempty"` // never a token, never a secret
	Result      string         `json:"result"`           // ok | partial | error
}

// Pass is the seam server.go needs from the reconciler: kick an out-of-band
// pass, and block for one that starts at or after the call (HB-1, Plan 06).
// *Reconciler satisfies it; tests use a fake so server routing can be tested
// without a real GitLab/meter behind it.
type Pass interface {
	Kick()
	WaitForNextPass(ctx context.Context) (ReconcileSummary, error)
}

// ReadyChecker backs GET /readyz: bot identity resolved, first reconcile
// completed, meter reachable (cmd/gonk-intake wires the real one). Nil is
// legal -- Server then reports ready unconditionally, which is what a routing
// test that does not care about readiness wants.
type ReadyChecker interface {
	Ready(ctx context.Context) error
}

// ServerConfig wires the two listeners Task 10 builds (ADR-003.2): a PUBLIC
// one that serves exactly the webhook, and a PRIVATE one carrying everything
// operational. Nothing here decides which port each is bound to -- that is
// cmd/gonk-intake's job (GONK_LISTEN_ADDR / GONK_PRIVATE_ADDR).
type ServerConfig struct {
	// Hook serves POST /hook/gitlab on the public listener. Build it with
	// ghook.NewHandler, never a bare &ghook.Handler{} -- NewHandler is what
	// fails closed on an unsafe configuration (nil verifier/deduper/sink, a
	// zero BotUserID). A nil Hook here makes the public listener serve nothing
	// at all, which is safe but useless; cmd/gonk-intake never leaves it nil.
	Hook http.Handler

	// Reg is where /metrics reads from. Nil disables the endpoint (404) rather
	// than panicking -- useful for a routing-only test.
	Reg *prometheus.Registry

	// Reconcile backs POST /admin/reconcile. Nil disables the endpoint (503)
	// rather than panicking.
	Reconcile Pass

	// Ready backs GET /readyz. Nil -> always 200.
	Ready ReadyChecker

	// AdminWaitTimeout bounds `?wait=true`. Zero -> DefaultAdminWaitTimeout.
	AdminWaitTimeout time.Duration

	// Rig serves the per-session CHECKOUT on the private listener: gonk-gate
	// registers a grant at the decision point and the agent pod fetches its tree
	// (pkg/rig, gonk-msz). It is how a pod gets a repository while holding no
	// forge credential -- the bytes come from here, not from the forge.
	//
	// Nil leaves the routes unmounted (404), which is the correct degraded state:
	// sessions then run on the no-checkout prompt, which says so.
	Rig *rig.Handler
}

// Server builds the two http.Handlers Task 10 specifies. It holds no
// listening sockets itself -- cmd/gonk-intake wraps Public()/Private() in its
// own *http.Server so it can set ReadHeaderTimeout and control the bind
// addresses.
type Server struct {
	cfg  ServerConfig
	pub  *http.ServeMux
	priv *http.ServeMux
}

func NewServer(cfg ServerConfig) *Server {
	s := &Server{cfg: cfg}

	s.pub = http.NewServeMux()
	s.pub.HandleFunc("/hook/gitlab", func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Hook == nil {
			http.NotFound(w, r)
			return
		}
		s.cfg.Hook.ServeHTTP(w, r)
	})

	s.priv = http.NewServeMux()
	s.priv.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	s.priv.HandleFunc("/readyz", s.readyz)
	if cfg.Reg != nil {
		s.priv.Handle("/metrics", promhttp.HandlerFor(cfg.Reg, promhttp.HandlerOpts{}))
	}
	s.priv.HandleFunc("/admin/reconcile", s.adminReconcile)
	// PRIVATE LISTENER ONLY, like /admin/reconcile: the NetworkPolicy is what
	// keeps registration to the controller and delivery to agent pods. Putting
	// this on the public listener would expose repository bytes to the ingress.
	if cfg.Rig != nil {
		cfg.Rig.Register(s.priv)
	}

	return s
}

// Public serves ONLY POST /hook/gitlab. This is the only thing that may sit
// behind the ingress (spec 9, ADR-003.2).
func (s *Server) Public() http.Handler { return s.pub }

// Private serves /healthz, /readyz, /metrics, and POST /admin/reconcile. It is
// cluster-internal only: never route it through the ingress.
func (s *Server) Private() http.Handler { return s.priv }

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.Ready == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := s.cfg.Ready.Ready(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// adminReconcile is POST /admin/reconcile (spec 5.2). It is unauthenticated
// and lives ONLY on the private listener: the NetworkPolicy (Plan 05) is what
// keeps it to Prometheus and the harness. It fires no order that /decide would
// not gate, but it is free work, so it must never be reachable from the
// ingress.
func (s *Server) adminReconcile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.Reconcile == nil {
		http.Error(w, "reconcile not wired", http.StatusServiceUnavailable)
		return
	}

	if r.URL.Query().Get("wait") != "true" {
		s.cfg.Reconcile.Kick()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]bool{"kicked": true})
		return
	}

	timeout := s.cfg.AdminWaitTimeout
	if timeout <= 0 {
		timeout = DefaultAdminWaitTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	rs, err := s.cfg.Reconcile.WaitForNextPass(ctx)
	if err != nil {
		http.Error(w, "reconcile: "+err.Error(), http.StatusGatewayTimeout)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(rs)
}
