package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/nxtw/fleetreeve/internal/pull"
)

// Where the fleetreeve server runs as a podman container (a quadlet, see
// packaging/container), `fleetreeve setup` does not install anything: the
// server, its certificate, the console and the configuration are there.
// It asks what is left to ask - whether HashiCorp Vault is to be the
// certificate authority for hosts - and writes the answer into the
// container's definition: the server's options, and a directory with its
// Vault token and Vault's CA, mounted into the container.
//
// What setup writes in the definition stands between two marker lines;
// everything else in the file is left alone.

const (
	ctrVaultDir = "/etc/fleetreeve/server-vault" // the same path in the container
	ctrToken    = ctrVaultDir + "/token"
	ctrCA       = ctrVaultDir + "/ca.pem"
	ctrBegin    = "# >>> written by fleetreeve setup (Vault as certificate authority); run it again to change this"
	ctrEnd      = "# <<< written by fleetreeve setup"
)

// quadletStrip removes what setup wrote in a definition earlier.
func quadletStrip(text string) string {
	var out []string
	inside := false
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "# >>> written by fleetreeve setup"):
			inside = true
		case strings.HasPrefix(line, "# <<< written by fleetreeve setup"):
			inside = false
		case !inside:
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// quadletValues lists the values a key has in a section.
func quadletValues(text, section, key string) []string {
	var vals []string
	current := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = line[1 : len(line)-1]
		} else if k, v, ok := strings.Cut(line, "="); ok && current == section && strings.TrimSpace(k) == key {
			vals = append(vals, strings.TrimSpace(v))
		}
	}
	return vals
}

// quadletDrop removes a key from a section.
func quadletDrop(text, section, key string) string {
	var out []string
	current := ""
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			current = t[1 : len(t)-1]
		} else if k, _, ok := strings.Cut(t, "="); ok && current == section && strings.TrimSpace(k) == key {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// quadletInsert puts lines at the top of a section, between the markers.
func quadletInsert(text, section string, lines []string) (string, error) {
	var out []string
	done := false
	for _, line := range strings.Split(text, "\n") {
		out = append(out, line)
		if !done && strings.TrimSpace(line) == "["+section+"]" {
			out = append(out, ctrBegin)
			out = append(out, lines...)
			out = append(out, ctrEnd)
			done = true
		}
	}
	if !done {
		return "", fmt.Errorf("the definition has no [%s] section", section)
	}
	return strings.Join(out, "\n"), nil
}

// withoutVault takes the Vault options out of a server's command line.
func withoutVault(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		switch strings.TrimLeft(args[i], "-") {
		case "vault-addr", "vault-token-file", "vault-pki", "vault-role", "vault-ca":
			if strings.HasPrefix(args[i], "-") {
				i++ // and its value
				continue
			}
		case "require-cert":
			if strings.HasPrefix(args[i], "-") {
				continue
			}
		}
		out = append(out, args[i])
	}
	return out
}

// withVault puts the Vault options into a server's command line, right
// after `serve`.
func withVault(args []string, ans *setupAnswers, ca bool) []string {
	args = withoutVault(args)
	if !ans.Vault || len(args) == 0 {
		return args
	}
	add := []string{"-vault-addr", ans.VaultAddr, "-vault-token-file", ctrToken, "-vault-pki", ans.VaultPKI, "-vault-role", ans.VaultRole}
	if ca {
		add = append(add, "-vault-ca", ctrCA)
	}
	if ans.RequireCert {
		add = append(add, "-require-cert")
	}
	return append(append([]string{args[0]}, add...), args[1:]...)
}

