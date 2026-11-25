package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/nxtw/fleetreeve/internal/pull"
)

// fakeServer answers like a fleetreeve server with one host that has a secret
// in its variables.
type fakeServer struct {
	down     bool
	operator bool
	exec     bool
	sent     []string
}

func (f *fakeServer) CanAct() bool  { return f.operator }
func (f *fakeServer) CanExec() bool { return f.exec }

func (f *fakeServer) SendExec(_ context.Context, path string, body any) ([]byte, error) {
	b, _ := json.Marshal(body)
	f.sent = append(f.sent, "EXEC "+path+" "+string(b))
	return []byte(`{"id":"cmd1"}`), nil
}

func (f *fakeServer) Send(_ context.Context, method, path string, body any) ([]byte, error) {
	if method == "GET" && path == "/v1/actions" {
		return []byte(`{"pending":[{"id":"p1","host":"web1","kind":"command","command":"cat /etc/shadow"}],
			"results":[{"id":"r1","host":"web1","kind":"command","command":"cat /etc/shadow","output":"root:$6$secret","status":"done"},
			           {"id":"r2","host":"web1","kind":"apply","status":"done","detail":"3 resources · in sync"}]}`), nil
	}
	b, _ := json.Marshal(body)
	f.sent = append(f.sent, method+" "+path+" "+string(b))
	return []byte(`{"id":"abc"}`), nil
}

func (f *fakeServer) Get(_ context.Context, path string) ([]byte, error) {
	if f.down {
		return nil, fmt.Errorf("server unavailable: connection refused")
	}
	entry := pull.StatusEntry{Host: "web1", Manifest: "site", Source: "pull", Mode: "apply", State: "failed"}
	host := pull.HostInfo{Name: "web1", Groups: []string{"web"}, Source: "inventory", Reports: []pull.StatusEntry{entry},
		Vars: map[string]any{"db_password": "hunter2"}}
	switch path {
	case "/v1/health":
		return []byte(`{"status":"ok","version":"9.9"}`), nil
	case "/v1/status":
		return json.Marshal(pull.StatusResponse{Reports: []pull.StatusEntry{entry}, Missing: []string{}})
	case "/v1/hosts":
		return json.Marshal(map[string]any{"hosts": []pull.HostInfo{host}})
	case "/v1/hosts/web1":
		return json.Marshal(host)
	case "/v1/hosts/web1/reports/site":
		return json.Marshal(pull.Report{Host: "web1", Manifest: "site", Error: "boom"})
	}
	return nil, fmt.Errorf("server refused (404 Not Found): no such host")
}

// signIn returns the session cookie for a name and password, or nil.
func signIn(t *testing.T, base, user, pass string) *http.Cookie {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"user": user, "password": pass})
	resp, err := http.Post(base+"/api/login", "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c
		}
	}
	return nil
}

