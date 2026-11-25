package pull

import (
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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Host certificates.
//
// With a certificate authority configured (see Vault), a host proves who
// it is with a certificate instead of its token:
//
//   - On its first run the host makes a key - which never leaves it - and
//     asks the server for a certificate, showing its token. The server has
//     the authority sign the request, for exactly that host's name.
//   - From then on the host presents the certificate on every connection,
//     and renews it by itself well before it expires, with a new key.
//   - The token is good for that first certificate only. Once a host has
//     one, a certificate can only be had by showing the current one - or
//     by an administrator starting over (re-enrolling the host).
//   - The server remembers the serial number of each host's current
//     certificate and of the one before it. Any other certificate for the
//     name is refused, however valid, so a copy of an old one is useless
//     and replacing a certificate needs no revocation list.
//
// With RequireCert the token alone opens nothing but that first request.

// certRecord is what the server knows of a host's certificates.
type certRecord struct {
	Serial   string `json:"serial"`
	NotAfter string `json:"not_after"`
	Issued   string `json:"issued"`
	Previous string `json:"previous,omitempty"` // accepted until the host is seen using the new one
}

var certMu sync.Mutex

func (s *Server) certFile(host string) (string, error) {
	if filepath.Base(host) != host || strings.HasPrefix(host, ".") || host == "" {
		return "", fmt.Errorf("host name cannot be used as a file name")
	}
	return filepath.Join(s.Certs, host+".json"), nil
}

func (s *Server) certOf(host string) *certRecord {
	p, err := s.certFile(host)
	if err != nil || s.Certs == "" {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var rec certRecord
	if json.Unmarshal(b, &rec) != nil || rec.Serial == "" {
		return nil
	}
	return &rec
}

func (s *Server) storeCert(host string, rec *certRecord) error {
	p, err := s.certFile(host)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Certs, 0o750); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(p+".tmp", append(b, '\n'), 0o640); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

// ForgetCert drops what the server knows of a host's certificate, so that
// the host can start over with its token; the certificate is revoked at
// the authority when it can be. Enrolling a host again does this.
func (s *Server) ForgetCert(ctx context.Context, host string) {
	if s.CA == nil || s.Certs == "" {
		return
	}
	certMu.Lock()
	defer certMu.Unlock()
	rec := s.certOf(host)
	if rec == nil {
		return
	}
	for _, serial := range []string{rec.Serial, rec.Previous} {
		if serial != "" {
			if err := s.CA.Revoke(ctx, serial); err != nil {
				s.Log.Warn("revoking a certificate at the authority failed", "host", host, "serial", serial, "error", err.Error())
			}
		}
	}
	if p, err := s.certFile(host); err == nil {
		os.Remove(p)
	}
	s.Log.Info("certificate forgotten", "host", host)
}

func normSerial(serial string) string {
	return strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(serial, ":", ""), "-", ""))
}

// certHost returns the host a request's client certificate speaks for,
// and "" when there is none the server accepts. A certificate is accepted
// when the authority signed it (the TLS layer checked that) and it is the
// host's current one, or the one before.
func (s *Server) certHost(r *http.Request) (host, why string) {
	if s.CA == nil || r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return "", ""
	}
	cert := r.TLS.PeerCertificates[0]
	name := cert.Subject.CommonName
	serial := fmt.Sprintf("%x", cert.SerialNumber)
	certMu.Lock()
	defer certMu.Unlock()
	rec := s.certOf(name)
	switch {
	case rec == nil:
		return "", "the server has no record of a certificate for " + name + " (was the host enrolled again?)"
	case normSerial(rec.Serial) == serial || strings.TrimLeft(normSerial(rec.Serial), "0") == serial:
		if rec.Previous != "" { // the new one is in use: the old one has served
			rec.Previous = ""
			s.storeCert(name, rec)
		}
		return name, ""
	case rec.Previous != "" && (normSerial(rec.Previous) == serial || strings.TrimLeft(normSerial(rec.Previous), "0") == serial):
		return name, ""
	}
	return "", "this is not the current certificate of " + name
}

// ClientAuth makes a TLS configuration ask hosts for their certificate
// and verify it against the authority. Hosts without one still connect:
// what they may do is decided per request.
func (s *Server) ClientAuth(ctx context.Context, conf *tls.Config) error {
	if s.CA == nil {
		return nil
	}
	chain, err := s.CA.CAChain(ctx)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(chain) {
		return fmt.Errorf("the authority's CA chain holds no certificates")
	}
	conf.ClientAuth, conf.ClientCAs = tls.VerifyClientCertIfGiven, pool
	return nil
}

