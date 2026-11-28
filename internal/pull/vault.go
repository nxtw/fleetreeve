package pull

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Vault is a HashiCorp Vault PKI secrets engine used as the certificate
// authority for hosts: the server asks it to sign a host's request, and
// trusts the hosts whose certificates it signed. Only the server talks to
// Vault and only the server holds a Vault token; hosts never do.
//
// It needs, in Vault: a PKI engine mounted at Mount, and in it a role
// (Role) that may issue client certificates for your host names. The
// token needs `update` on <mount>/sign/<role>, and on <mount>/revoke if
// superseded certificates are to be revoked there.
type Vault struct {
	Addr      string // https://vault.example.com:8200
	Token     string
	Mount     string // the PKI engine's mount point, default "pki"
	Role      string // the PKI role, default "fleetreeve"
	Namespace string // Vault Enterprise namespace, optional
	TTL       string // asked of Vault for each certificate, e.g. "72h"; empty: the role's default
	http      *http.Client
}

// NewVault prepares a client. caFile verifies Vault's own certificate,
// when that is not signed by an authority the system already trusts.
func NewVault(addr, token, mount, role, namespace, ttl, caFile string) (*Vault, error) {
	u, err := url.Parse(addr)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid Vault address %q (expected https://host:8200)", addr)
	}
	if token == "" {
		return nil, fmt.Errorf("Vault needs a token: -vault-token-file or $VAULT_TOKEN")
	}
	if mount == "" {
		mount = "pki"
	}
	if role == "" {
		role = "fleetreeve"
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s holds no certificates", caFile)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	}
	return &Vault{Addr: strings.TrimRight(addr, "/"), Token: token, Mount: strings.Trim(mount, "/"), Role: role, Namespace: namespace, TTL: ttl,
		http: &http.Client{Transport: tr, Timeout: 30 * time.Second}}, nil
}

func (v *Vault) call(ctx context.Context, method, path string, body any, auth bool) ([]byte, error) {
	var payload io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, v.Addr+"/v1/"+path, payload)
	if err != nil {
		return nil, err
	}
	if auth {
		req.Header.Set("X-Vault-Token", v.Token)
	}
	if v.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", v.Namespace)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := v.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Vault at %s cannot be reached: %v", v.Addr, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		var e struct{ Errors []string }
		msg := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &e) == nil && len(e.Errors) > 0 {
			msg = strings.Join(e.Errors, "; ")
		}
		return nil, fmt.Errorf("Vault refused (%s %s: %s): %.500s", method, path, resp.Status, msg)
	}
	return b, nil
}

// CAChain returns the certificates hosts' certificates are verified
// against, as PEM. It needs no token.
func (v *Vault) CAChain(ctx context.Context) ([]byte, error) {
	b, err := v.call(ctx, http.MethodGet, v.Mount+"/ca_chain", nil, false)
	if err != nil || !bytes.Contains(b, []byte("BEGIN CERTIFICATE")) {
		// A root-only engine has no chain to speak of; its CA will do.
		if b, err = v.call(ctx, http.MethodGet, v.Mount+"/ca/pem", nil, false); err != nil {
			return nil, err
		}
	}
	if !bytes.Contains(b, []byte("BEGIN CERTIFICATE")) {
		return nil, fmt.Errorf("the PKI engine at %s/%s has no CA certificate yet", v.Addr, v.Mount)
	}
	return b, nil
}

// Check tells whether the token can use the role, without issuing
// anything: it reads the role's definition when allowed to, and otherwise
// settles for the CA being there.
func (v *Vault) Check(ctx context.Context) error {
	if _, err := v.CAChain(ctx); err != nil {
		return err
	}
	b, err := v.call(ctx, http.MethodGet, "auth/token/lookup-self", nil, true)
	if err != nil {
		return fmt.Errorf("the Vault token is not accepted: %v", err)
	}
	var self struct {
		Data struct {
			TTL       int  `json:"ttl"`
			Renewable bool `json:"renewable"`
		}
	}
	if json.Unmarshal(b, &self) == nil && self.Data.TTL > 0 && self.Data.TTL < 24*3600 && !self.Data.Renewable {
		return fmt.Errorf("the Vault token expires within a day and cannot be renewed: give the server a periodic token")
	}
	return nil
}

