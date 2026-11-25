// Package pull implements fleetreeve's optional pull mode: nodes fetch their own
// catalog from a small HTTP service instead of being pushed to over SSH.
//
// The division of labour is the same as in a push run. The node sends its
// facts; the server compiles that node's catalog from the manifest and
// inventory and returns it; the node applies it and posts a report. Nodes
// never see manifests or other hosts' data, and the server never connects
// to a node.
package pull

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nxtw/fleetreeve/internal/compile"
	"github.com/nxtw/fleetreeve/internal/core"
	"github.com/nxtw/fleetreeve/internal/engine"
	"github.com/nxtw/fleetreeve/internal/remote"
	"github.com/nxtw/fleetreeve/internal/state"
)

// Report is what a node tells the server about one run.
type Report struct {
	Host string `json:"host"`
	// Manifest is the manifest's state name, and Source how the run was
	// started: "pull" (the node's own service) or "push" (fleetreeve apply/plan
	// from a controller). The server keeps the latest report per host and
	// manifest, so the two kinds of run do not overwrite each other.
	Manifest string `json:"manifest,omitempty"`
	Source   string `json:"source,omitempty"`
	Mode     string `json:"mode"` // "apply" or "plan"
	Version  string `json:"version"`
	Time     string `json:"time"`
	Cached   bool   `json:"cached,omitempty"` // server was unreachable; a cached catalog was enforced
	// Interval is how often the node pulls, in seconds (0: a one-off run).
	// It lets `fleetreeve status` tell when a node has gone quiet.
	Interval int `json:"interval,omitempty"`
	// Poll is how often the node asks the server for requests, in seconds.
	Poll    int             `json:"poll,omitempty"`
	Error   string          `json:"error,omitempty"`
	Summary engine.Summary  `json:"summary"`
	Results []engine.Result `json:"results"`
	// Restorable lists the resources the node could roll back, and those
	// on hold; Actions, the requests it carried out before this run.
	Restorable []engine.RollbackResult `json:"restorable,omitempty"`
	Actions    []ActionResult          `json:"actions,omitempty"`
}

// Stale reports whether the node missed at least two check-ins.
func (r *Report) Stale(now time.Time) bool {
	t, err := time.Parse(time.RFC3339, r.Time)
	return err == nil && r.Interval > 0 && now.Sub(t) > time.Duration(2*r.Interval+60)*time.Second
}

// Failed reports whether the run did not fully converge.
func (r *Report) Failed() bool { return r.Error != "" || r.Summary.Failed > 0 }

type catalogRequest struct {
	Host  string         `json:"host"`
	Facts map[string]any `json:"facts"`
}

// ---- tokens ----

// Token derives a node's bearer token from the server secret. The server
// stores no per-node credentials: a host is authorised when it presents the
// token for its own name and is listed in the inventory.
func Token(secret []byte, host string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(host))
	return hex.EncodeToString(mac.Sum(nil))
}

// JoinToken mints a token that lets any host in the inventory obtain its
// own node token until it expires. It is meant for provisioning (kickstart,
// cloud-init, images) where a per-host token cannot be handed out in advance.
func JoinToken(secret []byte, expires time.Time) string {
	exp := strconv.FormatInt(expires.Unix(), 10)
	return "join." + exp + "." + Token(secret, "join:"+exp)
}

// CheckJoinToken verifies a join token's signature and expiry.
func CheckJoinToken(secret []byte, token string, now time.Time) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "join" || !hmac.Equal([]byte(parts[2]), []byte(Token(secret, "join:"+parts[1]))) {
		return errors.New("invalid join token")
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || now.Unix() > exp {
		return errors.New("join token has expired")
	}
	return nil
}

