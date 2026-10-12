package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testCert writes a self-signed serving certificate for 127.0.0.1.
func testCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "auto-agent-webhook"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func listening(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	if err := c.Close(); err != nil {
		return false
	}
	return true
}

// ISS-101: the admission webhook is part of the agent's shutdown, so a
// review in flight at SIGTERM is finished, not cut off with the pod.
func TestRun_ShutdownStopsTheWebhook(t *testing.T) {
	const addr = "127.0.0.1:8443" // the webhook's fixed port
	if listening(addr) {
		t.Skip("port 8443 is in use on this machine")
	}
	certFile, keyFile := testCert(t)
	a := boot(t, map[string]string{"WEBHOOK_CERT_FILE": certFile, "WEBHOOK_KEY_FILE": keyFile})
	if !listening(addr) {
		t.Fatal("the webhook must serve while the agent runs")
	}
	a.stop(t)
	if listening(addr) {
		t.Fatal("shutdown must stop the webhook")
	}
}