// containerDefinition is the container's definition with the answers
// about Vault in it. command is how the server is started without them:
// the definition's own Exec, or else the image's command.
func containerDefinition(text string, command []string, imageCommand bool, ans *setupAnswers, ca bool) (string, error) {
	base := withoutVault(command)
	text = quadletDrop(quadletStrip(text), "Container", "Exec")
	var lines []string
	if ans.Vault {
		lines = append(lines, fmt.Sprintf("Volume=%s:%s:ro,Z", ctrVaultDir, ctrVaultDir))
	}
	// A Vault on this machine is reached by this machine's name, which a
	// container would take to be itself: point it at the host.
	if ans.Vault && ans.VaultDeploy {
		if u, err := url.Parse(ans.VaultAddr); err == nil && u.Hostname() != "" {
			lines = append(lines, "AddHost="+u.Hostname()+":host-gateway")
		}
	}
	if ans.Vault || !imageCommand {
		for _, arg := range withVault(base, ans, ca) {
			if arg == "" || strings.ContainsAny(arg, " \t\"'\\$%") {
				return "", fmt.Errorf("the server's command has an argument (%q) that setup cannot write back safely: put the Vault options in %s by hand (docs/certificates.md says which)", arg, setupContainer)
			}
		}
		lines = append(lines, "Exec="+strings.Join(withVault(base, ans, ca), " "))
	}
	var err error
	if len(lines) > 0 {
		if text, err = quadletInsert(text, "Container", lines); err != nil {
			return "", err
		}
	}
	if ans.VaultDeploy {
		if text, err = quadletInsert(text, "Unit", []string{"After=fleetreeve-vault.service", "Wants=fleetreeve-vault.service"}); err != nil {
			return "", err
		}
	}
	return text, nil
}

// imageConfig asks podman how an image starts and as whom.
func imageConfig(ctx context.Context, image string) (command []string, user string, err error) {
	out, err := exec.CommandContext(ctx, "podman", "image", "inspect", "--format", "{{json .Config}}", image).Output()
	if err != nil {
		return nil, "", fmt.Errorf("podman knows no image %s: %v", image, err)
	}
	var c struct {
		Cmd  []string
		User string
	}
	if err := json.Unmarshal(out, &c); err != nil {
		return nil, "", err
	}
	return c.Cmd, c.User, nil
}

