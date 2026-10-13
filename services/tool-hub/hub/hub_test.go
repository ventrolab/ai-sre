package hub

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ventrolab/ai-sre/pkg/tunnel"
)

type pki struct {
	ca     *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caPool *x509.CertPool
}

func newPKI(t *testing.T) *pki {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &pki{ca: ca, caKey: key, caPool: pool}
}

func (p *pki) issue(t *testing.T, cn string, server bool) tls.Certificate {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.DNSNames = []string{"localhost"}
		tmpl.IPAddresses = nil
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// End to end: an agent dials the hub over mTLS, HolmesGPT-like MCP client calls a tool.
func TestRoutingThroughTunnel(t *testing.T) {
	p := newPKI(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := tunnel.NewRegistry(log)
	connected, disconnected := make(chan *tunnel.Agent, 1), make(chan *tunnel.Agent, 1)
	reg.OnConnect = func(a *tunnel.Agent) { connected <- a }
	reg.OnDisconnect = func(a *tunnel.Agent) { disconnected <- a }
	hub := &Hub{reg: reg, log: log, timeout: 5 * time.Second, maxOutput: 200}

	srv := httptest.NewUnstartedServer(reg.Handler(false))
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: p.caPool,
		Certificates: []tls.Certificate{p.issue(t, "hub", true)}}
	srv.StartTLS()
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agentCtx, stopAgent := context.WithCancel(ctx)
	clientCert := p.issue(t, "edge-cluster", false)
	go tunnel.Run(agentCtx, tunnel.ClientConfig{
		URL: "wss" + strings.TrimPrefix(srv.URL, "https") + tunnel.Path,
		TLS: &tls.Config{RootCAs: p.caPool, Certificates: []tls.Certificate{clientCert}, ServerName: "localhost"},
		Log: log, Hello: tunnel.Hello{Kind: "cluster", Tools: []string{"k8s_list"}, Labels: map[string]string{"env": "test"}},
	}, func(ctx context.Context, tool string, args json.RawMessage) (string, error) {
		var a map[string]any
		_ = json.Unmarshal(args, &a)
		if _, leaked := a["target"]; leaked {
			t.Error("target must not reach the agent")
		}
		return "NAME READY\nweb-1 1/1\npassword=hunter22\n" + strings.Repeat("x", 400), nil
	})
	for i := 0; i < 50 && reg.Get("edge-cluster") == nil; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if reg.Get("edge-cluster") == nil {
		t.Fatal("agent did not register under its certificate CommonName")
	}

	ct, st := mcp.NewInMemoryTransports()
	server := hub.MCPServer("test")
	go server.Run(ctx, st)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	callText := func(name string, args map[string]any) (string, bool) {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return res.Content[0].(*mcp.TextContent).Text, res.IsError
	}

	out, isErr := callText("k8s_list", map[string]any{"kind": "pods", "target": "edge-cluster"})
	if isErr || !strings.Contains(out, "web-1") {
		t.Fatalf("routed call failed: %q", out)
	}
	if strings.Contains(out, "hunter22") || !strings.Contains(out, "truncated") {
		t.Fatalf("output not redacted/truncated: %q", out)
	}
	// Target may be omitted when only one agent serves the tool.
	if out, isErr = callText("k8s_list", map[string]any{"kind": "pods"}); isErr {
		t.Fatalf("implicit target failed: %q", out)
	}
	if out, isErr = callText("host_info", map[string]any{"target": "edge-cluster"}); !isErr || !strings.Contains(out, "does not serve") {
		t.Fatalf("expected capability error, got %q", out)
	}
	if out, _ = callText("list_targets", nil); !strings.Contains(out, "edge-cluster (cluster)") || !strings.Contains(out, "env=test") {
		t.Fatalf("list_targets: %q", out)
	}
	tl, err := cs.ListTools(ctx, nil)
	if err != nil || len(tl.Tools) < 10 {
		t.Fatalf("tools listed: %d, err %v", len(tl.Tools), err)
	}

	// Session hooks feed the persistent agent registry.
	select {
	case a := <-connected:
		if a.Name != "edge-cluster" || a.CertSerial == "" || a.CertNotAfter.Before(time.Now()) {
			t.Fatalf("connect hook: %+v", a)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnConnect not called")
	}
	stopAgent()
	select {
	case a := <-disconnected:
		if a.Name != "edge-cluster" {
			t.Fatalf("disconnect hook: %s", a.Name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnDisconnect not called")
	}
}