// get fetches a URL, signed in as user when one is given.
func get(t *testing.T, url, user, pass string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if user != "" {
		if c := signIn(t, url[:strings.Index(url[8:], "/")+8], user, pass); c != nil {
			req.AddCookie(c)
		}
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestDashboard(t *testing.T) {
	src := &fakeServer{}
	d := &Dashboard{Source: src, Server: "https://fleetreeve.example.com:8140", Log: slog.New(slog.DiscardHandler)}
	ts := httptest.NewServer(d.Handler())
	defer ts.Close()
	d.poll(context.Background())

	// The page and its assets are served from the binary.
	for path, want := range map[string]string{"/": "fleetreeve console", "/app.js": "openHost", "/app.css": "--accent"} {
		if code, body := get(t, ts.URL+path, "", ""); code != 200 || !strings.Contains(body, want) {
			t.Errorf("%s: %d", path, code)
		}
	}
	code, body := get(t, ts.URL+"/api/overview", "", "")
	var o Overview
	if err := json.Unmarshal([]byte(body), &o); err != nil || code != 200 || o.Version != "9.9" || len(o.Hosts) != 1 || len(o.History) != 1 || o.History[0].Failed != 1 {
		t.Errorf("overview: %d %s", code, body)
	}
	// Host variables can hold secrets; they never reach the browser.
	if strings.Contains(body, "hunter2") || strings.Contains(body, "db_password") {
		t.Error("overview leaks host variables")
	}
	code, body = get(t, ts.URL+"/api/hosts/web1", "", "")
	if code != 200 || strings.Contains(body, "hunter2") || !strings.Contains(body, `"error":"boom"`) || !strings.Contains(body, `"state":"failed"`) {
		t.Errorf("host detail: %d %s", code, body)
	}
	if code, _ = get(t, ts.URL+"/api/hosts/nobody", "", ""); code != 404 {
		t.Errorf("unknown host: %d", code)
	}

	// When the server goes away the last data stays, flagged as old.
	src.down = true
	d.poll(context.Background())
	_, body = get(t, ts.URL+"/api/overview", "", "")
	json.Unmarshal([]byte(body), &o)
	if o.Error == "" || len(o.Hosts) != 1 {
		t.Errorf("after an outage: error=%q hosts=%d", o.Error, len(o.Hosts))
	}

	// With accounts, everything but the health check and the sign-in page
	// needs signing in.
	d.Users, _ = NewAccounts("", "ops:s3cret")
	if code, _ = get(t, ts.URL+"/api/overview", "", ""); code != 401 {
		t.Errorf("without login: %d", code)
	}
	if code, _ = get(t, ts.URL+"/api/overview", "ops", "wrong"); code != 401 {
		t.Errorf("wrong password: %d", code)
	}
	if code, _ = get(t, ts.URL+"/api/overview", "ops", "s3cret"); code != 200 {
		t.Errorf("with login: %d", code)
	}
	if code, _ = get(t, ts.URL+"/healthz", "", ""); code != 200 {
		t.Errorf("health check: %d", code)
	}

	// A name and password sent with the request itself do not sign in.
	// Browsers keep resending those and cannot be told to stop, which
	// would make signing out impossible.
	for _, path := range []string{"/api/overview", "/api/me", "/"} {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.SetBasicAuth("ops", "s3cret")
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 && !strings.Contains(string(body), `"user":""`) {
			t.Errorf("%s accepted basic auth: %d %s", path, resp.StatusCode, body)
		}
	}
}

func TestDashboardActions(t *testing.T) {
	src := &fakeServer{}
	d := &Dashboard{Source: src, Log: slog.New(slog.DiscardHandler)}
	ts := httptest.NewServer(d.Handler())
	defer ts.Close()
	post := func(user string, header bool) (int, string) {
		req, _ := http.NewRequest("POST", ts.URL+"/api/hosts/web1/actions", strings.NewReader(`{"manifest":"site","kind":"rollback","resource":"file[/etc/motd]","host":"other","requested_by":"forged"}`))
		if user != "" {
			if c := signIn(t, ts.URL, user, "s3cret"); c != nil {
				req.AddCookie(c)
			}
		}
		if header {
			req.Header.Set("X-Fleetreeve-Console", "1")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// Read-only unless it has BOTH the operator token and a login.
	if code, body := post("", true); code != 403 || !strings.Contains(body, "read-only") {
		t.Errorf("no token, no login: %d %s", code, body)
	}
	src.operator = true
	if code, _ := post("", true); code != 403 {
		t.Errorf("operator token but no login: %d", code)
	}
	d.Users, _ = NewAccounts("", "ops:s3cret")
	if code, _ := post("", true); code != 401 {
		t.Errorf("not logged in: %d", code)
	}
	// A request another site makes with the browser's login lacks the header.
	if code, body := post("ops", false); code != 403 || !strings.Contains(body, "did not come from the console") {
		t.Errorf("forged cross-site request: %d %s", code, body)
	}
	if code, _ := post("ops", true); code != 202 || len(src.sent) != 1 {
		t.Fatalf("queue: %d %v", code, src.sent)
	}
	// The host comes from the URL and the requester from the login, not
	// from what the browser sent.
	if sent := src.sent[0]; !strings.HasPrefix(sent, "POST /v1/hosts/web1/actions ") || !strings.Contains(sent, `"host":"web1"`) || !strings.Contains(sent, `"requested_by":"ops (console)"`) || strings.Contains(sent, "forged") {
		t.Errorf("passed on: %s", sent)
	}
	d.poll(context.Background())
	if _, body := get(t, ts.URL+"/api/overview", "ops", "s3cret"); !strings.Contains(body, `"can_act":true`) {
		t.Error("overview does not offer rollback")
	}
}

func TestHostStateAndDemo(t *testing.T) {
	st := func(states ...string) string {
		h := pull.HostInfo{}
		for _, s := range states {
			h.Reports = append(h.Reports, pull.StatusEntry{State: s})
		}
		return HostState(h)
	}
	for want, states := range map[string][]string{"unknown": nil, "ok": {"ok"}, "failed": {"ok", "failed", "drift"}, "stale": {"changed", "stale"}, "drift": {"drift", "ok"}} {
		if got := st(states...); got != want {
			t.Errorf("%v: %s, want %s", states, got, want)
		}
	}
	// The demo fleet is served through the same paths as a real server.
	d := &Dashboard{Source: NewDemoSource(), Demo: true, Log: slog.New(slog.DiscardHandler)}
	d.poll(context.Background())
	if len(d.overview.Hosts) < 50 || d.overview.Error != "" || !d.overview.Demo {
		t.Errorf("demo: %d hosts, error %q", len(d.overview.Hosts), d.overview.Error)
	}
	// The log is passed on page by page, with its filters.
	ts := httptest.NewServer(d.Handler())
	defer ts.Close()
	page := func(query string) pull.HistoryPage {
		t.Helper()
		code, body := get(t, ts.URL+"/api/history"+query, "", "")
		var p pull.HistoryPage
		if err := json.Unmarshal([]byte(body), &p); err != nil || code != 200 {
			t.Fatalf("history%s: %d %s", query, code, body)
		}
		return p
	}
	first := page("?limit=20")
	second := page("?limit=20&before=" + first.Next)
	if len(first.Events) != 20 || len(second.Events) != 20 || first.Next == "" || second.Events[0].Time >= first.Events[19].Time {
		t.Errorf("paging: %d then %d events, next %q", len(first.Events), len(second.Events), first.Next)
	}
	for _, e := range page("?only=changes&host=web-ams-01&limit=30").Events {
		if e.Host != "web-ams-01" || !e.Eventful() {
			t.Errorf("filter let through: %+v", e)
		}
	}
}

// The page's script must at least parse: a syntax error there blanks the
// whole console, and nothing else in the build would notice.
func TestScriptParses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	if out, err := exec.Command(node, "--check", "static/app.js").CombinedOutput(); err != nil {
		t.Errorf("static/app.js: %s", out)
	}
}
