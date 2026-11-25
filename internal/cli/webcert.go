package cli

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/nxtw/fleetreeve/internal/pull"
)

// `fleetreeve vault webcert` gives the web interfaces on this machine a
// certificate from Vault instead of one they signed themselves: the
// fleetreeve console and, where fleetreeve runs it (fleetreeve vault deploy), Vault's
// own interface. A browser that trusts Vault's CA - import it once,
// `fleetreeve vault ca` prints it - then trusts both, for good.
//
// The certificates are valid for 90 days. This command replaces one when
// a third of its life is left, and a timer it installs runs it daily, so
// nobody has to think of it. The fleetreeve server's own certificate is left
// alone: hosts were enrolled against it.

const (
	consoleCert   = setupTLS + "/console.crt"
	consoleKey    = setupTLS + "/console.key"
	consoleQuad   = "/etc/containers/systemd/fleetreeve-dashboard.container"
	consoleUnitF  = setupUnitDir + "/fleetreeve-dashboard.service"
	webTimerName  = "fleetreeve-webcert"
	webTimerUsage = "sudo fleetreeve vault webcert -vault-addr <url> -vault-token-file <the server's> [-vault-ca <file>] [-web-names a,b]"
)

// webNames are the names the web interfaces on this machine are reached
// by: those given, or else this machine's own.
func webNames(given string, more ...string) []string {
	names := splitList(given)
	if len(names) == 0 {
		full := fqdn()
		short, _, _ := strings.Cut(full, ".")
		names = []string{full, short}
	}
	for _, m := range more {
		short, _, _ := strings.Cut(m, ".")
		names = append(names, m, short)
	}
	names = append(names, "localhost")
	var out []string
	seen := map[string]bool{}
	for _, n := range names {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// certsIn parses the certificates in a PEM file's contents.
func certsIn(pemBytes []byte) []*x509.Certificate {
	var out []*x509.Certificate
	for block, rest := pem.Decode(pemBytes); block != nil; block, rest = pem.Decode(rest) {
		if c, err := x509.ParseCertificate(block.Bytes); err == nil && block.Type == "CERTIFICATE" {
			out = append(out, c)
		}
	}
	return out
}

// webCertState says why a certificate file needs a new certificate, or
// "" when it is good: issued by the authority, for these names, with
// more than a third of its life left.
func webCertState(file string, names []string, authority []byte, now time.Time) (why string, fromAuthority bool) {
	b, err := os.ReadFile(file)
	if err != nil {
		return "there is none yet", false
	}
	certs := certsIn(b)
	if len(certs) == 0 {
		return "the file holds no certificate", false
	}
	leaf := certs[0]
	roots, inter := x509.NewCertPool(), x509.NewCertPool()
	roots.AppendCertsFromPEM(authority)
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	// Whether it is Vault's is asked as of when it was valid, so that an
	// expired one is still told from a self-signed one.
	at := now
	if at.After(leaf.NotAfter) {
		at = leaf.NotAfter.Add(-time.Second)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, CurrentTime: at, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return "the present one is not from Vault", false
	}
	for _, n := range names {
		if leaf.VerifyHostname(n) != nil {
			return "the present one is not valid for " + n, true
		}
	}
	if left, life := leaf.NotAfter.Sub(now), leaf.NotAfter.Sub(leaf.NotBefore); left < life/3 {
		if left <= 0 {
			return "the present one has expired", true
		}
		return fmt.Sprintf("the present one runs out on %s", leaf.NotAfter.Local().Format("02-01-2006")), true
	}
	return "", true
}

// writeLike writes a file in place (keeping what it is to the system: its
// owner, mode and label), or creates it with the owner and mode of like.
func writeLike(file string, data []byte, like string, mode os.FileMode) error {
	_, statErr := os.Stat(file)
	if err := os.WriteFile(file, data, mode); err != nil {
		return err
	}
	if statErr == nil {
		return nil // it was there: it is still whose it was
	}
	if st, err := os.Stat(like); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			os.Chown(file, int(sys.Uid), int(sys.Gid))
		}
		if mode&0o044 == 0 { // a key: as private as the one it is modelled on
			os.Chmod(file, st.Mode().Perm())
		}
	}
	return nil
}

