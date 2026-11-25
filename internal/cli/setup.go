package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	osuser "os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nxtw/fleetreeve/internal/dashboard"
	"github.com/nxtw/fleetreeve/internal/pull"
)

// `fleetreeve setup` turns a machine into a fleetreeve server by asking a few
// questions: the server and the web console as systemd services, a
// certificate, the server's own user and key, a first console account, a
// starting configuration, and the machine itself as the first host.
// Everything it asks has a default; -yes takes them all. Running it again
// is safe: what exists (secret, certificate, key, accounts, your
// configuration) is kept.

const (
	setupData      = "/var/lib/fleetreeve-server"
	setupTLS       = "/etc/fleetreeve/tls"
	setupConsole   = "/etc/fleetreeve/dashboard"
	setupEnv       = "/etc/fleetreeve/dashboard.env"
	setupUnitDir   = "/etc/systemd/system"
	setupContainer = "/etc/containers/systemd/fleetreeve-server.container"
)

// asker puts questions to the person at the terminal.
type asker struct {
	in  *bufio.Reader
	yes bool // take every default
}

func (a *asker) line() string {
	s, _ := a.in.ReadString('\n')
	return strings.TrimSpace(s)
}

// ask returns the answer to a question, or the default on an empty one.
func (a *asker) ask(question, def string) string {
	if a.yes {
		return def
	}
	if def != "" {
		fmt.Printf("%s [%s]: ", question, def)
	} else {
		fmt.Printf("%s: ", question)
	}
	if s := a.line(); s != "" {
		return s
	}
	return def
}

func (a *asker) confirm(question string, def bool) bool {
	hint := "Y/n"
	if !def {
		hint = "y/N"
	}
	for {
		if a.yes {
			return def
		}
		fmt.Printf("%s [%s]: ", question, hint)
		switch strings.ToLower(a.line()) {
		case "":
			return def
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
		fmt.Println("  Please answer y or n.")
	}
}

// choose returns the index of the option picked.
func (a *asker) choose(question string, options []string, def int) int {
	if a.yes {
		return def
	}
	fmt.Println(question)
	for i, o := range options {
		fmt.Printf("  %d) %s\n", i+1, o)
	}
	for {
		fmt.Printf("Choice [%d]: ", def+1)
		s := a.line()
		if s == "" {
			return def
		}
		if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= len(options) {
			return n - 1
		}
		fmt.Printf("  Please enter a number from 1 to %d.\n", len(options))
	}
}

// secret asks for a password without showing it, when on a terminal.
func (a *asker) secret(question string) string {
	if a.yes {
		return ""
	}
	fmt.Printf("%s: ", question)
	st, err := os.Stdin.Stat()
	tty := err == nil && st.Mode()&os.ModeCharDevice != 0
	if tty {
		quiet := exec.Command("stty", "-echo")
		quiet.Stdin = os.Stdin
		quiet.Run()
		defer func() {
			loud := exec.Command("stty", "echo")
			loud.Stdin = os.Stdin
			loud.Run()
			fmt.Println()
		}()
	}
	return a.line()
}

// setupAnswers is everything the questions decide.
type setupAnswers struct {
	Name     string // the name hosts reach the server by
	Port     int
	Dir      string // the configuration
	CertFile string // an existing certificate and key; empty: make one
	KeyFile  string
	Console  bool
	ConPort  int
	Admin    string
	Password string // empty: generate
	Powers   int    // 0 look only, 1 run and roll back, 2 also shell commands
	Identity string
	Firewall bool
	Self     bool // manage this machine as the first host

	// HashiCorp Vault as certificate authority for hosts (optional).
	Vault       bool
	VaultAddr   string
	VaultAdmin  string // an administrator's token: set Vault up with it. Empty: VaultToken is the server's
	VaultToken  string // file with a token made for the server earlier
	VaultPKI    string
	VaultRole   string
	VaultCA     string // to verify Vault itself, optional
	VaultDeploy bool   // run Vault on this machine, in a container
	Container   bool   // run the server and the console in podman containers
	RequireCert bool
}

const setupVaultToken = "/etc/fleetreeve/vault.token"

func step(format string, a ...any) { fmt.Printf("  ✔ "+format+"\n", a...) }

func fqdn() string {
	if out, err := exec.Command("hostname", "-f").Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		return strings.TrimSpace(string(out))
	}
	h, _ := os.Hostname()
	return h
}

// outboundIP is the address this machine uses to reach others.
func outboundIP() string {
	c, err := net.Dial("udp", "192.0.2.1:9") // nothing is sent
	if err != nil {
		return ""
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String()
}

func validPort(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("%q is not a port number", s)
	}
	return n, nil
}

