// Package dashboard is a read-only web console for a fleetreeve server. It runs
// as its own process (and container), talks to the server's management API
// with the read token, and serves a single-page UI to browsers. The token
// stays on the dashboard's side: browsers only ever see the dashboard's own
// endpoints, which cannot change anything and do not expose host variables.
package dashboard

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nxtw/fleetreeve/internal/pull"
)

//go:embed static
var static embed.FS

// Source is where the dashboard gets its data: the fleetreeve server's API, or
// a generated fleet for demonstrations.
type Source interface {
	Get(ctx context.Context, path string) ([]byte, error)
	// Send asks the server to queue or cancel a request for a node, or
	// to list requests. It fails when the dashboard was not given the
	// means to (CanAct).
	Send(ctx context.Context, method, path string, body any) ([]byte, error)
	CanAct() bool
	// SendExec queues a shell command for a node, which needs a token of
	// its own (CanExec).
	SendExec(ctx context.Context, path string, body any) ([]byte, error)
	CanExec() bool
}

type apiSource struct {
	client   *pull.Client
	token    string // read
	operator string // optional: runs and rollbacks
	exec     string // optional: shell commands
}

func (a apiSource) CanExec() bool { return a.exec != "" }

func (a apiSource) SendExec(ctx context.Context, path string, body any) ([]byte, error) {
	if a.exec == "" {
		return nil, fmt.Errorf("this console was not set up to run commands")
	}
	return a.client.Send(ctx, http.MethodPost, path, a.exec, body)
}

func (a apiSource) Get(ctx context.Context, path string) ([]byte, error) {
	return a.client.Get(ctx, path, a.token)
}

func (a apiSource) CanAct() bool { return a.operator != "" }

func (a apiSource) Send(ctx context.Context, method, path string, body any) ([]byte, error) {
	if a.operator == "" {
		return nil, fmt.Errorf("this console is read-only")
	}
	return a.client.Send(ctx, method, path, a.operator, body)
}

// NewAPISource reads from a fleetreeve server with its read token. With the
// operator token as well, the console can ask nodes to roll resources
// back; without it, it stays strictly read-only.
func NewAPISource(client *pull.Client, readToken, operatorToken, execToken string) Source {
	return apiSource{client, readToken, operatorToken, execToken}
}

// Sample is one point of the fleet's history as seen by this dashboard.
type Sample struct {
	Time    int64 `json:"t"` // unix seconds
	Hosts   int   `json:"hosts"`
	Healthy int   `json:"healthy"`
	Drift   int   `json:"drift"`
	Failed  int   `json:"failed"`
	Stale   int   `json:"stale"`
	Unknown int   `json:"unknown"`
}

// Overview is everything the front page needs, in one response.
type Overview struct {
	Server    string               `json:"server"`
	Version   string               `json:"version"`
	Demo      bool                 `json:"demo"`
	Fetched   string               `json:"fetched"`
	Error     string               `json:"error,omitempty"` // the last poll failed; data may be old
	Status    *pull.StatusResponse `json:"status"`
	Hosts     []pull.HostInfo      `json:"hosts"`
	History   []Sample             `json:"history"`
	PollEvery int                  `json:"poll_seconds"`
	// CanAct tells the page whether to offer runs and rollbacks at all;
	// CanExec, shell commands.
	CanAct  bool `json:"can_act"`
	CanExec bool `json:"can_exec"`
}

// Dashboard polls the server and serves the UI.
type Dashboard struct {
	Source Source
	Server string        // shown in the UI
	Demo   bool          // the data is generated
	Every  time.Duration // poll interval
	Users  *Accounts     // who may sign in; nil leaves the console open (and read-only)
	Log    *slog.Logger

	mu       sync.RWMutex
	overview Overview
}

const maxSamples = 24 * 60 * 4 // a day at the default 15s interval

// Run polls until ctx ends.
func (d *Dashboard) Run(ctx context.Context) {
	if d.Every <= 0 {
		d.Every = 15 * time.Second
	}
	for {
		d.poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(d.Every):
		}
	}
}

