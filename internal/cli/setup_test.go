package cli

import (
	"bufio"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nxtw/fleetreeve/internal/compile"
)

// The certificate setup makes is its own authority and valid for the
// server's name, so hosts can verify the server with it.
func TestSelfSigned(t *testing.T) {
	certPEM, keyPEM, err := selfSigned("fleetreeve.example.com")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	for _, name := range []string{"fleetreeve.example.com", "fleetreeve", "localhost", "127.0.0.1"} {
		if _, err := cert.Verify(x509.VerifyOptions{DNSName: name, Roots: pool}); err != nil {
			t.Errorf("not valid for %s: %v", name, err)
		}
	}
	if _, err := cert.Verify(x509.VerifyOptions{DNSName: "other.example.com", Roots: pool}); err == nil {
		t.Error("valid for another name")
	}
	if k, _ := pem.Decode(keyPEM); k == nil || k.Type != "PRIVATE KEY" {
		t.Error("no private key")
	}
}

// Questions take the default on Enter, insist on a usable answer, and
// -yes asks nothing.
func TestAsker(t *testing.T) {
	a := &asker{in: bufio.NewReader(strings.NewReader("\nweb.example.com\nmaybe\nN\n7\n2\n"))}
	if got := a.ask("Name", "default"); got != "default" {
		t.Errorf("empty answer: %q", got)
	}
	if got := a.ask("Name", "default"); got != "web.example.com" {
		t.Errorf("answer: %q", got)
	}
	if a.confirm("Sure", true) {
		t.Error("confirm did not wait for a yes or no")
	}
	if got := a.choose("Which", []string{"a", "b", "c"}, 0); got != 1 {
		t.Errorf("choose: %d", got)
	}
	yes := &asker{in: bufio.NewReader(strings.NewReader("")), yes: true}
	if yes.ask("Name", "d") != "d" || !yes.confirm("Sure", true) || yes.choose("Which", []string{"a", "b"}, 1) != 1 || yes.secret("Password") != "" {
		t.Error("-yes did not take the defaults")
	}
}

// The configuration setup starts you off with must be one fleetreeve accepts.
func TestStarterConfiguration(t *testing.T) {
	ans := &setupAnswers{Name: "fleetreeve.example.com", Port: 8140, Dir: "/srv/fleetreeve", Identity: "fleetreeve", Console: true, ConPort: 8141}
	dir := t.TempDir()
	for name, body := range starter(ans, "fleetreeve", "10.0.0.5") {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	inv, err := compile.LoadInventory(filepath.Join(dir, "inventory.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	hosts, _ := inv.Select("")
	if len(hosts) != 1 || hosts[0].User != "fleetreeve" || !hosts[0].Sudo() || hosts[0].Address != "10.0.0.5" {
		t.Fatalf("inventory: %+v", hosts)
	}
	cat, err := compile.Compile(compile.Input{Manifest: filepath.Join(dir, "site.yaml"), Host: hosts[0], Inventory: inv,
		Facts: map[string]any{"os": map[string]any{"family": "redhat"}, "hostname": "fleetreeve"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Resources) != 1 || !strings.Contains(cat.Resources[0].Str("content"), "comes from fleetreeve.example.com") {
		t.Errorf("catalog: %+v", cat.Resources)
	}
	// Without this machine as a host, the inventory is still valid YAML
	// the server can start with.
	if body := starter(ans, "", "")["inventory.yaml"]; !strings.Contains(body, "hosts: {}") {
		t.Errorf("empty inventory:\n%s", body)
	}
	// The settings file is read back as written.
	t.Setenv("FLEETREEVE_CONFIG", filepath.Join(dir, ".fleetreeve.yaml"))
	o, err := parse("enroll", nil)
	if err != nil {
		t.Fatal(err)
	}
	if o.runAs != "fleetreeve" || o.server != "https://fleetreeve.example.com:8140" || o.secretFile != "/var/lib/fleetreeve-server/secret" || !strings.HasSuffix(o.inventory, "inventory.yaml") {
		t.Errorf("settings: run-as %q server %q secret %q inventory %q", o.runAs, o.server, o.secretFile, o.inventory)
	}
	// With Vault, the server is started with its options; without, not.
	if strings.Contains(serverUnit(ans, "/usr/bin/fleetreeve"), "vault") {
		t.Error("Vault options without Vault")
	}
	withVault := *ans
	withVault.Vault, withVault.VaultAddr, withVault.VaultPKI, withVault.VaultRole, withVault.RequireCert = true, "https://vault.example.com:8200", "pki", "fleetreeve", true
	if unit := serverUnit(&withVault, "/usr/bin/fleetreeve"); !strings.Contains(unit, "    -vault-addr https://vault.example.com:8200 \\\n    -vault-token-file /etc/fleetreeve/vault.token \\\n    -vault-pki pki \\\n    -vault-role fleetreeve \\\n    -require-cert \\\n    site.yaml") {
		t.Errorf("unit with Vault:\n%s", unit)
	}
	// With Vault on this machine, the server starts after it.
	if strings.Contains(serverUnit(&withVault, "/usr/bin/fleetreeve"), "fleetreeve-vault") {
		t.Error("the server waits for a Vault that is not here")
	}
	withVault.VaultDeploy, withVault.VaultCA = true, vaultCert
	if unit := serverUnit(&withVault, "/usr/bin/fleetreeve"); !strings.Contains(unit, "After=network-online.target fleetreeve-vault.service\nWants=network-online.target fleetreeve-vault.service\n") || !strings.Contains(unit, "-vault-ca /etc/fleetreeve/vault/config/tls/vault.crt") {
		t.Errorf("unit with Vault here:\n%s", unit)
	}
	for _, unit := range []string{serverUnit(ans, "/usr/bin/fleetreeve"), consoleUnit(ans, "/usr/bin/fleetreeve")} {
		if !strings.Contains(unit, "User=fleetreeve") || !strings.Contains(unit, "ExecStart=/usr/bin/fleetreeve ") || !strings.Contains(unit, "WantedBy=multi-user.target") {
			t.Errorf("unit:\n%s", unit)
		}
	}
}

// The container Vault is deployed as: its port, its storage and
// configuration from this machine, unsealed by the service itself.
func TestVaultContainer(t *testing.T) {
	unit := vaultContainer("https://fleetreeve.example.com:8300", "8300", vaultImage, "/usr/bin/fleetreeve")
	for _, want := range []string{
		"Image=docker.io/hashicorp/vault:latest\n", "Exec=server\n", "PublishPort=8300:8200\n",
		"Volume=/etc/fleetreeve/vault/config:/vault/config:Z\n", "Volume=/var/lib/fleetreeve-vault:/vault/file:Z\n",
		"ExecStartPost=-/usr/bin/fleetreeve vault unseal -vault-addr https://fleetreeve.example.com:8300 -vault-ca /etc/fleetreeve/vault/config/tls/vault.crt\n",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("no %q in:\n%s", want, unit)
		}
	}
	hcl := vaultHCL("https://fleetreeve.example.com:8300")
	for _, want := range []string{`api_addr      = "https://fleetreeve.example.com:8300"`, `storage "raft"`, `tls_key_file  = "/vault/config/tls/vault.key"`} {
		if !strings.Contains(hcl, want) {
			t.Errorf("no %q in:\n%s", want, hcl)
		}
	}
	// Deploying is refused before anything is touched when it cannot work.
	if _, _, err := vaultDeploy(t.Context(), "http://fleetreeve.example.com:8200", ""); err == nil {
		t.Error("deployed without HTTPS, or without being root")
	}
}
