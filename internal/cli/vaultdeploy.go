package cli

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nxtw/fleetreeve/internal/pull"
)

// `fleetreeve vault deploy` runs HashiCorp Vault on this machine, for those
// who want Vault as certificate authority and have none: a podman
// container managed by systemd (a quadlet), with its own certificate and
// storage that stays, initialised and unsealed. `fleetreeve vault unseal`
// unseals it again, which the service does by itself at every start.
//
// The unseal key is kept on this machine, readable by root only, so that
// Vault comes back after a reboot without anyone. That is a choice: who
// has this machine's disk has the CA. Take the key away (and unseal by
// hand with `fleetreeve vault unseal`) if that is not good enough.

const (
	vaultDir       = "/etc/fleetreeve/vault"
	vaultConfigDir = vaultDir + "/config" // mounted in the container
	vaultCert      = vaultConfigDir + "/tls/vault.crt"
	vaultKey       = vaultConfigDir + "/tls/vault.key"
	vaultUnsealKey = vaultDir + "/unseal.key"
	vaultRootToken = vaultDir + "/root.token"
	vaultDataDir   = "/var/lib/fleetreeve-vault"
	vaultQuadlet   = "/etc/containers/systemd/fleetreeve-vault.container"
	vaultImage     = "docker.io/hashicorp/vault:latest"
)

// vaultHCL is Vault's configuration: one node with its own storage,
// HTTPS only.
func vaultHCL(addr string) string {
	return fmt.Sprintf(`# Written by fleetreeve vault deploy.
ui            = true
disable_mlock = true
api_addr      = "%s"
cluster_addr  = "https://127.0.0.1:8201"

storage "raft" {
  path    = "/vault/file"
  node_id = "fleetreeve"
}

listener "tcp" {
  address       = "0.0.0.0:8200"
  tls_cert_file = "/vault/config/tls/vault.crt"
  tls_key_file  = "/vault/config/tls/vault.key"
}
`, addr)
}

// vaultContainer is the quadlet: systemd runs the container, and
// unseals Vault each time it has started.
func vaultContainer(addr, port, image, bin string) string {
	return fmt.Sprintf(`# Written by fleetreeve vault deploy: HashiCorp Vault in a podman container.
#   systemctl status fleetreeve-vault      journalctl -u fleetreeve-vault
[Unit]
Description=HashiCorp Vault for fleetreeve (container)
After=network-online.target
Wants=network-online.target

[Container]
Image=%[1]s
ContainerName=fleetreeve-vault
Exec=server
PublishPort=%[2]s:8200
Volume=%[3]s:/vault/config:Z
Volume=%[4]s:/vault/file:Z

[Service]
# Unseal with the key kept in %[5]s; without that file, Vault
# waits for `+"`fleetreeve vault unseal`"+`.
ExecStartPost=-%[6]s vault unseal -vault-addr %[7]s -vault-ca %[8]s
Restart=on-failure
RestartSec=5
TimeoutStartSec=300

[Install]
WantedBy=multi-user.target
`, image, port, vaultConfigDir, vaultDataDir, vaultUnsealKey, bin, addr, vaultCert)
}

// vaultDeployed tells whether this machine runs Vault from fleetreeve vault deploy.
func vaultDeployed() bool {
	_, err := os.Stat(vaultQuadlet)
	return err == nil
}

// deployedVault is a client for the Vault deployed here.
func deployedVault(addr, token string) (*pull.Vault, error) {
	if token == "" {
		token = "-" // what is done without one
	}
	return pull.NewVault(addr, token, "", "", "", "", vaultCert)
}

// waitForVault waits until Vault answers.
func waitForVault(ctx context.Context, v *pull.Vault, patience time.Duration) (initialized, sealed bool, err error) {
	deadline := time.Now().Add(patience)
	for {
		if initialized, sealed, err = v.SealStatus(ctx); err == nil || time.Now().After(deadline) || ctx.Err() != nil {
			return
		}
		time.Sleep(time.Second)
	}
}

