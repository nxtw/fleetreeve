package pull

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/nxtw/fleetreeve/internal/compile"
	"github.com/nxtw/fleetreeve/internal/engine"
)

// The management API: read the state of the fleet and manage hosts and
// tokens over HTTP, for scripts, dashboards and monitoring. It sits next to
// the node endpoints (/v1/catalog, /v1/report, /v1/join) on the same port.
//
// Two bearer tokens, both derived from the server secret:
//
//	read   may call every GET endpoint
//	admin  may also register and remove hosts and mint tokens
//
// The admin token is as powerful as the secret itself: it can mint the
// token of any host. `fleetreeve token -api read|admin` prints them.
//
// Responses are JSON; errors are {"error": "..."} with a matching status.

// APIToken derives the bearer token of an API role ("read" or "admin").
// The name it is derived from starts with a NUL byte, which no host name
// can contain, so it can never collide with a node's token.
func APIToken(secret []byte, role string) string { return Token(secret, "\x00"+role) }

// StatusEntry is the latest run of one manifest on one host.
type StatusEntry struct {
	Host     string `json:"host"`
	Manifest string `json:"manifest,omitempty"`
	Source   string `json:"source,omitempty"` // "pull" or "push"
	Mode     string `json:"mode"`             // "apply" or "plan"
	// State is the one-word verdict: ok, changed, drift (a plan found
	// differences), failed, error, or stale (the node missed two check-ins).
	State      string         `json:"state"`
	Time       string         `json:"time"`
	AgeSeconds int            `json:"age_seconds"`
	Interval   int            `json:"interval_seconds,omitempty"`
	Poll       int            `json:"poll_seconds,omitempty"` // how soon the host picks up requests
	Cached     bool           `json:"cached_catalog,omitempty"`
	Error      string         `json:"error,omitempty"`
	Summary    engine.Summary `json:"summary"`
}

// Healthy reports whether the entry needs no attention.
func (e StatusEntry) Healthy() bool {
	return e.State != "failed" && e.State != "error" && e.State != "stale"
}

// StatusResponse is the state of the fleet as the server knows it.
type StatusResponse struct {
	Generated string        `json:"generated"`
	Healthy   bool          `json:"healthy"` // no entry is failed, in error or stale
	Reports   []StatusEntry `json:"reports"`
	Missing   []string      `json:"missing"` // known hosts that never reported
}

// HostInfo describes a host the server knows.
type HostInfo struct {
	Name       string         `json:"name"`
	Groups     []string       `json:"groups"`
	Source     string         `json:"source"` // "inventory" or "registered"
	Registered string         `json:"registered,omitempty"`
	Vars       map[string]any `json:"vars,omitempty"` // only in GET /v1/hosts/{name}
	Reports    []StatusEntry  `json:"reports"`
	Pending    []Action       `json:"pending_actions,omitempty"` // requests waiting for its next check-in
	// Recent is what the host carried out lately; only in GET
	// /v1/hosts/{name} for the operator, exec and admin tokens, since it
	// includes command output.
	Recent []ActionResult `json:"recent_actions,omitempty"`
}

// Entry condenses a report into a status entry.
func (r *Report) Entry(now time.Time) StatusEntry {
	e := StatusEntry{Host: r.Host, Manifest: r.Manifest, Source: r.Source, Mode: r.Mode, Time: r.Time,
		Interval: r.Interval, Poll: r.Poll, Cached: r.Cached, Error: r.Error, Summary: r.Summary, State: "ok"}
	if t, err := time.Parse(time.RFC3339, r.Time); err == nil {
		e.AgeSeconds = int(now.Sub(t).Seconds())
	}
	switch {
	case r.Stale(now):
		e.State = "stale"
	case r.Error != "":
		e.State = "error"
	case r.Summary.Failed > 0:
		e.State = "failed"
	case r.Summary.Drift() && r.Mode == "plan":
		e.State = "drift"
	case r.Summary.Drift():
		e.State = "changed"
	}
	return e
}