// InstallScript bootstraps a node: it downloads the fleetreeve build for the
// machine from the server and hands over to `fleetreeve install`.
const InstallScript = `#!/bin/sh
# fleetreeve bootstrap. Usage:
#   curl -fsSL https://SERVER:8140/install.sh | sudo sh -s -- -server https://SERVER:8140 -join TOKEN
# All arguments are passed on to "fleetreeve install" (see "fleetreeve help").
# FLEETREEVE_INSTALL_DIR (default /usr/local/bin) must allow executing files.
set -eu
server="" ca="" prev=""
for a in "$@"; do
    case "$prev" in
        -server|--server) server=$a ;;
        -ca|--ca) ca=$a ;;
    esac
    prev=$a
done
if [ -z "$server" ]; then
    echo "usage: install.sh -server <url> (-join <token> | -token-file <file>) [-ca <file>] [-host <name>] [-every 30m]" >&2
    exit 2
fi
server=${server%/}
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case $(uname -m) in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) echo "fleetreeve: unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac
fetch() {
    if [ -n "$ca" ]; then curl -fsSL --cacert "$ca" "$@"; else curl -fsSL "$@"; fi
}
dir=${FLEETREEVE_INSTALL_DIR:-/usr/local/bin}
mkdir -p "$dir"
tmp="$dir/.fleetreeve-bootstrap.$$"
trap 'rm -f "$tmp"' EXIT
fetch -o "$tmp" "$server/v1/agent/$os-$arch"
want=$(fetch "$server/v1/agent/$os-$arch.sha256")
if command -v sha256sum >/dev/null 2>&1; then
    have=$(sha256sum "$tmp" | cut -d' ' -f1)
else
    have=$(shasum -a 256 "$tmp" | cut -d' ' -f1)
fi
if [ "$want" != "$have" ]; then
    echo "fleetreeve: download is corrupt (checksum mismatch)" >&2
    exit 1
fi
chmod 755 "$tmp"
"$tmp" install "$@"
`

// LoadSecret reads the server secret. Only the server creates it (created
// reports that it just did); a secret invented anywhere else would produce
// tokens the server rejects.
func LoadSecret(path string, create bool) (secret []byte, created bool, err error) {
	b, created, err := loadSecret(path, create)
	if err == nil {
		if b = bytes.TrimSpace(b); len(b) < 32 {
			err = fmt.Errorf("%s: secret is too short (need at least 32 characters)", path)
		}
	}
	return b, created, err
}

func loadSecret(path string, create bool) ([]byte, bool, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) && !create {
		return nil, false, fmt.Errorf("%s does not exist; use the secret file of your `fleetreeve serve` (it creates one on first start)", path)
	}
	if os.IsNotExist(err) {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return nil, false, err
		}
		b = []byte(hex.EncodeToString(raw) + "\n")
		return b, true, os.WriteFile(path, b, 0o600)
	}
	return b, false, err
}

// ---- server ----

// Server compiles catalogs on request and collects reports.
type Server struct {
	Manifest  string
	Inventory string // re-read on every request, so edits take effect at once
	Extra     map[string]any
	Prune     bool
	Secret    []byte
	Reports   string // directory for the latest report of each host; "" disables
	// Hosts is a directory of hosts registered through the API (by `fleetreeve
	// enroll`), in addition to those in the inventory file; "" disables
	// registration. The inventory file is typically a read-only checkout,
	// so the server keeps these in its own data directory instead.
	Hosts string
	// Actions is a directory of requests waiting for nodes (rollbacks
	// asked for through the API); "" disables them.
	Actions string
	// History is a directory for the log of every run and request (see
	// history.go); "" keeps none. HistoryDays is how long it is kept
	// (0: for ever).
	History     string
	HistoryDays int
	// CA, when set, signs certificates for hosts (see cert.go); Certs is
	// where the server keeps track of them. RequireCert: a host's token
	// alone gets it nothing but its first certificate.
	CA          *Vault
	Certs       string
	RequireCert bool
	Version     string // reported by GET /v1/health
	Log         *slog.Logger
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/catalog", s.catalog)
	mux.HandleFunc("POST /v1/report", s.report)
	mux.HandleFunc("POST /v1/join", s.join)
	mux.HandleFunc("POST /v1/cert", s.issueCert)
	mux.HandleFunc("GET /v1/certs", s.listCerts)
	mux.HandleFunc("DELETE /v1/certs/{name}", s.dropCert)
	// Management API (see api.go).
	mux.HandleFunc("GET /v1/health", s.apiHealth)
	mux.HandleFunc("GET /v1/status", s.apiStatus)
	mux.HandleFunc("GET /v1/hosts", s.apiHosts)
	mux.HandleFunc("GET /v1/hosts/{name}", s.apiHost)
	mux.HandleFunc("GET /v1/hosts/{name}/reports/{manifest}", s.apiReport)
	mux.HandleFunc("POST /v1/tokens", s.apiToken)
	mux.HandleFunc("POST /v1/join-tokens", s.apiJoinToken)
	mux.HandleFunc("POST /v1/hosts/{name}/actions", s.queueAction)
	mux.HandleFunc("DELETE /v1/hosts/{name}/actions/{id}", s.cancelAction)
	mux.HandleFunc("POST /v1/actions/claim", s.claimActions)
	mux.HandleFunc("POST /v1/actions/results", s.storeResults)
	mux.HandleFunc("GET /v1/actions", s.listActions)
	mux.HandleFunc("GET /v1/history", s.apiHistory)
	mux.HandleFunc("PUT /v1/hosts/{name}", s.registerHost)
	mux.HandleFunc("DELETE /v1/hosts/{name}", s.unregisterHost)
	mux.HandleFunc("GET /v1/agent/{file}", s.agent)
	mux.HandleFunc("GET /install.sh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/x-shellscript")
		io.WriteString(w, InstallScript)
	})
	return mux
}

