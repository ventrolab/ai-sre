// Package agentkit is the common runtime of cluster and host agents:
// configuration, credentials (agent token or mutual TLS), the hub connection and a health
// endpoint.
package agentkit

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ventrolab/ai-sre/pkg/enroll"
	"github.com/ventrolab/ai-sre/pkg/tunnel"
)

// Common settings shared by all agents (part of each agent's YAML config).
type Common struct {
	HubURL string            `json:"hubURL"`
	Labels map[string]string `json:"labels"`
	// Auth is "token" (an agent token from tokenFile) or "mtls" (tls.cert and tls.key).
	// Empty: mtls when only tls.cert is set (configs before tokens), otherwise token.
	Auth      string `json:"auth"`
	TokenFile string `json:"tokenFile"`
	TLS       struct {
		// CA verifies the hub certificate; empty: the system roots (an ingress with a
		// public certificate). Required for mtls.
		CA   string `json:"ca"`
		Cert string `json:"cert"`
		Key  string `json:"key"`
		// ServerName overrides the name checked in the hub certificate.
		ServerName string `json:"serverName"`
	} `json:"tls"`
	HealthAddr string `json:"healthAddr"`
}

func (c *Common) Defaults() {
	if c.HealthAddr == "" {
		c.HealthAddr = ":8081"
	}
	if c.Auth == "" {
		c.Auth = enroll.AuthToken
		if c.TokenFile == "" && c.TLS.Cert != "" {
			c.Auth = enroll.AuthMTLS
		}
	}
}

func (c *Common) Validate() error {
	if c.HubURL == "" {
		return errors.New("hubURL is required")
	}
	switch c.Auth {
	case enroll.AuthToken:
		if c.TokenFile == "" {
			return errors.New("tokenFile is required (auth: token)")
		}
	case enroll.AuthMTLS:
		if c.TLS.CA == "" || c.TLS.Cert == "" || c.TLS.Key == "" {
			return errors.New("tls.ca, tls.cert and tls.key are required (auth: mtls)")
		}
	default:
		return fmt.Errorf("auth must be token or mtls, got %q", c.Auth)
	}
	return nil
}

// TLSConfig verifies the hub with tls.ca or the system roots and, for mtls, presents the
// client certificate; it is re-read on every connection so rotated certificates are
// picked up without a restart.
func (c *Common) TLSConfig() (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.TLS.ServerName}
	if c.TLS.CA != "" {
		ca, err := os.ReadFile(c.TLS.CA)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = x509.NewCertPool()
		if !cfg.RootCAs.AppendCertsFromPEM(ca) {
			return nil, fmt.Errorf("no certificates in %s", c.TLS.CA)
		}
	}
	if c.Auth == enroll.AuthMTLS {
		certFile, keyFile := c.TLS.Cert, c.TLS.Key
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, err := tls.LoadX509KeyPair(certFile, keyFile)
			return &cert, err
		}
	}
	return cfg, nil
}

// Token reads the agent token; nil for mtls.
func (c *Common) Token() func() (string, error) {
	if c.Auth != enroll.AuthToken {
		return nil
	}
	path := c.TokenFile
	return func() (string, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return "", fmt.Errorf("%s is empty", path)
		}
		return token, nil
	}
}

// Tool is one tool implementation on an agent.
type Tool func(ctx context.Context, args json.RawMessage) (string, error)

// Run serves tools over the hub tunnel until SIGTERM.
func Run(kind, version string, common Common, toolset map[string]Tool, log *slog.Logger) error {
	common.Defaults()
	if err := common.Validate(); err != nil {
		return err
	}
	tlsCfg, err := common.TLSConfig()
	if err != nil {
		return fmt.Errorf("tls: %w", err)
	}
	names := make([]string, 0, len(toolset))
	for n := range toolset {
		names = append(names, n)
	}
	sort.Strings(names)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var connected atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !connected.Load() {
			http.Error(w, "not connected to hub", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})
	srv := &http.Server{Addr: common.HealthAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("health server", "err", err)
		}
	}()
	defer srv.Shutdown(context.Background())

	handle := func(ctx context.Context, tool string, args json.RawMessage) (string, error) {
		fn, ok := toolset[tool]
		if !ok {
			return "", fmt.Errorf("tool %s is not available on this agent", tool)
		}
		return fn(ctx, args)
	}
	log.Info("agent starting", "kind", kind, "version", version, "hub", common.HubURL, "auth", common.Auth,
		"tools", names)
	tunnel.Run(ctx, tunnel.ClientConfig{
		URL: common.HubURL, TLS: tlsCfg, Token: common.Token(), Log: log,
		Hello:   tunnel.Hello{Kind: kind, Labels: common.Labels, Tools: names, Version: version},
		OnState: func(c bool) { connected.Store(c) },
	}, handle)
	return nil
}

// Decode unmarshals tool arguments strictly enough to catch typos.
func Decode(args json.RawMessage, v any) error {
	if len(args) == 0 {
		args = []byte("{}")
	}
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// Since parses a duration window with a default and an upper bound.
func Since(s string, def, max time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid duration %q (use e.g. 15m, 1h)", s)
	}
	if d > max {
		return 0, fmt.Errorf("window %s exceeds the maximum %s", d, max)
	}
	return d, nil
}

// Clamp bounds an integer argument.
func Clamp(v, def, max int) int {
	if v <= 0 {
		return def
	}
	return min(v, max)
}