// newWebCert has Vault issue a certificate for the names and returns the
// certificate with its issuers, and the key.
func newWebCert(ctx context.Context, v *pull.Vault, names []string, authority []byte) (certPEM, keyPEM []byte, err error) {
	keyPEM, csr, err := pull.NewKeyAndCSR(names[0])
	if err != nil {
		return nil, nil, err
	}
	signed, err := v.SignWeb(ctx, string(csr), names)
	if err != nil {
		return nil, nil, err
	}
	// The certificate, then its issuers up to and including the root: a
	// server hands these out, and the file also serves as "the CA to
	// verify this server with" wherever one is asked for.
	var bundle bytes.Buffer
	bundle.WriteString(signed.Certificate)
	seen := map[string]bool{strings.TrimSpace(signed.Certificate): true}
	for _, c := range append(signed.Chain, splitPEM(authority)...) {
		if c = strings.TrimSpace(c); c != "" && !seen[c] {
			seen[c] = true
			bundle.WriteString(c + "\n")
		}
	}
	return bundle.Bytes(), keyPEM, nil
}

// splitPEM cuts a PEM bundle into its certificates.
func splitPEM(b []byte) []string {
	var out []string
	for block, rest := pem.Decode(b); block != nil; block, rest = pem.Decode(rest) {
		out = append(out, string(pem.EncodeToMemory(block)))
	}
	return out
}

// consoleDefinition is where the console's service is defined on this
// machine: a unit file or a container definition. Empty: no console here.
func consoleDefinition() string {
	for _, f := range []string{consoleQuad, consoleUnitF} {
		if _, err := os.Stat(f); err == nil {
			return f
		}
	}
	return ""
}

var (
	tlsCertFlag = regexp.MustCompile(`(-tls-cert[ =])(\S+)`)
	tlsKeyFlag  = regexp.MustCompile(`(-tls-key[ =])(\S+)`)
)

// consoleUses rewrites a console's definition to serve with the given
// certificate and key. ok is false when the definition names none.
func consoleUses(def, cert, key string) (out string, ok bool) {
	if !tlsCertFlag.MatchString(def) || !tlsKeyFlag.MatchString(def) {
		return def, false
	}
	def = tlsCertFlag.ReplaceAllString(def, "${1}"+cert)
	return tlsKeyFlag.ReplaceAllString(def, "${1}"+key), true
}

// deployedVaultAddr is the address of the Vault that fleetreeve runs on this
// machine, or "".
func deployedVaultAddr() string {
	b, err := os.ReadFile(vaultQuadlet)
	if err != nil {
		return ""
	}
	if m := regexp.MustCompile(`-vault-addr (\S+)`).FindSubmatch(b); m != nil {
		return string(m[1])
	}
	return ""
}

// webTimerUnits are the service and timer that keep the certificates fresh.
func webTimerUnits(bin string, args []string) (service, timer string) {
	service = fmt.Sprintf(`# Written by fleetreeve vault webcert: renews the web interfaces' certificates
# from Vault when a third of their life is left.
[Unit]
Description=fleetreeve: certificates for the web interfaces, from Vault
After=network-online.target fleetreeve-vault.service
Wants=network-online.target

[Service]
Type=oneshot
Environment=FLEETREEVE_CONFIG=none
ExecStart=%s vault webcert %s
`, bin, strings.Join(args, " "))
	timer = `# Written by fleetreeve vault webcert.
[Unit]
Description=fleetreeve: check the web interfaces' certificates daily

[Timer]
OnCalendar=daily
RandomizedDelaySec=1h
Persistent=true

[Install]
WantedBy=timers.target
`
	return
}