func setupInContainer(ctx context.Context, o *options) error {
	b, err := os.ReadFile(setupContainer)
	if err != nil {
		return err
	}
	old := string(b)
	images := quadletValues(old, "Container", "Image")
	if len(images) != 1 {
		return fmt.Errorf("%s names no image", setupContainer)
	}
	imgCmd, imgUser, err := imageConfig(ctx, images[0])
	if err != nil {
		return err
	}
	// How the server is started now, and as which user.
	command, fromImage := imgCmd, true
	if execs := quadletValues(old, "Container", "Exec"); len(execs) > 0 {
		line := execs[len(execs)-1]
		if strings.ContainsAny(line, "\"'\\") {
			return fmt.Errorf("the Exec line in %s uses quoting that setup cannot rewrite safely: put the Vault options in by hand (docs/certificates.md says which)", setupContainer)
		}
		command = strings.Fields(line)
		fromImage = strings.Join(withoutVault(command), " ") == strings.Join(imgCmd, " ")
	}
	if len(command) == 0 || command[0] != "serve" {
		return fmt.Errorf("the container in %s does not seem to start `fleetreeve serve` (it starts %q)", setupContainer, strings.Join(command, " "))
	}
	user := imgUser
	if u := quadletValues(old, "Container", "User"); len(u) > 0 {
		user = u[len(u)-1]
		if g := quadletValues(old, "Container", "Group"); len(g) > 0 {
			user += ":" + g[len(g)-1]
		}
	}
	uid, gid := 0, 0
	if user != "" {
		us, gs, hasGroup := strings.Cut(user, ":")
		if uid, err = strconv.Atoi(us); err == nil && hasGroup {
			gid, err = strconv.Atoi(gs)
		} else if err == nil {
			gid = uid
		}
		if err != nil {
			return fmt.Errorf("the server runs as user %q in its container, and setup needs that as a number to give it its Vault token (User= in %s)", user, setupContainer)
		}
	}

	a := &asker{in: bufio.NewReader(os.Stdin), yes: o.yes}
	fmt.Printf("This machine runs the fleetreeve server in a container (%s).\nThe server, its certificate, the console and your configuration stay as they are;\nwhat is left to set up here is where hosts' certificates come from. Press Enter to accept the answer in brackets.\n", setupContainer)
	ans := &setupAnswers{Name: fqdn()}
	if u, err := url.Parse(o.server); err == nil && u.Hostname() != "" {
		ans.Name = u.Hostname()
	}
	had := len(quadletValues(old, "Container", "Exec")) > 0 && strings.Contains(old, "-vault-addr ")
	if err := vaultQuestions(a, o, ans, b, ctrToken); err != nil {
		return err
	}
	if !ans.Vault && !had {
		fmt.Println("\nNothing to change: hosts keep proving who they are with their token.")
		return nil
	}
	switch {
	case !ans.Vault:
		fmt.Print("\nAbout to: take the Vault options off the server, so that hosts use their tokens again")
	case ans.VaultDeploy:
		fmt.Printf("\nAbout to: run Vault in a container at %s, and have the server use it as certificate authority", ans.VaultAddr)
	default:
		fmt.Printf("\nAbout to: have the server use Vault at %s as certificate authority", ans.VaultAddr)
	}
	fmt.Println(". The server is restarted for it.")
	if !a.confirm("Go ahead", true) {
		return fmt.Errorf("nothing was changed")
	}
	fmt.Println()

	ca, err := containerVaultFiles(ctx, o, ans, uid, gid)
	if err != nil {
		return err
	}
	text, err := containerDefinition(old, command, fromImage, ans, ca)
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
	restart := func() error {
		if err := run("systemctl", "daemon-reload"); err != nil {
			return err
		}
		if err := run("systemctl", "restart", "fleetreeve-server.service"); err != nil {
			return err
		}
		return serverSettled(ctx, o, ans.Vault)
	}
	if err := os.WriteFile(setupContainer, []byte(text), 0o644); err != nil {
		return err
	}
	if err := restart(); err != nil {
		// Put back what worked.
		log := serverSaid(ctx)
		os.WriteFile(setupContainer, b, 0o644)
		back := "the definition was put back as it was and the server runs as before"
		if rerr := func() error {
			if err := run("systemctl", "daemon-reload"); err != nil {
				return err
			}
			return run("systemctl", "restart", "fleetreeve-server.service")
		}(); rerr != nil {
			back = fmt.Sprintf("the definition was put back as it was, but the server does not start with that either: %v", rerr)
		}
		return fmt.Errorf("the server does not run with the new options: %v%s\n       %s", err, log, back)
	}
	if !ans.Vault {
		webcertOff(ctx)
		step("Server restarted without Vault: hosts use their tokens again")
		fmt.Printf("\nDone. Hosts keep the certificates they have, unused. %s can be removed", ctrVaultDir)
		if vaultDeployed() {
			fmt.Print("; the Vault container still runs (docs/certificates.md says how to take it away)")
		}
		fmt.Println(".")
		return nil
	}
	step("Server restarted with Vault as certificate authority (%s)", setupContainer)
	webcertQuietly(ctx, o, ans, ctrToken)
	must := "hosts may still use their token; run setup again to require certificates when all have one"
	if ans.RequireCert {
		must = "required"
	}
	fmt.Printf("\nDone.\n\n  Certificates:   from Vault at %s (%s/%s); %s\n                  each host gets its certificate at its next run; `fleetreeve vault certs` lists them\n", ans.VaultAddr, ans.VaultPKI, ans.VaultRole, must)
	fmt.Printf("  Server's token: %s (readable by the server's user in the container only)\n", ctrToken)
	if ans.VaultDeploy {
		fmt.Printf("  Vault:          runs here in a container (systemctl status fleetreeve-vault). Its unseal key and root token are in\n                  %s: keep a copy of both away from this machine, and see docs/certificates.md\n", vaultDir)
	}
	return nil
}