func firewalldRunning() bool {
	return exec.Command("firewall-cmd", "--state").Run() == nil
}

// questions asks everything, with defaults that suit a first server.
func questions(a *asker, o *options) (*setupAnswers, error) {
	ans := &setupAnswers{}
	var err error
	fmt.Println("\nThe server")
	// How it runs is asked once: afterwards it is what is there.
	if def, err := os.ReadFile(setupContainer); err == nil {
		ans.Container = strings.Contains(string(def), ctrWritten)
	} else if _, err := os.Stat(filepath.Join(setupUnitDir, "fleetreeve-server.service")); err != nil {
		mode := 0
		if o.container {
			mode = 1
		}
		ans.Container = a.choose("How should the server and the console run?", []string{
			"As ordinary systemd services",
			"In containers (podman), managed by systemd: an image is made here from this fleetreeve",
		}, mode) == 1
	}
	if ans.Container {
		if _, err := exec.LookPath("podman"); err != nil {
			return nil, fmt.Errorf("running in containers takes podman, which this machine does not have (dnf install podman)")
		}
	}
	ans.Name = a.ask("Name the hosts will reach this server by (it must resolve on them)", fqdn())
	if ans.Name == "" || strings.ContainsAny(ans.Name, " /:") {
		return nil, fmt.Errorf("%q is not a host name", ans.Name)
	}
	if ans.Port, err = validPort(a.ask("Port for the server", "8140")); err != nil {
		return nil, err
	}
	ans.Dir = a.ask("Directory for your configuration (manifests, inventory, data)", "/srv/fleetreeve")
	if !filepath.IsAbs(ans.Dir) {
		return nil, fmt.Errorf("%s: give the directory as a full path", ans.Dir)
	}

	fmt.Println("\nEncryption")
	if _, err := os.Stat(filepath.Join(setupTLS, "server.crt")); err == nil {
		fmt.Printf("  There is a certificate in %s already; it is kept.\n", setupTLS)
	} else if a.choose("Hosts and browsers talk to the server over HTTPS. Its certificate:", []string{
		"Make one here (self-signed, valid 10 years; hosts get it when they are enrolled)",
		"Use a certificate and key I already have",
	}, 0) == 1 {
		ans.CertFile = a.ask("Certificate file (PEM, with any intermediates)", "")
		ans.KeyFile = a.ask("Its private key file (PEM)", "")
		for _, f := range []string{ans.CertFile, ans.KeyFile} {
			if _, err := os.Stat(f); err != nil {
				return nil, fmt.Errorf("%s cannot be read: %v", f, err)
			}
		}
	}

	fmt.Println("\nThe web console")
	if ans.Console = a.confirm("Install the web console", true); ans.Console {
		if ans.ConPort, err = validPort(a.ask("Port for the console", "8141")); err != nil {
			return nil, err
		}
		if ans.ConPort == ans.Port {
			return nil, fmt.Errorf("the console and the server need different ports")
		}
		users, _ := dashboard.LoadUsers(filepath.Join(setupConsole, "users.yaml"))
		if len(users) > 0 {
			fmt.Printf("  There are %d console account(s) already; they are kept.\n", len(users))
		} else {
			def := os.Getenv("SUDO_USER")
			if def == "" || def == "root" {
				def = "admin"
			}
			for {
				ans.Admin = strings.ToLower(a.ask("Name of the first administrator account", def))
				if dashboard.ValidUserName(ans.Admin) {
					break
				}
				if a.yes {
					return nil, fmt.Errorf("%q cannot be used as an account name", ans.Admin)
				}
				fmt.Println("  Use lower-case letters, digits, dot, dash or underscore.")
			}
			for {
				ans.Password = a.secret(fmt.Sprintf("Password for %s (at least %d characters; empty: one is made up and shown at the end)", ans.Admin, dashboard.MinPassword))
				if ans.Password == "" {
					break
				}
				if len(ans.Password) < dashboard.MinPassword {
					fmt.Printf("  That is too short: use at least %d characters.\n", dashboard.MinPassword)
					continue
				}
				if a.secret("Once more") == ans.Password {
					break
				}
				fmt.Println("  Those were not the same.")
			}
		}
		ans.Powers = a.choose("What may be done from the console?", []string{
			"Look only",
			"Also apply now, dry-run and roll back (for accounts with the operator role)",
			"Also run shell commands on hosts as root (for admin accounts) - only if you really want that in a browser",
		}, 1)
	}

	fmt.Println("\nAccess to hosts")
	fmt.Println("  The server logs in to hosts over SSH as a user of its own, with a key made here,")
	fmt.Println("  so that nothing depends on anyone's personal account.")
	ans.Identity = a.ask("Name of that user", defaultIdentity)
	if !validUser.MatchString(ans.Identity) {
		return nil, fmt.Errorf("%q is not a user name", ans.Identity)
	}
	ans.Self = a.confirm("Manage this machine itself as the first host", true)

	unit, _ := os.ReadFile(filepath.Join(setupUnitDir, "fleetreeve-server.service"))
	tokenFile := setupVaultToken
	if ans.Container {
		unit, _ = os.ReadFile(setupContainer)
		tokenFile = ctrToken
	}
	if err := vaultQuestions(a, o, ans, unit, tokenFile); err != nil {
		return nil, err
	}
	if firewalldRunning() {
		ans.Firewall = a.confirm("Open the port(s) in firewalld", true)
	}
	return ans, nil
}

