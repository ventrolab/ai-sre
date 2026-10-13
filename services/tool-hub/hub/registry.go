package hub

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/ventrolab/ai-sre/pkg/agentstore"
	"github.com/ventrolab/ai-sre/pkg/tunnel"
)

// registrySync mirrors tunnel sessions into the persistent agent registry. All writes go
// through one goroutine, so a quick disconnect/reconnect is applied in order, and the
// tunnel never waits for the database.
type registrySync struct {
	reg *tunnel.Registry
	log *slog.Logger
	ops chan func(context.Context, *agentstore.Store) error
	// Set once the database is reachable; enrollment answers 503 until then.
	store atomic.Pointer[agentstore.Store]
	// Revoked certificate serials, refreshed every 30 s. Until the first load nothing is
	// known to be revoked; after that the last good list is kept while the database is down.
	revoked atomic.Pointer[map[string]bool]
	// Active agent tokens by hash, refreshed with the serials: the fallback of token checks
	// while the database is down, and the list sessions are checked against (revocation).
	tokens atomic.Pointer[map[string]agentstore.TokenRef]
}

// authToken resolves an agent token: the database first, the last loaded list while the
// database is unreachable.
func (rs *registrySync) authToken(ctx context.Context, tokenHash string) (string, string, error) {
	if st := rs.store.Load(); st != nil {
		name, id, err := st.AgentForToken(ctx, tokenHash)
		if err == nil {
			return name, id, nil
		}
		if errors.Is(err, agentstore.ErrUnknownToken) {
			return "", "", tunnel.ErrUnknownToken
		}
		rs.log.Warn("agent registry: token lookup failed, using the cached list", "err", err)
	}
	if m := rs.tokens.Load(); m != nil {
		if ref, ok := (*m)[tokenHash]; ok {
			return ref.Name, ref.ID, nil
		}
		return "", "", tunnel.ErrUnknownToken
	}
	return "", "", errors.New("agent registry not loaded yet")
}

func (rs *registrySync) isRevoked(serial string) bool {
	m := rs.revoked.Load()
	return m != nil && (*m)[serial]
}

// refreshRevoked reloads the revoked serials and drops sessions that use one of them.
func (rs *registrySync) refreshRevoked(ctx context.Context, st *agentstore.Store) {
	opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	m, err := st.RevokedSerials(opCtx)
	if err != nil {
		rs.log.Error("agent registry: load revoked certificates", "err", err)
		return
	}
	rs.revoked.Store(&m)
	tokens, err := st.ActiveTokens(opCtx)
	if err != nil {
		rs.log.Error("agent registry: load agent tokens", "err", err)
	} else {
		rs.tokens.Store(&tokens)
	}
	active := map[string]bool{}
	for _, ref := range tokens {
		active[ref.ID] = true
	}
	for _, a := range rs.reg.List() {
		switch {
		case a.Auth == "mtls" && m[a.CertSerial]:
			rs.log.Warn("closing session with a revoked certificate", "agent", a.Name, "serial", a.CertSerial)
			a.Close("certificate revoked")
		case a.Auth == "token" && a.TokenID != staticTokenID && err == nil && !active[a.TokenID]:
			rs.log.Warn("closing session with a revoked agent token", "agent", a.Name, "token_id", a.TokenID)
			a.Close("agent token revoked")
		}
	}
}

func session(a *tunnel.Agent) agentstore.Session {
	return agentstore.Session{
		Name: a.Name, Kind: a.Kind, Labels: a.Labels, Tools: a.Tools, Version: a.Version,
		Remote: a.Remote, Auth: a.Auth, CertSerial: a.CertSerial, CertNotAfter: a.CertNotAfter, TokenID: a.TokenID,
		ConnectedAt: a.ConnectedAt,
	}
}

func startRegistrySync(ctx context.Context, url string, reg *tunnel.Registry, log *slog.Logger) *registrySync {
	rs := &registrySync{reg: reg, log: log, ops: make(chan func(context.Context, *agentstore.Store) error, 256)}
	reg.Revoked = rs.isRevoked
	reg.OnConnect = func(a *tunnel.Agent) {
		s := session(a)
		rs.enqueue(func(ctx context.Context, st *agentstore.Store) error { return st.Connected(ctx, s) })
	}
	reg.OnDisconnect = func(a *tunnel.Agent) {
		name := a.Name
		rs.enqueue(func(ctx context.Context, st *agentstore.Store) error { return st.Disconnected(ctx, name) })
	}
	go rs.run(ctx, url)
	return rs
}

func (rs *registrySync) enqueue(op func(context.Context, *agentstore.Store) error) {
	select {
	case rs.ops <- op:
	default:
		rs.log.Error("agent registry queue full, update dropped")
	}
}

func (rs *registrySync) run(ctx context.Context, url string) {
	var st *agentstore.Store
	for st == nil {
		var err error
		if st, err = agentstore.Open(ctx, url); err != nil {
			rs.log.Warn("agent registry: database not ready", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
	defer st.Close()
	rs.store.Store(st)
	rs.log.Info("agent registry: connected to the database")
	rs.refreshRevoked(ctx, st)
	// Agents that connected before the database was reachable.
	for _, a := range rs.reg.List() {
		if err := st.Connected(ctx, session(a)); err != nil {
			rs.log.Error("agent registry: record connect", "agent", a.Name, "err", err)
		}
	}
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	revokedTick := time.NewTicker(30 * time.Second)
	defer revokedTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-revokedTick.C:
			rs.refreshRevoked(ctx, st)
		case op := <-rs.ops:
			opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := op(opCtx, st); err != nil {
				rs.log.Error("agent registry update failed", "err", err)
			}
			cancel()
		case <-tick.C:
			var names []string
			for _, a := range rs.reg.List() {
				names = append(names, a.Name)
			}
			opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := st.Heartbeat(opCtx, names); err != nil {
				rs.log.Error("agent registry heartbeat failed", "err", err)
			}
			cancel()
		}
	}
}