// serverSettled waits for the restarted server: its service stays up,
// and - where its address is known - it answers and says what it does
// about certificates.
func serverSettled(ctx context.Context, o *options, vault bool) error {
	var client *pull.Client
	if o.server != "" {
		client, _ = pull.NewClient(o.server, "", "", o.ca, o.insecure)
	}
	var last error
	for i := 0; i < 40; i++ {
		time.Sleep(time.Second)
		state, _ := exec.CommandContext(ctx, "systemctl", "is-active", "fleetreeve-server.service").Output()
		if s := strings.TrimSpace(string(state)); s != "active" {
			last = fmt.Errorf("its service is %s", s)
			continue
		}
		if client == nil {
			if i >= 10 { // up for a while: a server that cannot use Vault stops at once
				return nil
			}
			continue
		}
		h, err := client.Health(ctx)
		if err != nil {
			last = err
			continue
		}
		if got := h["certificates"] == "vault"; got != vault {
			last = fmt.Errorf("it answers, but not with the new options")
			continue
		}
		return nil
	}
	return last
}

// serverSaid is what the server last logged as an error, for a message.
func serverSaid(ctx context.Context) string {
	out, _ := exec.CommandContext(ctx, "journalctl", "-u", "fleetreeve-server", "-n", "40", "--no-pager", "-o", "cat").Output()
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var entry struct{ Level, Msg string }
		if json.Unmarshal([]byte(lines[i]), &entry) == nil && entry.Level == "ERROR" && entry.Msg != "" {
			return "\n       it says: " + entry.Msg
		}
	}
	return "\n       (`journalctl -u fleetreeve-server` has its log)"
}

// containerVaultFiles carries out the answers about Vault for a server
// in a container: Vault itself if it is to run here, and a directory the
// container gets to see, with the server's token and Vault's CA in it,
// owned by the user the server runs as. ca says whether there is a CA
// file to verify Vault with.
func containerVaultFiles(ctx context.Context, o *options, ans *setupAnswers, uid, gid int) (bool, error) {
	ca := false
	if ans.Vault {
		if err := os.MkdirAll(ctrVaultDir, 0o700); err != nil {
			return false, err
		}
		os.Chmod(ctrVaultDir, 0o700)
		if err := os.Chown(ctrVaultDir, uid, gid); err != nil {
			return false, err
		}
		if err := vaultApply(ctx, o, ans, ctrToken, uid, gid); err != nil {
			return false, err
		}
		// Vault's CA, where the server's container can read it.
		if ans.VaultCA != "" {
			if ans.VaultCA != ctrCA {
				pem, err := os.ReadFile(ans.VaultCA)
				if err != nil {
					return false, err
				}
				if err := os.WriteFile(ctrCA, pem, 0o644); err != nil {
					return false, err
				}
			}
			os.Chown(ctrCA, uid, gid)
			ca = true
		} else {
			os.Remove(ctrCA)
		}
	}
	return ca, nil
}

// ctrWritten marks a server container that setup itself installed.
const ctrWritten = "# Written by fleetreeve setup: the fleetreeve server as a podman container."

// ctrImage is the image setup makes for the server and the console.
func ctrImage() string { return "localhost/fleetreeve:" + Version }

// buildImage makes the image the server and the console run from, out of
// this fleetreeve: the binary, the builds it hands to hosts, and the list of
// public certificate authorities - on an empty base, with nothing else.
func buildImage(ctx context.Context, bin string) (string, error) {
	dir, err := os.MkdirTemp("", "fleetreeve-image-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	copyFile := func(from, to string) error {
		b, err := os.ReadFile(from)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return err
		}
		return os.WriteFile(to, b, 0o755)
	}
	if err := copyFile(bin, filepath.Join(dir, "fleetreeve")); err != nil {
		return "", err
	}
	// The builds for hosts: those of the fleetreeve-server package, or at
	// least this one, for hosts like this machine.
	builds, _ := filepath.Glob("/usr/libexec/fleetreeve/agents/fleetreeve-*")
	if len(builds) == 0 {
		builds = nil
		if err := copyFile(bin, filepath.Join(dir, "agents", "fleetreeve-"+runtime.GOOS+"-"+runtime.GOARCH)); err != nil {
			return "", err
		}
	}
	for _, b := range builds {
		if err := copyFile(b, filepath.Join(dir, "agents", filepath.Base(b))); err != nil {
			return "", err
		}
	}
	file := "FROM scratch\nCOPY fleetreeve /usr/bin/fleetreeve\nCOPY agents /usr/libexec/fleetreeve/agents\n"
	for _, roots := range []string{"/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/certs/ca-certificates.crt"} {
		if copyFile(roots, filepath.Join(dir, "ca-certificates.crt")) == nil {
			file += "COPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt\n"
			break
		}
	}
	file += "WORKDIR /srv/fleetreeve\nENTRYPOINT [\"/usr/bin/fleetreeve\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Containerfile"), []byte(file), 0o644); err != nil {
		return "", err
	}
	out, err := exec.CommandContext(ctx, "podman", "build", "--quiet", "--tag", ctrImage(), dir).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("making the image: %s", strings.TrimSpace(string(out)))
	}
	return ctrImage(), nil
}