// vaultQuestions asks whether Vault is to be the certificate authority
// for hosts, and what goes with that. unit is how the server is started
// now (a service or container definition): what it says about Vault is
// the default. tokenFile is where the server's Vault token is kept.
func vaultQuestions(a *asker, o *options, ans *setupAnswers, unit []byte, tokenFile string) error {
	fmt.Println("\nCertificates for hosts")
	fmt.Println("  Hosts prove who they are with a token. If you run HashiCorp Vault, it can be the")
	fmt.Println("  certificate authority instead: every host then gets a certificate signed by it.")
	// A server that was set up with Vault before keeps it by default.
	if unit != nil {
		was := func(flag string) string {
			if m := regexp.MustCompile(`(?m)(?:^|\s)-` + flag + ` (\S+)`).FindSubmatch(unit); m != nil {
				return string(m[1])
			}
			return ""
		}
		o.vaultAddr, o.vaultPKI, o.vaultRole, o.vaultCA = firstOf(o.vaultAddr, was("vault-addr")), firstOf(o.vaultPKI, was("vault-pki")), firstOf(o.vaultRole, was("vault-role")), firstOf(o.vaultCA, was("vault-ca"))
		if was("vault-addr") != "" {
			if _, err := os.Stat(tokenFile); err == nil && o.vaultTokenFile == "" {
				o.vaultTokenFile = tokenFile
			}
			o.requireCert = o.requireCert || regexp.MustCompile(`(?m)(?:^|\s)-require-cert\b`).Match(unit)
		}
	}
	addr := o.vaultAddr
	if addr == "" {
		addr = os.Getenv("VAULT_ADDR")
	}
	here := o.vaultDeploy || vaultDeployed()
	if ans.Vault = a.confirm("Use HashiCorp Vault as certificate authority for hosts", addr != "" || here); ans.Vault {
		where := 0
		if here {
			where = 1
		}
		ans.VaultDeploy = a.choose("Where is Vault?", []string{
			"It runs somewhere already",
			"Nowhere yet: run it on this machine, in a container (needs podman), and prepare it",
		}, where) == 1
		if ans.VaultDeploy {
			if _, err := exec.LookPath("podman"); err != nil {
				return fmt.Errorf("running Vault here takes podman, which this machine does not have (dnf install podman)")
			}
			ans.VaultAddr = a.ask("Address Vault will have (this machine's name, and a port)", firstOf(o.vaultAddr, "https://"+ans.Name+":8200"))
			ans.VaultCA = vaultCert
		} else {
			ans.VaultAddr = a.ask("Vault's address", addr)
			if ans.VaultAddr == "" {
				return fmt.Errorf("Vault needs an address (https://vault.example.com:8200)")
			}
			ans.VaultCA = a.ask("CA file to verify Vault's own certificate with (empty: the system's)", firstOf(o.vaultCA, os.Getenv("VAULT_CACERT")))
		}
		ans.VaultPKI = a.ask("PKI engine in Vault (its mount point)", firstOf(o.vaultPKI, "pki"))
		ans.VaultRole = a.ask("Role in that engine", firstOf(o.vaultRole, "fleetreeve"))
		def := 1 // set it up, unless a token for the server is at hand
		if o.vaultTokenFile != "" {
			def = 0
		}
		if ans.VaultDeploy {
			// Its token is made when Vault is there; see setup.
		} else if a.choose("The server needs a Vault token that may sign with that role:", []string{
			"I have one in a file (Vault is prepared already)",
			"Prepare Vault now with an administrator's token: the engine and role if they are missing, a policy, and a token for this server",
		}, def) == 0 {
			ans.VaultToken = a.ask("File with the server's Vault token", o.vaultTokenFile)
			if _, err := os.Stat(ans.VaultToken); err != nil {
				return fmt.Errorf("%s cannot be read: %v", ans.VaultToken, err)
			}
		} else {
			if ans.VaultAdmin = os.Getenv("VAULT_TOKEN"); ans.VaultAdmin == "" || !a.yes && !a.confirm("Use the administrator's token in $VAULT_TOKEN", true) {
				ans.VaultAdmin = a.secret("Administrator's token for Vault (used now, not kept)")
			}
			if ans.VaultAdmin == "" {
				return fmt.Errorf("preparing Vault takes an administrator's token (or set it up with `fleetreeve vault setup` and give the resulting file)")
			}
		}
		ans.RequireCert = a.confirm("Require certificates from the start (no: hosts may still use their token, which suits a fleet that is moving over)", o.requireCert)
	}
	return nil
}

