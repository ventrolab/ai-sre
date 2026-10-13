// Action Guard: the single enforcement point for state-changing operations.
package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	decisionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "aisre_guard_decisions_total", Help: "Evaluation decisions",
	}, []string{"decision", "playbook"})
	reloadsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "aisre_guard_catalog_reloads_total", Help: "Catalog reload attempts",
	}, []string{"result"})
	catalogValid = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "aisre_guard_catalog_valid", Help: "1 if the last catalog load succeeded",
	})
	executionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "aisre_guard_executions_total", Help: "Executed actions by outcome",
	}, []string{"status", "playbook"})
	catalogPlaybooks = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "aisre_guard_catalog_playbooks", Help: "Playbooks in the active catalog",
	})
)

type Server struct {
	policy  Policy
	dir     string
	catalog atomic.Pointer[Catalog]
	lastErr atomic.Pointer[string]
	limiter Limiter
	notify  func(title, text string)
	log     *slog.Logger
	exec    *Executor
	edition string
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func loadPolicy() (Policy, error) {
	pol := Policy{Mode: Mode(env("SRE_MODE", "readonly"))}
	if pol.Mode != ModeReadonly && pol.Mode != ModeAutofix {
		return pol, fmt.Errorf("SRE_MODE must be readonly or autofix, got %q", pol.Mode)
	}
	risk, err := ParseRisk(env("AUTOFIX_MAX_RISK", "low"))
	if err != nil {
		return pol, fmt.Errorf("AUTOFIX_MAX_RISK: %w", err)
	}
	if risk == RiskHigh {
		return pol, errors.New("AUTOFIX_MAX_RISK cannot be high: high-risk actions always need approval")
	}
	pol.MaxRisk = risk
	n, err := strconv.Atoi(env("AUTOFIX_MAX_ACTIONS_PER_HOUR", "5"))
	if err != nil || n < 1 {
		return pol, errors.New("AUTOFIX_MAX_ACTIONS_PER_HOUR must be a positive integer")
	}
	pol.MaxActionsPerTargetHour = n
	return pol, nil
}

// reload swaps the catalog only if the whole new version is valid.
func (s *Server) reload() {
	version, err := CatalogVersion(s.dir)
	if err != nil {
		s.log.Error("catalog read failed", "err", err)
		return
	}
	if cur := s.catalog.Load(); cur != nil && cur.Version == version {
		return
	}
	cat, err := LoadCatalog(s.dir)
	if err != nil {
		msg := err.Error()
		if prev := s.lastErr.Load(); prev != nil && *prev == msg {
			return
		}
		s.lastErr.Store(&msg)
		reloadsTotal.WithLabelValues("rejected").Inc()
		catalogValid.Set(0)
		active := "none"
		if cur := s.catalog.Load(); cur != nil {
			active = cur.Version
		}
		s.log.Error("catalog rejected, keeping the active version", "active", active, "err", msg)
		s.notify("Playbook catalog rejected",
			fmt.Sprintf("The new catalog is invalid; the active version %s stays in use.\n\n%s", active, msg))
		return
	}
	s.catalog.Store(cat)
	s.lastErr.Store(nil)
	reloadsTotal.WithLabelValues("loaded").Inc()
	catalogValid.Set(1)
	catalogPlaybooks.Set(float64(len(cat.Playbooks)))
	s.log.Info("catalog loaded", "version", cat.Version, "playbooks", len(cat.Playbooks))
}

func (s *Server) watch(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if hub := os.Getenv("TOOL_HUB_URL"); hub != "" {
				poll, _ := time.ParseDuration(env("VERIFY_POLL_INTERVAL", "15s"))
				s.exec = &Executor{
					Hub:       &HubClient{URL: hub, Token: os.Getenv("TOOL_HUB_ACTION_TOKEN"), HTTP: &http.Client{Timeout: 2 * time.Minute}},
					PollEvery: poll, MaxTimeout: 20 * time.Minute,
				}
			}
			s.reload()
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) requireCatalog(w http.ResponseWriter) *Catalog {
	cat := s.catalog.Load()
	if cat == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no valid playbook catalog loaded"})
	}
	return cat
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	out := map[string]any{
		"edition":                      s.edition,
		"mode":                         s.policy.Mode,
		"autofix_max_risk":             s.policy.decider().MaxAutoRisk(s.policy).String(),
		"autofix_max_actions_per_hour": s.policy.MaxActionsPerTargetHour,
		"catalog_valid":                s.lastErr.Load() == nil,
	}
	if cat := s.catalog.Load(); cat != nil {
		out["catalog_version"] = cat.Version
		out["catalog_loaded_at"] = cat.LoadedAt
		out["playbooks"] = len(cat.Playbooks)
	}
	if e := s.lastErr.Load(); e != nil {
		out["catalog_error"] = *e
	}
	writeJSON(w, http.StatusOK, out)
}

