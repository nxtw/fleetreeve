package cli_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSSH stands in for ssh: it drops the connection options and runs the
// remote command locally, with a separate home directory per "host". The
// rest of the remote path (agent upload, facts, catalog over stdin, result
// stream) is exercised for real.
const fakeSSH = `#!/bin/sh
while [ $# -gt 0 ]; do
  case "$1" in
    -o|-p|-i) shift 2 ;;
    -*) shift ;;
    *) break ;;
  esac
done
host="$1"; shift
[ "$host" = down ] && { echo "connect to host down: Connection refused" >&2; exit 255; }
export HOME="$FAKE_ROOT/$host"
mkdir -p "$HOME" && cd "$HOME" && exec /bin/sh -c "$*"
`

func TestRemoteEndToEnd(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fleetreeve")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/nxtw/fleetreeve/cmd/fleetreeve").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	files := map[string]string{
		"ssh": fakeSSH,
		"inventory.yaml": `
vars: {root: ` + dir + `/nodes}
groups:
  web: {hosts: [web1, web2], vars: {role: web}}
hosts:
  web2: {vars: {role: special}}
  down: {}
`,
		"site.yaml": `
vars: {role: none}
resources:
  - file: "{{ .root }}/{{ .host.name }}"
    ensure: directory
  - file: "{{ .root }}/{{ .host.name }}/role"
    content: "{{ .role }} on {{ .facts.kernel.name }}\n"
    notify: exec[handler]
  - exec: handler
    command: "echo ran >> {{ .root }}/{{ .host.name }}/handler"
    refreshonly: true
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fleetreeve := func(wantExit int, args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "FLEETREEVE_CONFIG=none", "FLEETREEVE_SSH="+filepath.Join(dir, "ssh"), "FAKE_ROOT="+dir+"/homes", "FLEETREEVE_AGENT_DIR=")
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if code != wantExit {
			t.Fatalf("fleetreeve %v: exit %d, want %d\n%s", args, code, wantExit, out)
		}
		return string(out)
	}
	read := func(rel string) string {
		b, _ := os.ReadFile(filepath.Join(dir, "nodes", rel))
		return string(b)
	}

	// Plan reports drift (exit 2) and touches nothing.
	out := fleetreeve(2, "plan", "-i", "inventory.yaml", "-l", "web", "site.yaml")
	if !strings.Contains(out, "web1") || read("web1/role") != "" {
		t.Fatalf("plan:\n%s", out)
	}

	// Apply to all: the unreachable host fails (exit 1), the others converge.
	out = fleetreeve(1, "apply", "-i", "inventory.yaml", "site.yaml", "-report", "report.json")
	if !strings.Contains(out, "Connection refused") {
		t.Errorf("missing connection error:\n%s", out)
	}
	kernel := strings.ToLower(strings.TrimSpace(run(t, "uname", "-s")))
	if got := read("web1/role"); got != "web on "+kernel+"\n" {
		t.Errorf("web1 role: %q", got)
	}
	if got := read("web2/role"); got != "special on "+kernel+"\n" {
		t.Errorf("web2 role: %q", got)
	}
	if read("web1/handler") != "ran\n" {
		t.Errorf("handler: %q", read("web1/handler"))
	}
	var report struct {
		Hosts []struct {
			Host, Error string
			Summary     struct{ Changed, Refreshed int }
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, "report.json"))
	if err := json.Unmarshal(b, &report); err != nil || len(report.Hosts) != 3 {
		t.Fatalf("report: %v\n%s", err, b)
	}
	for _, h := range report.Hosts {
		if (h.Host == "down") != (h.Error != "") || (h.Host != "down" && (h.Summary.Changed != 2 || h.Summary.Refreshed != 1)) {
			t.Errorf("report entry: %+v", h)
		}
	}

	// The agent was uploaded once, under its content hash.
	agents, _ := filepath.Glob(filepath.Join(dir, "homes", "web1", ".cache", "fleetreeve", "agent-*"))
	if len(agents) != 1 {
		t.Errorf("agents on node: %v", agents)
	}

	// Converged: a second run is clean, and a plan exits 0.
	out = fleetreeve(0, "plan", "-i", "inventory.yaml", "-l", "web", "site.yaml")
	if !strings.Contains(out, "in sync") || read("web1/handler") != "ran\n" {
		t.Errorf("second plan:\n%s", out)
	}

	// State is kept on each node. Dropping the role file from the manifest
	// removes it from the nodes, since fleetreeve created it.
	if out = fleetreeve(0, "state", "-i", "inventory.yaml", "-l", "web1", "site.yaml"); !strings.Contains(out, "created  file["+dir+"/nodes/web1/role]") {
		t.Errorf("state:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "homes", "web1", ".local", "state", "fleetreeve", "site@web1.json")); err != nil {
		t.Errorf("state file on node: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "site.yaml"), []byte("resources:\n  - file: '{{ .root }}/{{ .host.name }}'\n    ensure: directory\n"), 0o644)
	if out = fleetreeve(2, "plan", "-i", "inventory.yaml", "-l", "web", "site.yaml"); !strings.Contains(out, "no longer in the manifest") || read("web1/role") == "" {
		t.Errorf("plan prune:\n%s", out)
	}
	fleetreeve(0, "apply", "-i", "inventory.yaml", "-l", "web", "site.yaml")
	if read("web1/role") != "" || read("web2/role") != "" || read("web1/handler") == "" {
		t.Errorf("prune: role=%q handler=%q", read("web1/role"), read("web1/handler"))
	}
	out = fleetreeve(0, "state", "rm", "-i", "inventory.yaml", "-l", "web1", "site.yaml", "file["+dir+"/nodes/web1]")
	if !strings.Contains(out, "0 resources") {
		t.Errorf("state rm:\n%s", out)
	}

	// Enrolment refuses a server address the node cannot reach, before
	// changing anything (the address here is a typo-style dead end).
	os.WriteFile(filepath.Join(dir, "secret"), []byte(strings.Repeat("s", 40)), 0o600)
	out = fleetreeve(1, "enroll", "-i", "inventory.yaml", "-l", "web1", "-server", "http://127.0.0.1:1", "-secret-file", "secret")
	if !strings.Contains(out, "cannot reach the server") && !strings.Contains(out, "systemd") {
		t.Errorf("enroll with unreachable server:\n%s", out)
	}

	// Ad-hoc command and remote facts.
	if out = fleetreeve(0, "run", "-i", "inventory.yaml", "-l", "web1", "--", "echo", "hello-$((1+1))"); !strings.Contains(out, "hello-2") {
		t.Errorf("run:\n%s", out)
	}
	if out = fleetreeve(0, "facts", "-i", "inventory.yaml", "-l", "web1"); !strings.Contains(out, `"hostname"`) {
		t.Errorf("facts:\n%s", out)
	}
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
