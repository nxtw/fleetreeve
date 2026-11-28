package pull

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A token Vault refuses is gone; so is one whose time is up but which
// Vault, just started, has not cleared away yet.
func TestTokenAlive(t *testing.T) {
	answer := func(w http.ResponseWriter, r *http.Request) {}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/token/lookup-self" {
			http.NotFound(w, r)
			return
		}
		answer(w, r)
	}))
	defer srv.Close()
	v, err := NewVault(srv.URL, "s.token", "pki", "fleetreeve", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(expire string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"data":{"ttl":0,"renewable":true,"expire_time":%s}}`, expire)
		}
	}
	stamp := func(d time.Duration) string {
		return `"` + time.Now().Add(d).UTC().Format(time.RFC3339Nano) + `"`
	}
	for name, c := range map[string]struct {
		answer func(http.ResponseWriter, *http.Request)
		alive  bool
		fails  bool
	}{
		"refused": {answer: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(403)
			io.WriteString(w, `{"errors":["permission denied","invalid token"]}`)
		}},
		"time is up, not cleared away yet": {answer: lookup(stamp(-36 * time.Hour))},
		"good for a while":                 {answer: lookup(stamp(20 * time.Hour)), alive: true},
		"never expires (a root token)":     {answer: lookup("null"), alive: true},
		"Vault is sealed": {answer: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(503)
			io.WriteString(w, `{"errors":["Vault is sealed"]}`)
		}, fails: true},
	} {
		answer = c.answer
		alive, err := v.TokenAlive(context.Background())
		if (err != nil) != c.fails || alive != c.alive {
			t.Errorf("%s: alive %v, error %v", name, alive, err)
		}
	}
}

func TestServerToken(t *testing.T) {
	var asked map[string]any
	orphanAllowed := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "s.admin" {
			w.WriteHeader(403)
			return
		}
		switch {
		case r.URL.Path == "/v1/auth/token/create-orphan" && !orphanAllowed:
			w.WriteHeader(403)
			io.WriteString(w, `{"errors":["permission denied"]}`)
		case strings.HasPrefix(r.URL.Path, "/v1/auth/token/create"):
			json.NewDecoder(r.Body).Decode(&asked)
			io.WriteString(w, `{"auth":{"client_token":"s.new"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	v, _ := NewVault(srv.URL, "s.admin", "pki", "fleetreeve", "", "", "")
	token, orphan, err := v.ServerToken(context.Background(), "")
	if err != nil || token != "s.new" || !orphan {
		t.Fatalf("token %q, orphan %v, %v", token, orphan, err)
	}
	// Long enough for a machine that is off over a holiday, and only the
	// policy Setup wrote - never more than the server needs.
	if asked["period"] != "720h" || fmt.Sprint(asked["policies"]) != "[fleetreeve-pki-fleetreeve]" {
		t.Errorf("asked Vault for %v", asked)
	}
	orphanAllowed = false
	if token, orphan, err = v.ServerToken(context.Background(), "48h"); err != nil || token != "s.new" || orphan || asked["period"] != "48h" {
		t.Errorf("as a child token: %q, orphan %v, %v, asked %v", token, orphan, err, asked)
	}
}
