package cli

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/nxtw/fleetreeve/internal/pull"
)

// `fleetreeve vault` is for working with HashiCorp Vault as the certificate
// authority for hosts (see docs/certificates.md):
//
//	fleetreeve vault setup    prepare Vault: PKI engine, CA, role, policy, and a
//	                     token for the fleetreeve server (with an administrator's token)
//	fleetreeve vault check    can the server's token do its work?
//	fleetreeve vault ca       print the CA certificate(s) hosts are verified against
//	fleetreeve vault certs    the certificate of every host that has one
//	fleetreeve vault revoke   revoke a host's certificate; it gets a new one at its next run
//	fleetreeve vault deploy   run Vault on this machine, in a container (vaultdeploy.go)
//	fleetreeve vault unseal   unseal it
//
// setup, check and ca talk to Vault; certs and revoke talk to the fleetreeve server.

const vaultUsage = `usage: fleetreeve vault setup  -vault-addr <url> -vault-token-file <file to write> [-vault-pki pki] [-vault-role fleetreeve]
                          [-cert-ttl 72h] [-domains a.example.com,b.example.com] [-web-names <names of the web interfaces>]
                          (the administrator's token comes from $VAULT_TOKEN, or is asked for)
       fleetreeve vault check  -vault-addr <url> -vault-token-file <file>   can the server's token sign and revoke?
       fleetreeve vault ca     -vault-addr <url>                            print the CA certificate(s)
       fleetreeve vault certs                                               every host's certificate (asks the fleetreeve server)
       fleetreeve vault revoke <host>...                                    revoke; the host gets a new one at its next run
       sudo fleetreeve vault deploy [-vault-addr https://<this machine>:8200] [-vault-image <image>] [-vault-token-file <file>]
                                                                       run Vault here in a container; with a token file, prepare it too
       fleetreeve vault unseal                                              unseal the Vault deployed here
       sudo fleetreeve vault webcert -vault-addr <url> -vault-token-file <the server's> [-web-names a,b]
                                                                       certificates from Vault for the web console and Vault's interface
       ($VAULT_ADDR, $VAULT_CACERT and $VAULT_NAMESPACE work as for Vault's own tools)`

// vaultClient builds a Vault client from the options, with the given token.
func vaultClient(o *options, token string) (*pull.Vault, error) {
	if o.vaultAddr == "" {
		o.vaultAddr = os.Getenv("VAULT_ADDR")
	}
	if o.vaultAddr == "" {
		return nil, fmt.Errorf("where is Vault? give -vault-addr <url> or set $VAULT_ADDR")
	}
	if o.vaultCA == "" {
		o.vaultCA = os.Getenv("VAULT_CACERT")
	}
	return pull.NewVault(o.vaultAddr, token, o.vaultPKI, o.vaultRole, os.Getenv("VAULT_NAMESPACE"), o.certTTL, o.vaultCA)
}

// serverVaultToken reads the token the fleetreeve server uses.
func serverVaultToken(o *options) (string, error) {
	if o.vaultTokenFile == "" {
		if t := os.Getenv("VAULT_TOKEN"); t != "" {
			return t, nil
		}
		return "", fmt.Errorf("which token? give -vault-token-file <file> (the server's), or set $VAULT_TOKEN")
	}
	b, err := os.ReadFile(o.vaultTokenFile)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// vaultSetup prepares Vault with an administrator's token and writes the
// token it makes for the server to tokenFile.
func vaultSetup(ctx context.Context, o *options, adminToken, tokenFile string, domains, web []string) ([]string, error) {
	v, err := vaultClient(o, adminToken)
	if err != nil {
		return nil, err
	}
	token, did, err := v.Setup(ctx, pull.VaultSetup{TTL: o.certTTL, Domains: domains, WebNames: web})
	if err != nil {
		return did, err
	}
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		return did, err
	}
	os.Chmod(tokenFile, 0o600)
	// Prove it before anyone relies on it.
	server, err := vaultClient(o, token)
	if err == nil {
		err = server.Check(ctx)
	}
	if err == nil {
		err = server.TestSign(ctx)
	}
	if err != nil {
		return did, fmt.Errorf("Vault is set up, but the server's new token does not work: %v", err)
	}
	return append(did, "checked: the server's token can sign and revoke"), nil
}

// apiClient is a client for the fleetreeve server's API with a token of the
// given role, derived from the secret (or $FLEETREEVE_API_TOKEN for reading).
func apiClient(o *options, role string) (*pull.Client, string, error) {
	if o.server == "" {
		return nil, "", fmt.Errorf("which fleetreeve server? give -server <url> (or set it in .fleetreeve.yaml)")
	}
	token := ""
	if o.secretFile != "" {
		secret, _, err := pull.LoadSecret(o.secretFile, false)
		if err != nil {
			return nil, "", err
		}
		token = pull.APIToken(secret, role)
	} else if role == "read" {
		token = os.Getenv("FLEETREEVE_API_TOKEN")
	}
	if token == "" {
		return nil, "", fmt.Errorf("this needs the server's secret: -secret-file <file> (or set it in .fleetreeve.yaml)")
	}
	c, err := pull.NewClient(o.server, "", "", o.ca, o.insecure)
	return c, token, err
}

