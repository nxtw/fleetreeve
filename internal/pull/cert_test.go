package pull

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeVault answers the few PKI calls fleetreeve makes, signing with a CA of
// its own, and counts what it was asked.
type fakeVault struct {
	ca      *x509.Certificate
	key     *ecdsa.PrivateKey
	caPEM   []byte
	serial  int64
	revoked []string
	ttl     time.Duration
	calls   []string // administrative calls, as "METHOD path"
	mounted bool
	noCA    bool
}

func newFakeVault(t *testing.T) (*fakeVault, *httptest.Server) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	v := &fakeVault{ca: ca, key: key, caPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), serial: 100, ttl: time.Hour}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("X-Vault-Token") == "s.test"
		admin := r.Header.Get("X-Vault-Token") == "s.admin"
		switch {
		case (r.URL.Path == "/v1/pki/ca_chain" || r.URL.Path == "/v1/pki/ca/pem") && v.noCA:
			w.WriteHeader(204)
		case r.URL.Path == "/v1/sys/mounts" && admin:
			if v.mounted {
				fmt.Fprint(w, `{"data":{"pki/":{"type":"pki"},"secret/":{"type":"kv"}}}`)
			} else {
				fmt.Fprint(w, `{"data":{"secret/":{"type":"kv"}}}`)
			}
		case admin && (r.URL.Path == "/v1/sys/mounts/pki" || r.URL.Path == "/v1/pki/root/generate/internal" || r.URL.Path == "/v1/pki/roles/fleetreeve" || r.URL.Path == "/v1/sys/policies/acl/fleetreeve-pki-fleetreeve"):
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			v.calls = append(v.calls, r.Method+" "+strings.TrimPrefix(r.URL.Path, "/v1/")+fmt.Sprint(" ", body["key_type"], body["client_flag"], body["allowed_domains"]))
			if strings.HasSuffix(r.URL.Path, "/internal") {
				v.noCA = false
			}
			fmt.Fprint(w, `{}`)
		case admin && r.URL.Path == "/v1/auth/token/create-orphan":
			v.calls = append(v.calls, "POST auth/token/create-orphan")
			fmt.Fprint(w, `{"auth":{"client_token":"s.test"}}`)
		case r.URL.Path == "/v1/pki/ca_chain":
			w.Write(v.caPEM)
		case r.URL.Path == "/v1/auth/token/lookup-self" && auth:
			fmt.Fprint(w, `{"data":{"ttl":0,"renewable":false}}`)
		case r.URL.Path == "/v1/pki/sign/fleetreeve" && auth:
			var req struct {
				CSR        string `json:"csr"`
				CommonName string `json:"common_name"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			block, _ := pem.Decode([]byte(req.CSR))
			csr, err := x509.ParseCertificateRequest(block.Bytes)
			if err != nil {
				http.Error(w, `{"errors":["bad csr"]}`, 400)
				return
			}
			v.serial++
			leaf := &x509.Certificate{SerialNumber: big.NewInt(v.serial), Subject: pkix.Name{CommonName: req.CommonName}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(v.ttl),
				KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
			der, _ := x509.CreateCertificate(rand.Reader, leaf, v.ca, csr.PublicKey, v.key)
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"certificate": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), "issuing_ca": string(v.caPEM), "serial_number": fmt.Sprintf("%02x", v.serial)}})
		case r.URL.Path == "/v1/pki/revoke" && auth:
			var req map[string]string
			json.NewDecoder(r.Body).Decode(&req)
			v.revoked = append(v.revoked, req["serial_number"])
			fmt.Fprint(w, `{}`)
		default:
			http.Error(w, `{"errors":["permission denied"]}`, 403)
		}
	}))
	t.Cleanup(ts.Close)
	return v, ts
}

// A host gets a certificate with its token, once; shows it from then on;
// renews it with the certificate itself; and starts over only when an
// administrator says so.
func TestHostCertificates(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "inv.yaml"), []byte("hosts:\n  web1: {}\n  web2: {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "site.yaml"), []byte("name: site\nresources: []\n"), 0o644)
	secret := []byte(strings.Repeat("s", 32))
	fv, vts := newFakeVault(t)
	vault, err := NewVault(vts.URL, "s.test", "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := vault.Check(ctx); err != nil {
		t.Fatal(err)
	}
	s := &Server{Manifest: filepath.Join(dir, "site.yaml"), Inventory: filepath.Join(dir, "inv.yaml"), Secret: secret, Hosts: filepath.Join(dir, "hosts"),
		CA: vault, Certs: filepath.Join(dir, "certs"), RequireCert: true, Log: slog.New(slog.DiscardHandler)}
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.TLS = &tls.Config{}
	if err := s.ClientAuth(ctx, ts.TLS); err != nil {
		t.Fatal(err)
	}
	ts.StartTLS()
	defer ts.Close()
	serverCA := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw})
	client := func(host string) *Client {
		c, err := NewClientCA(ts.URL, host, Token(secret, host), serverCA, false)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	// obtain gets a certificate for the client's host and has it shown.
	obtain := func(c *Client, host string) (certFile string, err error) {
		key, csr, err := NewKeyAndCSR(host)
		if err != nil {
			t.Fatal(err)
		}
		certPEM, err := c.Cert(ctx, csr)
		if err != nil {
			return "", err
		}
		certFile = filepath.Join(t.TempDir(), "node.crt")
		os.WriteFile(certFile, []byte(certPEM), 0o644)
		os.WriteFile(certFile+".key", key, 0o600)
		c.WithCert(certFile, certFile+".key")
		return certFile, nil
	}
	facts := map[string]any{"os": map[string]any{"family": "debian"}}

	// The server says what it expects.
	web1 := client("web1")
	if h, err := web1.Health(ctx); err != nil || h["certificates"] != "vault" || h["require_certificate"] != true {
		t.Fatalf("health: %v %v", h, err)
	}
	// Certificates are required: the token alone gets no catalog...
	if _, err := web1.Catalog(ctx, facts); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("catalog with only a token: %v", err)
	}
	// ...but it does get the host its first certificate, for its own name.
	first, err := obtain(web1, "web1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := web1.Catalog(ctx, facts); err != nil {
		t.Fatalf("catalog with a certificate: %v", err)
	}
	// Not for another host's name, and never a second time: once a host
	// has a certificate, a stolen token is worth nothing.
	if _, err := web1.Catalog(ctx, facts); err != nil {
		t.Fatal(err)
	}
	thief := client("web1")
	if _, err := obtain(thief, "web1"); err == nil || !strings.Contains(err.Error(), "already has a certificate") {
		t.Errorf("a second certificate with the token: %v", err)
	}
	if _, err := NewClientCA(ts.URL, "web2", Token(secret, "web1"), serverCA, false); err != nil {
		t.Fatal(err)
	}
	other, _ := NewClientCA(ts.URL, "web2", "", serverCA, false)
	other.WithCert(first, first+".key")
	if _, err := other.Catalog(ctx, facts); err == nil {
		t.Error("web1's certificate was accepted for web2")
	}
	// Renewal: the certificate itself is the credential. The old one
	// works until the new one has been used, then no more.
	old := client("web1")
	old.WithCert(first, first+".key")
	if _, err := obtain(web1, "web1"); err != nil {
		t.Fatalf("renewal: %v", err)
	}
	if _, err := old.Catalog(ctx, facts); err != nil {
		t.Errorf("the previous certificate before the new one was used: %v", err)
	}
	if _, err := web1.Catalog(ctx, facts); err != nil {
		t.Fatalf("the renewed certificate: %v", err)
	}
	old.tr.CloseIdleConnections()
	if _, err := old.Catalog(ctx, facts); err == nil || !strings.Contains(err.Error(), "not the current certificate") {
		t.Errorf("the superseded certificate: %v", err)
	}
	// A certificate the authority did not sign is no certificate.
	rogueKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(102), Subject: pkix.Name{CommonName: "web1"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &rogueKey.PublicKey, rogueKey)
	keyDER, _ := x509.MarshalPKCS8PrivateKey(rogueKey)
	rogueFile := filepath.Join(t.TempDir(), "rogue.crt")
	os.WriteFile(rogueFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	os.WriteFile(rogueFile+".key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600)
	rogue, _ := NewClientCA(ts.URL, "web1", "", serverCA, false)
	rogue.WithCert(rogueFile, rogueFile+".key")
	if _, err := rogue.Catalog(ctx, facts); err == nil {
		t.Error("a self-signed certificate was accepted")
	}
	// Enrolling the host again (it was rebuilt) starts it over: the old
	// certificates are revoked at the authority, and the token works once more.
	admin, _ := NewClientCA(ts.URL, "web1", "", serverCA, false)
	if _, err := admin.RegisterHost(ctx, AdminToken(secret), HostEntry{Name: "web1", NewCert: true}); err != nil {
		t.Fatal(err)
	}
	if len(fv.revoked) == 0 || !strings.Contains(fv.revoked[0], ":") && len(fv.revoked[0]) > 2 {
		t.Errorf("revoked at the authority: %v", fv.revoked)
	}
	if got := vaultSerial("6D3e688"); got != "06:d3:e6:88" {
		t.Errorf("vaultSerial: %s", got)
	}
	web1.tr.CloseIdleConnections()
	if _, err := web1.Catalog(ctx, facts); err == nil {
		t.Error("the certificate of before the re-enrolment still works")
	}
	rebuilt := client("web1")
	if _, err := obtain(rebuilt, "web1"); err != nil {
		t.Fatalf("a certificate after re-enrolment: %v", err)
	}
	if _, err := rebuilt.Catalog(ctx, facts); err != nil {
		t.Errorf("catalog after re-enrolment: %v", err)
	}
	// An unknown host gets nothing, and a broken request is refused.
	if _, err := obtain(client("stranger"), "stranger"); err == nil {
		t.Error("a certificate for a host the server does not know")
	}
	if _, err := client("web2").Cert(ctx, []byte("not a request")); err == nil || !strings.Contains(err.Error(), "not a PEM") {
		t.Errorf("a broken request: %v", err)
	}
	// Without the requirement, tokens keep working next to certificates.
	s.RequireCert = false
	if _, err := client("web2").Catalog(ctx, facts); err != nil {
		t.Errorf("token when certificates are optional: %v", err)
	}
	// What the server knows of certificates can be listed, and a host's
	// certificate revoked: it then gets a new one with its token.
	list := func() CertList {
		t.Helper()
		b, err := admin.Get(ctx, "/v1/certs", APIToken(secret, "read"))
		if err != nil {
			t.Fatal(err)
		}
		var l CertList
		json.Unmarshal(b, &l)
		return l
	}
	if l := list(); len(l.Certificates) != 1 || l.Certificates[0].Host != "web1" || l.Certificates[0].Expired || l.Certificates[0].Serial == "" || l.Authority != "vault" {
		t.Errorf("list: %+v", l)
	}
	before := len(fv.revoked)
	if _, err := admin.Send(ctx, http.MethodDelete, "/v1/certs/web1", APIToken(secret, "read"), nil); err == nil {
		t.Error("the read token could revoke")
	}
	if _, err := admin.Send(ctx, http.MethodDelete, "/v1/certs/web1", AdminToken(secret), nil); err != nil {
		t.Fatal(err)
	}
	if len(fv.revoked) != before+1 || len(list().Certificates) != 0 {
		t.Errorf("after revoking: %d revoked, list %+v", len(fv.revoked)-before, list())
	}
	if _, err := admin.Send(ctx, http.MethodDelete, "/v1/certs/web1", AdminToken(secret), nil); err == nil || !strings.Contains(err.Error(), "no certificate on record") {
		t.Errorf("revoking twice: %v", err)
	}
	// Where tokens still count, a host showing a certificate that was
	// revoked gets in on its token - and is told its certificate is not
	// worth anything, so that it asks for a new one.
	rebuilt.tr.CloseIdleConnections()
	rebuilt.CertRefused()
	if _, err := rebuilt.Catalog(ctx, facts); err != nil || !rebuilt.CertRefused() {
		t.Errorf("a revoked certificate where tokens count: err %v, told: no", err)
	}
	again := client("web1")
	if _, err := obtain(again, "web1"); err != nil {
		t.Errorf("a new certificate after revoking: %v", err)
	}
	_ = bytes.MinRead
}

// Vault is prepared with an administrator's token: what is missing is
// made, what is there is kept, and the server gets a token of its own.
func TestVaultSetup(t *testing.T) {
	fv, vts := newFakeVault(t)
	fv.noCA = true
	ctx := context.Background()
	admin, _ := NewVault(vts.URL, "s.admin", "", "", "", "", "")
	token, did, err := admin.Setup(ctx, VaultSetup{TTL: "48h", Domains: []string{"example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"POST sys/mounts/pki", "POST pki/root/generate/internal", "POST pki/roles/fleetreeve anytrue [example.com]", "PUT sys/policies/acl/fleetreeve-pki-fleetreeve", "POST auth/token/create-orphan"}
	for i, w := range want {
		if i >= len(fv.calls) || !strings.HasPrefix(fv.calls[i], w) {
			t.Fatalf("calls:\n got %q\nwant %q", fv.calls, want)
		}
	}
	if token != "s.test" || len(did) != 5 || !strings.Contains(did[0], "mounted") || !strings.Contains(did[2], "example.com") {
		t.Errorf("token %q, did %q", token, did)
	}
	// The token it made works for what the server does.
	server, _ := NewVault(vts.URL, token, "", "", "", "", "")
	if err := server.Check(ctx); err != nil {
		t.Error(err)
	}
	if err := server.TestSign(ctx); err != nil || len(fv.revoked) != 1 {
		t.Errorf("test signature: %v, revoked %v", err, fv.revoked)
	}
	// A second time nothing is mounted or generated again.
	fv.mounted, fv.calls = true, nil
	if _, did, err = admin.Setup(ctx, VaultSetup{}); err != nil || !strings.Contains(did[0], "there already") || !strings.Contains(did[1], "there already") || len(fv.calls) != 3 {
		t.Errorf("second setup: %v %q %q", err, did, fv.calls)
	}
	// Not with a token that may not administer Vault.
	if _, _, err := server.Setup(ctx, VaultSetup{}); err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Errorf("setup with the server's token: %v", err)
	}
}