// vaultDeploy makes sure Vault runs here, initialised and unsealed, and
// reports what it did. root is the root token when this machine still
// has it. Running it again keeps everything: certificate, data, keys.
func vaultDeploy(ctx context.Context, addr, image string) (did []string, root string, err error) {
	if os.Geteuid() != 0 {
		return nil, "", fmt.Errorf("deploying Vault installs a service: run it with sudo")
	}
	for _, tool := range []string{"podman", "systemctl"} {
		if _, err := exec.LookPath(tool); err != nil {
			return nil, "", fmt.Errorf("Vault is deployed as a podman container run by systemd, and this machine has no %s", tool)
		}
	}
	u, err := url.Parse(addr)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || strings.Trim(u.Path, "/") != "" {
		return nil, "", fmt.Errorf("%q: give the address Vault is to have as https://<this machine's name>:8200", addr)
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	if image == "" {
		image = vaultImage
	}
	for dir, mode := range map[string]os.FileMode{vaultDir: 0o755, vaultConfigDir: 0o755, filepath.Dir(vaultCert): 0o755, vaultDataDir: 0o700, filepath.Dir(vaultQuadlet): 0o755} {
		if err := os.MkdirAll(dir, mode); err != nil {
			return nil, "", err
		}
	}
	os.Chmod("/etc/fleetreeve", 0o755)

	// Its certificate: made once, then kept.
	if _, err := os.Stat(vaultCert); err != nil {
		c, k, err := selfSigned(u.Hostname())
		if err != nil {
			return nil, "", err
		}
		if err := os.WriteFile(vaultCert, c, 0o644); err != nil {
			return nil, "", err
		}
		if err := os.WriteFile(vaultKey, k, 0o600); err != nil {
			return nil, "", err
		}
		did = append(did, fmt.Sprintf("a certificate for %s (self-signed, valid 10 years): %s", u.Hostname(), vaultCert))
	} else {
		did = append(did, "its certificate was there already: "+vaultCert)
	}
	if err := os.WriteFile(filepath.Join(vaultConfigDir, "vault.hcl"), []byte(vaultHCL(addr)), 0o644); err != nil {
		return did, "", err
	}

	run := func(args ...string) error {
		out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
		}
		return nil
	}
	// The image, and the user Vault runs as in it: its private key and
	// its storage are that user's, and nobody else's.
	if exec.CommandContext(ctx, "podman", "image", "exists", image).Run() != nil {
		fmt.Printf("  … fetching %s\n", image)
		if err := run("podman", "pull", "--quiet", image); err != nil {
			return did, "", err
		}
	}
	var owner [2]int
	for i, which := range []string{"-u", "-g"} {
		out, err := exec.CommandContext(ctx, "podman", "run", "--rm", "--network", "none", "--entrypoint", "id", image, which).Output()
		if err == nil {
			owner[i], err = strconv.Atoi(strings.TrimSpace(string(out)))
		}
		if err != nil {
			return did, "", fmt.Errorf("cannot tell which user %s runs as: %v", image, err)
		}
	}
	if err := os.Chown(vaultKey, owner[0], owner[1]); err != nil {
		return did, "", err
	}
	if err := filepath.Walk(vaultDataDir, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(p, owner[0], owner[1])
	}); err != nil {
		return did, "", err
	}

	// The service.
	unit := vaultContainer(addr, port, image, selfPath())
	old, _ := os.ReadFile(vaultQuadlet)
	if err := os.WriteFile(vaultQuadlet, []byte(unit), 0o644); err != nil {
		return did, "", err
	}
	if err := run("systemctl", "daemon-reload"); err != nil {
		return did, "", err
	}
	verb := "start"
	if old != nil && string(old) != unit {
		verb = "restart"
	}
	if err := run("systemctl", verb, "fleetreeve-vault.service"); err != nil {
		return did, "", fmt.Errorf("%v\n       (`journalctl -u fleetreeve-vault` has its log)", err)
	}
	v, err := deployedVault(addr, "")
	if err != nil {
		return did, "", err
	}
	initialized, sealed, err := waitForVault(ctx, v, 90*time.Second)
	if err != nil {
		return did, "", fmt.Errorf("the container was started, but %v\n       (does %s resolve to this machine? `journalctl -u fleetreeve-vault` has its log)", err, u.Hostname())
	}
	did = append(did, fmt.Sprintf("Vault runs in a container (%s), as service fleetreeve-vault, with its data in %s", image, vaultDataDir))

	// A new Vault is initialised: one unseal key, kept here so that the
	// service can unseal by itself.
	if !initialized {
		key, token, err := v.Init(ctx)
		if err != nil {
			return did, "", err
		}
		for file, secret := range map[string]string{vaultUnsealKey: key, vaultRootToken: token} {
			if err := os.WriteFile(file, []byte(secret+"\n"), 0o600); err != nil {
				return did, "", fmt.Errorf("Vault is initialised, but %v\n       Its unseal key is %s and its root token %s: keep them, they are shown nowhere else", err, key, token)
			}
			os.Chmod(file, 0o600)
		}
		did = append(did, fmt.Sprintf("initialised: the unseal key is in %s and the root token in %s (readable by root only)", vaultUnsealKey, vaultRootToken))
		sealed = true
	} else {
		did = append(did, "it was initialised already")
	}
	if sealed {
		key, err := os.ReadFile(vaultUnsealKey)
		if err != nil {
			return did, "", fmt.Errorf("Vault is sealed and its unseal key is not on this machine (%s): unseal it with `fleetreeve vault unseal`, then run this again", vaultUnsealKey)
		}
		if err := v.Unseal(ctx, strings.TrimSpace(string(key))); err != nil {
			return did, "", err
		}
		did = append(did, "unsealed (the service does that by itself at every start)")
	}
	if err := waitReady(ctx, v); err != nil {
		return did, "", err
	}
	if b, err := os.ReadFile(vaultRootToken); err == nil {
		root = strings.TrimSpace(string(b))
	}
	return did, root, nil
}