// webcert does the work of `fleetreeve vault webcert` and reports what it did.
// It is quiet about what needed nothing.
func webcert(ctx context.Context, o *options, say func(format string, a ...any)) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("this writes certificates and restarts services: run it with sudo")
	}
	if o.vaultAddr == "" {
		o.vaultAddr = os.Getenv("VAULT_ADDR")
	}
	if o.vaultAddr == "" || o.vaultTokenFile == "" {
		return fmt.Errorf("usage: %s", webTimerUsage)
	}
	// The Vault that runs here is verified with its own certificate file,
	// whatever copy of it the options name.
	ownVault := vaultDeployed() && deployedVaultAddr() == o.vaultAddr
	if ownVault {
		o.vaultCA = vaultCert
	}
	token, err := serverVaultToken(o)
	if err != nil {
		return err
	}
	run := func(args ...string) error {
		out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
		}
		return nil
	}
	reloadVault := func() error {
		// Vault reads its certificate again on a hangup signal; if it
		// does not take to that, it is restarted (and unseals itself).
		v, err := deployedVault(o.vaultAddr, "")
		if err != nil {
			return err
		}
		if run("podman", "kill", "--signal", "HUP", "fleetreeve-vault") == nil {
			for i := 0; i < 10; i++ {
				if _, _, err := v.SealStatus(ctx); err == nil {
					return nil
				}
				time.Sleep(time.Second)
			}
		}
		if err := run("systemctl", "restart", "fleetreeve-vault.service"); err != nil {
			return err
		}
		if _, _, err := waitForVault(ctx, v, 60*time.Second); err != nil {
			return err
		}
		return waitReady(ctx, v)
	}
	// A Vault here whose certificate ran out cannot be asked for a new
	// one: it first gets one of its own making again, as when deployed.
	if ownVault {
		if b, err := os.ReadFile(vaultCert); err == nil {
			if certs := certsIn(b); len(certs) > 0 && time.Now().After(certs[0].NotAfter) {
				u, _ := url.Parse(o.vaultAddr)
				c, k, err := selfSigned(u.Hostname())
				if err != nil {
					return err
				}
				if err := writeLike(vaultKey, k, vaultKey, 0o600); err != nil {
					return err
				}
				if err := writeLike(vaultCert, c, vaultCert, 0o644); err != nil {
					return err
				}
				if err := reloadVault(); err != nil {
					return err
				}
				say("Vault's certificate had expired: it has a temporary one of its own making, to get a new one with")
			}
		}
	}
	v, err := vaultClient(o, token)
	if err != nil {
		return err
	}
	authority, err := v.CAChain(ctx)
	if err != nil {
		return err
	}
	u, _ := url.Parse(o.vaultAddr)
	var names []string
	if ownVault {
		names = webNames(o.webNames, u.Hostname())
	} else {
		names = webNames(o.webNames)
	}
	denied := func(err error) error {
		return fmt.Errorf("Vault did not issue a certificate for %s: %v\n       (the role %s-web and the server's leave to use it are made by `fleetreeve setup`, or by\n        `fleetreeve vault setup -web-names %s` with an administrator's token)", strings.Join(names, ", "), err, firstOf(o.vaultRole, "fleetreeve"), strings.Join(names, ","))
	}

	// The console.
	if def := consoleDefinition(); def != "" {
		why, _ := webCertState(consoleCert, names, authority, time.Now())
		if why != "" {
			cert, key, err := newWebCert(ctx, v, names, authority)
			if err != nil {
				return denied(err)
			}
			if err := writeLike(consoleKey, key, filepath.Join(setupTLS, "server.key"), 0o600); err != nil {
				return err
			}
			if err := writeLike(consoleCert, cert, filepath.Join(setupTLS, "server.crt"), 0o644); err != nil {
				return err
			}
		}
		old, err := os.ReadFile(def)
		if err != nil {
			return err
		}
		text, ok := consoleUses(string(old), consoleCert, consoleKey)
		switch {
		case !ok:
			say("! The console's definition (%s) names no -tls-cert and -tls-key: its certificate is in %s and %s for you to point it at", def, consoleCert, consoleKey)
		case text != string(old) || why != "":
			if text != string(old) {
				if err := os.WriteFile(def, []byte(text), 0o644); err != nil {
					return err
				}
				if err := run("systemctl", "daemon-reload"); err != nil {
					return err
				}
			}
			if err := run("systemctl", "restart", "fleetreeve-dashboard.service"); err != nil {
				os.WriteFile(def, old, 0o644)
				run("systemctl", "daemon-reload")
				run("systemctl", "restart", "fleetreeve-dashboard.service")
				return fmt.Errorf("the console does not start with its new certificate (%v); its definition was put back", err)
			}
			say("The console has a certificate from Vault, valid 90 days, for %s (%s)", strings.Join(names, ", "), why)
		}
	}

	// Vault's own interface, where fleetreeve runs Vault.
	if ownVault {
		why, wasVaults := webCertState(vaultCert, names, authority, time.Now())
		if why != "" {
			cert, key, err := newWebCert(ctx, v, names, authority)
			if err != nil {
				return denied(err)
			}
			oldCert, _ := os.ReadFile(vaultCert)
			oldKey, _ := os.ReadFile(vaultKey)
			if err := writeLike(vaultKey, key, vaultKey, 0o600); err != nil {
				return err
			}
			if err := writeLike(vaultCert, cert, vaultCert, 0o644); err != nil {
				return err
			}
			if err := reloadVault(); err != nil {
				writeLike(vaultKey, oldKey, vaultKey, 0o600)
				writeLike(vaultCert, oldCert, vaultCert, 0o644)
				run("systemctl", "restart", "fleetreeve-vault.service")
				return fmt.Errorf("Vault does not come back with its new certificate (%v); the old one was put back", err)
			}
			say("Vault's own interface has a certificate from its CA, valid 90 days (%s)", why)
			// What verified Vault with the certificate it made itself
			// now needs its CA: the copy a fleetreeve server in a container
			// reads, and that server itself, which read it when it started.
			if !wasVaults {
				if _, err := os.Stat(ctrCA); err == nil {
					if err := writeLike(ctrCA, cert, ctrCA, 0o644); err != nil {
						return err
					}
				}
				if exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", "fleetreeve-server.service").Run() == nil {
					if err := run("systemctl", "restart", "fleetreeve-server.service"); err != nil {
						return err
					}
					say("The fleetreeve server was restarted, to verify Vault by its CA from now on")
				}
			}
		}
	}

	// The timer that does this again.
	args := []string{"-vault-addr", o.vaultAddr, "-vault-token-file", o.vaultTokenFile}
	if o.vaultCA != "" {
		args = append(args, "-vault-ca", o.vaultCA)
	}
	for flag, val := range map[string]string{"-vault-pki": o.vaultPKI, "-vault-role": o.vaultRole} {
		if val != "" && val != map[string]string{"-vault-pki": "pki", "-vault-role": "fleetreeve"}[flag] {
			args = append(args, flag, val)
		}
	}
	if o.webNames != "" {
		args = append(args, "-web-names", strings.Join(splitList(o.webNames), ","))
	}
	service, timer := webTimerUnits(selfPath(), args)
	changed := false
	for file, body := range map[string]string{webTimerName + ".service": service, webTimerName + ".timer": timer} {
		p := filepath.Join(setupUnitDir, file)
		if old, _ := os.ReadFile(p); string(old) != body {
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				return err
			}
			changed = true
		}
	}
	if changed {
		if err := run("systemctl", "daemon-reload"); err != nil {
			return err
		}
		if err := run("systemctl", "enable", "--now", webTimerName+".timer"); err != nil {
			return err
		}
		say("A daily check renews them in time (systemctl list-timers %s)", webTimerName)
	}
	return nil
}