// TokenAlive reports whether Vault still takes this token. It is false
// when Vault refuses the token, and also when Vault still answers for a
// token whose time is up: after a start Vault takes a moment to clear
// those away, and a server started in that moment would find its token
// gone the next. An error means Vault could not be asked.
func (v *Vault) TokenAlive(ctx context.Context) (bool, error) {
	b, err := v.call(ctx, http.MethodGet, "auth/token/lookup-self", nil, true)
	if err != nil {
		if strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "invalid token") {
			return false, nil
		}
		return false, err
	}
	var self struct {
		Data struct {
			ExpireTime *time.Time `json:"expire_time"`
		}
	}
	if json.Unmarshal(b, &self) == nil && self.Data.ExpireTime != nil && !self.Data.ExpireTime.After(time.Now()) {
		return false, nil
	}
	return true, nil
}

// ServerTokenPeriod is how long the server's token lives without being
// renewed. The server renews it hourly, so this is how long the server
// may be off - a machine switched off over a holiday - before its token
// is gone.
const ServerTokenPeriod = "720h"

// ServerToken makes a token for the fleetreeve server, with the policy Setup
// wrote. It takes a token that may administer Vault. orphan says whether
// the new token stands on its own, or ends when the one used here does.
func (v *Vault) ServerToken(ctx context.Context, period string) (token string, orphan bool, err error) {
	if period == "" {
		period = ServerTokenPeriod
	}
	policy := "fleetreeve-" + v.Mount + "-" + v.Role
	req := map[string]any{"policies": []string{policy}, "period": period, "display_name": "fleetreeve-server", "no_default_policy": false}
	// Not a child of the administrator's token, whose end would be its end.
	orphan = true
	b, err := v.call(ctx, http.MethodPost, "auth/token/create-orphan", req, true)
	if err != nil {
		orphan = false
		if b, err = v.call(ctx, http.MethodPost, "auth/token/create", req, true); err != nil {
			return "", false, err
		}
	}
	var resp struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		}
	}
	if json.Unmarshal(b, &resp) != nil || resp.Auth.ClientToken == "" {
		return "", false, fmt.Errorf("Vault's answer held no token")
	}
	return resp.Auth.ClientToken, orphan, nil
}

// RenewSelf extends the token's life; a periodic token stays valid as
// long as this is done within its period.
func (v *Vault) RenewSelf(ctx context.Context) error {
	_, err := v.call(ctx, http.MethodPost, "auth/token/renew-self", map[string]any{}, true)
	return err
}

// Signed is a certificate as Vault issued it.
type Signed struct {
	Certificate string   // the host's certificate, PEM
	Chain       []string // its issuers, PEM, nearest first
	Serial      string
}

// Sign has Vault sign a certificate request for a host. The name in the
// certificate is the one given here, whatever the request says.
func (v *Vault) Sign(ctx context.Context, csrPEM, commonName string) (*Signed, error) {
	body := map[string]any{"csr": csrPEM, "common_name": commonName, "format": "pem", "exclude_cn_from_sans": false}
	if v.TTL != "" {
		body["ttl"] = v.TTL
	}
	b, err := v.call(ctx, http.MethodPost, v.Mount+"/sign/"+v.Role, body, true)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data struct {
			Certificate  string   `json:"certificate"`
			IssuingCA    string   `json:"issuing_ca"`
			CAChain      []string `json:"ca_chain"`
			SerialNumber string   `json:"serial_number"`
		}
	}
	if err := json.Unmarshal(b, &resp); err != nil || resp.Data.Certificate == "" {
		return nil, fmt.Errorf("Vault's answer held no certificate")
	}
	s := &Signed{Certificate: strings.TrimSpace(resp.Data.Certificate) + "\n", Serial: resp.Data.SerialNumber, Chain: resp.Data.CAChain}
	if len(s.Chain) == 0 && resp.Data.IssuingCA != "" {
		s.Chain = []string{resp.Data.IssuingCA}
	}
	return s, nil
}