// vaultApply carries out the answers about Vault: it runs Vault here if
// that was asked, and sees to it that tokenFile holds a working token
// for the server, readable by the user the server runs as only.
func vaultApply(ctx context.Context, o *options, ans *setupAnswers, tokenFile string, uid, gid int) error {
	if ans.VaultDeploy {
		did, root, err := vaultDeploy(ctx, ans.VaultAddr, o.vaultImage)
		for _, line := range did {
			step("Vault: %s", line)
		}
		if err != nil {
			return err
		}
		// A token the server has from an earlier run is kept while it
		// works; otherwise Vault is prepared with its root token.
		vo := *o
		vo.vaultAddr, vo.vaultPKI, vo.vaultRole, vo.vaultCA = ans.VaultAddr, ans.VaultPKI, ans.VaultRole, ans.VaultCA
		works := false
		if b, err := os.ReadFile(tokenFile); err == nil {
			if v, err := vaultClient(&vo, strings.TrimSpace(string(b))); err == nil && v.Check(ctx) == nil && v.TestSign(ctx) == nil {
				works = true
			}
		}
		switch {
		case works:
			ans.VaultToken = tokenFile
			// Kept from before there were certificates for the web
			// interfaces, maybe: the role for those is seen to here.
			if root != "" {
				if admin, err := vaultClient(&vo, root); err == nil {
					if err := admin.WebRole(ctx, setupWebNames(o, ans)); err != nil {
						fmt.Printf("  ! Vault: the role for the web interfaces' certificates could not be written: %v\n", err)
					}
				}
			}
		case root != "":
			ans.VaultAdmin = root
		default:
			return fmt.Errorf("Vault runs, but the server has no working token for it and Vault's root token is no longer in %s:\n       make one with `VAULT_TOKEN=<an administrator's token> fleetreeve vault setup -vault-addr %s -vault-ca %s -vault-token-file %s`, then run setup again", vaultRootToken, ans.VaultAddr, vaultCert, tokenFile)
		}
	}
	if ans.Vault {
		vo := *o
		vo.vaultAddr, vo.vaultPKI, vo.vaultRole, vo.vaultCA = ans.VaultAddr, ans.VaultPKI, ans.VaultRole, ans.VaultCA
		if ans.VaultAdmin != "" {
			did, err := vaultSetup(ctx, &vo, ans.VaultAdmin, tokenFile, nil, setupWebNames(o, ans))
			for _, line := range did {
				step("Vault: %s", line)
			}
			if err != nil {
				return err
			}
		} else {
			b, err := os.ReadFile(ans.VaultToken)
			if err != nil {
				return err
			}
			v, err := vaultClient(&vo, strings.TrimSpace(string(b)))
			if err == nil {
				if err = v.Check(ctx); err == nil {
					err = v.TestSign(ctx)
				}
			}
			if err != nil {
				return fmt.Errorf("the Vault token in %s does not do: %v", ans.VaultToken, err)
			}
			if ans.VaultToken != tokenFile {
				if err := os.WriteFile(tokenFile, b, 0o600); err != nil {
					return err
				}
			}
			step("Vault: the token can sign with role %s, and revoke", ans.VaultRole)
		}
		os.Chmod(tokenFile, 0o600)
		if err := os.Chown(tokenFile, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

// selfSigned makes a certificate that is its own authority, for name and
// every address of this machine.
func selfSigned(name string) (cert, key []byte, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, nil, err
	}
	short, _, _ := strings.Cut(name, ".")
	names := []string{name}
	for _, n := range []string{short, "localhost"} {
		if n != name {
			names = append(names, n)
		}
	}
	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
				ips = append(ips, n.IP)
			}
		}
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: name, Organization: []string{"fleetreeve"}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true, DNSNames: names, IPAddresses: ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

// firstOf returns the first string that is not empty.
func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// vaultFlags are the server's options for Vault, as unit file lines.
func vaultFlags(ans *setupAnswers) string {
	if !ans.Vault {
		return ""
	}
	flags := fmt.Sprintf("    -vault-addr %s \\\n    -vault-token-file %s \\\n    -vault-pki %s \\\n    -vault-role %s \\\n", ans.VaultAddr, setupVaultToken, ans.VaultPKI, ans.VaultRole)
	if ans.VaultCA != "" {
		flags += fmt.Sprintf("    -vault-ca %s \\\n", ans.VaultCA)
	}
	if ans.RequireCert {
		flags += "    -require-cert \\\n"
	}
	return flags
}

func serverUnit(ans *setupAnswers, bin string) string {
	return fmt.Sprintf(`# Written by fleetreeve setup. Change it with: systemctl edit fleetreeve-server
[Unit]
Description=fleetreeve server
After=network-online.target%[8]s
Wants=network-online.target%[8]s
ConditionPathExists=%[1]s/site.yaml

[Service]
User=%[2]s
Group=%[2]s
WorkingDirectory=%[1]s
ExecStart=%[3]s serve \
    -i inventory.yaml \
    -secret-file %[4]s/secret \
    -tls-cert %[5]s/server.crt \
    -tls-key %[5]s/server.key \
    -reports %[4]s/reports \
    -hosts %[4]s/hosts \
    -actions %[4]s/actions \
    -listen :%[6]d \
%[7]s    site.yaml
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=%[4]s

[Install]
WantedBy=multi-user.target
`, ans.Dir, ans.Identity, bin, setupData, setupTLS, ans.Port, vaultFlags(ans), map[bool]string{true: " fleetreeve-vault.service"}[ans.VaultDeploy])
}

func consoleUnit(ans *setupAnswers, bin string) string {
	return fmt.Sprintf(`# Written by fleetreeve setup. Its settings and tokens are in %[5]s.
[Unit]
Description=fleetreeve web console
After=network-online.target fleetreeve-server.service
Wants=network-online.target

[Service]
User=%[1]s
Group=%[1]s
EnvironmentFile=%[5]s
ExecStart=%[2]s dashboard \
    -tls-cert %[3]s/server.crt \
    -tls-key %[3]s/server.key \
    -listen :%[4]d
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=%[6]s

[Install]
WantedBy=multi-user.target
`, ans.Identity, bin, setupTLS, ans.ConPort, setupEnv, setupConsole)
}

// starter is a first configuration: small, harmless, and yours to change.
func starter(ans *setupAnswers, self, address string) map[string]string {
	url := fmt.Sprintf("https://%s:%d", ans.Name, ans.Port)
	hosts := "hosts: {}\n"
	if self != "" {
		hosts = fmt.Sprintf("hosts:\n  %s:\n    address: %s\n", self, address)
	}
	rpm := ""
	if m, _ := filepath.Glob("/usr/share/fleetreeve/rpm/*.rpm"); len(m) > 0 {
		rpm = "  rpm: /usr/share/fleetreeve/rpm          # new hosts get the client as a package\n"
	}
	return map[string]string{
		"site.yaml": `# What every host is kept to. This is a starting point: replace it with
# your own. See 'fleetreeve types' for what can be managed, and the README.
name: site

data:
  dir: data

resources:
  - file: /etc/fleetreeve-managed
    content: |
      This host is managed by fleetreeve. Its configuration comes from {{ .managed_by }}.
      Changes made by hand are put back at the next run.
    mode: 0644
`,
		"inventory.yaml": `# The hosts: which exist, and how the server reaches them over SSH. The
# server also uses this as its access list. 'fleetreeve enroll' adds hosts for
# you; list them here when you want them in groups.
defaults:
  user: ` + ans.Identity + `             # the server's own user on every host
  become: true            # changes are made through sudo

` + hosts,
		"data/common.yaml": "# Values for every host. Hosts and groups can override them in\n# data/hosts/<name>.yaml and data/groups/<name>.yaml.\nmanaged_by: " + ans.Name + "\n",
		".fleetreeve.yaml": `# Defaults for fleetreeve commands run in this directory. Options given on the
# command line still win.
run-as: ` + ans.Identity + `   # commands that connect to hosts run as the server's own user

server: ` + url + `
secret-file: ` + setupData + `/secret
ca: ` + setupTLS + `/server.crt

plan:     {inventory: inventory.yaml}
apply:    {inventory: inventory.yaml}
validate: {inventory: inventory.yaml}
lookup:   {inventory: inventory.yaml}
state:    {inventory: inventory.yaml}
facts:    {inventory: inventory.yaml}
run:      {inventory: inventory.yaml}
rollback: {inventory: inventory.yaml}

# fleetreeve enroll <host> -bootstrap-user <an existing login>
enroll:
  inventory: inventory.yaml
` + rpm,
	}
}

func setup(ctx context.Context, o *options) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("setup installs services and creates a user: run it with sudo")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("setup installs systemd services, and this machine has no systemctl")
	}
	// A container that was not set up here is its administrator's: only
	// the Vault part is offered for it.
	if def, err := os.ReadFile(setupContainer); err == nil && !strings.Contains(string(def), ctrWritten) {
		return setupInContainer(ctx, o)
	}
	a := &asker{in: bufio.NewReader(os.Stdin), yes: o.yes}
	fmt.Println("This sets up a fleetreeve server on this machine. Press Enter to accept the answer in brackets.")
	ans, err := questions(a, o)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://%s:%d", ans.Name, ans.Port)
	fmt.Printf("\nAbout to: run the server at %s as user %s%s", url, ans.Identity, map[bool]string{true: ", in a container"}[ans.Container])
	if ans.Console {
		fmt.Printf(", the console at https://%s:%d", ans.Name, ans.ConPort)
	}
	if ans.VaultDeploy {
		fmt.Printf(", Vault in a container at %s", ans.VaultAddr)
	}
	fmt.Printf(", with your configuration in %s.\n", ans.Dir)
	if !a.confirm("Go ahead", true) {
		return fmt.Errorf("nothing was changed")
	}
	fmt.Println()
	bin := selfPath()
	run := func(args ...string) error {
		out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
		}
		return nil
	}

	// The server's own user and key. Its output is the long version; here
	// one line will do.
	quiet := *o
	quiet.secretFile = ""
	stdout := os.Stdout
	os.Stdout, _ = os.Open(os.DevNull)
	err = identityInit(&quiet, ans.Identity)
	os.Stdout = stdout
	if err != nil {
		return err
	}
	u, err := osuser.Lookup(ans.Identity)
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	step("User %s, with its own SSH key pair", ans.Identity)

	// Directories.
	for dir, mode := range map[string]os.FileMode{setupData: 0o750, setupConsole: 0o700} {
		if err := os.MkdirAll(dir, mode); err != nil {
			return err
		}
		os.Chmod(dir, mode)
		if err := os.Chown(dir, uid, gid); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(setupTLS, 0o750); err != nil {
		return err
	}
	os.Chmod("/etc/fleetreeve", 0o755)
	os.Chmod(setupTLS, 0o750)
	os.Chown(setupTLS, 0, gid)

	// Certificate.
	crt, key := filepath.Join(setupTLS, "server.crt"), filepath.Join(setupTLS, "server.key")
	if _, err := os.Stat(crt); err != nil {
		var c, k []byte
		if ans.CertFile != "" {
			if c, err = os.ReadFile(ans.CertFile); err == nil {
				k, err = os.ReadFile(ans.KeyFile)
			}
		} else {
			c, k, err = selfSigned(ans.Name)
		}
		if err != nil {
			return err
		}
		if err := os.WriteFile(crt, c, 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(key, k, 0o640); err != nil {
			return err
		}
		step("Certificate for %s in %s", ans.Name, setupTLS)
	} else {
		step("Certificate in %s kept", setupTLS)
	}
	os.Chown(key, 0, gid)
	os.Chmod(key, 0o640)

	// Vault as the certificate authority: the server's token, in a file
	// only the server's user can read - made now, or brought along.
	tokenFile, vaultCA, image := setupVaultToken, false, ""
	if ans.Container {
		tokenFile = ctrToken
		if vaultCA, err = containerVaultFiles(ctx, o, ans, uid, gid); err != nil {
			return err
		}
		if image, err = buildImage(ctx, bin); err != nil {
			return err
		}
		step("Image %s made from this fleetreeve", image)
	} else if err := vaultApply(ctx, o, ans, setupVaultToken, uid, gid); err != nil {
		return err
	}

	// A first configuration, where there is none.
	self, address := "", ""
	if ans.Self {
		self, _, _ = strings.Cut(fqdn(), ".")
		if address = outboundIP(); address == "" {
			address = ans.Name
		}
	}
	if _, err := os.Stat(filepath.Join(ans.Dir, "site.yaml")); err != nil {
		for name, body := range starter(ans, self, address) {
			p := filepath.Join(ans.Dir, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if _, err := os.Stat(p); err == nil {
				continue
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				return err
			}
		}
		step("A starting configuration in %s", ans.Dir)
	} else {
		step("Your configuration in %s kept as it is", ans.Dir)
		if _, err := os.Stat(filepath.Join(ans.Dir, ".fleetreeve.yaml")); err != nil {
			os.WriteFile(filepath.Join(ans.Dir, ".fleetreeve.yaml"), []byte(starter(ans, "", "")[".fleetreeve.yaml"]), 0o644)
		}
	}

	// The server.
	if ans.Container {
		def, err := containerDefinition(serverQuadlet(ans, image, uid, gid), serverCommand(ans), false, ans, vaultCA)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(setupContainer), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(setupContainer, []byte(def), 0o644); err != nil {
			return err
		}
	} else if err := os.WriteFile(filepath.Join(setupUnitDir, "fleetreeve-server.service"), []byte(serverUnit(ans, bin)), 0o644); err != nil {
		return err
	}
	if err := run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if !ans.Container { // a container's service starts at boot by its definition
		if err := run("systemctl", "enable", "fleetreeve-server.service"); err != nil {
			return err
		}
	}
	if err := run("systemctl", "restart", "fleetreeve-server.service"); err != nil {
		return err
	}
	client, err := pull.NewClient(url, "", "", crt, false)
	if err != nil {
		return err
	}
	var pingErr error
	for i := 0; i < 30; i++ {
		if pingErr = client.Ping(ctx); pingErr == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if pingErr != nil {
		return fmt.Errorf("the server was started but does not answer at %s: %v\n       (does %s resolve to this machine? `journalctl -u fleetreeve-server` has its log)", url, pingErr, ans.Name)
	}
	step("Server running at %s", url)
	secret, _, err := pull.LoadSecret(filepath.Join(setupData, "secret"), false)
	if err != nil {
		return err
	}

	// The console.
	password := ""
	if ans.Console {
		usersFile := filepath.Join(setupConsole, "users.yaml")
		if ans.Admin != "" {
			users, _ := dashboard.LoadUsers(usersFile)
			if users == nil {
				users = map[string]dashboard.User{}
			}
			pw := ans.Password
			if pw == "" {
				pw = dashboard.GeneratePassword()
				password = pw
			}
			users[ans.Admin] = dashboard.User{Name: ans.Admin, Role: dashboard.RoleAdmin, Hash: dashboard.HashPassword(pw)}
			if err := dashboard.SaveUsers(usersFile, users); err != nil {
				return err
			}
		}
		filepath.Walk(setupConsole, func(p string, _ os.FileInfo, _ error) error { return os.Chown(p, uid, gid) })
		env := fmt.Sprintf("# Written by fleetreeve setup: the console's settings and its tokens for the server.\nFLEETREEVE_SERVER=%s\nFLEETREEVE_CA=%s\nFLEETREEVE_DASHBOARD_USERS=%s\nFLEETREEVE_API_TOKEN=%s\n",
			url, crt, usersFile, pull.APIToken(secret, "read"))
		if ans.Powers >= 1 {
			env += "FLEETREEVE_OPERATOR_TOKEN=" + pull.APIToken(secret, "operator") + "\n"
		}
		if ans.Powers >= 2 {
			env += "FLEETREEVE_EXEC_TOKEN=" + pull.APIToken(secret, "exec") + "\n"
		}
		if err := os.WriteFile(setupEnv, []byte(env), 0o600); err != nil {
			return err
		}
		os.Chmod(setupEnv, 0o600)
		conUnit, conFile := consoleUnit(ans, bin), filepath.Join(setupUnitDir, "fleetreeve-dashboard.service")
		steps := [][]string{{"systemctl", "daemon-reload"}, {"systemctl", "enable", "fleetreeve-dashboard.service"}, {"systemctl", "restart", "fleetreeve-dashboard.service"}}
		if ans.Container {
			conUnit, conFile = consoleQuadlet(ans, image, uid, gid), consoleQuad
			steps = [][]string{steps[0], steps[2]}
		}
		if _, err := os.Stat(consoleCert); err == nil && ans.Vault {
			conUnit, _ = consoleUses(conUnit, consoleCert, consoleKey) // its certificate from Vault, see webcert.go
		}
		if err := os.WriteFile(conFile, []byte(conUnit), 0o644); err != nil {
			return err
		}
		for _, args := range steps {
			if err := run(args...); err != nil {
				return err
			}
		}
		up := false
		for i := 0; i < 30 && !up; i++ {
			if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", ans.ConPort), time.Second); err == nil {
				c.Close()
				up = true
			} else {
				time.Sleep(500 * time.Millisecond)
			}
		}
		if !up {
			return fmt.Errorf("the console was started but does not listen on port %d (`journalctl -u fleetreeve-dashboard` has its log)", ans.ConPort)
		}
		step("Console running at https://%s:%d", ans.Name, ans.ConPort)
	}

	// With Vault, the web interfaces get their certificates from it.
	if ans.Vault {
		webcertQuietly(ctx, o, ans, tokenFile)
	} else {
		webcertOff(ctx)
	}

	if ans.Firewall {
		ports := []int{ans.Port}
		if ans.Console {
			ports = append(ports, ans.ConPort)
		}
		for _, p := range ports {
			port := fmt.Sprintf("--add-port=%d/tcp", p)
			if err := run("firewall-cmd", "--quiet", "--permanent", port); err != nil {
				return err
			}
			run("firewall-cmd", "--quiet", port)
		}
		step("Firewall open for %s", strings.Trim(strings.Join(strings.Fields(fmt.Sprint(ports)), ", "), "[]"))
	}

	// This machine as the first host: the server's user may log in to it
	// like to any other host, and it pulls its configuration.
	var selfErr error
	if ans.Self {
		selfErr = func() error {
			pub, err := os.ReadFile(filepath.Join(u.HomeDir, ".ssh", "id_ed25519.pub"))
			if err != nil {
				return err
			}
			sh := exec.CommandContext(ctx, "sh", "-s")
			sh.Stdin = strings.NewReader(bootstrapScript(ans.Identity, strings.TrimSpace(string(pub)), false))
			out, err := sh.CombinedOutput()
			if err != nil {
				return fmt.Errorf("preparing this machine: %s", strings.TrimSpace(string(out)))
			}
			prepared := string(out)
			// Its own host keys, read from where they are.
			var trust bytes.Buffer
			if _, keys, ok := strings.Cut(string(out), "FLEETREEVE-HOST-KEYS\n"); ok {
				for _, line := range strings.Split(keys, "\n") {
					if f := strings.Fields(line); len(f) >= 2 {
						fmt.Fprintf(&trust, "%s %s %s\n", address, f[0], f[1])
					}
				}
			}
			known := exec.CommandContext(ctx, "runuser", "-u", ans.Identity, "--", bin, "identity", "trust")
			known.Stdin, known.Env = &trust, append(os.Environ(), "HOME="+u.HomeDir)
			if out, err := known.CombinedOutput(); err != nil {
				return fmt.Errorf("recording this machine's host key: %s", strings.TrimSpace(string(out)))
			}
			join := pull.JoinToken(secret, time.Now().Add(10*time.Minute))
			inst := exec.CommandContext(ctx, bin, "install", "-server", url, "-ca", crt, "-host", self, "-join", join, "-log-format", "text", "-no-diff")
			inst.Env = append(os.Environ(), "FLEETREEVE_CONFIG=none")
			if out, err := inst.CombinedOutput(); err != nil {
				return fmt.Errorf("installing the client here: %s", strings.TrimSpace(string(out)))
			}
			for _, line := range strings.Split(string(out), "\n") {
				_ = line
			}
			for _, line := range strings.Split(prepared, "\n") {
				if limits, ok := strings.CutPrefix(line, "FLEETREEVE-SSHD-LIMITS "); ok {
					fmt.Printf("  ! This machine's sshd does not let user %s in (%s); pushing to it over SSH will be refused until it does.\n", ans.Identity, limits)
				}
			}
			return nil
		}()
		if selfErr == nil {
			step("This machine is the first host: %s", self)
		}
	}

	fmt.Printf("\nDone.\n\n  Server:         %s\n", url)
	if ans.Console {
		fmt.Printf("  Console:        https://%s:%d\n", ans.Name, ans.ConPort)
		if ans.Admin != "" {
			fmt.Printf("  Sign in as:     %s\n", ans.Admin)
		}
		if password != "" {
			fmt.Printf("  Password:       %s   (shown this once; change it in the console)\n", password)
		}
		if ans.CertFile == "" {
			fmt.Println("                  Your browser will warn about the certificate: it was made here, not by an authority it knows.")
		}
	}
	if ans.Vault {
		must := "hosts may still use their token; add -require-cert to the service when all have a certificate"
		if ans.RequireCert {
			must = "required"
		}
		fmt.Printf("  Certificates:   from Vault at %s (%s/%s); %s\n                  `fleetreeve vault certs` lists them\n", ans.VaultAddr, ans.VaultPKI, ans.VaultRole, must)
		if ans.VaultDeploy {
			fmt.Printf("  Vault:          runs here in a container (systemctl status fleetreeve-vault). Its unseal key and root token are in\n                  %s: keep a copy of both away from this machine, and see docs/certificates.md\n", vaultDir)
		}
	}
	fmt.Printf("  Configuration:  %s   (site.yaml, inventory.yaml, data/)\n", ans.Dir)
	fmt.Printf("  Services:       systemctl status fleetreeve-server%s\n", map[bool]string{true: " fleetreeve-dashboard", false: ""}[ans.Console])
	fmt.Printf("\nNext, from %s:\n  fleetreeve enroll <host> -bootstrap-user <an existing login on it>    add a host\n  fleetreeve plan site.yaml                                            see what a run would change\n  fleetreeve status                                                    the last run of every host\n", ans.Dir)
	if selfErr != nil {
		return fmt.Errorf("the server is set up, but this machine could not be made its first host: %v", selfErr)
	}
	return nil
}