// issueCert answers POST /v1/cert: a certificate for a host's request.
func (s *Server) issueCert(w http.ResponseWriter, r *http.Request) {
	var req struct{ Host, CSR string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		apiError(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	fail := func(code int, msg string) {
		s.Log.Warn("certificate refused", "remote", r.RemoteAddr, "host", req.Host, "status", code, "reason", msg)
		apiError(w, msg, code)
	}
	if s.CA == nil || s.Certs == "" {
		fail(http.StatusNotFound, "this server issues no certificates")
		return
	}
	if _, _, code, msg := s.hostAllowed(req.Host); code != 0 {
		fail(code, msg)
		return
	}
	// Who is asking? The host with its current certificate (a renewal),
	// or - only while it has none - the host with its token.
	byCert, why := s.certHost(r)
	token := []byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	certMu.Lock()
	rec := s.certOf(req.Host)
	certMu.Unlock()
	current := rec != nil
	if current {
		if t, err := time.Parse(time.RFC3339, rec.NotAfter); err == nil && time.Now().After(t) {
			current = false // it expired unrenewed: the host starts over
		}
	}
	how := ""
	switch {
	case byCert != "" && byCert == req.Host:
		how = "certificate"
	case !current && hmacEqual(token, []byte(Token(s.Secret, req.Host))):
		how = "token"
	case current && hmacEqual(token, []byte(Token(s.Secret, req.Host))):
		fail(http.StatusForbidden, "this host already has a certificate, so its token no longer gets it one. If the host was rebuilt or lost its key, enrol it again with -new-cert")
		return
	default:
		if why == "" {
			why = "invalid host, token or certificate"
		}
		fail(http.StatusUnauthorized, why)
		return
	}
	block, _ := pem.Decode([]byte(req.CSR))
	if block == nil {
		fail(http.StatusBadRequest, "csr is not a PEM certificate request")
		return
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || csr.CheckSignature() != nil {
		fail(http.StatusBadRequest, "csr is not a valid certificate request")
		return
	}
	signed, err := s.CA.Sign(r.Context(), req.CSR, req.Host)
	if err != nil {
		s.Log.Error("the authority did not sign", "host", req.Host, "error", err.Error())
		apiError(w, "the certificate authority did not sign the request: "+err.Error(), http.StatusBadGateway)
		return
	}
	leaf, _ := pem.Decode([]byte(signed.Certificate))
	if leaf == nil {
		apiError(w, "the certificate authority returned no certificate", http.StatusBadGateway)
		return
	}
	cert, err := x509.ParseCertificate(leaf.Bytes)
	if err != nil || cert.Subject.CommonName != req.Host {
		s.Log.Error("the authority issued a certificate for another name", "host", req.Host)
		apiError(w, "the certificate authority issued a certificate for another name than the host's; check the PKI role", http.StatusBadGateway)
		return
	}
	next := &certRecord{Serial: fmt.Sprintf("%x", cert.SerialNumber), NotAfter: cert.NotAfter.UTC().Format(time.RFC3339), Issued: time.Now().UTC().Format(time.RFC3339)}
	certMu.Lock()
	if rec != nil && how == "certificate" {
		next.Previous = rec.Serial
	}
	err = s.storeCert(req.Host, next)
	certMu.Unlock()
	if err != nil {
		apiError(w, "storing the certificate's record failed", http.StatusInternalServerError)
		return
	}
	s.Log.Info("certificate issued", "remote", r.RemoteAddr, "host", req.Host, "shown", how, "serial", next.Serial, "not_after", next.NotAfter)
	writeJSON(w, http.StatusOK, map[string]any{"certificate": signed.Certificate + strings.Join(signed.Chain, "\n"), "not_after": next.NotAfter})
}

// NewKeyAndCSR makes a private key and a certificate request for a host,
// both as PEM.
func NewKeyAndCSR(host string) (keyPEM, csrPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: host}}, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// CertInfo is what the server knows of one host's certificate.
type CertInfo struct {
	Host     string `json:"host"`
	Serial   string `json:"serial"`
	Issued   string `json:"issued"`
	NotAfter string `json:"not_after"`
	Expired  bool   `json:"expired"`
}

// CertList answers GET /v1/certs.
type CertList struct {
	Authority    string     `json:"authority"`
	Required     bool       `json:"required"`
	Certificates []CertInfo `json:"certificates"`
}

// listCerts answers GET /v1/certs: the certificate of every host that has one.
func (s *Server) listCerts(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, false) {
		return
	}
	if s.CA == nil {
		apiError(w, "this server issues no certificates (it has no certificate authority configured)", http.StatusNotFound)
		return
	}
	out := CertList{Authority: "vault", Required: s.RequireCert, Certificates: []CertInfo{}}
	files, _ := filepath.Glob(filepath.Join(s.Certs, "*.json"))
	certMu.Lock()
	for _, f := range files {
		host := strings.TrimSuffix(filepath.Base(f), ".json")
		if rec := s.certOf(host); rec != nil {
			info := CertInfo{Host: host, Serial: vaultSerial(rec.Serial), Issued: rec.Issued, NotAfter: rec.NotAfter}
			if t, err := time.Parse(time.RFC3339, rec.NotAfter); err == nil {
				info.Expired = time.Now().After(t)
			}
			out.Certificates = append(out.Certificates, info)
		}
	}
	certMu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

// dropCert answers DELETE /v1/certs/{name}: the host's certificate is
// revoked at the authority and forgotten here. The host itself stays
// known, so at its next run it gets a new certificate with its token.
func (s *Server) dropCert(w http.ResponseWriter, r *http.Request) {
	if !s.admin(w, r) {
		return
	}
	if s.CA == nil {
		apiError(w, "this server issues no certificates (it has no certificate authority configured)", http.StatusNotFound)
		return
	}
	name := r.PathValue("name")
	certMu.Lock()
	rec := s.certOf(name)
	certMu.Unlock()
	if rec == nil {
		apiError(w, "there is no certificate on record for "+name, http.StatusNotFound)
		return
	}
	s.ForgetCert(r.Context(), name)
	w.WriteHeader(http.StatusNoContent)
}