// Status builds the fleet status from stored reports. expected lists the
// hosts that should be reporting; those without any report are "missing".
func Status(reports []Report, expected []string, now time.Time) *StatusResponse {
	res := &StatusResponse{Generated: now.UTC().Format(time.RFC3339), Healthy: true, Reports: []StatusEntry{}, Missing: []string{}}
	seen := map[string]bool{}
	for i := range reports {
		e := reports[i].Entry(now)
		seen[e.Host] = true
		res.Healthy = res.Healthy && e.Healthy()
		res.Reports = append(res.Reports, e)
	}
	sort.Slice(res.Reports, func(a, b int) bool {
		if res.Reports[a].Host != res.Reports[b].Host {
			return res.Reports[a].Host < res.Reports[b].Host
		}
		return res.Reports[a].Manifest < res.Reports[b].Manifest
	})
	for _, h := range expected {
		if !seen[h] {
			res.Missing = append(res.Missing, h)
		}
	}
	return res
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// apiError answers with {"error": msg}. Every endpoint uses it, so
// clients can rely on one error format.
func apiError(w http.ResponseWriter, msg string, code int) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// allow checks the bearer token of a management request.
func (s *Server) allow(w http.ResponseWriter, r *http.Request, write bool) bool {
	token := []byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	ok := hmac.Equal(token, []byte(APIToken(s.Secret, "admin")))
	if !write {
		ok = ok || hmac.Equal(token, []byte(APIToken(s.Secret, "read")))
	}
	if !ok {
		need := "the read or admin API token"
		if write {
			need = "the admin API token"
		}
		s.Log.Warn("request refused", "remote", r.RemoteAddr, "path", r.URL.Path, "status", http.StatusUnauthorized, "reason", "invalid API token")
		apiError(w, "this request needs "+need+" (fleetreeve token -api read|admin)", http.StatusUnauthorized)
	}
	return ok
}

// fleet loads what the API reports on: the hosts the server knows (and
// where each comes from) and the stored reports.
func (s *Server) fleet() (inv *compile.Inventory, registered map[string]HostEntry, reports []Report, err error) {
	if inv, err = compile.LoadInventory(s.Inventory); err != nil {
		return nil, nil, nil, err
	}
	registered = map[string]HostEntry{}
	entries, err := LoadHosts(s.Hosts)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, e := range entries {
		if inv.AddHost(e.Name, e.Groups, e.Vars) {
			registered[e.Name] = e
		}
	}
	if s.Reports != "" {
		if reports, err = LoadReports(s.Reports); err != nil {
			return nil, nil, nil, err
		}
	}
	return inv, registered, reports, nil
}

func hostInfo(h *compile.Host, registered map[string]HostEntry, reports []Report, now time.Time) HostInfo {
	info := HostInfo{Name: h.Name, Groups: append([]string{}, h.Groups...), Source: "inventory", Reports: []StatusEntry{}}
	if e, ok := registered[h.Name]; ok {
		info.Source, info.Registered = "registered", e.Registered
	}
	for i := range reports {
		if reports[i].Host == h.Name {
			info.Reports = append(info.Reports, reports[i].Entry(now))
		}
	}
	sort.Slice(info.Reports, func(a, b int) bool { return info.Reports[a].Manifest < info.Reports[b].Manifest })
	return info
}

// hostInfo is a host as the API shows it. What waits for the host is
// shown to everyone who may look; the text of a shell command that waits
// only to tokens that may ask for such things themselves.
func (s *Server) hostInfo(r *http.Request, h *compile.Host, registered map[string]HostEntry, reports []Report, now time.Time) HostInfo {
	info := hostInfo(h, registered, reports, now)
	info.Pending = s.pending(h.Name)
	if !s.holds(r, "operator", "exec") {
		for i := range info.Pending {
			info.Pending[i].Command = ""
		}
	}
	return info
}

func (s *Server) apiHealth(w http.ResponseWriter, _ *http.Request) {
	h := map[string]any{"status": "ok", "version": s.Version}
	if s.CA != nil {
		// Hosts read this to know they should get a certificate.
		h["certificates"], h["require_certificate"] = "vault", s.RequireCert
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, false) {
		return
	}
	inv, _, reports, err := s.fleet()
	if err != nil {
		apiError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	expected := make([]string, len(inv.Hosts))
	for i, h := range inv.Hosts {
		expected[i] = h.Name
	}
	writeJSON(w, http.StatusOK, Status(reports, expected, time.Now()))
}

func (s *Server) apiHosts(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, false) {
		return
	}
	inv, registered, reports, err := s.fleet()
	if err != nil {
		apiError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hosts := make([]HostInfo, len(inv.Hosts))
	for i, h := range inv.Hosts {
		hosts[i] = s.hostInfo(r, h, registered, reports, time.Now())
	}
	sort.Slice(hosts, func(a, b int) bool { return hosts[a].Name < hosts[b].Name })
	writeJSON(w, http.StatusOK, map[string]any{"hosts": hosts})
}

func (s *Server) apiHost(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, false) {
		return
	}
	inv, registered, reports, err := s.fleet()
	if err != nil {
		apiError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h, ok := inv.Host(r.PathValue("name"))
	if !ok {
		apiError(w, "no such host", http.StatusNotFound)
		return
	}
	info := s.hostInfo(r, h, registered, reports, time.Now())
	if s.holds(r, "operator", "exec") {
		info.Recent = s.results(h.Name)
	}
	// The variables as the host's catalog sees them: inventory, groups
	// and host merged. They may hold secrets, so they are for the admin
	// token only - not for whoever may merely look.
	if s.holds(r) {
		info.Vars = inv.HostVars(h)
	}
	writeJSON(w, http.StatusOK, info)
}

// apiReport returns a stored report in full, resource by resource.
func (s *Server) apiReport(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, false) {
		return
	}
	_, _, reports, err := s.fleet()
	if err != nil {
		apiError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	host, manifest := r.PathValue("name"), r.PathValue("manifest")
	for i := range reports {
		// "-" addresses the report of a client too old to name its manifest.
		if reports[i].Host == host && (reports[i].Manifest == manifest || (manifest == "-" && reports[i].Manifest == "")) {
			writeJSON(w, http.StatusOK, reports[i])
			return
		}
	}
	apiError(w, "no such report", http.StatusNotFound)
}

// apiToken hands out the node token of a known host.
func (s *Server) apiToken(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, true) {
		return
	}
	var req struct{ Host string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		apiError(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if _, _, code, msg := s.hostAllowed(req.Host); code != 0 {
		apiError(w, msg, http.StatusNotFound)
		return
	}
	s.Log.Info("node token issued through the API", "remote", r.RemoteAddr, "host", req.Host)
	writeJSON(w, http.StatusOK, map[string]string{"host": req.Host, "token": Token(s.Secret, req.Host)})
}

// apiJoinToken mints a join token for provisioning new nodes.
func (s *Server) apiJoinToken(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, true) {
		return
	}
	req := struct{ TTL string }{TTL: "24h"}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil && err.Error() != "EOF" {
		apiError(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	ttl, err := time.ParseDuration(req.TTL)
	if err != nil || ttl <= 0 {
		apiError(w, `ttl must be a positive duration such as "24h"`, http.StatusBadRequest)
		return
	}
	expires := time.Now().Add(ttl)
	s.Log.Info("join token issued through the API", "remote", r.RemoteAddr, "ttl", ttl.String())
	writeJSON(w, http.StatusOK, map[string]string{"token": JoinToken(s.Secret, expires), "expires": expires.UTC().Format(time.RFC3339)})
}

// ---- client side ----

// Get calls a management GET endpoint and returns the raw JSON.
func (c *Client) Get(ctx context.Context, path, apiToken string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, path, apiToken, nil)
}

// Send calls a management endpoint that changes something and returns the
// raw JSON answer.
func (c *Client) Send(ctx context.Context, method, path, apiToken string, body any) ([]byte, error) {
	return c.do(ctx, method, path, apiToken, body)
}

// Status fetches the fleet status with a read or admin API token.
func (c *Client) Status(ctx context.Context, apiToken string) (*StatusResponse, error) {
	b, err := c.do(ctx, http.MethodGet, "/v1/status", apiToken, nil)
	if err != nil {
		return nil, err
	}
	res := &StatusResponse{}
	return res, json.Unmarshal(b, res)
}