func (d *Dashboard) poll(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var (
		health struct{ Version string }
		status pull.StatusResponse
		hosts  struct{ Hosts []pull.HostInfo }
	)
	err := d.fetch(ctx, "/v1/health", &health)
	if err == nil {
		err = d.fetch(ctx, "/v1/status", &status)
	}
	if err == nil {
		err = d.fetch(ctx, "/v1/hosts", &hosts)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.overview.Server, d.overview.Demo, d.overview.PollEvery = d.Server, d.Demo, int(d.Every.Seconds())
	if err != nil {
		// Keep showing the last good data, flagged as possibly old.
		d.overview.Error = err.Error()
		d.Log.Warn("polling the fleetreeve server failed", "error", err.Error())
		return
	}
	d.overview.Error = ""
	d.overview.Version = health.Version
	d.overview.Fetched = time.Now().UTC().Format(time.RFC3339)
	// Never pass host variables on, whatever the server sends: they can
	// hold secrets, and browsers have no business with them.
	for i := range hosts.Hosts {
		hosts.Hosts[i].Vars = nil
	}
	d.overview.Status, d.overview.Hosts = &status, hosts.Hosts
	d.overview.History = append(d.overview.History, sample(hosts.Hosts))
	if n := len(d.overview.History); n > maxSamples {
		d.overview.History = d.overview.History[n-maxSamples:]
	}
}

func (d *Dashboard) fetch(ctx context.Context, path string, into any) error {
	b, err := d.Source.Get(ctx, path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

// HostState is the worst state among a host's reports; "unknown" when it
// never reported.
func HostState(h pull.HostInfo) string {
	rank := map[string]int{"ok": 1, "changed": 2, "drift": 3, "stale": 4, "failed": 5, "error": 5}
	state := "unknown"
	for _, r := range h.Reports {
		if rank[r.State] > rank[state] {
			state = r.State
		}
	}
	return state
}

func sample(hosts []pull.HostInfo) Sample {
	s := Sample{Time: time.Now().Unix(), Hosts: len(hosts)}
	for _, h := range hosts {
		switch HostState(h) {
		case "ok", "changed":
			s.Healthy++
		case "drift":
			s.Drift++
		case "failed", "error":
			s.Failed++
		case "stale":
			s.Stale++
		default:
			s.Unknown++
		}
	}
	return s
}

// Handler serves the UI and the dashboard's own read-only endpoints.
func (d *Dashboard) Handler() http.Handler {
	mux := http.NewServeMux()
	files, _ := fs.Sub(static, "static")
	mux.Handle("GET /", http.FileServer(http.FS(files)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /api/overview", func(w http.ResponseWriter, r *http.Request) {
		d.mu.RLock()
		o := d.overview
		d.mu.RUnlock()
		o.CanAct, o.CanExec = d.canAct(r), d.canExec(r) // depends on who is asking
		writeJSON(w, http.StatusOK, o)
	})
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := d.identify(r); ok || d.Users == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		http.ServeFileFS(w, r, files, "login.html")
	})
	mux.HandleFunc("POST /api/login", d.login)
	mux.HandleFunc("POST /api/logout", d.logout)
	mux.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
		u, _ := d.identify(r)
		writeJSON(w, http.StatusOK, map[string]any{"user": u.Name, "role": u.Role, "login": d.Users != nil, "can_act": d.canAct(r),
			"can_exec": d.canExec(r), "can_manage": u.Role == RoleAdmin, "min_password": MinPassword})
	})
	mux.HandleFunc("GET /api/users", d.listUsers)
	mux.HandleFunc("POST /api/users", d.addUser)
	mux.HandleFunc("PATCH /api/users/{name}", d.changeUser)
	mux.HandleFunc("DELETE /api/users/{name}", d.removeUser)
	mux.HandleFunc("POST /api/password", d.changePassword)
	mux.HandleFunc("GET /api/hosts/{name}", d.host)
	mux.HandleFunc("GET /api/history", d.history)
	mux.HandleFunc("POST /api/run", d.run)
	mux.HandleFunc("GET /api/actions", d.actions)
	mux.HandleFunc("POST /api/hosts/{name}/actions", d.queue)
	mux.HandleFunc("DELETE /api/hosts/{name}/actions/{id}", d.cancel)
	return d.secure(mux)
}

// canAct: a rollback may be asked for only when the dashboard holds the
// operator token AND the person asking is signed in with the operator
// role. Without accounts anyone who can reach the port could change
// hosts, so the token alone is never enough.
func (d *Dashboard) canAct(r *http.Request) bool {
	if !d.Source.CanAct() {
		return false
	}
	if d.Demo && d.Users == nil {
		return true // generated data; nothing real to change
	}
	u, ok := d.identify(r)
	return ok && MayAct(u.Role)
}

// canExec: shell commands on hosts are for admins, on a console that was
// deliberately given the exec token. They are root on the host.
func (d *Dashboard) canExec(r *http.Request) bool {
	if !d.Source.CanExec() {
		return false
	}
	if d.Demo && d.Users == nil {
		return true
	}
	u, ok := d.identify(r)
	return ok && u.Role == RoleAdmin
}

const sessionCookie = "fleetreeve_session"