// vaultUnseal unseals the Vault at the address, with the key kept on
// this machine or one that is typed in.
func vaultUnseal(ctx context.Context, o *options) error {
	if o.vaultAddr == "" {
		o.vaultAddr = os.Getenv("VAULT_ADDR")
	}
	if o.vaultCA == "" {
		if o.vaultCA = os.Getenv("VAULT_CACERT"); o.vaultCA == "" && vaultDeployed() {
			o.vaultCA = vaultCert
		}
	}
	if o.vaultAddr == "" {
		return fmt.Errorf("where is Vault? give -vault-addr <url> or set $VAULT_ADDR")
	}
	v, err := pull.NewVault(o.vaultAddr, "-", "", "", os.Getenv("VAULT_NAMESPACE"), "", o.vaultCA)
	if err != nil {
		return err
	}
	initialized, sealed, err := waitForVault(ctx, v, 60*time.Second)
	if err != nil {
		return err
	}
	if !initialized {
		return fmt.Errorf("Vault at %s has not been initialised yet (`sudo fleetreeve vault deploy` does that for one deployed here)", o.vaultAddr)
	}
	if !sealed {
		fmt.Println("Vault is not sealed.")
		return renewServerToken(ctx, o)
	}
	key := ""
	if b, err := os.ReadFile(vaultUnsealKey); err == nil {
		key = strings.TrimSpace(string(b))
	} else if onTerminal() {
		key = (&asker{in: bufio.NewReader(os.Stdin)}).secret("Unseal key")
	} else {
		return fmt.Errorf("Vault is sealed, and its unseal key cannot be read from %s (%v): run `fleetreeve vault unseal` at a terminal to enter it", vaultUnsealKey, err)
	}
	if key == "" {
		return fmt.Errorf("no unseal key given")
	}
	if err := v.Unseal(ctx, key); err != nil {
		return err
	}
	if err := waitReady(ctx, v); err != nil {
		return err
	}
	fmt.Println("Vault is unsealed.")
	return renewServerToken(ctx, o)
}