func vaultCmd(ctx context.Context, o *options) error {
	if len(o.args) == 0 {
		return fmt.Errorf("%s", vaultUsage)
	}
	switch verb := o.args[0]; verb {
	case "setup":
		if o.vaultTokenFile == "" {
			return fmt.Errorf("where should the server's token go? give -vault-token-file <file> (for example /etc/fleetreeve/vault.token)")
		}
		admin := os.Getenv("VAULT_TOKEN")
		if admin == "" {
			a := &asker{in: bufio.NewReader(os.Stdin)}
			admin = a.secret("A Vault token that may administer it (it is used now and not kept)")
		}
		if admin == "" {
			return fmt.Errorf("setting Vault up takes an administrator's token: set $VAULT_TOKEN or enter one")
		}
		did, err := vaultSetup(ctx, o, admin, o.vaultTokenFile, splitList(o.domains), splitList(o.webNames))
		for _, line := range did {
			fmt.Println("  ✔ " + line)
		}
		if err != nil {
			return err
		}
		fmt.Printf("\nThe server's token is in %s (readable by its owner only; make that the user the server runs as).\nStart the server with:\n  fleetreeve serve … -vault-addr %s -vault-token-file %s", o.vaultTokenFile, o.vaultAddr, o.vaultTokenFile)
		for flag, val := range map[string]string{"-vault-pki": o.vaultPKI, "-vault-role": o.vaultRole} {
			if val != "" {
				fmt.Printf(" %s %s", flag, val)
			}
		}
		fmt.Println("\nor run `sudo fleetreeve setup`, which asks for these.")
		return nil

	case "deploy":
		return vaultDeployCmd(ctx, o)

	case "webcert":
		return webcert(ctx, o, func(format string, a ...any) { fmt.Printf("  ✔ "+format+"\n", a...) })

	case "unseal":
		return vaultUnseal(ctx, o)

	case "check":
		token, err := serverVaultToken(o)
		if err != nil {
			return err
		}
		v, err := vaultClient(o, token)
		if err != nil {
			return err
		}
		chain, err := v.CAChain(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("  ✔ Vault answers at %s\n", o.vaultAddr)
		for block, rest := pem.Decode(chain); block != nil; block, rest = pem.Decode(rest) {
			if c, err := x509.ParseCertificate(block.Bytes); err == nil {
				left := time.Until(c.NotAfter)
				note := fmt.Sprintf("valid until %s", c.NotAfter.Format("02-01-2006"))
				if left < 30*24*time.Hour {
					note += " - THAT IS SOON"
				}
				fmt.Printf("  ✔ CA at %s/: %s (%s)\n", v.Mount, c.Subject.CommonName, note)
			}
		}
		if err := v.Check(ctx); err != nil {
			return err
		}
		fmt.Println("  ✔ the token is accepted, and will not run out on the server")
		if err := v.TestSign(ctx); err != nil {
			return fmt.Errorf("the token cannot do the server's work with role %s: %v", v.Role, err)
		}
		fmt.Printf("  ✔ it can sign with role %s, and revoke (tried with a throwaway certificate)\n", v.Role)
		return nil

	case "ca":
		v, err := vaultClient(o, "-") // reading the CA needs no token
		if err != nil {
			return err
		}
		chain, err := v.CAChain(ctx)
		if err != nil {
			return err
		}
		fmt.Print(strings.TrimSpace(string(chain)) + "\n")
		return nil

	case "certs":
		c, token, err := apiClient(o, "read")
		if err != nil {
			return err
		}
		b, err := c.Get(ctx, "/v1/certs", token)
		if err != nil {
			return err
		}
		var list pull.CertList
		if err := json.Unmarshal(b, &list); err != nil {
			return fmt.Errorf("unexpected answer from the server")
		}
		must := "Hosts may still use their token instead (the server runs without -require-cert)."
		if list.Required {
			must = "Hosts must show their certificate."
		}
		if len(list.Certificates) == 0 {
			fmt.Printf("No host has a certificate yet; each gets one at its next run. %s\n", must)
			return nil
		}
		width := 4
		for _, ci := range list.Certificates {
			width = max(width, len(ci.Host))
		}
		for _, ci := range list.Certificates {
			until := ci.NotAfter
			if t, err := time.Parse(time.RFC3339, ci.NotAfter); err == nil {
				until = t.Local().Format("02-01-2006 15:04")
			}
			state := "valid until " + until
			if ci.Expired {
				state = "EXPIRED " + until + " (the host gets a new one at its next run)"
			}
			fmt.Printf("%-*s  %s  %s\n", width, ci.Host, state, ci.Serial)
		}
		fmt.Printf("%d host(s) with a certificate. %s\n", len(list.Certificates), must)
		return nil

	case "revoke":
		if len(o.args) < 2 {
			return fmt.Errorf("usage: fleetreeve vault revoke <host>...")
		}
		c, token, err := apiClient(o, "admin")
		if err != nil {
			return err
		}
		failed := 0
		for _, host := range o.args[1:] {
			if _, err := c.Send(ctx, http.MethodDelete, "/v1/certs/"+url.PathEscape(host), token, nil); err != nil {
				fmt.Fprintf(os.Stderr, "fleetreeve: %s: %v\n", host, err)
				failed++
				continue
			}
			fmt.Printf("%s: certificate revoked in Vault and no longer accepted. The host gets a new one at its next run.\n", host)
		}
		if failed > 0 {
			return fmt.Errorf("%d host(s) could not be revoked", failed)
		}
		fmt.Println("To keep a host out for good, remove it from the inventory (or `fleetreeve enroll -remove` it).")
		return nil
	}
	return fmt.Errorf("unknown: fleetreeve vault %s\n%s", o.args[0], vaultUsage)
}
