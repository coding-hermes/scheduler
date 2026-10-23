package main

// SCHED-GAP-1600 — TLS listener arm.
//
// The gap this pins: the dashboard is a PLAIN HTTP origin behind a plain-HTTP
// tailnet forward, so a browser refuses Notification and serviceWorker there
// outright (measured: isSecureContext false, Notification.permission "denied",
// no serviceWorker). A TLS listener is the only in-daemon cure — the same
// handler tree served over https is a secure context.
//
// What these tests refuse to let regress:
//   - the pair stays all-or-nothing (a half-configured pair must NOT silently
//     serve cleartext: the operator would believe the origin is secure);
//   - the empty pair keeps the historical cleartext listener, so existing
//     deployments are untouched;
//   - the TLS arm serves THE SAME handler (transport changes, authorization
//     does not — no auth widening on the new origin).

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTLSPairValid pins the all-or-nothing rule. The half-configured arms are
// the whole point: each must be an ERROR naming both flags, never nil.
func TestTLSPairValid(t *testing.T) {
	cases := []struct {
		name     string
		cert     string
		key      string
		wantErr  bool
		wantHint string
	}{
		{name: "neither set keeps cleartext", cert: "", key: "", wantErr: false},
		{name: "cert only refuses", cert: "/tmp/c.pem", key: "", wantErr: true, wantHint: "--tls-key"},
		{name: "key only refuses", cert: "", key: "/tmp/k.pem", wantErr: true, wantHint: "--tls-cert"},
		{name: "both set is valid", cert: "/tmp/c.pem", key: "/tmp/k.pem", wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tlsPairValid(tc.cert, tc.key)
			if tc.wantErr && err == nil {
				t.Fatalf("tlsPairValid(%q, %q) = nil, want an error — a half-configured pair must never fall back to cleartext", tc.cert, tc.key)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("tlsPairValid(%q, %q) = %v, want nil", tc.cert, tc.key, err)
			}
			if err != nil && !strings.Contains(err.Error(), tc.wantHint) {
				t.Errorf("error %q does not name %q — an operator must learn which half is missing", err, tc.wantHint)
			}
		})
	}
}

// TestServeDashboardTLSAndCleartext drives the real listener arm both ways.
//
// TLS arm: a self-signed cert generated here, requested with a RootCAs pool
// holding that cert — so the handshake is genuinely verified, not skipped.
// Cleartext arm: the empty pair, which must still answer over plain HTTP.
// Both arms hit the SAME handler marker, proving the TLS front serves the
// identical tree rather than a reduced one.
func TestServeDashboardTLSAndCleartext(t *testing.T) {
	certFile, keyFile, cert := writeTestCert(t)

	const marker = "sched1600-same-handler-tree"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, marker)
			return
		}
		http.NotFound(w, r)
	})

	t.Run("tls arm serves the same handler over https", func(t *testing.T) {
		addr := freeLoopbackAddr(t)
		srv := &http.Server{Addr: addr, Handler: handler}
		errCh := make(chan error, 1)
		go func() { errCh <- serveDashboard(srv, certFile, keyFile) }()
		t.Cleanup(func() { _ = srv.Close() })

		pool := x509.NewCertPool()
		pool.AddCert(cert)
		client := &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: tlsConfigWithRoots(pool),
			},
		}
		body, scheme, err := getWithRetry(t, client, "https://"+addr+"/health", errCh)
		if err != nil {
			t.Fatalf("https GET %s: %v", addr, err)
		}
		if scheme != "https" {
			t.Fatalf("served scheme = %q, want https", scheme)
		}
		if body != marker {
			t.Fatalf("body = %q, want the shared handler marker %q — the TLS arm must serve the same tree", body, marker)
		}
	})

	t.Run("cleartext arm unchanged when no pair is set", func(t *testing.T) {
		addr := freeLoopbackAddr(t)
		srv := &http.Server{Addr: addr, Handler: handler}
		errCh := make(chan error, 1)
		go func() { errCh <- serveDashboard(srv, "", "") }()
		t.Cleanup(func() { _ = srv.Close() })

		client := &http.Client{Timeout: 5 * time.Second}
		body, scheme, err := getWithRetry(t, client, "http://"+addr+"/health", errCh)
		if err != nil {
			t.Fatalf("http GET %s: %v", addr, err)
		}
		if scheme != "http" {
			t.Fatalf("served scheme = %q, want http", scheme)
		}
		if body != marker {
			t.Fatalf("body = %q, want %q", body, marker)
		}
	})

	t.Run("half-configured pair never listens", func(t *testing.T) {
		addr := freeLoopbackAddr(t)
		srv := &http.Server{Addr: addr, Handler: handler}
		// Bounded on purpose: the regression this guards against is a silent
		// cleartext fallback, which would BLOCK here instead of returning. A
		// hang under `go test -timeout` would be a failure too, but an explicit
		// 3s bound names the defect instead of looking like a stuck suite.
		errCh := make(chan error, 1)
		go func() { errCh <- serveDashboard(srv, certFile, "") }()
		select {
		case err := <-errCh:
			if err == nil {
				t.Fatal("serveDashboard(cert, \"\") = nil, want an error — a half-configured pair must not open a cleartext listener")
			}
			if !strings.Contains(err.Error(), "--tls-key") {
				t.Errorf("error %q does not name the missing --tls-key", err)
			}
		case <-time.After(3 * time.Second):
			_ = srv.Close()
			t.Fatal("serveDashboard(cert, \"\") neither returned nor errored within 3s — a half-configured pair opened a listener (silent cleartext fallback)")
		}
	})
}

// tlsConfigWithRoots trusts exactly the test certificate, so the TLS arm is
// proven with real verification (no InsecureSkipVerify escape hatch that would
// pass even if the server served the wrong cert).
func tlsConfigWithRoots(pool *x509.CertPool) *tls.Config {
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

// writeTestCert generates a short-lived self-signed cert for 127.0.0.1 and
// returns the PEM file paths plus the parsed certificate (for the client pool).
func writeTestCert(t *testing.T) (certFile, keyFile string, cert *x509.Certificate) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1600),
		Subject:               pkix.Name{CommonName: "sched1600-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certFile, keyFile, parsed
}

// freeLoopbackAddr reserves an ephemeral loopback port and releases it.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return addr
}

// getWithRetry polls until the server answers, the listener reports an error,
// or the deadline passes — the daemon goroutine needs a moment to bind.
func getWithRetry(t *testing.T, client *http.Client, url string, errCh <-chan error) (body, scheme string, err error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case serveErr := <-errCh:
			if serveErr != nil && !strings.Contains(serveErr.Error(), "Server closed") {
				return "", "", serveErr
			}
		default:
		}
		resp, reqErr := client.Get(url)
		if reqErr == nil {
			if resp.TLS != nil {
				scheme = "https"
			} else {
				scheme = "http"
			}
			b, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				return "", scheme, readErr
			}
			return string(b), scheme, nil
		}
		err = reqErr
		time.Sleep(50 * time.Millisecond)
	}
	return "", "", err
}