// vaultSerial writes a serial number the way Vault knows certificates
// by: pairs of hex digits separated by colons.
func vaultSerial(serial string) string {
	s := strings.ToLower(strings.NewReplacer(":", "", "-", "").Replace(serial))
	if len(s)%2 == 1 {
		s = "0" + s
	}
	var b strings.Builder
	for i := 0; i < len(s); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(s[i : i+2])
	}
	return b.String()
}

// Revoke puts a certificate on Vault's revocation list.
func (v *Vault) Revoke(ctx context.Context, serial string) error {
	_, err := v.call(ctx, http.MethodPost, v.Mount+"/revoke", map[string]any{"serial_number": vaultSerial(serial)}, true)
	return err
}

// VaultSetup says what Setup is to make sure of.
type VaultSetup struct {
	TTL     string   // lifetime of host certificates, e.g. "72h"
	Domains []string // limit certificates to these domains; empty: any name
	CAName  string   // common name of a CA created here
	Period  string   // of the token made for the server, e.g. "24h"
	// WebNames, when given, are the names this machine's web interfaces
	// are reached by: a second role is made that issues server
	// certificates for exactly those names.
	WebNames []string
}

// Setup prepares Vault for fleetreeve, with a token that may administer it:
// the PKI engine (mounted if it is not there), a CA in it (generated if
// it has none), the role, a policy that allows signing with that role
// and revoking - nothing else - and a periodic token with that policy,
// which is returned for the fleetreeve server. What already exists is kept;
// the role and the policy are rewritten. It reports what it did.
func (v *Vault) Setup(ctx context.Context, s VaultSetup) (token string, did []string, err error) {
	if s.TTL == "" {
		s.TTL = "72h"
	}
	if s.CAName == "" {
		s.CAName = "fleetreeve hosts CA"
	}
	if s.Period == "" {
		s.Period = ServerTokenPeriod
	}
	// The engine.
	b, err := v.call(ctx, http.MethodGet, "sys/mounts", nil, true)
	if err != nil {
		return "", did, fmt.Errorf("this token may not look at Vault's secrets engines; it takes an administrator's token to set Vault up: %v", err)
	}
	var mounts map[string]json.RawMessage
	json.Unmarshal(b, &mounts)
	if data, ok := mounts["data"]; ok { // newer answers nest the list
		var inner map[string]json.RawMessage
		if json.Unmarshal(data, &inner) == nil && len(inner) > 0 {
			mounts = inner
		}
	}
	if raw, ok := mounts[v.Mount+"/"]; ok {
		var m struct{ Type string }
		json.Unmarshal(raw, &m)
		if m.Type != "pki" {
			return "", did, fmt.Errorf("%s/ in Vault is a %q secrets engine, not pki: choose another mount with -vault-pki", v.Mount, m.Type)
		}
		did = append(did, "PKI engine at "+v.Mount+"/ was there already")
	} else {
		if _, err := v.call(ctx, http.MethodPost, "sys/mounts/"+v.Mount, map[string]any{"type": "pki", "description": "certificates for fleetreeve hosts",
			"config": map[string]any{"max_lease_ttl": "87600h"}}, true); err != nil {
			return "", did, err
		}
		did = append(did, "mounted a PKI engine at "+v.Mount+"/")
	}
	// Its CA.
	if _, err := v.CAChain(ctx); err != nil {
		if _, err := v.call(ctx, http.MethodPost, v.Mount+"/root/generate/internal", map[string]any{"common_name": s.CAName, "ttl": "87600h"}, true); err != nil {
			return "", did, err
		}
		did = append(did, fmt.Sprintf("generated a CA in it (%q, valid 10 years; its key stays in Vault)", s.CAName))
	} else {
		did = append(did, "the engine's CA was there already")
	}
	// The role: client certificates only, for keys of any kind (hosts
	// make elliptic-curve keys).
	role := map[string]any{"server_flag": false, "client_flag": true, "key_type": "any", "ttl": s.TTL, "max_ttl": s.TTL, "enforce_hostnames": false}
	if len(s.Domains) > 0 {
		role["allowed_domains"], role["allow_subdomains"], role["allow_bare_domains"] = s.Domains, true, true
	} else {
		role["allow_any_name"] = true
	}
	if _, err := v.call(ctx, http.MethodPost, v.Mount+"/roles/"+v.Role, role, true); err != nil {
		return "", did, err
	}
	names := "any name"
	if len(s.Domains) > 0 {
		names = "names in " + strings.Join(s.Domains, ", ")
	}
	did = append(did, fmt.Sprintf("role %s: client certificates for %s, valid %s", v.Role, names, s.TTL))
	// Certificates for the web interfaces on this machine, if asked for.
	if len(s.WebNames) > 0 {
		if err := v.writeWebRole(ctx, s.WebNames); err != nil {
			return "", did, err
		}
		did = append(did, fmt.Sprintf("role %s: server certificates for %s, valid 90 days (for the web interfaces)", v.webRole(), strings.Join(s.WebNames, ", ")))
	}
	// The policy and the server's token.
	policy, err := v.writePolicy(ctx)
	if err != nil {
		return "", did, err
	}
	did = append(did, "policy "+policy+": sign with those roles, and revoke")
	token, orphan, err := v.ServerToken(ctx, s.Period)
	if err != nil {
		return "", did, err
	}
	if orphan {
		did = append(did, "a token for the server (periodic: the server renews it hourly, and it lives for 30 days without that)")
	} else {
		did = append(did, "a token for the server (a child of the token used here: it ends when that one does)")
	}
	return token, did, nil
}

