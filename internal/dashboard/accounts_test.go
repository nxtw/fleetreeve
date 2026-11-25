package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestAccountManagement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.yaml")
	SaveUsers(path, map[string]User{
		"root1": {Role: RoleAdmin, Hash: HashPassword("the admin's password")},
		"olive": {Role: RoleOperator, Hash: HashPassword("olive's long password")},
	})
	accounts, _ := NewAccounts(path, "shared:shared-password")
	d := &Dashboard{Source: &fakeServer{operator: true}, Users: accounts, Log: slog.New(slog.DiscardHandler)}
	ts := httptest.NewServer(d.Handler())
	defer ts.Close()
	d.poll(t.Context()) // so it knows which hosts exist

	session := func(user, pw string) *http.Client {
		jar, _ := cookiejar.New(nil)
		c := &http.Client{Jar: jar}
		b, _ := json.Marshal(map[string]string{"user": user, "password": pw})
		resp, err := c.Post(ts.URL+"/api/login", "application/json", strings.NewReader(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil
		}
		return c
	}
	do := func(c *http.Client, method, path, body string, header bool) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if header {
			req.Header.Set("X-Fleetreeve-Console", "1")
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var out map[string]any
		json.Unmarshal(b, &out)
		return resp.StatusCode, out
	}
	admin, olive := session("root1", "the admin's password"), session("olive", "olive's long password")

	// Only admins manage accounts - not operators, and not other sites
	// using an admin's browser.
	if code, _ := do(olive, "GET", "/api/users", "", true); code != 403 {
		t.Errorf("an operator listed accounts: %d", code)
	}
	if code, _ := do(olive, "POST", "/api/users", `{"name":"mallory","role":"admin"}`, true); code != 403 {
		t.Errorf("an operator added an account: %d", code)
	}
	if code, _ := do(admin, "POST", "/api/users", `{"name":"mallory","role":"admin"}`, false); code != 403 {
		t.Errorf("a request without the console header was accepted: %d", code)
	}

	// Listing: file accounts, plus the fixed one from the environment.
	code, out := do(admin, "GET", "/api/users", "", true)
	if list, _ := out["users"].([]any); code != 200 || len(list) != 3 || out["you"] != "root1" || list[2].(map[string]any)["fixed"] != true {
		t.Fatalf("list: %d %v", code, out)
	}

	// Adding: the generated password is returned once and works; nothing
	// in the file is a password.
	code, out = do(admin, "POST", "/api/users", `{"name":"Vera","role":"viewer"}`, true)
	pw, _ := out["password"].(string)
	if code != 201 || len(pw) < 20 || session("vera", pw) == nil {
		t.Fatalf("add: %d %v", code, out)
	}
	for body, want := range map[string]string{
		`{"name":"vera","role":"viewer"}`:     "already",
		`{"name":"shared","role":"viewer"}`:   "already",
		`{"name":"Bad Name","role":"viewer"}`: "cannot be used",
		`{"name":"zed","role":"root"}`:        "role must be",
	} {
		if code, out = do(admin, "POST", "/api/users", body, true); code != 400 || !strings.Contains(out["error"].(string), want) {
			t.Errorf("%s: %d %v", body, code, out)
		}
	}

	// A role change takes effect at once and ends the account's sessions.
	vera := session("vera", pw)
	if code, _ = do(admin, "PATCH", "/api/users/vera", `{"role":"operator"}`, true); code != 200 {
		t.Errorf("role change: %d", code)
	}
	if code, _ = do(vera, "GET", "/api/me", "", true); code != 401 {
		t.Errorf("session survived a role change: %d", code)
	}
	if _, out = do(session("vera", pw), "GET", "/api/me", "", true); out["role"] != "operator" || out["can_act"] != true {
		t.Errorf("after promotion: %v", out)
	}

	// Reset: the old password stops working, the new one works.
	_, out = do(admin, "PATCH", "/api/users/vera", `{"reset":true}`, true)
	pw2, _ := out["password"].(string)
	if pw2 == "" || pw2 == pw || session("vera", pw) != nil || session("vera", pw2) == nil {
		t.Errorf("reset: %v", out)
	}

	// Changing one's own password: needs the current one, a long enough
	// new one; this session stays, the others end.
	vera, other := session("vera", pw2), session("vera", pw2)
	for body, want := range map[string]string{
		`{"current":"wrong","new":"a brand new password"}`: "not right",
		`{"current":"` + pw2 + `","new":"short"}`:          "at least",
	} {
		if code, out = do(vera, "POST", "/api/password", body, true); code != 400 || !strings.Contains(out["error"].(string), want) {
			t.Errorf("%s: %d %v", want, code, out)
		}
	}
	if code, _ = do(vera, "POST", "/api/password", `{"current":"`+pw2+`","new":"a brand new password"}`, true); code != 204 {
		t.Fatalf("password change: %d", code)
	}
	if code, _ = do(vera, "GET", "/api/me", "", true); code != 200 {
		t.Errorf("the session that changed the password was ended: %d", code)
	}
	if code, _ = do(other, "GET", "/api/me", "", true); code != 401 {
		t.Errorf("another session survived a password change: %d", code)
	}
	if session("vera", pw2) != nil || session("vera", "a brand new password") == nil {
		t.Error("the changed password does not take effect")
	}

	// Running: operators may ask for runs; commands are for admins, and
	// only on a console that was given the exec token.
	src := d.Source.(*fakeServer)
	if code, _ = do(olive, "POST", "/api/run", `{"hosts":["web1"],"kind":"apply"}`, true); code != 202 {
		t.Errorf("an operator asks for a run: %d", code)
	}
	if code, out = do(olive, "POST", "/api/run", `{"hosts":["web1"],"kind":"command","command":"id"}`, true); code != 403 {
		t.Errorf("an operator ran a command: %d %v", code, out)
	}
	if code, _ = do(admin, "POST", "/api/run", `{"hosts":["web1"],"kind":"command","command":"id"}`, true); code != 403 {
		t.Errorf("a command on a console without the exec token: %d", code)
	}
	src.exec = true
	sent := len(src.sent)
	code, out = do(admin, "POST", "/api/run", `{"hosts":["web1","ghost"],"kind":"command","command":"id"}`, true)
	if code != 202 || len(out["queued"].([]any)) != 1 || out["failed"].(map[string]any)["ghost"] == nil || len(src.sent) != sent+1 ||
		!strings.HasPrefix(src.sent[sent], "EXEC /v1/hosts/web1/actions") || !strings.Contains(src.sent[sent], `"requested_by":"root1 (console)"`) {
		t.Errorf("an admin's command: %d %v %v", code, out, src.sent[sent:])
	}
	if code, _ = do(admin, "POST", "/api/run", `{"hosts":["web1"],"kind":"command","command":"id"}`, false); code != 403 {
		t.Errorf("a command without the console header: %d", code)
	}
	// What commands said is for admins; operators see that they ran.
	_, out = do(olive, "GET", "/api/actions", "", true)
	if b, _ := json.Marshal(out); strings.Contains(string(b), "shadow") || strings.Contains(string(b), "secret") || !strings.Contains(string(b), "in sync") {
		t.Errorf("an operator sees command text or output: %s", b)
	}
	_, out = do(admin, "GET", "/api/actions", "", true)
	if b, _ := json.Marshal(out); !strings.Contains(string(b), "root:$6$secret") {
		t.Errorf("an admin does not see command output: %s", b)
	}
	if code, _ = do(vera, "GET", "/api/actions", "", true); code != 200 {
		t.Errorf("vera is an operator by now: %d", code)
	}

	// Guard rails: not yourself, not the last admin, not the fixed account.
	if code, out = do(admin, "DELETE", "/api/users/root1", "", true); code != 400 || !strings.Contains(out["error"].(string), "your own") {
		t.Errorf("removing yourself: %d %v", code, out)
	}
	if code, out = do(admin, "PATCH", "/api/users/root1", `{"role":"viewer"}`, true); code != 400 || !strings.Contains(out["error"].(string), "only admin") {
		t.Errorf("demoting the last admin: %d %v", code, out)
	}
	if code, _ = do(admin, "DELETE", "/api/users/shared", "", true); code != 400 {
		t.Errorf("removing the fixed account: %d", code)
	}
	shared := session("shared", "shared-password")
	if code, out = do(shared, "POST", "/api/password", `{"current":"shared-password","new":"a brand new password"}`, true); code != 400 || !strings.Contains(out["error"].(string), "environment") {
		t.Errorf("changing the fixed account's password: %d %v", code, out)
	}

	// Removing ends the account and its sessions.
	if code, _ = do(admin, "DELETE", "/api/users/vera", "", true); code != 204 {
		t.Errorf("remove: %d", code)
	}
	if code, _ = do(vera, "GET", "/api/me", "", true); code != 401 || session("vera", "a brand new password") != nil {
		t.Errorf("a removed account still works: %d", code)
	}
	// With a second admin, the first may step down.
	do(admin, "PATCH", "/api/users/olive", `{"role":"admin"}`, true)
	if code, _ = do(admin, "PATCH", "/api/users/root1", `{"role":"viewer"}`, true); code != 200 {
		t.Errorf("stepping down with another admin present: %d", code)
	}
	users, _ := LoadUsers(path)
	if len(users) != 2 || users["root1"].Role != RoleViewer || users["olive"].Role != RoleAdmin {
		t.Errorf("file at the end: %+v", users)
	}
}
