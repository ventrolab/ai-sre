// Tool Hub: routes read-only tool calls from HolmesGPT (MCP) to agents that keep an
// outbound WebSocket tunnel to the hub, authenticated by an agent token or mutual TLS.
package hub

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ventrolab/ai-sre/pkg/enroll"
	"github.com/ventrolab/ai-sre/pkg/tunnel"
)

// CommunityMaxAgents is how many agents the community edition serves at once.
const CommunityMaxAgents = 5

// Options customize the hub; the zero value is the community edition.
type Options struct {
	Version string
	Edition string // "community" when empty
	// MaxAgents connected at once; 0 means CommunityMaxAgents, negative means unlimited.
	MaxAgents int
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func bearer(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if token == "" || subtle.ConstantTimeCompare([]byte(got), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// tunnelTLS reloads the server certificate on every handshake (cert-manager or chart
// rotation) and verifies client certificates against the CA. A certificate is optional at
// the TLS layer: enrollment runs before the agent has one, and token agents never have
// one; the tunnel handler refuses connections without a certificate or a token.
func tunnelTLS(certFile, keyFile, caFile string) (*tls.Config, error) {
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("no certificates in %s", caFile)
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs:  pool,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			cert, err := tls.LoadX509KeyPair(certFile, keyFile)
			return &cert, err
		},
	}, nil
}

const staticTokenID = "static"

// loadStaticTokens reads agent tokens deployed with the hub (the chart's local agent):
// one file per agent, named after it, holding the token. Returns token hash -> name.
func loadStaticTokens(dir string) (map[string]string, error) {
	out := map[string]string{}
	if dir == "" {
		return out, nil
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		// Secret volumes hold ..data and ..<timestamp> links next to the keys.
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		if token := strings.TrimSpace(string(data)); token != "" {
			out[enroll.HashToken(token)] = e.Name()
		}
	}
	return out, nil
}

func jsonHandler(fn func(*http.Request) any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fn(r))
	})
}