// hostAllowed loads the inventory and looks the host up in it.
func (s *Server) hostAllowed(host string) (*compile.Inventory, *compile.Host, int, string) {
	inv, err := compile.LoadInventory(s.Inventory)
	if err != nil {
		return nil, nil, http.StatusInternalServerError, "inventory: " + err.Error()
	}
	if err := MergeHosts(inv, s.Hosts); err != nil {
		return nil, nil, http.StatusInternalServerError, "registered hosts: " + err.Error()
	}
	h, ok := inv.Host(host)
	if !ok {
		return nil, nil, http.StatusForbidden, "host is not in the inventory"
	}
	return inv, h, 0, ""
}

// HostEntry is a host registered with the server at run time: what an
// inventory file would say about it, minus how to reach it.
type HostEntry struct {
	Name       string         `yaml:"name" json:"name"`
	Groups     []string       `yaml:"groups,omitempty" json:"groups,omitempty"`
	Vars       map[string]any `yaml:"vars,omitempty" json:"vars,omitempty"`
	Registered string         `yaml:"registered,omitempty" json:"registered,omitempty"`
	// NewCert: forget the host's certificate, so that it gets a new one
	// with its token. For a host that was rebuilt or lost its key.
	NewCert bool `json:"new_certificate,omitempty" yaml:"-"`
}

// HostResult is the server's answer to a registration.
type HostResult struct {
	// Status is "registered", or "inventory" when the host is already in
	// the server's inventory file (which then stays authoritative).
	Status string `json:"status"`
	// UnknownGroups are groups of the host that the server's inventory
	// does not define, so they bring the host no variables there.
	UnknownGroups []string `json:"unknown_groups,omitempty"`
}

// AdminToken is the API token allowed to change things; see APIToken.
func AdminToken(secret []byte) string { return APIToken(secret, "admin") }

func hostFile(dir, name string) (string, error) {
	if name == "" || filepath.Base(name) != name || strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("host name %q cannot be used as a file name", name)
	}
	return filepath.Join(dir, name+".yaml"), nil
}

// LoadHosts reads the hosts registered in dir ("" yields none).
func LoadHosts(dir string) ([]HostEntry, error) {
	if dir == "" {
		return nil, nil
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
	sort.Strings(files)
	var out []HostEntry
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var e HostEntry
		if err := yaml.Unmarshal(b, &e); err != nil {
			return nil, fmt.Errorf("%s: %v", f, err)
		}
		if e.Name != "" {
			out = append(out, e)
		}
	}
	return out, nil
}

// MergeHosts adds the hosts registered in dir to an inventory. Hosts the
// inventory file already lists are left as the file has them.
func MergeHosts(inv *compile.Inventory, dir string) error {
	entries, err := LoadHosts(dir)
	for _, e := range entries {
		inv.AddHost(e.Name, e.Groups, e.Vars)
	}
	return err
}