// waitReady waits for an unsealed Vault to take up work, which takes it
// a few seconds: it first has to elect itself leader.
func waitReady(ctx context.Context, v *pull.Vault) error {
	for i := 0; ; i++ {
		err := v.Ready(ctx)
		if err == nil {
			return nil
		}
		if i == 60 || ctx.Err() != nil {
			return fmt.Errorf("Vault is unsealed but not ready for work: %v", err)
		}
		time.Sleep(time.Second)
	}
}

// onTerminal tells whether someone is there to answer a question.
func onTerminal() bool {
	st, err := os.Stdin.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(st, null)
}

// vaultDeployCmd is `fleetreeve vault deploy`.
func vaultDeployCmd(ctx context.Context, o *options) error {
	addr := firstOf(o.vaultAddr, os.Getenv("VAULT_ADDR"), "https://"+fqdn()+":8200")
	did, root, err := vaultDeploy(ctx, addr, o.vaultImage)
	for _, line := range did {
		fmt.Println("  ✔ " + line)
	}
	if err != nil {
		return err
	}
	o.vaultAddr, o.vaultCA = addr, vaultCert
	// With a file to put the server's token in, go on and prepare Vault
	// as certificate authority.
	if o.vaultTokenFile != "" {
		if root == "" {
			return fmt.Errorf("Vault runs, but its root token is no longer in %s: prepare it with\n  VAULT_TOKEN=<an administrator's token> fleetreeve vault setup -vault-addr %s -vault-ca %s -vault-token-file %s", vaultRootToken, addr, vaultCert, o.vaultTokenFile)
		}
		u, _ := url.Parse(addr)
		names := webNames(o.webNames, u.Hostname())
		did, err := vaultSetup(ctx, o, root, o.vaultTokenFile, splitList(o.domains), names)
		for _, line := range did {
			fmt.Println("  ✔ " + line)
		}
		if err != nil {
			return err
		}
		// And certificates from it for the web interfaces here.
		if err := webcert(ctx, o, func(format string, a ...any) { fmt.Printf("  ✔ "+format+"\n", a...) }); err != nil {
			fmt.Printf("  ! The web interfaces keep their own certificates: %v\n", err)
		}
	}
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(addr, "https://"))
	fmt.Printf("\nVault is at %s (its web interface too). To use Vault's own tools:\n  export VAULT_ADDR=%s VAULT_CACERT=%s\n", addr, addr, vaultCert)
	if root != "" {
		fmt.Printf("  export VAULT_TOKEN=$(sudo cat %s)\n", vaultRootToken)
	}
	fmt.Printf(`
Two files on this machine are all there is to this Vault's safety:
  %s   opens Vault after a restart. Kept here so that the service can do that
        by itself; whoever can read it, and the data in %s, has the CA.
  %s   may do anything in Vault. Store it somewhere safe and delete it here
        once you have made yourself another way in.
Keep a copy of both away from this machine: without the unseal key, Vault's data cannot be read again.
The port (%s) is not opened in the firewall: hosts never talk to Vault, only the fleetreeve server on %s does.
`, vaultUnsealKey, vaultDataDir, vaultRootToken, port, host)
	if o.vaultTokenFile != "" {
		fmt.Printf("\nStart the fleetreeve server with:\n  fleetreeve serve … -vault-addr %s -vault-ca %s -vault-token-file %s\n", addr, vaultCert, o.vaultTokenFile)
	} else {
		fmt.Printf("\nNext: `sudo fleetreeve setup` (answer that Vault runs here), or prepare Vault yourself:\n  sudo fleetreeve vault deploy -vault-token-file /etc/fleetreeve/vault.token\n")
	}
	return nil
}

// splitList splits a comma-separated option.
func splitList(s string) []string {
	var out []string
	for _, d := range strings.Split(s, ",") {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}
	return out
}

