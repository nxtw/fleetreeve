package cli_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// selfSigned writes a certificate and key for 127.0.0.1.
func selfSigned(t *testing.T, dir string) (cert, key string) {
	t.Helper()
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fleetreeve test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(priv)
	cert, key = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	return cert, key
}

// jsonLines parses output that must consist of JSON objects only.
func jsonLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil || e["time"] == nil || e["level"] == nil || e["msg"] == nil {
			t.Fatalf("not a JSON log record: %q", line)
		}
		events = append(events, e)
	}
	return events
}

func TestPullMode(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fleetreeve")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/nxtw/fleetreeve/cmd/fleetreeve").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("inventory.yaml", "vars: {root: "+dir+"/node}\ngroups:\n  web: {hosts: [web1], vars: {role: web}}\nhosts:\n  db1: {}\n")
	manifest := func(extra string) {
		write("site.yaml", "resources:\n  - file: '{{ .root }}'\n    ensure: directory\n  - file: '{{ .root }}/role'\n    content: \"{{ .role }} {{ .host.name }} {{ .facts.kernel.name }}\\n\"\n"+extra)
	}
	manifest("  - file: '{{ .root }}/extra'\n    content: x\n")
	cert, key := selfSigned(t, dir)

	// Find a free port, then start the server on it.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	var serverLog logBuffer
	start := func() *exec.Cmd {
		srv := exec.Command(bin, "serve", "-i", "inventory.yaml", "-secret-file", "secret", "-listen", addr,
			"-tls-cert", cert, "-tls-key", key, "-reports", "reports", "-actions", "actions", "site.yaml")
		srv.Dir, srv.Stderr = dir, &serverLog
		if err := srv.Start(); err != nil {
			t.Fatal(err)
		}
		for i := 0; ; i++ {
			if c, err := net.Dial("tcp", addr); err == nil {
				c.Close()
				return srv
			}
			if i > 100 {
				t.Fatalf("server did not start:\n%s", serverLog.String())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	srv := start()
	defer func() { srv.Process.Kill() }()

	fleetreeve := func(wantExit int, args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "FLEETREEVE_CONFIG=none", "FLEETREEVE_STATE_DIR="+dir+"/state")
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if code != wantExit {
			t.Fatalf("fleetreeve %v: exit %d, want %d\n%s\nserver log:\n%s", args, code, wantExit, out, serverLog.String())
		}
		return string(out)
	}
	read := func(name string) string { b, _ := os.ReadFile(filepath.Join(dir, "node", name)); return string(b) }
	pull := func(wantExit int, extra ...string) string {
		t.Helper()
		return fleetreeve(wantExit, append([]string{"pull", "-server", "https://" + addr, "-ca", cert, "-host", "web1", "-token-file", "token"}, extra...)...)
	}

	write("token", fleetreeve(0, "token", "-secret-file", "secret", "web1"))
	write("token-db1", fleetreeve(0, "token", "-secret-file", "secret", "db1"))

	// Dry run first: reports drift, changes nothing.
	if out := pull(2, "-plan"); read("role") != "" {
		t.Fatalf("pull -plan changed the node:\n%s", out)
	}
	// Apply: the catalog was compiled on the server from this node's facts
	// and its inventory data.
	pull(0)
	kernel, _ := exec.Command("uname", "-s").Output()
	if want := "web web1 " + strings.ToLower(strings.TrimSpace(string(kernel))) + "\n"; read("role") != want {
		t.Errorf("role: %q, want %q", read("role"), want)
	}
	if out := pull(0); !strings.Contains(out, "in sync") {
		t.Errorf("second pull:\n%s", out)
	}

	// Authentication: another host's token, a host not in the inventory,
	// and plain http to a non-local server are all refused.
	if out := pull(1, "-token-file", "token-db1"); !strings.Contains(out, "401") {
		t.Errorf("wrong token:\n%s", out)
	}
	write("token-x", fleetreeve(0, "token", "-secret-file", "secret", "intruder"))
	if out := pull(1, "-host", "intruder", "-token-file", "token-x"); !strings.Contains(out, "not in the inventory") {
		t.Errorf("unknown host:\n%s", out)
	}
	if out := fleetreeve(1, "pull", "-server", "http://example.com", "-token-file", "token"); !strings.Contains(out, "plain http") {
		t.Errorf("plain http:\n%s", out)
	}

	// A manifest edit is picked up without restarting the server, and state
	// works as in push mode: the dropped file is removed.
	time.Sleep(10 * time.Millisecond)
	manifest("")
	pull(0)
	if read("extra") != "" {
		t.Error("resource dropped from the manifest was not removed")
	}
	// A broken manifest is refused by the server; the node is untouched.
	write("site.yaml", "resources:\n  - flie: /x\n")
	if out := pull(1); !strings.Contains(out, `did you mean "file"`) {
		t.Errorf("compile error not relayed:\n%s", out)
	}
	manifest("")

	// Reports: diffs stay on the node; status reads the collected reports.
	rep, _ := os.ReadFile(filepath.Join(dir, "reports", "site@web1.json"))
	if !strings.Contains(string(rep), `"host": "web1"`) || strings.Contains(string(rep), `"diff"`) {
		t.Errorf("report:\n%s", rep)
	}
	out := fleetreeve(0, "status", "-reports", "reports", "-i", "inventory.yaml")
	if !strings.Contains(out, "web1") || !strings.Contains(out, "never reported") {
		t.Errorf("status:\n%s", out)
	}

	// Logging: with -log-format json a run is one JSON object per line,
	// and the server's own log is JSON throughout.
	os.WriteFile(filepath.Join(dir, "node", "role"), []byte("drift"), 0o644)
	events := jsonLines(t, pull(0, "-log-format", "json"))
	last := events[len(events)-1]
	if len(events) != 2 || events[0]["msg"] != "resource" || events[0]["status"] != "changed" || events[0]["drifted"] != true ||
		last["msg"] != "run finished" || last["host"] != "web1" || last["changed"] != float64(1) || last["level"] != "INFO" {
		t.Errorf("json run log: %v", events)
	}
	if fail := jsonLines(t, pull(1, "-log-format", "json", "-token-file", "token-db1")); len(fail) != 1 || fail[0]["level"] != "ERROR" {
		t.Errorf("json error log: %v", fail)
	}
	seen := map[any]bool{}
	for _, e := range jsonLines(t, serverLog.String()) {
		seen[e["msg"]] = true
	}
	for _, msg := range []string{"serving", "catalog served", "report received", "request refused", "catalog: compile error"} {
		if !seen[msg] {
			t.Errorf("server log has no %q record", msg)
		}
	}

	// A push run (fleetreeve apply from a controller) can report to the same
	// server. It is kept next to the node's own pull report, not over it.
	os.WriteFile(filepath.Join(dir, "local.yaml"), []byte("hosts:\n  web1: {connection: local}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "push.yaml"), []byte("name: pushed\nresources:\n  - file: "+dir+"/node/pushed\n    content: secret-content\n"), 0o644)
	out = fleetreeve(0, "apply", "-i", "local.yaml", "push.yaml", "-report-to", "https://"+addr, "-ca", cert, "-secret-file", "secret")
	if !strings.Contains(out, "Reported 1 host(s)") {
		t.Errorf("push report:\n%s", out)
	}
	pushed, _ := os.ReadFile(filepath.Join(dir, "reports", "pushed@web1.json"))
	if !strings.Contains(string(pushed), `"source": "push"`) || !strings.Contains(string(pushed), `"changed": 1`) || strings.Contains(string(pushed), "secret-content") {
		t.Errorf("stored push report:\n%s", pushed)
	}
	out = fleetreeve(0, "status", "-reports", "reports")
	if !strings.Contains(out, "pushed (push)") || !strings.Contains(out, "site (pull)") {
		t.Errorf("status with both kinds of run:\n%s", out)
	}
	// The same status from anywhere, through the server's API: with the
	// secret (from which the read token is derived) or a token on its own.
	remote := fleetreeve(0, "status", "-server", "https://"+addr, "-ca", cert, "-secret-file", "secret")
	if !strings.Contains(remote, "pushed (push)") || !strings.Contains(remote, "site (pull)") || !strings.Contains(remote, "never reported") {
		t.Errorf("status through the API:\n%s", remote)
	}
	apiToken := strings.TrimSpace(fleetreeve(0, "token", "-secret-file", "secret", "-api", "read"))
	os.Setenv("FLEETREEVE_API_TOKEN", apiToken)
	if out = fleetreeve(0, "status", "-server", "https://"+addr, "-ca", cert); !strings.Contains(out, "pushed (push)") {
		t.Errorf("status with FLEETREEVE_API_TOKEN:\n%s", out)
	}
	os.Unsetenv("FLEETREEVE_API_TOKEN")
	if out = fleetreeve(1, "status", "-server", "https://"+addr, "-ca", cert); !strings.Contains(out, "API token") {
		t.Errorf("status without a token:\n%s", out)
	}

	// An enrolled node reports push runs by itself: the controller is
	// given neither the server nor its secret, only the node has its own
	// settings and token.
	write("pull.conf", "# node settings\nFLEETREEVE_SERVER=\"https://"+addr+"\"\nFLEETREEVE_HOST=\"web1\"\nFLEETREEVE_TOKEN_FILE="+filepath.Join(dir, "token")+"\nFLEETREEVE_CA="+cert+"\n")
	os.Setenv("FLEETREEVE_NODE_CONF", filepath.Join(dir, "pull.conf"))
	os.WriteFile(filepath.Join(dir, "own.yaml"), []byte("name: own\nresources:\n  - file: "+dir+"/node/own\n    content: secret-content\n"), 0o644)
	if out = fleetreeve(0, "apply", "-i", "local.yaml", "own.yaml"); !strings.Contains(out, "Reported by 1 host(s) to https://"+addr) {
		t.Errorf("a node reporting its own push run:\n%s", out)
	}
	own, _ := os.ReadFile(filepath.Join(dir, "reports", "own@web1.json"))
	if !strings.Contains(string(own), `"source": "push"`) || !strings.Contains(string(own), `"mode": "apply"`) || !strings.Contains(string(own), `"changed": 1`) || strings.Contains(string(own), "secret-content") {
		t.Errorf("stored report of the node's own push run:\n%s", own)
	}
	// A dry run is reported as one; -no-report keeps a run private; and
	// with -report-to the controller reports, not the node as well.
	os.WriteFile(filepath.Join(dir, "own.yaml"), []byte("name: own\nresources:\n  - file: "+dir+"/node/own\n    content: changed\n"), 0o644)
	fleetreeve(2, "plan", "-i", "local.yaml", "own.yaml")
	if own, _ = os.ReadFile(filepath.Join(dir, "reports", "own@web1.json")); !strings.Contains(string(own), `"mode": "plan"`) {
		t.Errorf("stored report of a dry run:\n%s", own)
	}
	if out = fleetreeve(0, "apply", "-i", "local.yaml", "own.yaml", "-no-report"); strings.Contains(out, "Reported") {
		t.Errorf("-no-report:\n%s", out)
	}
	if own, _ = os.ReadFile(filepath.Join(dir, "reports", "own@web1.json")); !strings.Contains(string(own), `"mode": "plan"`) {
		t.Error("-no-report still reported")
	}
	if out = fleetreeve(0, "apply", "-i", "local.yaml", "own.yaml", "-report-to", "https://"+addr, "-ca", cert, "-secret-file", "secret"); !strings.Contains(out, "Reported 1 host(s)") || strings.Contains(out, "Reported by") {
		t.Errorf("-report-to on an enrolled node:\n%s", out)
	}
	// A server that cannot be reached is a warning, not a failed run.
	write("pull.conf", "FLEETREEVE_SERVER=\"https://127.0.0.1:1\"\nFLEETREEVE_HOST=\"web1\"\nFLEETREEVE_TOKEN_FILE="+filepath.Join(dir, "token")+"\n")
	if out = fleetreeve(0, "apply", "-i", "local.yaml", "own.yaml"); !strings.Contains(out, "could not report this run") {
		t.Errorf("unreachable server:\n%s", out)
	}
	os.Setenv("FLEETREEVE_NODE_CONF", "none")

	// A host the server does not know is refused, and says so, without
	// failing the run itself.
	os.WriteFile(filepath.Join(dir, "other.yaml"), []byte("hosts:\n  stranger: {connection: local}\n"), 0o644)
	if out = fleetreeve(0, "apply", "-i", "other.yaml", "push.yaml", "-report-to", "https://"+addr, "-ca", cert, "-secret-file", "secret"); !strings.Contains(out, "not in the inventory") {
		t.Errorf("push report for unknown host:\n%s", out)
	}

	// A rollback asked for through the server: queued with the operator
	// token, collected and carried out by the node at its next pull, and
	// reported back. The server never connects to the node.
	manifest("  - file: '{{ .root }}/app.conf'\n    content: v1\n")
	pull(0)
	manifest("  - file: '{{ .root }}/app.conf'\n    content: v2\n")
	pull(0)
	pem, _ := os.ReadFile(cert)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	api := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	queue := func(kind string) int {
		operator := strings.TrimSpace(fleetreeve(0, "token", "-secret-file", "secret", "-api", "operator"))
		req, _ := http.NewRequest("POST", "https://"+addr+"/v1/hosts/web1/actions",
			strings.NewReader(`{"manifest":"site","kind":"`+kind+`","resource":"file[`+dir+`/node/app.conf]","requested_by":"test"}`))
		req.Header.Set("Authorization", "Bearer "+operator)
		resp, err := api.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := queue("rollback"); code != 202 {
		t.Fatalf("queueing a rollback: http %d\n%s", code, serverLog.String())
	}
	if out = pull(0, "-plan"); read("app.conf") != "v2" { // a dry run leaves requests alone
		t.Errorf("a dry run carried out the rollback:\n%s", out)
	}
	out = pull(0)
	if read("app.conf") != "v1" || !strings.Contains(out, "held") || !strings.Contains(out, "rollback of file[") {
		t.Errorf("rollback through the server: content %q\n%s", read("app.conf"), out)
	}
	stored, _ := os.ReadFile(filepath.Join(dir, "reports", "site@web1.json"))
	if !strings.Contains(string(stored), `"status": "restored"`) || !strings.Contains(string(stored), `"requested_by": "test"`) || !strings.Contains(string(stored), `"restorable"`) {
		t.Errorf("report lacks the action:\n%s", stored)
	}
	if queue("release") != 202 {
		t.Fatal("queueing a release")
	}
	if pull(0); read("app.conf") != "v2" {
		t.Errorf("after release: %q", read("app.conf"))
	}
	manifest("")
	pull(0)

	// "Run now" and a command, asked for through the server. A node
	// running as a service (-every) asks the server for requests between
	// its scheduled runs.
	post := func(tokenRole, body string) int {
		tok := strings.TrimSpace(fleetreeve(0, "token", "-secret-file", "secret", "-api", tokenRole))
		req, _ := http.NewRequest("POST", "https://"+addr+"/v1/hosts/web1/actions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := api.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	svc := exec.Command(bin, "pull", "-server", "https://"+addr, "-ca", cert, "-host", "web1", "-token-file", "token", "-every", "1h", "-poll", "5s", "-log-format", "json")
	svc.Dir = dir
	svc.Env = append(os.Environ(), "FLEETREEVE_CONFIG=none", "FLEETREEVE_STATE_DIR="+dir+"/state")
	var svcLog logBuffer
	svc.Stdout, svc.Stderr = &svcLog, &svcLog
	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	defer svc.Process.Kill()
	time.Sleep(1500 * time.Millisecond) // its first scheduled run
	os.WriteFile(filepath.Join(dir, "node", "role"), []byte("tampered"), 0o644)
	marker := filepath.Join(dir, "node", "ran-as")
	if post("operator", `{"kind":"command","command":"id"}`) != 403 {
		t.Error("the operator token queued a command")
	}
	if post("exec", `{"kind":"command","command":"echo ran-on-$(hostname) > `+marker+`; echo out; exit 4","requested_by":"admin"}`) != 202 || post("operator", `{"kind":"apply","requested_by":"op"}`) != 202 {
		t.Fatal("queueing a command and a run")
	}
	deadline := time.Now().Add(20 * time.Second)
	for read("role") == "tampered" && time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
	}
	if read("role") == "tampered" || read("ran-as") == "" {
		t.Fatalf("the node did not act on the requests within 20s (next scheduled run: in 1h)\n%s", svcLog.String())
	}
	time.Sleep(500 * time.Millisecond)
	results, _ := os.ReadFile(filepath.Join(dir, "actions", "web1.results.json"))
	for _, want := range []string{`"kind": "command"`, `"output": "out\n"`, `"exit": 4`, `"kind": "apply"`, `"status": "done"`, `"requested_by": "op"`} {
		if !strings.Contains(string(results), want) {
			t.Errorf("stored results lack %s:\n%s", want, results)
		}
	}
	svc.Process.Kill()
	svc.Wait()

	// Server down: the node keeps enforcing its cached catalog.
	srv.Process.Kill()
	srv.Wait()
	os.WriteFile(filepath.Join(dir, "node", "role"), []byte("tampered"), 0o644)
	if out := pull(0); !strings.Contains(out, "cached catalog") || read("role") == "tampered" {
		t.Errorf("offline pull:\n%s", out)
	}
	// Back up: the report shows up again.
	srv = start()
	pull(0)
}