type playbookView struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Risk        string `json:"risk"`
	Enabled     bool   `json:"enabled"`
	Primitive   string `json:"primitive"`
	Match       Match  `json:"match"`
}

func (s *Server) handlePlaybooks(w http.ResponseWriter, r *http.Request) {
	cat := s.requireCatalog(w)
	if cat == nil {
		return
	}
	q := r.URL.Query()
	list := cat.Sorted()
	if q.Get("alertname") != "" || q.Get("target_kind") != "" {
		list = Matching(cat, q.Get("alertname"), q.Get("target_kind"))
	}
	views := make([]playbookView, 0, len(list))
	for _, pb := range list {
		views = append(views, playbookView{
			Name: pb.Name(), Description: pb.Spec.Description, Risk: pb.Risk().String(),
			Enabled: pb.Enabled(), Primitive: pb.Spec.Action.Primitive, Match: pb.Spec.Match,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"catalog_version": cat.Version, "playbooks": views})
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request) (*EvalRequest, *Catalog) {
	cat := s.requireCatalog(w)
	if cat == nil {
		return nil, nil
	}
	var req EvalRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return nil, nil
	}
	return &req, cat
}

func (s *Server) handleEvaluate(w http.ResponseWriter, r *http.Request) {
	req, cat := s.decode(w, r)
	if req == nil {
		return
	}
	resp := Evaluate(cat, s.policy, s.limiter, *req)
	decisionsTotal.WithLabelValues(string(resp.Decision), req.Playbook).Inc()
	s.log.Info("evaluate", "incident", req.IncidentID, "playbook", req.Playbook,
		"target", req.Target.Key(), "decision", resp.Decision, "reason", resp.Reason, "catalog", cat.Version)
	writeJSON(w, http.StatusOK, resp)
}

// handleExecute re-evaluates the request right before execution: the mode or
// the catalog may have changed since the proposal was made.
func (s *Server) handleExecute(w http.ResponseWriter, r *http.Request) {
	req, cat := s.decode(w, r)
	if req == nil {
		return
	}
	resp := Evaluate(cat, s.policy, s.limiter, *req)
	switch {
	case resp.Decision == DecisionExecute:
	case resp.Decision == DecisionApproval && req.ApprovedBy != "":
		resp.Reason = "approved by " + req.ApprovedBy
	default:
		s.log.Warn("execute refused", "incident", req.IncidentID, "playbook", req.Playbook,
			"decision", resp.Decision, "reason", resp.Reason)
		writeJSON(w, http.StatusForbidden, map[string]any{"status": "refused", "evaluation": resp})
		return
	}
	if s.exec == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"status": "not_implemented", "reason": "TOOL_HUB_URL is not configured", "evaluation": resp,
		})
		return
	}
	target := TargetName(req.Target)
	// Counts against the rate limit whether or not it works.
	s.limiter.Record(req.Target.Key())
	s.log.Info("execute", "incident", req.IncidentID, "playbook", req.Playbook, "target", target, "approved_by", req.ApprovedBy)
	result := s.exec.Run(r.Context(), req.IncidentID, resp, target)
	executionsTotal.WithLabelValues(result.Status, req.Playbook).Inc()
	s.log.Info("execute result", "incident", req.IncidentID, "playbook", req.Playbook, "status", result.Status, "reason", result.Reason)
	writeJSON(w, http.StatusOK, result)
}

