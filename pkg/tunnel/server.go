package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Agent is one connected agent.
type Agent struct {
	Name        string            `json:"name"`
	Kind        string            `json:"kind"`
	Labels      map[string]string `json:"labels,omitempty"`
	Tools       []string          `json:"tools"`
	Version     string            `json:"version,omitempty"`
	ConnectedAt time.Time         `json:"connected_at"`
	Remote      string            `json:"remote"`
	// How the agent authenticated: "token" or "mtls".
	Auth string `json:"auth"`
	// Client certificate the agent authenticated with (mtls).
	CertSerial   string    `json:"cert_serial,omitempty"`
	CertNotAfter time.Time `json:"cert_not_after,omitempty"`
	// Public id of the agent token it authenticated with (token).
	TokenID string `json:"token_id,omitempty"`

	conn    *websocket.Conn
	writeMu sync.Mutex
	pending sync.Map // id -> chan Response
	seq     atomic.Uint64
}

func (a *Agent) HasTool(tool string) bool {
	for _, t := range a.Tools {
		if t == tool {
			return true
		}
	}
	return false
}

func (a *Agent) write(ctx context.Context, f Frame) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	return a.conn.Write(ctx, websocket.MessageText, data)
}

// Call sends a request to the agent and waits for its response.
func (a *Agent) Call(ctx context.Context, tool string, args json.RawMessage, timeout time.Duration) (string, error) {
	id := fmt.Sprintf("%d", a.seq.Add(1))
	ch := make(chan Response, 1)
	a.pending.Store(id, ch)
	defer a.pending.Delete(id)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req := Request{ID: id, Tool: tool, Args: args, TimeoutMS: int(timeout.Milliseconds())}
	if err := a.write(ctx, Frame{Type: FrameRequest, Request: &req}); err != nil {
		return "", fmt.Errorf("send to agent %s: %w", a.Name, err)
	}
	select {
	case resp := <-ch:
		if !resp.OK {
			return "", errors.New(resp.Error)
		}
		return resp.Result, nil
	case <-ctx.Done():
		return "", fmt.Errorf("agent %s did not answer: %w", a.Name, ctx.Err())
	}
}

// Registry keeps the connected agents by name.
type Registry struct {
	mu       sync.RWMutex
	agents   map[string]*Agent
	log      *slog.Logger
	OnChange func(connected int)
	// OnConnect and OnDisconnect report agent sessions, e.g. to a persistent registry.
	// OnDisconnect is not called when a newer connection of the same agent replaced it.
	OnConnect    func(*Agent)
	OnDisconnect func(*Agent)
	// Revoked reports revoked client certificate serials (hex); such agents are refused.
	Revoked func(serial string) bool
	// AllowCert accepts agents by a client certificate verified by the TLS layer; its
	// CommonName names the agent.
	AllowCert bool
	// TokenAuth resolves a bearer agent token to the agent name and the token's public id.
	// It returns ErrUnknownToken for unknown or revoked tokens. nil disables tokens.
	TokenAuth func(ctx context.Context, token string) (name, tokenID string, err error)
	// MaxAgents connected at once (0: unlimited); a new agent beyond it is refused, a
	// reconnecting one is not.
	MaxAgents int
}

// full reports whether a new agent of this name would exceed MaxAgents.
func (r *Registry) full(name string) bool {
	if r.MaxAgents <= 0 {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, known := r.agents[name]
	return !known && len(r.agents) >= r.MaxAgents
}

// ErrUnknownToken is returned by TokenAuth for tokens that do not authenticate an agent.
var ErrUnknownToken = errors.New("unknown or revoked agent token")

// Close ends the agent's session, e.g. after its certificate was revoked.
func (a *Agent) Close(reason string) {
	a.conn.Close(websocket.StatusPolicyViolation, reason)
}

func NewRegistry(log *slog.Logger) *Registry {
	return &Registry{agents: map[string]*Agent{}, log: log, AllowCert: true}
}

func (r *Registry) Get(name string) *Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.agents[name]
}

func (r *Registry) List() []*Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Agent, 0, len(r.agents))
	for _, a := range r.agents {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Registry) set(a *Agent) (replaced *Agent) {
	r.mu.Lock()
	replaced = r.agents[a.Name]
	r.agents[a.Name] = a
	n := len(r.agents)
	r.mu.Unlock()
	if r.OnChange != nil {
		r.OnChange(n)
	}
	return replaced
}

func (r *Registry) remove(a *Agent) (removed bool) {
	r.mu.Lock()
	if r.agents[a.Name] == a {
		delete(r.agents, a.Name)
		removed = true
	}
	n := len(r.agents)
	r.mu.Unlock()
	if r.OnChange != nil {
		r.OnChange(n)
	}
	return removed
}

// identity is who an agent connection authenticated as.
type identity struct {
	name, auth, serial, tokenID string
	notAfter                    time.Time
}

