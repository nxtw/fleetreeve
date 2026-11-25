package cli

import (
	"os"
	"strings"
	"testing"
	"time"
)

// The Vault answers go into the container's definition between markers;
// the rest of the file stays as it is, and doing it again - or undoing
// it - leaves nothing behind.
func TestContainerDefinition(t *testing.T) {
	b, err := os.ReadFile("../../packaging/container/fleetreeve-server.container")
	if err != nil {
		t.Fatal(err)
	}
	orig := string(b)
	image := []string{"serve", "-i", "inventory.yaml", "-secret-file", "/var/lib/fleetreeve-server/secret", "site.yaml"}
	ans := &setupAnswers{Vault: true, VaultAddr: "https://vault.example.com:8200", VaultPKI: "pki", VaultRole: "fleetreeve"}

	with, err := containerDefinition(orig, image, true, ans, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[Container]\n" + ctrBegin + "\nVolume=/etc/fleetreeve/server-vault:/etc/fleetreeve/server-vault:ro,Z\n" +
			"Exec=serve -vault-addr https://vault.example.com:8200 -vault-token-file /etc/fleetreeve/server-vault/token -vault-pki pki -vault-role fleetreeve -vault-ca /etc/fleetreeve/server-vault/ca.pem -i inventory.yaml -secret-file /var/lib/fleetreeve-server/secret site.yaml\n" + ctrEnd + "\nImage=",
		"Volume=/srv/fleetreeve:/srv/fleetreeve:ro,Z", "PublishPort=8140:8140",
	} {
		if !strings.Contains(with, want) {
			t.Errorf("no %q in:\n%s", want, with)
		}
	}
	if strings.Contains(with, "fleetreeve-vault.service") || strings.Contains(with, "AddHost") {
		t.Error("the server waits for a Vault that is not here")
	}
	if quadletStrip(with) != orig {
		t.Errorf("more than the marked part was changed:\n%s", quadletStrip(with))
	}

	// Again, now with Vault on this machine and certificates required:
	// the command is read back from the definition.
	ans.VaultDeploy, ans.RequireCert = true, true
	command := strings.Fields(quadletValues(with, "Container", "Exec")[0])
	again, err := containerDefinition(with, command, true, ans, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(again, "-vault-addr") != 1 || strings.Count(again, "Exec=") != 1 || !strings.Contains(again, "-vault-ca /etc/fleetreeve/server-vault/ca.pem -require-cert -i inventory.yaml") ||
		!strings.Contains(again, ":ro,Z\nAddHost=vault.example.com:host-gateway\nExec=") ||
		!strings.Contains(again, "[Unit]\n"+ctrBegin+"\nAfter=fleetreeve-vault.service\nWants=fleetreeve-vault.service\n"+ctrEnd+"\n") {
		t.Errorf("second time:\n%s", again)
	}

	// Without Vault after all: back to the definition as it was.
	off, err := containerDefinition(again, command, true, &setupAnswers{}, false)
	if err != nil || off != orig {
		t.Errorf("not undone (%v):\n%s", err, off)
	}

	// A command of the administrator's own is kept, with and without.
	own := strings.Replace(orig, "ReadOnly=true", "Exec=serve -listen :9000 -require-cert -vault-addr https://old:1 site.yaml\nReadOnly=true", 1)
	command = strings.Fields(quadletValues(own, "Container", "Exec")[0])
	ans.VaultDeploy, ans.RequireCert = false, false
	kept, _ := containerDefinition(own, command, false, ans, false)
	if !strings.Contains(kept, "Exec=serve -vault-addr https://vault.example.com:8200 -vault-token-file /etc/fleetreeve/server-vault/token -vault-pki pki -vault-role fleetreeve -listen :9000 site.yaml\n") || strings.Count(kept, "Exec=") != 1 {
		t.Errorf("own command:\n%s", kept)
	}
	kept, _ = containerDefinition(kept, strings.Fields(quadletValues(kept, "Container", "Exec")[0]), false, &setupAnswers{}, false)
	if !strings.Contains(kept, "Exec=serve -listen :9000 site.yaml\n") || strings.Contains(kept, "Volume=/etc/fleetreeve/server-vault") {
		t.Errorf("own command without Vault:\n%s", kept)
	}

	// What cannot be written back safely is refused, not mangled.
	if _, err := containerDefinition(orig, []string{"serve", "-i", "my inventory.yaml"}, false, ans, false); err == nil {
		t.Error("an argument with a space was written back")
	}
}

// The console is pointed at its own certificate in either kind of
// definition, and back; and a certificate is replaced when it is not
// Vault's, not for the names, or two thirds through its life.
func TestWebCert(t *testing.T) {
	unit := consoleUnit(&setupAnswers{Identity: "fleetreeve", ConPort: 8141}, "/usr/bin/fleetreeve")
	got, ok := consoleUses(unit, consoleCert, consoleKey)
	if !ok || !strings.Contains(got, "-tls-cert /etc/fleetreeve/tls/console.crt \\\n    -tls-key /etc/fleetreeve/tls/console.key \\\n") || strings.Contains(got, "server.crt") {
		t.Errorf("unit:\n%s", got)
	}
	if back, _ := consoleUses(got, "/etc/fleetreeve/tls/server.crt", "/etc/fleetreeve/tls/server.key"); back != unit {
		t.Error("not undone")
	}
	quad, _ := os.ReadFile("../../packaging/container/fleetreeve-dashboard.container")
	got, ok = consoleUses(string(quad), consoleCert, consoleKey)
	if !ok || !strings.Contains(got, "Exec=-tls-cert /etc/fleetreeve/tls/console.crt -tls-key /etc/fleetreeve/tls/console.key\n") || !strings.Contains(got, "EnvironmentFile=/etc/fleetreeve/dashboard.env") {
		t.Errorf("container:\n%s", got)
	}
	if _, ok := consoleUses("ExecStart=/usr/bin/fleetreeve dashboard\n", consoleCert, consoleKey); ok {
		t.Error("a console without a certificate was rewritten")
	}

	names := webNames("Fleetreeve.example.com, fleetreeve", "vault.example.com", "fleetreeve.example.com")
	if strings.Join(names, " ") != "fleetreeve.example.com fleetreeve vault.example.com vault localhost" {
		t.Errorf("names: %v", names)
	}

	// A self-signed certificate stands in for Vault's CA and for what it issued.
	dir := t.TempDir()
	cert, _, err := selfSigned("fleetreeve.example.com")
	if err != nil {
		t.Fatal(err)
	}
	file := dir + "/c.crt"
	if why, _ := webCertState(file, []string{"fleetreeve.example.com"}, cert, time.Now()); why == "" {
		t.Error("a missing certificate is fine")
	}
	os.WriteFile(file, cert, 0o644)
	other, _, _ := selfSigned("fleetreeve.example.com")
	if why, from := webCertState(file, []string{"fleetreeve.example.com"}, other, time.Now()); why == "" || from {
		t.Errorf("another authority's: %q %v", why, from)
	}
	if why, from := webCertState(file, []string{"fleetreeve.example.com", "localhost"}, cert, time.Now()); why != "" || !from {
		t.Errorf("a good one: %q %v", why, from)
	}
	if why, from := webCertState(file, []string{"www.example.com"}, cert, time.Now()); why == "" || !from {
		t.Errorf("other names: %q", why)
	}
	if why, _ := webCertState(file, []string{"fleetreeve.example.com"}, cert, time.Now().AddDate(7, 0, 0)); !strings.Contains(why, "runs out") {
		t.Errorf("late in its life: %q", why)
	}
	if why, from := webCertState(file, []string{"fleetreeve.example.com"}, cert, time.Now().AddDate(11, 0, 0)); !strings.Contains(why, "expired") || !from {
		t.Errorf("expired: %q %v", why, from)
	}

	service, timer := webTimerUnits("/usr/bin/fleetreeve", []string{"-vault-addr", "https://v:8200", "-vault-token-file", "/etc/fleetreeve/vault.token"})
	if !strings.Contains(service, "ExecStart=/usr/bin/fleetreeve vault webcert -vault-addr https://v:8200 -vault-token-file /etc/fleetreeve/vault.token\n") || !strings.Contains(timer, "OnCalendar=daily") {
		t.Errorf("timer:\n%s%s", service, timer)
	}
}

// The containers setup installs itself: the server's command and the
// Vault options stand in the marked part, the rest is plain, and setup
// recognises its own work.
func TestSetupContainers(t *testing.T) {
	ans := &setupAnswers{Name: "fleetreeve.example.com", Port: 8140, ConPort: 8141, Dir: "/srv/fleetreeve", Identity: "fleetreeve"}
	def, err := containerDefinition(serverQuadlet(ans, "localhost/fleetreeve:1", 990, 991), serverCommand(ans), false, ans, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{ctrWritten, "Image=localhost/fleetreeve:1\n", "User=990\nGroup=991\n", "PublishPort=8140:8140\n",
		"Volume=/srv/fleetreeve:/srv/fleetreeve:ro,Z\n", "Volume=/var/lib/fleetreeve-server:/var/lib/fleetreeve-server:Z\n",
		ctrBegin + "\nExec=serve -i inventory.yaml -secret-file /var/lib/fleetreeve-server/secret -tls-cert /etc/fleetreeve/tls/server.crt -tls-key /etc/fleetreeve/tls/server.key -reports /var/lib/fleetreeve-server/reports -hosts /var/lib/fleetreeve-server/hosts -actions /var/lib/fleetreeve-server/actions -listen :8140 site.yaml\n" + ctrEnd} {
		if !strings.Contains(def, want) {
			t.Errorf("no %q in:\n%s", want, def)
		}
	}
	if strings.Contains(def, "vault") {
		t.Error("Vault without Vault")
	}
	ans.Vault, ans.VaultDeploy, ans.VaultAddr, ans.VaultPKI, ans.VaultRole = true, true, "https://fleetreeve.example.com:8200", "pki", "fleetreeve"
	def, _ = containerDefinition(serverQuadlet(ans, "localhost/fleetreeve:1", 990, 991), serverCommand(ans), false, ans, true)
	if !strings.Contains(def, "AddHost=fleetreeve.example.com:host-gateway\nExec=serve -vault-addr https://fleetreeve.example.com:8200 -vault-token-file /etc/fleetreeve/server-vault/token") || !strings.Contains(def, "After=fleetreeve-vault.service") {
		t.Errorf("with Vault:\n%s", def)
	}
	con := consoleQuadlet(ans, "localhost/fleetreeve:1", 990, 991)
	for _, want := range []string{"AddHost=fleetreeve.example.com:host-gateway\n", "EnvironmentFile=/etc/fleetreeve/dashboard.env\n", "Exec=dashboard -tls-cert /etc/fleetreeve/tls/server.crt -tls-key /etc/fleetreeve/tls/server.key -listen :8141\n", "Volume=/etc/fleetreeve/dashboard:/etc/fleetreeve/dashboard:rw,Z\n"} {
		if !strings.Contains(con, want) {
			t.Errorf("no %q in:\n%s", want, con)
		}
	}
	if got, ok := consoleUses(con, consoleCert, consoleKey); !ok || !strings.Contains(got, "Exec=dashboard -tls-cert /etc/fleetreeve/tls/console.crt -tls-key /etc/fleetreeve/tls/console.key -listen :8141\n") {
		t.Errorf("console with its own certificate:\n%s", got)
	}
}
