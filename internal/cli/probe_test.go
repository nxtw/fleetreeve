package cli

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nxtw/fleetreeve/internal/remote"
)

// testCA is a certificate authority that signs host certificates.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fleetreeve hosts CA"}, // the same name on every server
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{cert, key}
}

// issue writes a host certificate and its key into dir, as a node keeps them.
func (ca *testCA) issue(t *testing.T, dir string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "web1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile := nodeCertFiles(dir)
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
}

// A host moved from another server still holds that server's certificate.
// The new server ends the connection on seeing it, so the node could never
// ask for a new one: the probe must notice, for enroll to replace it.
func TestProbeNoticesForeignCertificate(t *testing.T) {
	ours, theirs := newTestCA(t), newTestCA(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("#!/bin/sh\n# fleetreeve install\n"))
	}))
	pool := x509.NewCertPool()
	pool.AddCert(ours.cert)
	srv.TLS = &tls.Config{ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: pool} // as Server.ClientAuth sets it
	srv.StartTLS()
	defer srv.Close()
	serverCA := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})

	old := nodeCertDir
	defer func() { nodeCertDir = old }()
	probe := func(prepare func(dir string)) remote.ProbeResult {
		t.Helper()
		nodeCertDir = t.TempDir()
		prepare(nodeCertDir)
		res, err := probeServer(context.Background(), remote.ProbeRequest{Server: srv.URL, CA: serverCA})
		if err != nil {
			t.Fatalf("the server can be reached, but the probe failed: %v", err)
		}
		return res
	}
	if res := probe(func(string) {}); res.CertRefused {
		t.Error("a host without a certificate was reported as holding a refused one")
	}
	if res := probe(func(dir string) { ours.issue(t, dir) }); res.CertRefused {
		t.Error("a certificate from this server's authority was reported as refused")
	}
	if res := probe(func(dir string) { theirs.issue(t, dir) }); !res.CertRefused {
		t.Error("a certificate from another server's authority was not noticed")
	}
	// Half a pair is no certificate to show; the service replaces it.
	if res := probe(func(dir string) {
		theirs.issue(t, dir)
		os.Remove(filepath.Join(dir, "node.key"))
	}); res.CertRefused {
		t.Error("a certificate without its key was reported as refused")
	}
}
