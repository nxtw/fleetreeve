package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPasswordsAndUsersFile(t *testing.T) {
	h := HashPassword("correct horse battery")
	if !CheckPassword(h, "correct horse battery") || CheckPassword(h, "correct horse batterx") || CheckPassword("", "x") || CheckPassword("plain:text", "plain:text") {
		t.Error("password check")
	}
	if h == HashPassword("correct horse battery") {
		t.Error("two hashes of one password are equal: no salt")
	}
	if strings.Contains(h, "correct") {
		t.Error("the hash contains the password")
	}
	if a, b := GeneratePassword(), GeneratePassword(); a == b || len(a) != 23 {
		t.Errorf("generated passwords: %q %q", a, b)
	}

	path := filepath.Join(t.TempDir(), "users.yaml")
	users, err := LoadUsers(path) // a missing file is an empty one
	if err != nil || len(users) != 0 {
		t.Fatalf("missing file: %v %v", users, err)
	}
	users["ann"] = User{Role: RoleOperator, Hash: HashPassword("ann's long password")}
	users["bob"] = User{Role: RoleViewer, Hash: HashPassword("bob's long password")}
	if err := SaveUsers(path, users); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("users file mode: %v", st.Mode())
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "long password") {
		t.Error("users file holds a password in the clear")
	}
	back, err := LoadUsers(path)
	if err != nil || back["ann"].Role != RoleOperator || back["bob"].Name != "bob" || !CheckPassword(back["bob"].Hash, "bob's long password") {
		t.Errorf("round trip: %+v %v", back, err)
	}
	os.WriteFile(path, []byte("users:\n  eve: {role: root, password: x}\n"), 0o600)
	if _, err := LoadUsers(path); err == nil {
		t.Error("an unknown role was accepted")
	}
}

func TestSignInRolesAndLockout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.yaml")
	SaveUsers(path, map[string]User{
		"ann": {Role: RoleOperator, Hash: HashPassword("ann's long password")},
		"bob": {Role: RoleViewer, Hash: HashPassword("bob's long password")},
	})
	accounts, err := NewAccounts(path, "")
	if err != nil {
		t.Fatal(err)
	}
	src := &fakeServer{operator: true}
	d := &Dashboard{Source: src, Users: accounts, Log: slog.New(slog.DiscardHandler)}
	ts := httptest.NewServer(d.Handler())
	defer ts.Close()

	browser := func() *http.Client {
		jar, _ := cookiejar.New(nil)
		return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	do := func(c *http.Client, method, path, body string) (int, string, http.Header) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.Header.Set("X-Fleetreeve-Console", "1")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}
	signIn := func(c *http.Client, user, pw string) int {
		b, _ := json.Marshal(map[string]string{"user": user, "password": pw})
		code, _, _ := do(c, "POST", "/api/login", string(b))
		return code
	}
	action := `{"manifest":"site","kind":"rollback","resource":"file[/etc/motd]"}`

	// Not signed in: pages go to the sign-in page, the API says so, and
	// the sign-in page with what it needs is reachable.
	anon := browser()
	if code, _, h := do(anon, "GET", "/", ""); code != 303 || h.Get("Location") != "/login" {
		t.Errorf("page when signed out: %d %s", code, h.Get("Location"))
	}
	if code, _, _ := do(anon, "GET", "/api/overview", ""); code != 401 {
		t.Errorf("api when signed out: %d", code)
	}
	for _, p := range []string{"/login", "/login.js", "/app.css", "/healthz"} {
		if code, _, _ := do(anon, "GET", p, ""); code != 200 {
			t.Errorf("%s when signed out: %d", p, code)
		}
	}
	if code, _, _ := do(anon, "GET", "/app.js", ""); code != 303 {
		t.Errorf("the console's script is served to anyone: %d", code)
	}

	// A wrong password and an unknown name answer alike.
	if signIn(anon, "ann", "wrong") != 401 || signIn(anon, "nobody", "ann's long password") != 401 {
		t.Error("bad sign-ins were not refused alike")
	}

	// The operator signs in, sees that she may act, and does; the request
	// is recorded under her name.
	ann := browser()
	if signIn(ann, "Ann", "ann's long password") != 200 { // names are not case-sensitive
		t.Fatal("ann cannot sign in")
	}
	if _, body, _ := do(ann, "GET", "/api/me", ""); !strings.Contains(body, `"user":"ann"`) || !strings.Contains(body, `"can_act":true`) {
		t.Errorf("me: %s", body)
	}
	if code, _, _ := do(ann, "POST", "/api/hosts/web1/actions", action); code != 202 || !strings.Contains(src.sent[len(src.sent)-1], `"requested_by":"ann (console)"`) {
		t.Errorf("operator action: %d %v", code, src.sent)
	}

	// The viewer signs in, sees everything, and may not act.
	bob := browser()
	signIn(bob, "bob", "bob's long password")
	if code, body, _ := do(bob, "GET", "/api/overview", ""); code != 200 || !strings.Contains(body, `"can_act":false`) {
		t.Errorf("viewer overview: %d %s", code, body)
	}
	sent := len(src.sent)
	if code, body, _ := do(bob, "POST", "/api/hosts/web1/actions", action); code != 403 || !strings.Contains(body, "operator role") || len(src.sent) != sent {
		t.Errorf("viewer action: %d %s", code, body)
	}

	// The session cookie is not readable by scripts and not sent cross-site.
	jar := bob.Jar.Cookies(mustURL(ts.URL))
	_, _, h := do(browser(), "POST", "/api/login", `{"user":"bob","password":"bob's long password"}`)
	if c := h.Get("Set-Cookie"); len(jar) != 1 || !strings.Contains(c, "HttpOnly") || !strings.Contains(c, "SameSite=Strict") {
		t.Errorf("session cookie: %s", c)
	}

	// Signing out ends the session.
	do(bob, "POST", "/api/logout", "")
	if code, _, _ := do(bob, "GET", "/api/overview", ""); code != 401 {
		t.Errorf("after signing out: %d", code)
	}
	if code, body, _ := do(bob, "GET", "/login", ""); code != 200 || !strings.Contains(body, "Sign in") {
		t.Errorf("after signing out the sign-in page is not shown: %d", code)
	}

	// Removing an account ends its sessions; no restart is needed.
	time.Sleep(15 * time.Millisecond)
	SaveUsers(path, map[string]User{"bob": {Role: RoleViewer, Hash: HashPassword("bob's long password")}})
	if code, _, _ := do(ann, "GET", "/api/overview", ""); code != 401 {
		t.Errorf("a removed account still has a session: %d", code)
	}

	// Too many wrong passwords lock the name for a while - even for the
	// right password.
	for i := 0; i < maxFailures; i++ {
		signIn(anon, "bob", "guess")
	}
	if code := signIn(anon, "bob", "bob's long password"); code != 429 {
		t.Errorf("after %d wrong attempts: %d", maxFailures, code)
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}