// identify returns who is making a request, by its session cookie.
//
// A name and password sent along with the request (HTTP basic auth) are
// deliberately not accepted. A browser that was once given them keeps
// sending them with every request and offers no way to stop, so signing
// out would do nothing: the next request would sign straight back in.
// Scripts should use the fleetreeve server's API with a token instead.
func (d *Dashboard) identify(r *http.Request) (User, bool) {
	if d.Users == nil {
		return User{}, false
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		return d.Users.Session(c.Value)
	}
	return User{}, false
}

func (d *Dashboard) login(w http.ResponseWriter, r *http.Request) {
	if d.Users == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "this console has no accounts"})
		return
	}
	var req struct{ User, Password string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	u, ok, wait := d.Users.Verify(req.User, req.Password)
	switch {
	case wait > 0:
		d.Log.Warn("sign-in refused: too many attempts", "user", req.User, "remote", r.RemoteAddr)
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": fmt.Sprintf("Too many attempts for this name. Try again in %d minutes.", int(wait.Minutes())+1)})
		return
	case !ok:
		d.Log.Warn("sign-in failed", "user", req.User, "remote", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "That name and password do not match an account."})
		return
	}
	d.Log.Info("signed in", "user", u.Name, "role", u.Role, "remote", r.RemoteAddr)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: d.Users.Open(u), Path: "/", MaxAge: int(sessionLife.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
	writeJSON(w, http.StatusOK, map[string]string{"user": u.Name, "role": u.Role})
}

func (d *Dashboard) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && d.Users != nil {
		if u, ok := d.Users.Session(c.Value); ok {
			d.Log.Info("signed out", "user", u.Name, "remote", r.RemoteAddr)
		}
		d.Users.Close(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

// guard refuses requests that change something unless they clearly come
// from the console itself: browsers send the login along with requests
// made by other sites too, but cannot add this header to them.
func (d *Dashboard) guard(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case !d.Source.CanAct() || (d.Users == nil && !d.Demo):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this console is read-only: it was started without the operator token, or without accounts"})
	case !d.canAct(r):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "your account may look but not change: rolling back needs the operator role"})
	case r.Header.Get("X-Fleetreeve-Console") != "1":
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "request refused: it did not come from the console"})
	default:
		return true
	}
	return false
}

// queue asks the server to have a node roll a resource back (or release
// it) at its next check-in.
func (d *Dashboard) queue(w http.ResponseWriter, r *http.Request) {
	if !d.guard(w, r) {
		return
	}
	var a pull.Action
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&a); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request: " + err.Error()})
		return
	}
	if a.Kind != "rollback" && a.Kind != "release" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "kind must be rollback or release"})
		return
	}
	a.Host = r.PathValue("name")
	a.RequestedBy = "the console"
	if u, ok := d.identify(r); ok {
		a.RequestedBy = u.Name + " (console)"
	}
	b, err := d.Source.Send(r.Context(), http.MethodPost, "/v1/hosts/"+url.PathEscape(a.Host)+"/actions", a)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	d.Log.Info("rollback request passed on", "host", a.Host, "kind", a.Kind, "resource", a.Resource, "by", a.RequestedBy, "remote", r.RemoteAddr)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	w.Write(b)
}

// run asks hosts to apply their configuration now, to do a dry run, or -
// admins only - to run a shell command. Hosts pick the request up the
// next time they ask the server, usually within a minute.
func (d *Dashboard) run(w http.ResponseWriter, r *http.Request) {
	if !d.guard(w, r) {
		return
	}
	var req struct {
		Hosts   []string
		Kind    string
		Command string
		Timeout int
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<18)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request: " + err.Error()})
		return
	}
	who := "the console"
	if u, ok := d.identify(r); ok {
		who = u.Name + " (console)"
	}
	switch req.Kind {
	case "apply", "plan":
	case "command":
		if !d.canExec(r) {
			d.Log.Warn("command refused", "by", who, "remote", r.RemoteAddr)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "running commands needs the admin role, on a console set up for it"})
			return
		}
		if strings.TrimSpace(req.Command) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "enter a command"})
			return
		}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "kind must be apply, plan or command"})
		return
	}
	// Only hosts the server knows; the list comes from the last poll.
	d.mu.RLock()
	known := map[string]bool{}
	for _, h := range d.overview.Hosts {
		known[h.Name] = true
	}
	d.mu.RUnlock()
	if len(req.Hosts) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "choose at least one host"})
		return
	}
	queued, failed := []json.RawMessage{}, map[string]string{}
	for _, host := range req.Hosts {
		if !known[host] {
			failed[host] = "not a host the server knows"
			continue
		}
		a := pull.Action{Host: host, Kind: req.Kind, Command: req.Command, Timeout: req.Timeout, RequestedBy: who}
		path := "/v1/hosts/" + url.PathEscape(host) + "/actions"
		var b []byte
		var err error
		if req.Kind == "command" {
			b, err = d.Source.SendExec(r.Context(), path, a)
		} else {
			b, err = d.Source.Send(r.Context(), http.MethodPost, path, a)
		}
		if err != nil {
			failed[host] = err.Error()
			continue
		}
		queued = append(queued, b)
	}
	// Commands are logged in full: they are the most that can be done here.
	d.Log.Info("run requested", "kind", req.Kind, "command", req.Command, "hosts", req.Hosts, "queued", len(queued), "refused", len(failed), "by", who, "remote", r.RemoteAddr)
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": queued, "failed": failed})
}

