// Package enroll lets an agent obtain its credential from the Tool Hub with a one-time
// join token, so nothing secret is copied by hand.
//
// Token mode (default): the agent sends the join token to POST /v1/enroll and receives a
// long-lived agent token for the name stored with the join token. The hub endpoint is
// verified with the system roots (an ingress with a public certificate), or, when a CA
// fingerprint is given, with the hub CA pinned like below.
//
// Certificate mode (mutual TLS, like kubeadm join): the agent fetches the hub CA from
// GET /v1/ca without TLS verification, accepts it only if its SHA-256 matches the pinned
// fingerprint, then sends a CSR with the join token over verified TLS. The hub signs the
// CSR with the stored name as CommonName; the private key never leaves the host.
package enroll

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	PathCA     = "/v1/ca"
	PathEnroll = "/v1/enroll"
)

type Request struct {
	Token string `json:"token"`
	// PEM CSR asks for a client certificate; without it the hub issues an agent token.
	CSR string `json:"csr,omitempty"`
}

type Response struct {
	Name string `json:"name"`
	// Agent token (token mode).
	AgentToken string `json:"agent_token,omitempty"`
	TokenID    string `json:"token_id,omitempty"`
	// Client certificate (certificate mode).
	Certificate string `json:"certificate,omitempty"` // PEM
	CA          string `json:"ca,omitempty"`          // PEM
	NotAfter    string `json:"not_after,omitempty"`
}

// Auth modes of an agent.
const (
	AuthToken = "token"
	AuthMTLS  = "mtls"
)

// HashToken is how tokens are stored: hex SHA-256 of the whole token string.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Fingerprint is the hex SHA-256 of a certificate's DER bytes.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// Sign issues a client certificate for the CSR with the given CommonName; any subject in
// the CSR is ignored. Returns the PEM certificate, its hex serial and expiry.
func Sign(ca *x509.Certificate, caKey crypto.Signer, csrPEM []byte, name string, validity time.Duration) ([]byte, string, time.Time, error) {
	csr, err := ParseCSR(csrPEM)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, "", time.Time{}, err
	}
	now := time.Now()
	notAfter := now.Add(validity).UTC()
	if notAfter.After(ca.NotAfter) {
		notAfter = ca.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, csr.PublicKey, caKey)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), serial.Text(16), notAfter, nil
}

// ParseCSR decodes a PEM CSR and checks its signature (proof of key possession).
func ParseCSR(csrPEM []byte) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("csr: expected a PEM CERTIFICATE REQUEST")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("csr: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("csr signature: %w", err)
	}
	return csr, nil
}

// LoadCA reads the CA certificate and key the hub signs with.
func LoadCA(certFile, keyFile string) (*x509.Certificate, crypto.Signer, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, nil, err
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, nil, errors.New("ca key cannot sign")
	}
	return cert, signer, nil
}

// BaseURL turns the agent's tunnel URL (wss://host:port/v1/tunnel) into https://host:port.
func BaseURL(hubURL string) (string, error) {
	u, err := url.Parse(hubURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "wss", "https":
	default:
		return "", fmt.Errorf("hub URL must use wss:// or https://, got %q", u.Scheme)
	}
	return "https://" + u.Host, nil
}

// Files are where the agent keeps its credentials (the paths of its config).
type Files struct {
	CA, Cert, Key string
	// Token is the agent token file (token mode).
	Token string
	// ServerName overrides the name checked in the hub certificate (tls.serverName).
	ServerName string
}

// Options of an enrollment.
type Options struct {
	HubURL    string // the agent's tunnel URL
	JoinToken string
	// CASHA256 pins the hub CA (hex SHA-256). Required for mtls; in token mode only when
	// the hub endpoint has no publicly trusted certificate (the hub's own TLS port).
	CASHA256 string
	Auth     string // token (default) or mtls
	Files    Files
}

// Client enrolls an agent and writes its credentials: the agent token (mode 0600), or the
// CA, the issued certificate and a new private key (mode 0600). With a CA pin the CA is
// written too. Returns the name the hub assigned.
func Client(ctx context.Context, o Options) (string, error) {
	base, err := BaseURL(o.HubURL)
	if err != nil {
		return "", err
	}
	if o.Auth == "" {
		o.Auth = AuthToken
	}
	if o.Auth != AuthToken && o.Auth != AuthMTLS {
		return "", fmt.Errorf("auth must be %s or %s, got %q", AuthToken, AuthMTLS, o.Auth)
	}
	pin := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(o.CASHA256), "sha256:"))
	if pin != "" && len(pin) != 64 {
		return "", errors.New("ca fingerprint must be a hex SHA-256 (64 characters)")
	}
	if o.Auth == AuthMTLS && pin == "" {
		return "", errors.New("certificate enrollment needs the hub CA fingerprint")
	}

	// 1. With a pin: the CA over unverified TLS, accepted only if it matches the pin.
	tlsCfg := &tls.Config{ServerName: o.Files.ServerName, MinVersion: tls.VersionTLS12}
	var caPEM []byte
	if pin != "" {
		insecure := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}}} // #nosec G402 -- pinned below
		if caPEM, err = get(ctx, insecure, base+PathCA); err != nil {
			return "", fmt.Errorf("fetch CA: %w", err)
		}
		block, _ := pem.Decode(caPEM)
		if block == nil {
			return "", errors.New("hub returned no CA certificate")
		}
		ca, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return "", err
		}
		if got := Fingerprint(ca); got != pin {
			return "", fmt.Errorf("hub CA fingerprint %s does not match the expected %s", got, pin)
		}
		tlsCfg.RootCAs = x509.NewCertPool()
		tlsCfg.RootCAs.AddCert(ca)
	}

	// 2. Certificate mode: key and CSR; the subject is decided by the hub.
	enrollReq := Request{Token: o.JoinToken}
	var key *ecdsa.PrivateKey
	if o.Auth == AuthMTLS {
		if key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			return "", err
		}
		csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
		if err != nil {
			return "", err
		}
		enrollReq.CSR = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))
	}
	body, _ := json.Marshal(enrollReq)

	// 3. Enroll over verified TLS.
	verified := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+PathEnroll, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := verified.Do(req)
	if err != nil {
		return "", fmt.Errorf("request to %s: %w", base+PathEnroll, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("hub answered %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out Response
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}

	type file struct {
		path string
		data []byte
		mode os.FileMode
	}
	var files []file
	if o.Auth == AuthToken {
		if out.AgentToken == "" {
			return "", errors.New("hub answered without an agent token (does it allow token authentication?)")
		}
		files = append(files, file{o.Files.Token, []byte(out.AgentToken + "\n"), 0o600})
	} else {
		if out.Certificate == "" {
			return "", errors.New("hub answered without a certificate (does it allow mutual TLS?)")
		}
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			return "", err
		}
		files = append(files, file{o.Files.Key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600},
			file{o.Files.Cert, []byte(out.Certificate), 0o644})
	}
	if caPEM != nil && o.Files.CA != "" {
		files = append(files, file{o.Files.CA, caPEM, 0o644})
	}
	for _, f := range files {
		if f.path == "" {
			return "", errors.New("no path configured for an enrollment file")
		}
		if err := writeFile(f.path, f.data, f.mode); err != nil {
			return "", err
		}
	}
	return out.Name, nil
}

// NewAgentToken generates an agent token.
func NewAgentToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "aisre-agent-" + base64.RawURLEncoding.EncodeToString(b), nil
}

func get(ctx context.Context, c *http.Client, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// writeFile replaces a file atomically, so a running agent never reads half a key.
func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