// connectNATS returns nil when NATS_URL is unset. The connection retries in the background,
// so the guard starts even when NATS comes up later.
func connectNATS(log *slog.Logger) *nats.Conn {
	url := os.Getenv("NATS_URL")
	if url == "" {
		log.Warn("NATS_URL is not set: no catalog notifications, rate limits in memory only")
		return nil
	}
	nc, err := nats.Connect(url, nats.Name("action-guard"), nats.MaxReconnects(-1), nats.RetryOnFailedConnect(true))
	if err != nil {
		log.Error("nats connect failed: no catalog notifications, rate limits in memory only", "err", err)
		return nil
	}
	return nc
}

func newLimiter(nc *nats.Conn, log *slog.Logger) Limiter {
	if nc == nil {
		return NewRateLimiter()
	}
	return NewKVLimiter(func() (kvStore, error) { return openRateLimitBucket(nc) }, log)
}

func newNotifier(nc *nats.Conn, log *slog.Logger) func(title, text string) {
	if nc == nil {
		return func(string, string) {}
	}
	return func(title, text string) {
		msg, _ := json.Marshal(map[string]any{
			"id":         fmt.Sprintf("ntf_guard_%d", time.Now().UnixNano()),
			"type":       "system",
			"title":      title,
			"text":       text,
			"severity":   "warning",
			"created_at": time.Now().UTC(),
		})
		if err := nc.Publish("notify.chat", msg); err != nil {
			log.Error("notify failed", "err", err)
		}
	}
}

// Options customize the guard; the zero value is the community edition.
type Options struct {
	// Decider makes the final decision for a valid, rendered playbook; nil: CommunityDecider.
	Decider Decider
	// Edition is reported in /v1/status ("community" when empty).
	Edition string
}

// Run starts the Action Guard HTTP server and blocks until SIGINT/SIGTERM.
func Run(opts Options) error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	pol, err := loadPolicy()
	if err != nil {
		log.Error("invalid configuration", "err", err)
		return err
	}
	pol.Decider = opts.Decider
	if opts.Edition == "" {
		opts.Edition = "community"
	}
	nc := connectNATS(log)
	s := &Server{
		edition: opts.Edition,
		policy:  pol,
		dir:     env("PLAYBOOKS_PATH", "/etc/ai-sre/playbooks"),
		limiter: newLimiter(nc, log),
		notify:  newNotifier(nc, log),
		log:     log,
	}
	if hub := os.Getenv("TOOL_HUB_URL"); hub != "" {
		poll, _ := time.ParseDuration(env("VERIFY_POLL_INTERVAL", "15s"))
		s.exec = &Executor{
			Hub:       &HubClient{URL: hub, Token: os.Getenv("TOOL_HUB_ACTION_TOKEN"), HTTP: &http.Client{Timeout: 2 * time.Minute}},
			PollEvery: poll, MaxTimeout: 20 * time.Minute,
		}
	}
	s.reload()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go s.watch(ctx, 10*time.Second)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("GET /v1/playbooks", s.handlePlaybooks)
	mux.HandleFunc("POST /v1/evaluate", s.handleEvaluate)
	mux.HandleFunc("POST /v1/execute", s.handleExecute)
	mux.Handle("GET /metrics", promhttp.Handler())

	// No WriteTimeout: /v1/execute waits for verification (up to 20 minutes).
	srv := &http.Server{Addr: env("LISTEN_ADDR", ":8080"), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("action-guard listening", "addr", srv.Addr, "edition", opts.Edition, "mode", pol.Mode,
		"max_risk", pol.MaxRisk.String())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server failed", "err", err)
		return err
	}
	return nil
}
