package hub

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/ventrolab/ai-sre/pkg/agentstore"
	"github.com/ventrolab/ai-sre/pkg/enroll"
	"github.com/ventrolab/ai-sre/pkg/tunnel"
)

var enrollmentsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "aisre_hub_enrollments_total", Help: "Agent enrollment attempts by result",
}, []string{"result"})

type enrollStore interface {
	RedeemEnrollment(ctx context.Context, tokenHash, from string) (string, error)
	RecordIssued(ctx context.Context, tokenHash, serial, name string, notAfter time.Time) error
	IssueToken(ctx context.Context, joinHash, name, tokenHash string) (string, error)
}

// Enroller serves GET /v1/ca and POST /v1/enroll on the tunnel ports. Both are reachable
// without credentials: the join token is the credential. A request with a CSR gets a
// client certificate (when Certs), one without gets an agent token (when Tokens).
type Enroller struct {
	CA       *x509.Certificate
	CAKey    crypto.Signer // nil: no certificates
	Validity time.Duration
	Certs    bool
	Tokens   bool
	Store    func() enrollStore // nil until the database is reachable
	Log      *slog.Logger

	mu       sync.Mutex
	failures map[string][]time.Time // per client IP
}

const (
	maxFailures   = 10
	failureWindow = 10 * time.Minute
)

// Register adds the endpoints to a tunnel listener; behindProxy as in tunnel.Registry.Handler.
func (e *Enroller) Register(mux *http.ServeMux, behindProxy bool) {
	var caPEM []byte
	if e.CA != nil {
		caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: e.CA.Raw})
		mux.HandleFunc("GET "+enroll.PathCA, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/x-pem-file")
			_, _ = w.Write(caPEM)
		})
	}
	mux.HandleFunc("POST "+enroll.PathEnroll, func(w http.ResponseWriter, r *http.Request) {
		e.handle(w, r, string(caPEM), tunnel.ClientIP(r, behindProxy))
	})
}

func (e *Enroller) handle(w http.ResponseWriter, r *http.Request, caPEM, ip string) {
	if e.blocked(ip) {
		enrollmentsTotal.WithLabelValues("throttled").Inc()
		http.Error(w, "too many failed attempts, try later", http.StatusTooManyRequests)
		return
	}
	var store enrollStore
	if e.Store != nil {
		store = e.Store()
	}
	if store == nil {
		http.Error(w, "agent registry not available", http.StatusServiceUnavailable)
		return
	}
	var req enroll.Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil || req.Token == "" {
		http.Error(w, "expected JSON with token and csr", http.StatusBadRequest)
		return
	}
	// Check the request before the join token is spent.
	wantCert := req.CSR != ""
	switch {
	case wantCert && (!e.Certs || e.CAKey == nil):
		http.Error(w, "this hub does not issue client certificates (tunnel auth is token): enroll without a CSR",
			http.StatusBadRequest)
		return
	case !wantCert && !e.Tokens:
		http.Error(w, "this hub does not issue agent tokens (tunnel auth is mtls): send a CSR", http.StatusBadRequest)
		return
	}
	if wantCert {
		if _, err := enroll.ParseCSR([]byte(req.CSR)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	hash := enroll.HashToken(req.Token)
	name, err := store.RedeemEnrollment(r.Context(), hash, ip)
	if errors.Is(err, agentstore.ErrInvalidToken) {
		e.fail(ip)
		enrollmentsTotal.WithLabelValues("invalid_token").Inc()
		e.Log.Warn("enrollment refused: invalid token", "remote", ip)
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if err != nil {
		e.Log.Error("enrollment: redeem token", "err", err)
		http.Error(w, "agent registry error", http.StatusServiceUnavailable)
		return
	}
	if !wantCert {
		e.issueToken(w, r, store, hash, name, ip)
		return
	}
	certPEM, serial, notAfter, err := enroll.Sign(e.CA, e.CAKey, []byte(req.CSR), name, e.Validity)
	if err != nil {
		e.Log.Error("enrollment: sign", "agent", name, "err", err)
		http.Error(w, "signing failed", http.StatusInternalServerError)
		return
	}
	if err := store.RecordIssued(r.Context(), hash, serial, name, notAfter); err != nil {
		// The certificate exists but cannot be revoked by serial until recorded: refuse it.
		e.Log.Error("enrollment: record certificate", "agent", name, "err", err)
		http.Error(w, "agent registry error", http.StatusServiceUnavailable)
		return
	}
	enrollmentsTotal.WithLabelValues("issued").Inc()
	e.Log.Info("agent enrolled", "agent", name, "auth", "mtls", "serial", serial, "not_after", notAfter, "remote", ip)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(enroll.Response{Name: name, Certificate: string(certPEM), CA: caPEM,
		NotAfter: notAfter.Format(time.RFC3339)})
}

func (e *Enroller) issueToken(w http.ResponseWriter, r *http.Request, store enrollStore, joinHash, name, ip string) {
	token, err := enroll.NewAgentToken()
	if err != nil {
		http.Error(w, "token generation failed", http.StatusInternalServerError)
		return
	}
	id, err := store.IssueToken(r.Context(), joinHash, name, enroll.HashToken(token))
	if err != nil {
		e.Log.Error("enrollment: record agent token", "agent", name, "err", err)
		http.Error(w, "agent registry error", http.StatusServiceUnavailable)
		return
	}
	enrollmentsTotal.WithLabelValues("issued").Inc()
	e.Log.Info("agent enrolled", "agent", name, "auth", "token", "token_id", id, "remote", ip)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(enroll.Response{Name: name, AgentToken: token, TokenID: id})
}

func (e *Enroller) recent(ip string, now time.Time) []time.Time {
	kept := e.failures[ip][:0]
	for _, t := range e.failures[ip] {
		if now.Sub(t) < failureWindow {
			kept = append(kept, t)
		}
	}
	e.failures[ip] = kept
	return kept
}

func (e *Enroller) blocked(ip string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.failures == nil {
		e.failures = map[string][]time.Time{}
	}
	return len(e.recent(ip, time.Now())) >= maxFailures
}

func (e *Enroller) fail(ip string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.failures == nil {
		e.failures = map[string][]time.Time{}
	}
	e.failures[ip] = append(e.recent(ip, time.Now()), time.Now())
}
