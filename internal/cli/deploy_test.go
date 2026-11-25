package cli_test

import (
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Client deployment: the bootstrap script downloads and verifies the
// server's build, and an installed client updates itself to it.
func TestClientDeployment(t *testing.T) {
	dir := t.TempDir()
	build := func(out string, flags ...string) {
		t.Helper()
		args := append(append([]string{"build"}, flags...), "-o", out, "github.com/nxtw/fleetreeve/cmd/fleetreeve")
		if b, err := exec.Command("go", args...).CombinedOutput(); err != nil {
			t.Fatalf("build: %v\n%s", err, b)
		}
	}
	// The server offers a newer build than the one the node runs.
	os.MkdirAll(filepath.Join(dir, "agents"), 0o755)
	offered := filepath.Join(dir, "agents", "fleetreeve-"+runtime.GOOS+"-"+runtime.GOARCH)
	build(offered, "-ldflags", "-X github.com/nxtw/fleetreeve/internal/cli.Version=9.9.9")
	server, node := filepath.Join(dir, "fleetreeve"), filepath.Join(dir, "node", "fleetreeve")
	build(server)
	os.MkdirAll(filepath.Dir(node), 0o755)
	build(node)
	sha := func(p string) string { b, _ := os.ReadFile(p); return fmt.Sprintf("%x", sha256.Sum256(b)) }

	os.WriteFile(filepath.Join(dir, "inventory.yaml"), []byte("vars: {root: "+dir+"/out}\nhosts:\n  web1: {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "site.yaml"), []byte("resources:\n  - file: '{{ .root }}'\n    content: 'v={{ .host.name }}'\n"), 0o644)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	var log logBuffer
	srv := exec.Command(server, "serve", "-i", "inventory.yaml", "-secret-file", "secret", "-listen", addr, "site.yaml")
	srv.Dir, srv.Stderr = dir, &log
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Process.Kill()
	for i := 0; ; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			break
		}
		if i > 100 {
			t.Fatalf("server did not start:\n%s", log.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	url := "http://" + addr
	run := func(bin string, args ...string) (string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "FLEETREEVE_CONFIG=none", "FLEETREEVE_STATE_DIR="+dir+"/state", "FLEETREEVE_INSTALL_DIR="+dir+"/bootstrap")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// A join token, printed as a ready-made command.
	out, err := run(server, "token", "-secret-file", "secret", "-ttl", "1h", "-server", url)
	if err != nil || !strings.Contains(out, "curl -fsSL "+url+"/install.sh | sh -s -- -server "+url+" -join join.") {
		t.Fatalf("join command: %v\n%s", err, out)
	}
	join := strings.TrimSpace(out[strings.LastIndex(out, "join."):])

	// The bootstrap script fetches the build, checks it and hands over to
	// `fleetreeve install`. On a machine that cannot host the service (no
	// systemd, or not root) that is where it must stop - having proved
	// that the downloaded build ran.
	script := "curl -fsSL " + url + "/install.sh | sh -s -- -server " + url + " -join " + join + " -host web1"
	out, err = run("sh", "-c", script)
	if runtime.GOOS != "linux" || os.Getuid() != 0 {
		if err == nil || !(strings.Contains(out, "systemd") || strings.Contains(out, "needs root")) {
			t.Errorf("bootstrap: %v\n%s", err, out)
		}
	}
	if !strings.Contains(log.String(), "download") {
		t.Errorf("bootstrap did not download the build; server log:\n%s", log.String())
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "bootstrap", ".fleetreeve-bootstrap.*")); len(left) != 0 {
		t.Errorf("bootstrap left files behind: %v", left)
	}

	// Self-update: without the flag the node keeps its build...
	out, _ = run(server, "token", "-secret-file", "secret", "web1")
	os.WriteFile(filepath.Join(dir, "token"), []byte(out), 0o600)
	before := sha(node)
	if out, err = run(node, "pull", "-server", url, "-host", "web1", "-token-file", "token"); err != nil || sha(node) != before {
		t.Fatalf("pull without -self-update: %v\n%s", err, out)
	}
	// ...with it, the binary is replaced by the server's build, restarts
	// in place and completes the run.
	out, err = run(node, "pull", "-server", url, "-host", "web1", "-token-file", "token", "-self-update")
	if err != nil || !strings.Contains(out, "updated to the server's build") || !strings.Contains(out, "in sync") {
		t.Fatalf("self-update: %v\n%s", err, out)
	}
	if sha(node) != sha(offered) {
		t.Error("node binary is not the server's build after self-update")
	}
	if out, _ = run(node, "version"); !strings.Contains(out, "9.9.9") {
		t.Errorf("version after update: %s", out)
	}
	// The packaged service passes no flags: settings come from the
	// environment (/etc/fleetreeve/pull.conf).
	env := exec.Command(node, "pull")
	env.Dir = dir
	env.Env = append(os.Environ(), "FLEETREEVE_CONFIG=none", "FLEETREEVE_STATE_DIR="+dir+"/state", "FLEETREEVE_SERVER="+url, "FLEETREEVE_HOST=web1", "FLEETREEVE_TOKEN_FILE="+dir+"/token")
	if b, err := env.CombinedOutput(); err != nil || !strings.Contains(string(b), "in sync") {
		t.Errorf("pull configured by environment: %v\n%s", err, b)
	}

	// Up to date now: nothing to do, and no restart loop.
	if out, err = run(node, "pull", "-server", url, "-host", "web1", "-token-file", "token", "-self-update"); err != nil || strings.Contains(out, "updated") {
		t.Errorf("second pull: %v\n%s", err, out)
	}
}
