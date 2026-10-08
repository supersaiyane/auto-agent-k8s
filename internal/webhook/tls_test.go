package webhook

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCert writes a self-signed certificate for 127.0.0.1 with the given
// serial and returns its paths.
func writeCert(t *testing.T, dir string, serial int64) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "auto-agent-webhook"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func servedSerial(t *testing.T, addr string) int64 {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // test reads the served certificate
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0].SerialNumber.Int64()
}

// ISS-032: the webhook serves TLS as soon as Start returns, and picks up a
// renewed certificate without a restart.
func TestStart_ServesAndReloadsCertificate(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeCert(t, dir, 1)
	port := freePort(t)
	v := NewValidator(Config{Port: port})
	if err := v.Start(certFile, keyFile); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.srv.Close() })
	addr := net.JoinHostPort("127.0.0.1", strconvItoa(port))
	if got := servedSerial(t, addr); got != 1 {
		t.Fatalf("served serial %d, want 1", got)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // test
	resp, err := client.Get("https://" + addr + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz over TLS: %v %v", resp, err)
	}
	resp.Body.Close()

	time.Sleep(10 * time.Millisecond) // a distinct modification time
	writeCert(t, dir, 2)
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(certFile, future, future); err != nil {
		t.Fatal(err)
	}
	if got := servedSerial(t, addr); got != 2 {
		t.Fatalf("after renewal served serial %d, want 2", got)
	}

	if err := os.WriteFile(certFile, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	later := future.Add(time.Minute)
	if err := os.Chtimes(certFile, later, later); err != nil {
		t.Fatal(err)
	}
	if got := servedSerial(t, addr); got != 2 {
		t.Fatalf("a broken rewrite keeps the last good certificate, served %d", got)
	}
}

// ISS-032: no certificate, or a port in use, fails the start.
func TestStart_FailsClosed(t *testing.T) {
	v := NewValidator(Config{Port: freePort(t)})
	if err := v.Start("/nonexistent/tls.crt", "/nonexistent/tls.key"); err == nil || !strings.Contains(err.Error(), "serving certificate") {
		t.Fatalf("missing certificate: %v", err)
	}
	dir := t.TempDir()
	certFile, keyFile := writeCert(t, dir, 1)
	if err := os.WriteFile(keyFile, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := v.Start(certFile, keyFile); err == nil {
		t.Fatal("an unreadable key pair must fail the start")
	}
	certFile, keyFile = writeCert(t, dir, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	taken := &Validator{srv: &http.Server{Addr: ln.Addr().String()}}
	if err := taken.Start(certFile, keyFile); err == nil || !strings.Contains(err.Error(), "listen on") {
		t.Fatalf("port in use: %v", err)
	}
}

func strconvItoa(i int) string { return big.NewInt(int64(i)).String() }
