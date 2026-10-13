package hub

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ventrolab/ai-sre/pkg/agentstore"
	"github.com/ventrolab/ai-sre/pkg/enroll"
	"github.com/ventrolab/ai-sre/pkg/tunnel"
)

type fakeEnrollStore struct {
	mu     sync.Mutex
	tokens map[string]string // hash -> name
	issued map[string]string // serial -> name
	agents map[string]string // agent token hash -> name
}

func (f *fakeEnrollStore) IssueToken(_ context.Context, _, name, tokenHash string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.agents == nil {
		f.agents = map[string]string{}
	}
	for h, n := range f.agents {
		if n == name {
			delete(f.agents, h) // re-enrollment replaces the token
		}
	}
	f.agents[tokenHash] = name
	return agentstore.TokenID(tokenHash), nil
}

func (f *fakeEnrollStore) tokenAuth(_ context.Context, token string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	hash := enroll.HashToken(token)
	if name, ok := f.agents[hash]; ok {
		return name, agentstore.TokenID(hash), nil
	}
	return "", "", tunnel.ErrUnknownToken
}

func (f *fakeEnrollStore) RedeemEnrollment(_ context.Context, hash, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name, ok := f.tokens[hash]
	if !ok {
		return "", agentstore.ErrInvalidToken
	}
	delete(f.tokens, hash) // one-time
	return name, nil
}

func (f *fakeEnrollStore) RecordIssued(_ context.Context, _, serial, name string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issued[serial] = name
	return nil
}

// Enrollment end to end: pinned CA, one-time token, certificate usable for the tunnel,
// revoked serial refused, throttling of guessed tokens.
func TestEnrollment(t *testing.T) {
	p := newPKI(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := &fakeEnrollStore{tokens: map[string]string{enroll.HashToken("join-ok"): "db1"}, issued: map[string]string{}}
	reg := tunnel.NewRegistry(log)
	revoked := map[string]bool{}
	var revokedMu sync.Mutex
	reg.Revoked = func(s string) bool { revokedMu.Lock(); defer revokedMu.Unlock(); return revoked[s] }

	mux := http.NewServeMux()
	mux.Handle(tunnel.Path, reg.Handler(false))
	(&Enroller{CA: p.ca, CAKey: p.caKey, Validity: 24 * time.Hour, Log: log, Certs: true,
		Store: func() enrollStore { return store }}).Register(mux, false)
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: p.caPool,
		Certificates: []tls.Certificate{p.issue(t, "hub", true)}}
	srv.StartTLS()
	defer srv.Close()

	ctx := context.Background()
	hubURL := "wss" + strings.TrimPrefix(srv.URL, "https") + tunnel.Path
	dir := t.TempDir()
	files := enroll.Files{CA: filepath.Join(dir, "ca.crt"), Cert: filepath.Join(dir, "tls.crt"),
		Key: filepath.Join(dir, "tls.key"), ServerName: "localhost"}
	pin := enroll.Fingerprint(p.ca)
	enrollWith := func(join, pin string) (string, error) {
		return enroll.Client(ctx, enroll.Options{HubURL: hubURL, JoinToken: join, CASHA256: pin, Auth: enroll.AuthMTLS,
			Files: files})
	}

	if _, err := enrollWith("join-ok", strings.Repeat("0", 64)); err == nil ||
		!strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("wrong CA pin must fail before the token is sent: %v", err)
	}
	if _, err := enroll.Client(ctx, enroll.Options{HubURL: hubURL, JoinToken: "join-ok", CASHA256: pin,
		Files: files}); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("a token request to a certificate-only hub must fail before the join token is spent: %v", err)
	}
	name, err := enrollWith("join-ok", "sha256:"+strings.ToUpper(pin))
	if err != nil || name != "db1" {
		t.Fatalf("enroll: %q %v", name, err)
	}
	if st, _ := os.Stat(files.Key); st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", st.Mode().Perm())
	}
	certPEM, _ := os.ReadFile(files.Cert)
	block, _ := pem.Decode(certPEM)
	cert, _ := x509.ParseCertificate(block.Bytes)
	if cert.Subject.CommonName != "db1" || store.issued[cert.SerialNumber.Text(16)] != "db1" {
		t.Fatalf("issued certificate: CN %q, recorded %v", cert.Subject.CommonName, store.issued)
	}
	if _, err := enrollWith("join-ok", pin); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a used token must be refused: %v", err)
	}

	// The certificate opens the tunnel and is refused once revoked.
	pair, err := tls.LoadX509KeyPair(files.Cert, files.Key)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: p.caPool, ServerName: "localhost", Certificates: []tls.Certificate{pair}}}}
	tunnelStatus := func() int {
		resp, err := client.Get(srv.URL + tunnel.Path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := tunnelStatus(); code == http.StatusForbidden || code == http.StatusUnauthorized {
		t.Fatalf("enrolled certificate refused at the tunnel: %d", code)
	}
	revokedMu.Lock()
	revoked[cert.SerialNumber.Text(16)] = true
	revokedMu.Unlock()
	if code := tunnelStatus(); code != http.StatusForbidden {
		t.Fatalf("revoked certificate: status %d, want 403", code)
	}
	// Without a client certificate the tunnel stays closed even though TLS allows it.
	resp, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: p.caPool,
		ServerName: "localhost"}}}).Get(srv.URL + tunnel.Path)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("tunnel without certificate: %v %v", resp, err)
	}

	// Guessing tokens is throttled per client address.
	for i := 0; i < maxFailures; i++ {
		_, _ = enrollWith("guess", pin)
	}
	if _, err := enrollWith("guess", pin); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("expected throttling, got %v", err)
	}
}