// webcertQuietly is for setup: it does the work, and a failure is a
// warning - the interfaces keep the certificates they have.
func webcertQuietly(ctx context.Context, o *options, ans *setupAnswers, tokenFile string) {
	wo := *o
	wo.vaultAddr, wo.vaultPKI, wo.vaultRole, wo.vaultCA, wo.vaultTokenFile = ans.VaultAddr, ans.VaultPKI, ans.VaultRole, ans.VaultCA, tokenFile
	wo.webNames = strings.Join(setupWebNames(o, ans), ",")
	if err := webcert(ctx, &wo, func(format string, a ...any) { step(format, a...) }); err != nil {
		fmt.Printf("  ! The web interfaces keep their own certificates: %v\n", err)
	}
}

// setupWebNames are the names setup asks certificates for: the server's
// name as answered, and this machine's own.
func setupWebNames(o *options, ans *setupAnswers) []string {
	if o.webNames != "" {
		return webNames(o.webNames)
	}
	full := fqdn()
	short, _, _ := strings.Cut(full, ".")
	names := webNames(strings.Join([]string{ans.Name, full, short}, ","))
	if ans.VaultDeploy {
		if u, err := url.Parse(ans.VaultAddr); err == nil {
			names = webNames(strings.Join(names, ","), u.Hostname())
		}
	}
	return names
}

// webcertOff undoes webcert where fleetreeve no longer uses Vault: the timer
// goes, and the console serves with the server's certificate again.
// Vault's own interface, if Vault still runs here, keeps the certificate
// it has until that runs out.
func webcertOff(ctx context.Context) {
	run := func(args ...string) error { return exec.CommandContext(ctx, args[0], args[1:]...).Run() }
	reload := false
	if _, err := os.Stat(filepath.Join(setupUnitDir, webTimerName+".timer")); err == nil {
		run("systemctl", "disable", "--now", webTimerName+".timer")
		os.Remove(filepath.Join(setupUnitDir, webTimerName+".timer"))
		os.Remove(filepath.Join(setupUnitDir, webTimerName+".service"))
		reload = true
	}
	if def := consoleDefinition(); def != "" {
		if old, err := os.ReadFile(def); err == nil && strings.Contains(string(old), consoleCert) {
			text, _ := consoleUses(string(old), filepath.Join(setupTLS, "server.crt"), filepath.Join(setupTLS, "server.key"))
			if os.WriteFile(def, []byte(text), 0o644) == nil {
				run("systemctl", "daemon-reload")
				run("systemctl", "restart", "fleetreeve-dashboard.service")
				reload = false
				step("The console serves with the server's certificate again")
			}
		}
	}
	if reload {
		run("systemctl", "daemon-reload")
	}
}