var vaultFlag = regexp.MustCompile(`-vault-(pki|role)[ =](\S+)`)

// renewServerToken gives the fleetreeve server on this machine a new Vault
// token when the one it has is gone. The server keeps its token alive by
// renewing it; a machine that was off for longer than the token's period
// comes back with a token Vault no longer takes, and the server would
// refuse to start. Where Vault was deployed here its root token is on
// this machine too, so what an administrator would do by hand is done
// here: at every start of Vault, right after it is unsealed and before
// the server starts.
//
// It does nothing where there is nothing to do or nothing to do it with:
// no Vault deployed here, no server token, no root token (taken away, as
// the documentation suggests for more security), or a token that works.
// Failing here must not keep Vault from counting as started, so problems
// are said, not returned.
func renewServerToken(ctx context.Context, o *options) error {
	if !vaultDeployed() {
		return nil
	}
	// Where the server keeps its token, and how it is told to use Vault.
	tokenFile, unit := "", ""
	for _, c := range [][2]string{
		{ctrVaultDir + "/token", setupContainer}, // the server in a container
		{setupVaultToken, filepath.Join(setupUnitDir, "fleetreeve-server.service")},
	} {
		if _, err := os.Stat(c[0]); err == nil {
			tokenFile, unit = c[0], c[1]
			break
		}
	}
	if tokenFile == "" {
		return nil
	}
	// The token file is in a directory the server's user owns: see
	// ownedfile.go for why it is not simply read and written by name.
	b, err := readOwned(tokenFile)
	if err != nil {
		fmt.Printf("The fleetreeve server's Vault token could not be read: %v\n", err)
		return nil
	}
	so := *o
	so.vaultPKI, so.vaultRole = "", ""
	if def, err := os.ReadFile(unit); err == nil {
		for _, m := range vaultFlag.FindAllStringSubmatch(string(def), -1) {
			if m[1] == "pki" {
				so.vaultPKI = m[2]
			} else {
				so.vaultRole = m[2]
			}
		}
	}
	// An empty file holds no token: that is a token that is gone, too.
	if have := strings.TrimSpace(string(b)); have != "" {
		server, err := vaultClient(&so, have)
		if err != nil {
			return nil
		}
		alive, err := server.TokenAlive(ctx)
		if err != nil {
			fmt.Printf("The fleetreeve server's Vault token could not be checked: %v\n", err)
			return nil
		}
		if alive {
			return nil
		}
	}
	root, err := os.ReadFile(vaultRootToken)
	if err != nil {
		fmt.Printf("The fleetreeve server's Vault token (%s) is no longer accepted - it runs out when the server has not renewed it for its period, as when this machine was off - and Vault's root token is not in %s to make a new one with. Make one with:\n  VAULT_TOKEN=<an administrator's token> fleetreeve vault setup -vault-addr %s -vault-ca %s -vault-token-file %s\n",
			tokenFile, vaultRootToken, o.vaultAddr, o.vaultCA, tokenFile)
		return nil
	}
	admin, err := vaultClient(&so, strings.TrimSpace(string(root)))
	if err != nil {
		return nil
	}
	token, _, err := admin.ServerToken(ctx, "")
	if err != nil {
		fmt.Printf("The fleetreeve server's Vault token is no longer accepted, and making a new one failed: %v\n", err)
		return nil
	}
	// In place: the file keeps its owner, the user the server runs as.
	if err := writeOwned(tokenFile, []byte(token+"\n"), 0o600, -1, -1); err != nil {
		fmt.Printf("The fleetreeve server's Vault token is no longer accepted, and the new one could not be written: %v\n", err)
		return nil
	}
	fmt.Printf("The fleetreeve server's Vault token had run out (it does when the server has not renewed it for its period, as when this machine was off). A new one is in %s.\n", tokenFile)
	// A server that is running holds the old token in memory.
	exec.CommandContext(ctx, "systemctl", "try-restart", "--no-block", "fleetreeve-server.service").Run()
	return nil
}