// Token enrollment end to end, as through an ingress: the hub endpoint is plain HTTP behind
// a TLS-terminating proxy (here: an httptest TLS server in front of the plain mux).
func TestTokenEnrollment(t *testing.T) {
	p := newPKI(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := &fakeEnrollStore{tokens: map[string]string{enroll.HashToken("join-1"): "db1",
		enroll.HashToken("join-2"): "db1"}, issued: map[string]string{}}
	reg := tunnel.NewRegistry(log)
	reg.AllowCert = false
	reg.TokenAuth = store.tokenAuth

	mux := http.NewServeMux()
	mux.Handle(tunnel.Path, reg.Handler(true))
	(&Enroller{Log: log, Tokens: true, Store: func() enrollStore { return store }}).Register(mux, true)
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{p.issue(t, "hub", true)}}
	srv.StartTLS()
	defer srv.Close()

	ctx := context.Background()
	hubURL := "wss" + strings.TrimPrefix(srv.URL, "https") + tunnel.Path
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	tlsCfg := &tls.Config{RootCAs: p.caPool, ServerName: "localhost"}
	// The test CA plays the publicly trusted one: enroll trusts it through SSL_CERT_FILE.
	caFile := filepath.Join(dir, "roots.pem")
	_ = os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.ca.Raw}), 0o644)
	t.Setenv("SSL_CERT_FILE", caFile)

	if _, err := enroll.Client(ctx, enroll.Options{HubURL: hubURL, JoinToken: "join-1", Auth: enroll.AuthMTLS,
		CASHA256: strings.Repeat("0", 64), Files: enroll.Files{}}); err == nil {
		t.Fatal("mtls enrollment must fail")
	}
	name, err := enroll.Client(ctx, enroll.Options{HubURL: hubURL, JoinToken: "join-1",
		Files: enroll.Files{Token: tokenFile, ServerName: "localhost"}})
	if err != nil || name != "db1" {
		t.Fatalf("enroll: %q %v", name, err)
	}
	if st, _ := os.Stat(tokenFile); st.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v", st.Mode().Perm())
	}
	data, _ := os.ReadFile(tokenFile)
	first := strings.TrimSpace(string(data))
	if !strings.HasPrefix(first, "aisre-agent-") {
		t.Fatalf("token %q", first)
	}

	// The token opens the tunnel; the agent registers under the enrolled name.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go tunnel.Run(runCtx, tunnel.ClientConfig{URL: hubURL, TLS: tlsCfg, Log: log,
		Token: func() (string, error) { return first, nil },
		Hello: tunnel.Hello{Kind: "host", Tools: []string{"host_info"}}},
		func(context.Context, string, json.RawMessage) (string, error) { return "ok", nil })
	var a *tunnel.Agent
	for i := 0; i < 50 && a == nil; i++ {
		time.Sleep(50 * time.Millisecond)
		a = reg.Get("db1")
	}
	if a == nil || a.Auth != "token" || a.TokenID != agentstore.TokenID(enroll.HashToken(first)) {
		t.Fatalf("agent not registered with its token: %+v", a)
	}
	stop()

	status := func(header string) int {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+tunnel.Path, nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		resp, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := status(""); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	if code := status("Bearer aisre-agent-guess"); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", code)
	}

	// Re-enrollment replaces the token: the old one stops working.
	if _, err := enroll.Client(ctx, enroll.Options{HubURL: hubURL, JoinToken: "join-2",
		Files: enroll.Files{Token: tokenFile, ServerName: "localhost"}}); err != nil {
		t.Fatal(err)
	}
	if code := status("Bearer " + first); code != http.StatusUnauthorized {
		t.Fatalf("replaced token: %d", code)
	}
	if code := status("Bearer " + strings.Repeat("x", 3)); code != http.StatusUnauthorized {
		t.Fatalf("garbage token: %d", code)
	}

	// Throttling behind a proxy counts the client address from X-Real-IP, not the proxy.
	e := &Enroller{Log: log, Tokens: true, Store: func() enrollStore { return store }}
	pm := http.NewServeMux()
	e.Register(pm, true)
	post := func(ip string) int {
		req := httptest.NewRequest(http.MethodPost, enroll.PathEnroll, strings.NewReader(`{"token":"guess"}`))
		req.Header.Set("X-Real-IP", ip)
		rec := httptest.NewRecorder()
		pm.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 0; i < maxFailures; i++ {
		post("203.0.113.7")
	}
	if code := post("203.0.113.7"); code != http.StatusTooManyRequests {
		t.Fatalf("expected throttling, got %d", code)
	}
	if code := post("203.0.113.8"); code != http.StatusForbidden {
		t.Fatalf("another client must not be throttled: %d", code)
	}
}