func (s *Server) admin(w http.ResponseWriter, r *http.Request) bool { return s.allow(w, r, true) }

// registerHost adds a host to the server's registry, so it may pull
// without being written into the inventory file.
func (s *Server) registerHost(w http.ResponseWriter, r *http.Request) {
	if !s.admin(w, r) {
		return
	}
	name := r.PathValue("name")
	var e HostEntry
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&e); err != nil {
		apiError(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	inv, err := compile.LoadInventory(s.Inventory)
	if err != nil {
		apiError(w, "inventory: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if e.NewCert {
		// The host starts over: its certificate is forgotten (and
		// revoked), and its token gets it a new one.
		s.ForgetCert(r.Context(), name)
	}
	res := HostResult{Status: "inventory"}
	if _, listed := inv.Host(name); !listed {
		if s.Hosts == "" {
			apiError(w, "host is not in the server's inventory, and this server does not accept registrations (it was started without -hosts)", http.StatusConflict)
			return
		}
		path, err := hostFile(s.Hosts, name)
		if err != nil {
			apiError(w, err.Error(), http.StatusBadRequest)
			return
		}
		e.Name, e.Registered = name, time.Now().UTC().Format(time.RFC3339)
		b, _ := yaml.Marshal(e)
		if err = os.MkdirAll(s.Hosts, 0o750); err == nil {
			if err = os.WriteFile(path+".tmp", b, 0o640); err == nil {
				err = os.Rename(path+".tmp", path)
			}
		}
		if err != nil {
			s.Log.Error("registering host failed", "host", name, "error", err.Error())
			apiError(w, "storing the host failed", http.StatusInternalServerError)
			return
		}
		res.Status = "registered"
		for _, g := range e.Groups {
			if !inv.HasGroup(g) {
				res.UnknownGroups = append(res.UnknownGroups, g)
			}
		}
		s.Log.Info("host registered", "remote", r.RemoteAddr, "host", name, "groups", e.Groups, "unknown_groups", res.UnknownGroups)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// unregisterHost removes a host from the registry; its token stops
// working unless the inventory file lists it too.
func (s *Server) unregisterHost(w http.ResponseWriter, r *http.Request) {
	if !s.admin(w, r) {
		return
	}
	name := r.PathValue("name")
	s.ForgetCert(r.Context(), name)
	if s.Hosts != "" {
		if path, err := hostFile(s.Hosts, name); err == nil && os.Remove(path) == nil {
			s.Log.Info("host unregistered", "remote", r.RemoteAddr, "host", name)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// join lets a new node obtain its own token by presenting a join token.
func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	var req struct{ Host string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		apiError(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	fail := func(code int, msg string) {
		s.Log.Warn("join refused", "remote", r.RemoteAddr, "host", req.Host, "status", code, "reason", msg)
		apiError(w, msg, code)
	}
	if err := CheckJoinToken(s.Secret, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), time.Now()); err != nil {
		fail(http.StatusUnauthorized, err.Error())
		return
	}
	if _, _, code, msg := s.hostAllowed(req.Host); code != 0 {
		fail(code, msg)
		return
	}
	s.Log.Info("join: token issued", "remote", r.RemoteAddr, "host", req.Host)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": Token(s.Secret, req.Host)})
}

var platformRE = regexp.MustCompile(`^([a-z0-9]+)-([a-z0-9]+)(\.sha256)?$`)

// agent serves the fleetreeve build for a platform, or its checksum. The
// binaries are public, so no token is needed; nodes use this to install
// and to keep themselves on the server's version.
func (s *Server) agent(w http.ResponseWriter, r *http.Request) {
	m := platformRE.FindStringSubmatch(r.PathValue("file"))
	if m == nil {
		http.NotFound(w, r)
		return
	}
	path, err := remote.FindAgent(m[1], m[2])
	if err != nil {
		s.Log.Warn("agent download: no build", "remote", r.RemoteAddr, "platform", m[1]+"-"+m[2], "error", err.Error())
		apiError(w, fmt.Sprintf("the server has no fleetreeve build for %s/%s", m[1], m[2]), http.StatusNotFound)
		return
	}
	if m[3] == "" {
		s.Log.Info("agent download", "remote", r.RemoteAddr, "platform", m[1]+"-"+m[2])
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, path)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		apiError(w, "reading agent failed", http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "%x\n", sha256.Sum256(data))
}

// authorise checks the bearer token for host and returns its inventory entry.
func (s *Server) authorise(w http.ResponseWriter, r *http.Request, host string) (*compile.Inventory, *compile.Host) {
	fail := func(code int, msg string) (*compile.Inventory, *compile.Host) {
		s.Log.Warn("request refused", "remote", r.RemoteAddr, "path", r.URL.Path, "host", host, "status", code, "reason", msg)
		apiError(w, msg, code)
		return nil, nil
	}
	// A host shows its certificate, or - where that is still allowed -
	// its token.
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	byCert, why := s.certHost(r)
	if why != "" {
		// The host showed a certificate that does not count (any more).
		// It is told, so that it asks for a new one even where its token
		// still gets it in.
		w.Header().Set("X-Fleetreeve-Certificate", "refused")
	}
	switch {
	case host != "" && byCert == host:
	case s.RequireCert:
		if why == "" {
			why = "this server only accepts hosts that show their certificate; the host gets one at its next run (fleetreeve pull)"
		}
		return fail(http.StatusUnauthorized, why)
	case host == "" || !hmac.Equal([]byte(token), []byte(Token(s.Secret, host))):
		return fail(http.StatusUnauthorized, "invalid host or token")
	}
	// A valid token for a host that left the inventory: revoked.
	inv, h, code, msg := s.hostAllowed(host)
	if code != 0 {
		return fail(code, msg)
	}
	return inv, h
}

func (s *Server) catalog(w http.ResponseWriter, r *http.Request) {
	var req catalogRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		apiError(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	inv, h := s.authorise(w, r, req.Host)
	if h == nil {
		return
	}
	cat, err := compile.Compile(compile.Input{Manifest: s.Manifest, Host: h, Inventory: inv, Facts: req.Facts, Extra: s.Extra, Prune: s.Prune})
	if err != nil {
		s.Log.Error("catalog: compile error", "remote", r.RemoteAddr, "host", h.Name, "error", err.Error())
		apiError(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	s.Log.Info("catalog served", "remote", r.RemoteAddr, "host", h.Name, "resources", len(cat.Resources))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cat)
}

func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	var rep Report
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&rep); err != nil {
		apiError(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if _, h := s.authorise(w, r, rep.Host); h == nil {
		return
	}
	rep.Time = time.Now().UTC().Format(time.RFC3339) // the server's clock, not the node's
	sum := rep.Summary
	level := slog.LevelInfo
	if rep.Failed() {
		level = slog.LevelError
	}
	attrs := []any{"remote", r.RemoteAddr, "host", rep.Host, "mode", rep.Mode, "resources", sum.Total, "changed", sum.Changed,
		"pending", sum.Pending, "refreshed", sum.Refreshed, "failed", sum.Failed, "skipped", sum.Skipped, "cached_catalog", rep.Cached}
	if rep.Error != "" {
		attrs = append(attrs, "error", rep.Error)
	}
	attrs = append(attrs, "manifest", rep.Manifest, "source", rep.Source)
	s.Log.Log(r.Context(), level, "report received", attrs...)
	s.record(runEvent(&rep))
	if s.Reports != "" {
		if filepath.Base(rep.Host) != rep.Host || strings.HasPrefix(rep.Host, ".") {
			apiError(w, "host name cannot be used as a file name", http.StatusBadRequest)
			return
		}
		if rep.Manifest != "" && !state.ValidName(rep.Manifest) {
			apiError(w, "invalid manifest name", http.StatusBadRequest)
			return
		}
		b, _ := json.MarshalIndent(rep, "", "  ")
		legacy := filepath.Join(s.Reports, rep.Host+".json") // written for clients that send no manifest name
		p := legacy
		if rep.Manifest != "" {
			p = filepath.Join(s.Reports, rep.Manifest+"@"+rep.Host+".json")
			// Once a host's pull client is upgraded, its old-style
			// report would linger as a second, ever older entry. A push
			// report says nothing about the pull client, so it must
			// leave that entry alone.
			if old, err := os.ReadFile(legacy); err == nil && rep.Source == "pull" {
				var prev Report
				if json.Unmarshal(old, &prev) == nil && prev.Manifest == "" {
					os.Remove(legacy)
				}
			}
		}
		err := os.MkdirAll(s.Reports, 0o750)
		if err == nil {
			err = os.WriteFile(p+".tmp", append(b, '\n'), 0o640)
		}
		if err == nil {
			err = os.Rename(p+".tmp", p)
		}
		if err != nil {
			s.Log.Error("storing report failed", "host", rep.Host, "error", err.Error())
			apiError(w, "storing report failed", http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// LoadReports reads the latest report of every host from a reports directory.
func LoadReports(dir string) ([]Report, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []Report
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var r Report
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("%s: %v", f, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// ---- client ----

// ErrUnavailable marks failures to reach the server (as opposed to the
// server refusing the request), in which case a cached catalog may be used.
var ErrUnavailable = errors.New("server unavailable")

type Client struct {
	server, host, token string
	http                *http.Client
	tr                  *http.Transport
	certRefused         bool
}

// CertRefused reports whether the server said, since the last time this
// was asked, that the certificate shown does not count.
func (c *Client) CertRefused() bool {
	was := c.certRefused
	c.certRefused = false
	return was
}

// WithCert makes the client show the host's certificate. The files are
// read at every connection, so a renewed certificate is picked up.
func (c *Client) WithCert(certFile, keyFile string) {
	if c.tr.TLSClientConfig == nil {
		c.tr.TLSClientConfig = &tls.Config{}
	}
	c.tr.TLSClientConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return &tls.Certificate{}, nil // none to show; the server says what that means
		}
		return &cert, nil
	}
	c.tr.CloseIdleConnections()
}

// Health asks the server how it is, and what it expects of hosts.
func (c *Client) Health(ctx context.Context) (map[string]any, error) {
	b, err := c.do(ctx, http.MethodGet, "/v1/health", "", nil)
	if err != nil {
		return nil, err
	}
	var h map[string]any
	return h, json.Unmarshal(b, &h)
}

// Cert has the server issue a certificate for the host's request. The
// host shows its current certificate, or its token while it has none.
func (c *Client) Cert(ctx context.Context, csrPEM []byte) (string, error) {
	b, err := c.do(ctx, http.MethodPost, "/v1/cert", c.token, map[string]string{"host": c.host, "csr": string(csrPEM)})
	if err != nil {
		return "", err
	}
	var resp struct{ Certificate string }
	if err := json.Unmarshal(b, &resp); err != nil || resp.Certificate == "" {
		return "", fmt.Errorf("unexpected response from server")
	}
	return resp.Certificate, nil
}

// NewClient prepares a client. Plain http is refused for anything but
// localhost unless insecure is set, because the token travels in a header.
func NewClient(server, host, token, caFile string, insecure bool) (*Client, error) {
	var ca []byte
	if caFile != "" {
		var err error
		if ca, err = os.ReadFile(caFile); err != nil {
			return nil, err
		}
	}
	return NewClientCA(server, host, token, ca, insecure)
}

// NewClientCA is NewClient with the CA certificate given as PEM data.
func NewClientCA(server, host, token string, ca []byte, insecure bool) (*Client, error) {
	u, err := url.Parse(server)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid server URL %q (expected https://host:port)", server)
	}
	if u.Scheme == "http" && !insecure {
		if ip := net.ParseIP(u.Hostname()); u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, fmt.Errorf("refusing to send the node token over plain http to %s; use https, or -insecure if the link is otherwise protected", u.Host)
		}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if len(ca) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, fmt.Errorf("the CA file holds no certificates")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	}
	return &Client{
		server: strings.TrimRight(server, "/"), host: host, token: token,
		http: &http.Client{Transport: tr, Timeout: 2 * time.Minute}, tr: tr,
	}, nil
}

func (c *Client) post(ctx context.Context, path string, body any) ([]byte, error) {
	return c.do(ctx, http.MethodPost, path, c.token, body)
}

// do sends one request. body, when not nil, is sent as JSON.
func (c *Client) do(ctx context.Context, method, path, token string, body any) ([]byte, error) {
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.server+path, payload)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Fleetreeve-Certificate") == "refused" {
		c.certRefused = true
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	msg := strings.TrimSpace(string(b))
	var apiErr struct{ Error string }
	if resp.StatusCode >= 300 && json.Unmarshal(b, &apiErr) == nil && apiErr.Error != "" {
		msg = apiErr.Error
	}
	switch {
	case resp.StatusCode >= 500:
		return nil, fmt.Errorf("%w: %s: %.300s", ErrUnavailable, resp.Status, msg)
	case resp.StatusCode >= 300:
		return nil, fmt.Errorf("server refused (%s): %.2000s", resp.Status, msg)
	}
	return b, nil
}

// Catalog sends the node's facts and returns its compiled catalog.
func (c *Client) Catalog(ctx context.Context, facts map[string]any) (*core.Catalog, error) {
	b, err := c.post(ctx, "/v1/catalog", catalogRequest{Host: c.host, Facts: facts})
	if err != nil {
		return nil, err
	}
	cat := &core.Catalog{}
	if err := json.Unmarshal(b, cat); err != nil {
		return nil, fmt.Errorf("unexpected response from server: %v", err)
	}
	return cat, nil
}

// Ping checks that the server can be reached and is a fleetreeve server, with
// the same address and TLS trust the pull client will use.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	b, err := c.do(ctx, http.MethodGet, "/install.sh", "", nil)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("no answer from %s within 10s", c.server)
		}
		return err
	}
	if !strings.Contains(string(b), "fleetreeve") {
		return fmt.Errorf("%s answered, but not as a fleetreeve server", c.server)
	}
	return nil
}

// ErrNoRegistry means the server predates host registration.
var ErrNoRegistry = errors.New("the server does not support host registration (older version)")

// RegisterHost makes a host known to the server, so it is allowed to pull.
func (c *Client) RegisterHost(ctx context.Context, adminToken string, e HostEntry) (*HostResult, error) {
	b, err := c.do(ctx, http.MethodPut, "/v1/hosts/"+url.PathEscape(e.Name), adminToken, e)
	if err != nil {
		if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "405") {
			return nil, ErrNoRegistry
		}
		return nil, err
	}
	res := &HostResult{}
	if err := json.Unmarshal(b, res); err != nil {
		return nil, fmt.Errorf("unexpected response from server: %v", err)
	}
	return res, nil
}

// UnregisterHost removes a host from the server's registry.
func (c *Client) UnregisterHost(ctx context.Context, adminToken, name string) error {
	_, err := c.do(ctx, http.MethodDelete, "/v1/hosts/"+url.PathEscape(name), adminToken, nil)
	if err != nil && (strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "405")) {
		return nil
	}
	return err
}

// Join trades a join token for this host's own node token.
func (c *Client) Join(ctx context.Context, joinToken string) (string, error) {
	b, err := c.do(ctx, http.MethodPost, "/v1/join", joinToken, map[string]string{"host": c.host})
	if err != nil {
		return "", err
	}
	var resp struct{ Token string }
	if err := json.Unmarshal(b, &resp); err != nil || resp.Token == "" {
		return "", fmt.Errorf("unexpected response from server")
	}
	return resp.Token, nil
}

// AgentSHA returns the SHA-256 of the fleetreeve build the server offers for a
// platform ("linux-amd64").
func (c *Client) AgentSHA(ctx context.Context, platform string) (string, error) {
	b, err := c.do(ctx, http.MethodGet, "/v1/agent/"+platform+".sha256", "", nil)
	return strings.TrimSpace(string(b)), err
}

// Agent downloads the fleetreeve build for a platform.
func (c *Client) Agent(ctx context.Context, platform string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, "/v1/agent/"+platform, "", nil)
}

func (c *Client) Report(ctx context.Context, rep *Report) error {
	_, err := c.post(ctx, "/v1/report", rep)
	return err
}

// hmacEqual compares two secrets without revealing where they differ.
func hmacEqual(a, b []byte) bool { return hmac.Equal(a, b) }