// TestSign proves the token can do its work: it has a throwaway request
// signed for a name nobody uses, for a minute, and revokes the result.
func (v *Vault) TestSign(ctx context.Context) error {
	_, csr, err := NewKeyAndCSR("fleetreeve-check.invalid")
	if err != nil {
		return err
	}
	ttl := v.TTL
	v.TTL = "1m"
	signed, err := v.Sign(ctx, string(csr), "fleetreeve-check.invalid")
	v.TTL = ttl
	if err != nil {
		return err
	}
	if err := v.Revoke(ctx, signed.Serial); err != nil {
		return fmt.Errorf("signing works, revoking does not: %v", err)
	}
	return nil
}

// SealStatus tells whether Vault has been initialised and whether it is
// sealed. It needs no token.
func (v *Vault) SealStatus(ctx context.Context) (initialized, sealed bool, err error) {
	b, err := v.call(ctx, http.MethodGet, "sys/seal-status", nil, false)
	if err != nil {
		return false, false, err
	}
	var s struct {
		Initialized bool `json:"initialized"`
		Sealed      bool `json:"sealed"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return false, false, fmt.Errorf("unexpected answer from Vault at %s", v.Addr)
	}
	return s.Initialized, s.Sealed, nil
}

// Init initialises a new Vault with a single unseal key, and returns
// that key and the root token. Both exist only in this answer.
func (v *Vault) Init(ctx context.Context) (unsealKey, rootToken string, err error) {
	b, err := v.call(ctx, http.MethodPost, "sys/init", map[string]any{"secret_shares": 1, "secret_threshold": 1}, false)
	if err != nil {
		return "", "", err
	}
	var r struct {
		Keys      []string `json:"keys_base64"`
		RootToken string   `json:"root_token"`
	}
	if json.Unmarshal(b, &r) != nil || len(r.Keys) != 1 || r.RootToken == "" {
		return "", "", fmt.Errorf("Vault's answer held no unseal key and root token")
	}
	return r.Keys[0], r.RootToken, nil
}

// Unseal gives Vault its unseal key.
func (v *Vault) Unseal(ctx context.Context, key string) error {
	b, err := v.call(ctx, http.MethodPost, "sys/unseal", map[string]any{"key": key}, false)
	if err != nil {
		return err
	}
	var s struct {
		Sealed bool `json:"sealed"`
	}
	if json.Unmarshal(b, &s) == nil && s.Sealed {
		return fmt.Errorf("Vault is still sealed: it wants more than this one key")
	}
	return nil
}

// Ready tells whether Vault is initialised, unsealed and active: able to
// do work.
func (v *Vault) Ready(ctx context.Context) error {
	_, err := v.call(ctx, http.MethodGet, "sys/health", nil, false)
	return err
}

// webRole is the role that issues server certificates for the web
// interfaces: the fleetreeve console and, where fleetreeve runs it, Vault's own.
func (v *Vault) webRole() string { return v.Role + "-web" }

// WebTTL is the lifetime of those certificates.
const WebTTL = "2160h"

func (v *Vault) writeWebRole(ctx context.Context, names []string) error {
	_, err := v.call(ctx, http.MethodPost, v.Mount+"/roles/"+v.webRole(), map[string]any{
		"server_flag": true, "client_flag": false, "key_type": "any", "ttl": WebTTL, "max_ttl": WebTTL,
		"allowed_domains": names, "allow_bare_domains": true, "allow_subdomains": false, "allow_glob_domains": false,
		"allow_localhost": true, "allow_ip_sans": false, "allow_any_name": false,
		// The names are the ones asked for here, not what a request says.
		"use_csr_common_name": false, "use_csr_sans": false,
	}, true)
	return err
}

// writePolicy writes what the fleetreeve server's token may do: sign with the
// two roles, and revoke. Tokens that have the policy follow at once.
func (v *Vault) writePolicy(ctx context.Context) (string, error) {
	policy := "fleetreeve-" + v.Mount + "-" + v.Role
	rules := fmt.Sprintf("# Written by fleetreeve: what the fleetreeve server may do.\npath \"%[1]s/sign/%[2]s\" { capabilities = [\"update\"] }\npath \"%[1]s/sign/%[3]s\" { capabilities = [\"update\"] }\npath \"%[1]s/revoke\" { capabilities = [\"update\"] }\n", v.Mount, v.Role, v.webRole())
	_, err := v.call(ctx, http.MethodPut, "sys/policies/acl/"+policy, map[string]any{"policy": rules}, true)
	return policy, err
}

// WebRole makes (or rewrites) the role for the web interfaces' server
// certificates, for exactly these names, and lets the server's token use
// it. It takes a token that may administer Vault.
func (v *Vault) WebRole(ctx context.Context, names []string) error {
	if err := v.writeWebRole(ctx, names); err != nil {
		return err
	}
	_, err := v.writePolicy(ctx)
	return err
}

// SignWeb has Vault sign a server certificate for the names, the first
// of which is its common name.
func (v *Vault) SignWeb(ctx context.Context, csrPEM string, names []string) (*Signed, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("a certificate needs a name")
	}
	body := map[string]any{"csr": csrPEM, "common_name": names[0], "alt_names": strings.Join(names, ","), "format": "pem", "ttl": WebTTL}
	b, err := v.call(ctx, http.MethodPost, v.Mount+"/sign/"+v.webRole(), body, true)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data struct {
			Certificate  string   `json:"certificate"`
			IssuingCA    string   `json:"issuing_ca"`
			CAChain      []string `json:"ca_chain"`
			SerialNumber string   `json:"serial_number"`
		}
	}
	if err := json.Unmarshal(b, &resp); err != nil || resp.Data.Certificate == "" {
		return nil, fmt.Errorf("Vault's answer held no certificate")
	}
	s := &Signed{Certificate: strings.TrimSpace(resp.Data.Certificate) + "\n", Serial: resp.Data.SerialNumber, Chain: resp.Data.CAChain}
	if len(s.Chain) == 0 && resp.Data.IssuingCA != "" {
		s.Chain = []string{resp.Data.IssuingCA}
	}
	return s, nil
}