// serverCommand is how the server is started in its container.
func serverCommand(ans *setupAnswers) []string {
	return []string{"serve", "-i", "inventory.yaml", "-secret-file", setupData + "/secret",
		"-tls-cert", setupTLS + "/server.crt", "-tls-key", setupTLS + "/server.key",
		"-reports", setupData + "/reports", "-hosts", setupData + "/hosts", "-actions", setupData + "/actions",
		"-listen", fmt.Sprintf(":%d", ans.Port), "site.yaml"}
}

// serverQuadlet is the server's container, without its command (see
// containerDefinition). It runs as the server's own user of this
// machine, so that the files it shares with the commands run here - the
// secret, the reports - are that user's in either world.
func serverQuadlet(ans *setupAnswers, image string, uid, gid int) string {
	return fmt.Sprintf(`%[1]s
# Run setup again to change it; what stands between marker lines is rewritten.
#   systemctl status fleetreeve-server      journalctl -u fleetreeve-server
[Unit]
Description=fleetreeve server (container)
After=network-online.target
Wants=network-online.target
ConditionPathExists=%[2]s/site.yaml

[Container]
Image=%[3]s
ContainerName=fleetreeve-server
User=%[4]d
Group=%[5]d
PublishPort=%[6]d:%[6]d
Volume=%[2]s:/srv/fleetreeve:ro,Z
Volume=%[7]s:%[7]s:ro,z
Volume=%[8]s:%[8]s:Z
ReadOnly=true
DropCapability=ALL
NoNewPrivileges=true

[Service]
Restart=on-failure
RestartSec=5
TimeoutStartSec=300

[Install]
WantedBy=multi-user.target
`, ctrWritten, ans.Dir, image, uid, gid, ans.Port, setupTLS, setupData)
}

// consoleQuadlet is the console's container. The server's name leads to
// this machine from inside it, not to the container itself.
func consoleQuadlet(ans *setupAnswers, image string, uid, gid int) string {
	return fmt.Sprintf(`# Written by fleetreeve setup: the fleetreeve web console as a podman container.
# Its settings and tokens are in %[1]s.
[Unit]
Description=fleetreeve web console (container)
After=network-online.target fleetreeve-server.service
Wants=network-online.target

[Container]
Image=%[2]s
ContainerName=fleetreeve-dashboard
User=%[3]d
Group=%[4]d
PublishPort=%[5]d:%[5]d
AddHost=%[6]s:host-gateway
EnvironmentFile=%[1]s
Volume=%[7]s:%[7]s:ro,z
Volume=%[8]s:%[8]s:rw,Z
Exec=dashboard -tls-cert %[7]s/server.crt -tls-key %[7]s/server.key -listen :%[5]d
ReadOnly=true
DropCapability=ALL
NoNewPrivileges=true

[Service]
Restart=on-failure
RestartSec=5
TimeoutStartSec=300

[Install]
WantedBy=multi-user.target
`, setupEnv, image, uid, gid, ans.ConPort, ans.Name, setupTLS, setupConsole)
}