// Run starts the Tool Hub and blocks until SIGINT/SIGTERM.
func Run(opts Options) {
	version := opts.Version
	if opts.Edition == "" {
		opts.Edition = "community"
	}
	if opts.MaxAgents == 0 {
		opts.MaxAgents = CommunityMaxAgents
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	token := os.Getenv("TOOL_HUB_TOKEN")
	actionToken := os.Getenv("TOOL_HUB_ACTION_TOKEN")
	if token == "" || actionToken == "" || token == actionToken {
		log.Error("TOOL_HUB_TOKEN and a different TOOL_HUB_ACTION_TOKEN are required")
		os.Exit(1)
	}
	timeout, _ := time.ParseDuration(env("CALL_TIMEOUT", "60s"))
	maxOutput, _ := strconv.Atoi(env("MAX_OUTPUT_BYTES", "30000"))

	// token: agent tokens only; mtls: client certificates only; both.
	auth := env("TUNNEL_AUTH", "token")
	if auth != "token" && auth != "mtls" && auth != "both" {
		log.Error("TUNNEL_AUTH must be token, mtls or both", "got", auth)
		os.Exit(1)
	}
	allowTokens, allowCerts := auth != "mtls", auth != "token"
	static, err := loadStaticTokens(os.Getenv("STATIC_TOKENS_DIR"))
	if err != nil {
		log.Error("static agent tokens", "err", err)
		os.Exit(1)
	}

	reg := tunnel.NewRegistry(log)
	reg.AllowCert = allowCerts
	reg.MaxAgents = max(opts.MaxAgents, 0)
	reg.OnChange = func(n int) { agentsConnected.Set(float64(n)) }
	hub := &Hub{reg: reg, log: log, timeout: timeout, maxOutput: maxOutput}
	server := hub.MCPServer(version)

	api := http.NewServeMux()
	api.Handle("/mcp", bearer(token, mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)))
	api.Handle("GET /v1/targets", bearer(token, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"targets": reg.List()})
	})))
	// Write path and verification queries: Action Guard only.
	api.Handle("POST /v1/actions", bearer(actionToken, jsonHandler(func(r *http.Request) any {
		var req ActionRequest
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&req); err != nil {
			return callResult{Error: err.Error()}
		}
		return hub.Action(r.Context(), req)
	})))
	api.Handle("POST /v1/query", bearer(actionToken, jsonHandler(func(r *http.Request) any {
		var req QueryRequest
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&req); err != nil {
			return callResult{Error: err.Error()}
		}
		return hub.Query(r.Context(), req)
	})))
	api.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	api.Handle("GET /metrics", promhttp.Handler())

	tlsCfg, err := tunnelTLS(env("TUNNEL_CERT", "/etc/tool-hub/tls/tls.crt"),
		env("TUNNEL_KEY", "/etc/tool-hub/tls/tls.key"), env("TUNNEL_CA", "/etc/tool-hub/ca/ca.crt"))
	if err != nil {
		log.Error("tunnel tls", "err", err)
		os.Exit(1)
	}
	// The TLS port serves agents inside the cluster and mutual TLS; the plain port (token
	// auth only) sits behind an ingress that terminates TLS.
	tunnelMux := http.NewServeMux()
	tunnelMux.Handle(tunnel.Path, reg.Handler(false))
	proxiedMux := http.NewServeMux()
	proxiedMux.Handle(tunnel.Path, reg.Handler(true))

	servers := []*http.Server{
		{Addr: env("LISTEN_ADDR", ":8080"), Handler: api, ReadHeaderTimeout: 10 * time.Second},
		{Addr: env("TUNNEL_ADDR", ":8443"), Handler: tunnelMux, TLSConfig: tlsCfg, ReadHeaderTimeout: 10 * time.Second},
	}
	if allowTokens {
		servers = append(servers, &http.Server{Addr: env("TUNNEL_HTTP_ADDR", ":8081"), Handler: proxiedMux,
			ReadHeaderTimeout: 10 * time.Second})
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var rs *registrySync
	if url := os.Getenv("DATABASE_URL"); url != "" {
		rs = startRegistrySync(ctx, url, reg, log)
	} else {
		log.Warn("DATABASE_URL is not set: agents are not recorded, the console shows connected agents only")
	}
	if allowTokens {
		reg.TokenAuth = func(ctx context.Context, token string) (string, string, error) {
			hash := enroll.HashToken(token)
			if name, ok := static[hash]; ok {
				return name, staticTokenID, nil
			}
			if rs == nil {
				return "", "", tunnel.ErrUnknownToken
			}
			return rs.authToken(ctx, hash)
		}
	}
	// Enrollment needs the registry (join tokens); certificates also need the CA key.
	if rs != nil {
		days, _ := strconv.Atoi(env("ENROLL_CERT_DAYS", "365"))
		e := &Enroller{Validity: time.Duration(days) * 24 * time.Hour, Log: log, Tokens: allowTokens,
			Store: func() enrollStore {
				if st := rs.store.Load(); st != nil {
					return st
				}
				return nil
			}}
		if caKey := env("CA_KEY", "/etc/tool-hub/ca/ca.key"); fileExists(caKey) {
			if e.CA, e.CAKey, err = enroll.LoadCA(env("CA_CERT", "/etc/tool-hub/ca/ca.crt"), caKey); err != nil {
				log.Error("certificate enrollment disabled: cannot load the CA", "err", err)
				e.CA, e.CAKey = nil, nil
			}
		}
		e.Certs = allowCerts && e.CAKey != nil
		if e.Tokens || e.Certs {
			e.Register(tunnelMux, false)
			if allowTokens {
				e.Register(proxiedMux, true)
			}
			pin := ""
			if e.CA != nil {
				pin = enroll.Fingerprint(e.CA)
			}
			log.Info("agent enrollment enabled", "tokens", e.Tokens, "certificates", e.Certs, "cert_days", days,
				"ca_sha256", pin)
		}
	} else {
		log.Info("agent enrollment disabled (needs DATABASE_URL)")
	}
	errs := make(chan error, len(servers))
	go func() { errs <- servers[0].ListenAndServe() }()
	go func() { errs <- servers[1].ListenAndServeTLS("", "") }()
	if len(servers) > 2 {
		go func() { errs <- servers[2].ListenAndServe() }()
	}
	log.Info("tool hub listening", "api", servers[0].Addr, "tunnel_tls", servers[1].Addr, "auth", auth,
		"static_tokens", len(static), "version", version, "edition", opts.Edition, "max_agents", opts.MaxAgents)
	if len(servers) > 2 {
		log.Info("token tunnel for a TLS-terminating proxy", "addr", servers[2].Addr)
	}
	select {
	case <-ctx.Done():
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "err", err)
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(sctx)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