// ClientIP is the address of the client; behind a reverse proxy (behindProxy) it is taken
// from X-Real-IP or the first X-Forwarded-For entry. Used for logs and throttling only.
func ClientIP(req *http.Request, behindProxy bool) string {
	if behindProxy {
		if ip := strings.TrimSpace(req.Header.Get("X-Real-IP")); ip != "" {
			return ip
		}
		if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}

// authenticate checks the client certificate (when the TLS layer verified one) or the
// bearer agent token. It returns an HTTP status and message on failure.
func (r *Registry) authenticate(req *http.Request) (identity, int, string) {
	if req.TLS != nil && len(req.TLS.PeerCertificates) > 0 && r.AllowCert {
		cert := req.TLS.PeerCertificates[0]
		id := identity{name: cert.Subject.CommonName, auth: "mtls", serial: cert.SerialNumber.Text(16),
			notAfter: cert.NotAfter}
		if r.Revoked != nil && r.Revoked(id.serial) {
			r.log.Warn("tunnel: revoked certificate refused", "agent", id.name, "serial", id.serial)
			return id, http.StatusForbidden, "certificate revoked"
		}
		if id.name == "" {
			return id, http.StatusForbidden, "client certificate without CommonName"
		}
		return id, 0, ""
	}
	if token, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer "); ok && r.TokenAuth != nil {
		ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
		defer cancel()
		name, tokenID, err := r.TokenAuth(ctx, strings.TrimSpace(token))
		switch {
		case errors.Is(err, ErrUnknownToken):
			return identity{}, http.StatusUnauthorized, "invalid agent token"
		case err != nil:
			r.log.Error("tunnel: token check failed", "err", err)
			return identity{}, http.StatusServiceUnavailable, "agent registry not available"
		}
		return identity{name: name, auth: "token", tokenID: tokenID}, 0, ""
	}
	switch {
	case r.TokenAuth != nil && r.AllowCert:
		return identity{}, http.StatusUnauthorized, "agent token or client certificate required"
	case r.TokenAuth != nil:
		return identity{}, http.StatusUnauthorized, "agent token required"
	default:
		return identity{}, http.StatusUnauthorized, "client certificate required"
	}
}

// Handler accepts agent connections, authenticated by a client certificate (verified by
// the TLS layer) or a bearer agent token. behindProxy: the listener sits behind a reverse
// proxy (an ingress), so the client address comes from its headers.
func (r *Registry) Handler(behindProxy bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		remote := ClientIP(req, behindProxy)
		id, status, msg := r.authenticate(req)
		if status != 0 {
			if status == http.StatusUnauthorized {
				r.log.Warn("tunnel: connection refused", "remote", remote, "reason", msg)
			}
			http.Error(w, msg, status)
			return
		}
		if r.full(id.name) {
			r.log.Warn("tunnel: agent limit reached", "agent", id.name, "max_agents", r.MaxAgents)
			http.Error(w, fmt.Sprintf("agent limit reached (%d agents); the enterprise edition serves more", r.MaxAgents),
				http.StatusForbidden)
			return
		}
		conn, err := websocket.Accept(w, req, nil)
		if err != nil {
			r.log.Warn("tunnel accept failed", "agent", id.name, "err", err)
			return
		}
		conn.SetReadLimit(8 << 20)
		r.serve(req.Context(), conn, id, remote)
	})
}

func (r *Registry) serve(ctx context.Context, conn *websocket.Conn, id identity, remote string) {
	name := id.name
	defer conn.CloseNow()
	helloCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, data, err := conn.Read(helloCtx)
	cancel()
	var f Frame
	if err != nil || json.Unmarshal(data, &f) != nil || f.Type != FrameHello || f.Hello == nil {
		r.log.Warn("tunnel: expected hello", "agent", name, "err", err)
		conn.Close(websocket.StatusPolicyViolation, "expected hello")
		return
	}
	a := &Agent{
		Name: name, Kind: f.Hello.Kind, Labels: f.Hello.Labels, Tools: f.Hello.Tools,
		Version: f.Hello.Version, ConnectedAt: time.Now().UTC(), Remote: remote, conn: conn,
		Auth: id.auth, CertSerial: id.serial, TokenID: id.tokenID,
	}
	if !id.notAfter.IsZero() {
		a.CertNotAfter = id.notAfter.UTC()
	}
	if old := r.set(a); old != nil {
		old.conn.Close(websocket.StatusGoingAway, "replaced by a new connection")
	}
	r.log.Info("agent connected", "agent", name, "kind", a.Kind, "auth", a.Auth, "tools", len(a.Tools), "remote", remote)
	if r.OnConnect != nil {
		r.OnConnect(a)
	}
	defer func() {
		if r.remove(a) && r.OnDisconnect != nil {
			r.OnDisconnect(a)
		}
		r.log.Info("agent disconnected", "agent", name)
	}()

	// Keepalive: a silent network drop is noticed within a minute.
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				err := conn.Ping(pctx)
				cancel()
				if err != nil {
					conn.Close(websocket.StatusGoingAway, "ping timeout")
					return
				}
			}
		}
	}()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var f Frame
		if json.Unmarshal(data, &f) != nil || f.Type != FrameResponse || f.Response == nil {
			continue
		}
		if ch, ok := a.pending.Load(f.Response.ID); ok {
			ch.(chan Response) <- *f.Response
		}
	}
}
