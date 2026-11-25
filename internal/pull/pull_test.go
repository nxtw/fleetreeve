package pull

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/nxtw/fleetreeve/internal/types"
)

func TestJoinToken(t *testing.T) {
	secret, other := []byte(strings.Repeat("s", 32)), []byte(strings.Repeat("o", 32))
	now := time.Now()
	tok := JoinToken(secret, now.Add(time.Hour))
	if err := CheckJoinToken(secret, tok, now); err != nil {
		t.Errorf("valid token refused: %v", err)
	}
	if err := CheckJoinToken(secret, tok, now.Add(2*time.Hour)); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired token: %v", err)
	}
	if CheckJoinToken(other, tok, now) == nil {
		t.Error("token from another secret accepted")
	}
	// Extending the expiry invalidates the signature.
	parts := strings.Split(tok, ".")
	if CheckJoinToken(secret, "join.9999999999."+parts[2], now) == nil {
		t.Error("tampered expiry accepted")
	}
	// A node token is not a join token, and vice versa.
	if CheckJoinToken(secret, Token(secret, "web1"), now) == nil || Token(secret, "join:"+parts[1]) == Token(secret, "web1") {
		t.Error("node token accepted as join token")
	}
}

func TestJoinAndAgentEndpoints(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "inv.yaml"), []byte("hosts:\n  web1: {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "fleetreeve-plan9-amd64"), []byte("fake build"), 0o755)
	t.Setenv("FLEETREEVE_AGENT_DIR", dir)
	secret := []byte(strings.Repeat("s", 32))
	s := &Server{Inventory: filepath.Join(dir, "inv.yaml"), Secret: secret, Log: slog.New(slog.DiscardHandler)}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	ctx := context.Background()
	client := func(host string) *Client {
		c, err := NewClient(ts.URL, host, "", "", false)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	// A join token yields exactly the host's own node token.
	join := JoinToken(secret, time.Now().Add(time.Minute))
	tok, err := client("web1").Join(ctx, join)
	if err != nil || tok != Token(secret, "web1") {
		t.Errorf("join: %q %v", tok, err)
	}
	// Not for hosts outside the inventory, expired tokens or node tokens.
	if _, err := client("intruder").Join(ctx, join); err == nil || !strings.Contains(err.Error(), "not in the inventory") {
		t.Errorf("join as unknown host: %v", err)
	}
	if _, err := client("web1").Join(ctx, JoinToken(secret, time.Now().Add(-time.Minute))); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired join: %v", err)
	}
	if _, err := client("web1").Join(ctx, Token(secret, "web1")); err == nil {
		t.Error("node token accepted for joining")
	}

	// Ping: a reachable fleetreeve server, and one that is not there.
	if err := client("web1").Ping(ctx); err != nil {
		t.Errorf("ping: %v", err)
	}
	gone, _ := NewClient("http://127.0.0.1:1", "web1", "", "", false)
	if err := gone.Ping(ctx); err == nil {
		t.Error("ping of an unreachable server succeeded")
	}

	// A node that missed two check-ins is stale; a one-off run never is.
	now := time.Now()
	old := now.Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	if !(&Report{Time: old, Interval: 1800}).Stale(now) || (&Report{Time: old}).Stale(now) ||
		(&Report{Time: now.Add(-40 * time.Minute).UTC().Format(time.RFC3339), Interval: 1800}).Stale(now) {
		t.Error("staleness")
	}

	// Reports are kept per host and manifest. A push report must not
	// displace the pull report of a client too old to name its manifest;
	// that entry goes only when the pull client itself starts naming it.
	s.Reports = filepath.Join(dir, "reports")
	node, _ := NewClient(ts.URL, "web1", Token(secret, "web1"), "", false)
	stored := func() string {
		files, _ := filepath.Glob(filepath.Join(s.Reports, "*.json"))
		for i, f := range files {
			files[i] = filepath.Base(f)
		}
		return strings.Join(files, " ")
	}
	node.Report(ctx, &Report{Host: "web1", Mode: "apply"})
	node.Report(ctx, &Report{Host: "web1", Mode: "apply", Manifest: "fleet", Source: "push"})
	if got := stored(); got != "fleet@web1.json web1.json" {
		t.Errorf("after a push report: %s", got)
	}
	node.Report(ctx, &Report{Host: "web1", Mode: "apply", Manifest: "site", Source: "pull"})
	if got := stored(); got != "fleet@web1.json site@web1.json" {
		t.Errorf("after an upgraded pull report: %s", got)
	}

	// Host registration: a host outside the inventory file is refused
	// until the controller registers it; then it gets its catalog data
	// from the registry, and unregistering revokes it again.
	admin := AdminToken(secret)
	s.Manifest = filepath.Join(dir, "site.yaml")
	os.WriteFile(s.Manifest, []byte("resources:\n  - file: /tmp/x\n    content: '{{ .colour }} {{ .host.groups | join \",\" }} {{ .size }}'\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "inv.yaml"), []byte("vars: {colour: grey, size: 1}\ngroups:\n  web: {hosts: [web1], vars: {colour: green}}\nhosts:\n  web1: {}\n"), 0o644)
	newNode, _ := NewClient(ts.URL, "new1", Token(secret, "new1"), "", false)
	if _, err := newNode.Catalog(ctx, nil); err == nil || !strings.Contains(err.Error(), "not in the inventory") {
		t.Errorf("unregistered host: %v", err)
	}
	entry := HostEntry{Name: "new1", Groups: []string{"web", "edge"}, Vars: map[string]any{"size": 7}}
	if _, err := newNode.RegisterHost(ctx, admin, entry); err == nil || !strings.Contains(err.Error(), "-hosts") {
		t.Errorf("registration on a server without a registry: %v", err)
	}
	s.Hosts = filepath.Join(dir, "hosts")
	if _, err := newNode.RegisterHost(ctx, Token(secret, "new1"), entry); err == nil {
		t.Error("a node token was accepted as admin token")
	}
	res, err := newNode.RegisterHost(ctx, admin, entry)
	if err != nil || res.Status != "registered" || len(res.UnknownGroups) != 1 || res.UnknownGroups[0] != "edge" {
		t.Errorf("register: %+v %v", res, err)
	}
	cat, err := newNode.Catalog(ctx, nil)
	if err != nil || len(cat.Resources) != 1 || cat.Resources[0].Attrs["content"] != "green web,edge 7" {
		t.Errorf("catalog of a registered host: %v %v", cat, err)
	}
	// A host the inventory file lists is not duplicated in the registry.
	if res, err = client("web1").RegisterHost(ctx, admin, HostEntry{Name: "web1"}); err != nil || res.Status != "inventory" {
		t.Errorf("register a listed host: %+v %v", res, err)
	}
	if files, _ := filepath.Glob(filepath.Join(s.Hosts, "*")); len(files) != 1 {
		t.Errorf("registry: %v", files)
	}
	if err := newNode.UnregisterHost(ctx, admin, "new1"); err != nil {
		t.Error(err)
	}
	if _, err := newNode.Catalog(ctx, nil); err == nil {
		t.Error("unregistered host can still pull")
	}

	// ---- management API ----
	call := func(method, path, token, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if resp.StatusCode == http.StatusNoContent {
			return resp.StatusCode, out
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("%s %s: response is not JSON: %v", method, path, err)
		}
		return resp.StatusCode, out
	}
	read, node1 := APIToken(secret, "read"), Token(secret, "web1")
	s.Version = "1.2.3"

	// Health needs no token; everything else does, and a node's own
	// token is not an API token.
	if code, out := call("GET", "/v1/health", "", ""); code != 200 || out["status"] != "ok" || out["version"] != "1.2.3" {
		t.Errorf("health: %d %v", code, out)
	}
	for _, tok := range []string{"", node1, "garbage"} {
		if code, out := call("GET", "/v1/status", tok, ""); code != 401 || out["error"] == nil {
			t.Errorf("status with token %q: %d %v", tok, code, out)
		}
	}

	// Status: the stored reports, their verdict, and who never reported.
	newNode.RegisterHost(ctx, admin, entry)
	code, out := call("GET", "/v1/status", read, "")
	reports, _ := out["reports"].([]any)
	if code != 200 || len(reports) != 2 || out["healthy"] != true || len(out["missing"].([]any)) != 1 || out["missing"].([]any)[0] != "new1" {
		t.Errorf("status: %d %v", code, out)
	}
	if first := reports[0].(map[string]any); first["host"] != "web1" || first["manifest"] != "fleet" || first["source"] != "push" || first["state"] != "ok" {
		t.Errorf("status entry: %v", first)
	}

	// Hosts: where each comes from; details include the merged variables,
	// for the admin token only.
	code, out = call("GET", "/v1/hosts", read, "")
	hosts, _ := out["hosts"].([]any)
	if code != 200 || len(hosts) != 2 || hosts[0].(map[string]any)["source"] != "registered" || hosts[1].(map[string]any)["source"] != "inventory" {
		t.Errorf("hosts: %d %v", code, out)
	}
	code, out = call("GET", "/v1/hosts/new1", admin, "")
	if vars, _ := out["vars"].(map[string]any); code != 200 || vars["colour"] != "green" || vars["size"] != float64(7) || out["registered"] == nil {
		t.Errorf("host detail: %d %v", code, out)
	}
	if code, out = call("GET", "/v1/hosts/new1", read, ""); code != 200 || out["vars"] != nil || out["registered"] == nil {
		t.Errorf("host detail with the read token shows variables: %d %v", code, out)
	}
	if code, _ = call("GET", "/v1/hosts/nobody", read, ""); code != 404 {
		t.Errorf("unknown host: %d", code)
	}
	if code, out = call("GET", "/v1/hosts/web1/reports/fleet", read, ""); code != 200 || out["source"] != "push" {
		t.Errorf("report: %d %v", code, out)
	}
	if code, _ = call("GET", "/v1/hosts/web1/reports/nope", read, ""); code != 404 {
		t.Errorf("unknown report: %d", code)
	}

	// Minting tokens and changing hosts is for the admin token only.
	if code, _ = call("POST", "/v1/tokens", read, `{"host":"web1"}`); code != 401 {
		t.Errorf("read token minted a node token: %d", code)
	}
	if code, _ = call("DELETE", "/v1/hosts/new1", read, ""); code != 401 {
		t.Errorf("read token removed a host: %d", code)
	}
	if code, out = call("POST", "/v1/tokens", admin, `{"host":"web1"}`); code != 200 || out["token"] != node1 {
		t.Errorf("node token: %d %v", code, out)
	}
	if code, _ = call("POST", "/v1/tokens", admin, `{"host":"nobody"}`); code != 404 {
		t.Errorf("token for an unknown host: %d", code)
	}
	code, out = call("POST", "/v1/join-tokens", admin, `{"ttl":"2h"}`)
	if tok, _ := out["token"].(string); code != 200 || CheckJoinToken(secret, tok, time.Now().Add(time.Hour)) != nil || CheckJoinToken(secret, tok, time.Now().Add(3*time.Hour)) == nil {
		t.Errorf("join token: %d %v", code, out)
	}
	if code, _ = call("POST", "/v1/join-tokens", admin, `{"ttl":"soon"}`); code != 400 {
		t.Errorf("bad ttl: %d", code)
	}
	newNode.UnregisterHost(ctx, admin, "new1")

	// ---- actions: rollback requests queued for a node ----
	operator := APIToken(secret, "operator")
	body := `{"manifest":"site","kind":"rollback","resource":"File[/etc/motd]","requested_by":"ops"}`
	if code, out = call("POST", "/v1/hosts/web1/actions", operator, body); code != 409 || !strings.Contains(out["error"].(string), "-actions") {
		t.Errorf("queue on a server without actions: %d %v", code, out)
	}
	s.Actions = filepath.Join(dir, "actions")
	for _, tok := range []string{read, node1, ""} { // reading, or being a node, is not enough
		if code, _ = call("POST", "/v1/hosts/web1/actions", tok, body); code != 401 {
			t.Errorf("queue with token %q: %d", tok, code)
		}
	}
	for bad, want := range map[string]int{
		`{"manifest":"site","kind":"reboot","resource":"file[/x]"}`:    400, // not a kind of request
		`{"manifest":"../x","kind":"rollback","resource":"file[/x]"}`:  400,
		`{"manifest":"site","kind":"rollback","resource":"not a ref"}`: 400,
	} {
		if code, _ = call("POST", "/v1/hosts/web1/actions", operator, bad); code != want {
			t.Errorf("%s: %d, want %d", bad, code, want)
		}
	}
	if code, _ = call("POST", "/v1/hosts/nobody/actions", operator, body); code != 404 {
		t.Errorf("queue for an unknown host: %d", code)
	}
	code, out = call("POST", "/v1/hosts/web1/actions", operator, body)
	actionID, _ := out["id"].(string)
	if code != 202 || actionID == "" || out["resource"] != "file[/etc/motd]" || out["requested_by"] != "ops" {
		t.Errorf("queue: %d %v", code, out)
	}
	if code, _ = call("POST", "/v1/hosts/web1/actions", operator, body); code != 409 {
		t.Errorf("the same request twice: %d", code)
	}
	// It shows as waiting on the host, can be withdrawn, and a node
	// collects what is waiting exactly once - only its own.
	if _, out = call("GET", "/v1/hosts/web1", read, ""); len(out["pending_actions"].([]any)) != 1 {
		t.Errorf("pending: %v", out["pending_actions"])
	}
	if code, _ = call("DELETE", "/v1/hosts/web1/actions/"+actionID, operator, ""); code != 204 {
		t.Errorf("cancel: %d", code)
	}
	if code, _ = call("DELETE", "/v1/hosts/web1/actions/"+actionID, operator, ""); code != 404 {
		t.Errorf("cancel again: %d", code)
	}
	call("POST", "/v1/hosts/web1/actions", operator, body)
	if got, err := node.Claim(ctx); err != nil || len(got) != 1 || got[0].Kind != "rollback" || got[0].Host != "web1" {
		t.Errorf("claim: %+v %v", got, err)
	}
	if got, _ := node.Claim(ctx); len(got) != 0 {
		t.Errorf("claimed twice: %+v", got)
	}
	if code, _ = call("POST", "/v1/actions/claim", operator, `{"host":"web1"}`); code != 401 {
		t.Errorf("an operator collected a node's requests: %d", code)
	}

	// Runs are for the operator token; a command is root on the node and
	// needs the exec token, which the operator token is not.
	execToken := APIToken(secret, "exec")
	if code, out = call("POST", "/v1/hosts/web1/actions", operator, `{"kind":"apply","command":"smuggled","resource":"x"}`); code != 202 || out["command"] != nil || out["resource"] != nil {
		t.Errorf("queue a run: %d %v", code, out)
	}
	if code, _ = call("POST", "/v1/hosts/web1/actions", operator, `{"kind":"plan"}`); code != 202 {
		t.Errorf("queue a dry run: %d", code)
	}
	if code, out = call("POST", "/v1/hosts/web1/actions", operator, `{"kind":"command","command":"id"}`); code != 403 || !strings.Contains(out["error"].(string), "exec") {
		t.Errorf("a command with the operator token: %d %v", code, out)
	}
	for bad, want := range map[string]int{`{"kind":"command","command":"  "}`: 400, `{"kind":"command","command":"id","timeout":9999}`: 400} {
		if code, _ = call("POST", "/v1/hosts/web1/actions", execToken, bad); code != want {
			t.Errorf("%s: %d", bad, code)
		}
	}
	if code, out = call("POST", "/v1/hosts/web1/actions", execToken, `{"kind":"command","command":"echo hi; echo err >&2; exit 3","requested_by":"root1"}`); code != 202 || out["timeout"] != float64(60) {
		t.Errorf("queue a command: %d %v", code, out)
	}
	// Who may only look sees that a command waits, not what it says.
	waiting := func(token string) string {
		_, out := call("GET", "/v1/hosts/web1", token, "")
		list, _ := out["pending_actions"].([]any)
		for _, a := range list {
			if m := a.(map[string]any); m["kind"] == "command" {
				cmd, _ := m["command"].(string)
				return "command:" + cmd
			}
		}
		return "none"
	}
	if got := waiting(read); got != "command:" {
		t.Errorf("the read token sees of a waiting command: %q", got)
	}
	if got := waiting(AdminToken(secret)); !strings.HasPrefix(got, "command:echo hi") {
		t.Errorf("the admin token sees of a waiting command: %q", got)
	}

	// The node collects them, carries out the command itself, hands the
	// runs back to its caller, and reports.
	claimed, _ := node.Claim(ctx)
	done, runs := Perform(ctx, "web1", claimed)
	if len(claimed) != 3 || len(runs) != 2 || len(done) != 1 || done[0].Exit != 3 || done[0].Status != "failed" || done[0].Output != "hi\nerr\n" {
		t.Fatalf("perform: runs=%d done=%+v", len(runs), done)
	}
	if err := node.PostResults(ctx, done); err != nil {
		t.Fatal(err)
	}
	// Results: visible with the operator token, not with the read token.
	if code, _ = call("GET", "/v1/actions", read, ""); code != 401 {
		t.Errorf("results with the read token: %d", code)
	}
	code, out = call("GET", "/v1/actions", operator, "")
	if list, _ := out["results"].([]any); code != 200 || len(list) != 1 || list[0].(map[string]any)["output"] != "hi\nerr\n" || list[0].(map[string]any)["host"] != "web1" {
		t.Errorf("results: %d %v", code, out)
	}
	// A node reports for itself only, whatever host it names.
	newNode.RegisterHost(ctx, admin, entry)
	newNode.PostResults(ctx, []ActionResult{{Action: Action{ID: "x", Host: "web1", Kind: "command"}, Status: "done", Output: "forged"}})
	_, out = call("GET", "/v1/actions", operator, "")
	for _, r := range out["results"].([]any) {
		if m := r.(map[string]any); m["output"] == "forged" && m["host"] != "new1" {
			t.Errorf("a node reported a result in another host's name: %v", m)
		}
	}
	newNode.UnregisterHost(ctx, admin, "new1")

	// A command that outlives its time is stopped; long output is cut.
	if out, exit, note := runCommand(ctx, "sleep 5", 1); exit != -1 || !strings.Contains(note, "did not finish") || out != "" {
		t.Errorf("timeout: %q %d %q", out, exit, note)
	}
	if out, _, _ := runCommand(ctx, "head -c 200000 /dev/zero | tr '\\0' x", 10); len(out) > maxOutput+100 || !strings.Contains(out, "cut off") {
		t.Errorf("long output: %d bytes", len(out))
	}

	// Builds and their checksums.
	c := client("web1")
	if b, err := c.Agent(ctx, "plan9-amd64"); err != nil || string(b) != "fake build" {
		t.Errorf("agent: %q %v", b, err)
	}
	if sha, err := c.AgentSHA(ctx, "plan9-amd64"); err != nil || len(sha) != 64 {
		t.Errorf("sha: %q %v", sha, err)
	}
	if _, err := c.Agent(ctx, "plan9-mips"); err == nil || !strings.Contains(err.Error(), "no fleetreeve build") {
		t.Errorf("missing platform: %v", err)
	}
	if _, err := c.Agent(ctx, "..%2f..%2fetc%2fpasswd"); err == nil {
		t.Error("path traversal not refused")
	}

	// The install script is valid shell.
	script := filepath.Join(dir, "install.sh")
	os.WriteFile(script, []byte(InstallScript), 0o644)
	if out, err := exec.Command("sh", "-n", script).CombinedOutput(); err != nil {
		t.Errorf("install.sh: %v\n%s", err, out)
	}
}