// actions lists what is waiting for hosts and what they carried out
// lately. The text and output of commands are for admins only.
func (d *Dashboard) actions(w http.ResponseWriter, r *http.Request) {
	if !d.canAct(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this needs the operator role, on a console set up for it"})
		return
	}
	b, err := d.Source.Send(r.Context(), http.MethodGet, "/v1/actions", nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	var list struct {
		Pending []pull.Action       `json:"pending"`
		Results []pull.ActionResult `json:"results"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "unexpected answer from the server"})
		return
	}
	if !d.canExec(r) {
		for i := range list.Pending {
			if list.Pending[i].Kind == "command" {
				list.Pending[i].Command = ""
			}
		}
		for i := range list.Results {
			if list.Results[i].Kind == "command" {
				list.Results[i].Command, list.Results[i].Output = "", ""
			}
		}
	}
	if list.Pending == nil {
		list.Pending = []pull.Action{}
	}
	if list.Results == nil {
		list.Results = []pull.ActionResult{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (d *Dashboard) cancel(w http.ResponseWriter, r *http.Request) {
	if !d.guard(w, r) {
		return
	}
	path := "/v1/hosts/" + url.PathEscape(r.PathValue("name")) + "/actions/" + url.PathEscape(r.PathValue("id"))
	if _, err := d.Source.Send(r.Context(), http.MethodDelete, path, nil); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// host returns one host with its full reports. The host's variables are
// deliberately left out: they can hold secrets, and this console is meant
// to be shown to people who should not see them.
func (d *Dashboard) host(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var info pull.HostInfo
	if err := d.fetch(r.Context(), "/v1/hosts/"+url.PathEscape(name), &info); err != nil {
		code := http.StatusBadGateway
		if strings.Contains(err.Error(), "no such host") {
			code = http.StatusNotFound
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	info.Vars = nil
	reports := []pull.Report{}
	for _, e := range info.Reports {
		m := e.Manifest
		if m == "" {
			m = "-"
		}
		var rep pull.Report
		if err := d.fetch(r.Context(), "/v1/hosts/"+url.PathEscape(name)+"/reports/"+url.PathEscape(m), &rep); err == nil {
			reports = append(reports, rep)
		}
	}
	sort.Slice(reports, func(a, b int) bool { return reports[a].Manifest < reports[b].Manifest })
	writeJSON(w, http.StatusOK, map[string]any{"host": info, "state": HostState(info), "reports": reports, "can_act": d.canAct(r)})
}

// history passes a page of the server's log on: every run and request,
// newest first. Anyone signed in may read it; it holds no command text,
// command output or file contents.
func (d *Dashboard) history(w http.ResponseWriter, r *http.Request) {
	q := url.Values{}
	for _, k := range []string{"host", "only", "before", "limit"} {
		if v := r.URL.Query().Get(k); v != "" {
			q.Set(k, v)
		}
	}
	var page pull.HistoryPage
	if err := d.fetch(r.Context(), "/v1/history?"+q.Encode(), &page); err != nil {
		code, msg := http.StatusBadGateway, err.Error()
		if strings.Contains(msg, "keeps no history") {
			code = http.StatusNotFound
		}
		writeJSON(w, code, map[string]string{"error": msg})
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// secure adds basic auth (when configured) and conservative headers.
func (d *Dashboard) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'")
		// With accounts, everything but the sign-in page (and what it
		// needs) requires being signed in.
		public := map[string]bool{"/healthz": true, "/login": true, "/login.js": true, "/app.css": true, "/api/login": true}
		if d.Users != nil && !public[r.URL.Path] {
			if _, ok := d.identify(r); !ok {
				if strings.HasPrefix(r.URL.Path, "/api/") {
					writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in first"})
				} else {
					http.Redirect(w, r, "/login", http.StatusSeeOther)
				}
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
