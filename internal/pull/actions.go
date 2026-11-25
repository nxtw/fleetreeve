package pull

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nxtw/fleetreeve/internal/core"
	"github.com/nxtw/fleetreeve/internal/engine"
	"github.com/nxtw/fleetreeve/internal/state"
)

// Actions let an operator ask a node to do something without anyone
// connecting to it: the request is queued on the server, and the node
// collects and carries it out at its next check-in, then reports the
// outcome. The server still never reaches out to a node.
//
// What can be asked, and with which API token:
//
//	operator  apply     run the node's configuration now
//	          plan      do a dry run now
//	          rollback  restore a resource to the version before fleetreeve
//	                    last changed it
//	          release   end the hold a rollback puts a resource on
//	exec      command   run a shell command, as root
//
// The operator kinds give no new power over a node: they do what its
// next check-in would do, or return to something the node itself kept.
// "command" is different in kind - it is root on the node - and so has
// its own token, which a server operator has to hand out deliberately.
// (The admin token may ask for anything.)

// Action is one queued request for a node.
type Action struct {
	ID          string `json:"id"`
	Host        string `json:"host"`
	Manifest    string `json:"manifest"`
	Kind        string `json:"kind"` // apply, plan, rollback, release or command
	Resource    string `json:"resource,omitempty"`
	Command     string `json:"command,omitempty"`
	Timeout     int    `json:"timeout,omitempty"` // seconds, for a command
	RequestedBy string `json:"requested_by,omitempty"`
	Queued      string `json:"queued"`
}

// ActionResult is what a node reports about an action it carried out.
type ActionResult struct {
	Action
	Status string `json:"status"` // done, restored, released or failed
	Detail string `json:"detail,omitempty"`
	Output string `json:"output,omitempty"` // of a command, both streams, possibly cut short
	Exit   int    `json:"exit"`             // of a command
	Done   string `json:"done"`
}

const (
	maxCommand     = 4096
	maxOutput      = 64 << 10
	defaultTimeout = 60
	maxTimeout     = 600
	keepResults    = 30 // per host
)

var actionsMu sync.Mutex // one server process owns the queue files

func (s *Server) actionsFile(host string) (string, error) {
	p, err := hostFile(s.Actions, host)
	return strings.TrimSuffix(p, ".yaml") + ".json", err
}

func (s *Server) pending(host string) []Action {
	if s.Actions == "" {
		return nil
	}
	p, err := s.actionsFile(host)
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var list []Action
	json.Unmarshal(b, &list)
	return list
}

func (s *Server) storePending(host string, list []Action) error {
	p, err := s.actionsFile(host)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		os.Remove(p)
		return nil
	}
	if err := os.MkdirAll(s.Actions, 0o750); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(list, "", "  ")
	if err := os.WriteFile(p+".tmp", b, 0o640); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

// holds reports whether a request carries one of the given API tokens
// (the admin token always counts).
func (s *Server) holds(r *http.Request, roles ...string) bool {
	token := []byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	for _, role := range append(roles, "admin") {
		if hmac.Equal(token, []byte(APIToken(s.Secret, role))) {
			return true
		}
	}
	return false
}

// operator checks for a token that may ask nodes for something.
func (s *Server) operator(w http.ResponseWriter, r *http.Request) bool {
	if s.holds(r, "operator", "exec") {
		return true
	}
	s.Log.Warn("request refused", "remote", r.RemoteAddr, "path", r.URL.Path, "status", http.StatusUnauthorized, "reason", "invalid API token")
	apiError(w, "this request needs the operator, exec or admin API token (fleetreeve token -api ...)", http.StatusUnauthorized)
	return false
}

func (s *Server) resultsFile(host string) (string, error) {
	p, err := hostFile(s.Actions, host)
	return strings.TrimSuffix(p, ".yaml") + ".results.json", err
}

// results returns what a host reported about requests it carried out,
// newest first.
func (s *Server) results(host string) []ActionResult {
	if s.Actions == "" {
		return nil
	}
	p, err := s.resultsFile(host)
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var list []ActionResult
	json.Unmarshal(b, &list)
	return list
}

// queueAction accepts a rollback or release request for a host.
func (s *Server) queueAction(w http.ResponseWriter, r *http.Request) {
	if !s.operator(w, r) {
		return
	}
	if s.Actions == "" {
		apiError(w, "this server does not accept actions (it was started without -actions)", http.StatusConflict)
		return
	}
	var a Action
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&a); err != nil {
		apiError(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	a.Host = r.PathValue("name")
	if _, _, code, msg := s.hostAllowed(a.Host); code != 0 {
		apiError(w, msg, http.StatusNotFound)
		return
	}
	switch a.Kind {
	case "rollback", "release":
		id, err := core.ParseRef(a.Resource)
		if !state.ValidName(a.Manifest) {
			apiError(w, "manifest must be the name of a manifest", http.StatusBadRequest)
			return
		}
		if err != nil {
			apiError(w, err.Error(), http.StatusBadRequest)
			return
		}
		a.Resource, a.Command, a.Timeout = id, "", 0
	case "apply", "plan":
		a.Manifest, a.Resource, a.Command, a.Timeout = "", "", "", 0
	case "command":
		// Root on the node: this needs the token made for it.
		if !s.holds(r, "exec") {
			s.Log.Warn("request refused", "remote", r.RemoteAddr, "path", r.URL.Path, "status", http.StatusForbidden, "reason", "a command needs the exec token")
			apiError(w, "running a command needs the exec or admin API token (fleetreeve token -api exec)", http.StatusForbidden)
			return
		}
		a.Command = strings.TrimSpace(a.Command)
		if a.Command == "" || len(a.Command) > maxCommand {
			apiError(w, "command must be given, and be at most 4096 characters", http.StatusBadRequest)
			return
		}
		if a.Timeout <= 0 {
			a.Timeout = defaultTimeout
		}
		if a.Timeout > maxTimeout {
			apiError(w, "timeout can be at most 600 seconds", http.StatusBadRequest)
			return
		}
		a.Manifest, a.Resource = "", ""
	default:
		apiError(w, "kind must be apply, plan, rollback, release or command", http.StatusBadRequest)
		return
	}
	raw := make([]byte, 8)
	rand.Read(raw)
	a.ID, a.Queued = hex.EncodeToString(raw), time.Now().UTC().Format(time.RFC3339)
	actionsMu.Lock()
	defer actionsMu.Unlock()
	list := s.pending(a.Host)
	for _, p := range list {
		if p.Manifest == a.Manifest && p.Resource == a.Resource && p.Kind == a.Kind && p.Command == a.Command {
			apiError(w, "the same request is already waiting for this host", http.StatusConflict)
			return
		}
	}
	if len(list) >= 50 {
		apiError(w, "too many requests are waiting for this host", http.StatusTooManyRequests)
		return
	}
	if err := s.storePending(a.Host, append(list, a)); err != nil {
		s.Log.Error("queueing action failed", "host", a.Host, "error", err.Error())
		apiError(w, "storing the request failed", http.StatusInternalServerError)
		return
	}
	s.Log.Info("action queued", "remote", r.RemoteAddr, "host", a.Host, "kind", a.Kind, "resource", a.Resource, "manifest", a.Manifest,
		"command", a.Command, "requested_by", a.RequestedBy, "id", a.ID)
	writeJSON(w, http.StatusAccepted, a)
}

// cancelAction withdraws a request the node has not collected yet.
func (s *Server) cancelAction(w http.ResponseWriter, r *http.Request) {
	if !s.operator(w, r) {
		return
	}
	host, id := r.PathValue("name"), r.PathValue("id")
	actionsMu.Lock()
	defer actionsMu.Unlock()
	var keep []Action
	found := false
	for _, a := range s.pending(host) {
		if a.ID == id {
			found = true
			continue
		}
		keep = append(keep, a)
	}
	if !found {
		apiError(w, "no such request is waiting; the host may have collected it already", http.StatusNotFound)
		return
	}
	s.storePending(host, keep)
	s.Log.Info("action cancelled", "remote", r.RemoteAddr, "host", host, "id", id)
	w.WriteHeader(http.StatusNoContent)
}

// claimActions hands a node its waiting requests, once.
func (s *Server) claimActions(w http.ResponseWriter, r *http.Request) {
	var req struct{ Host string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		apiError(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if _, h := s.authorise(w, r, req.Host); h == nil {
		return
	}
	actionsMu.Lock()
	defer actionsMu.Unlock()
	list := s.pending(req.Host)
	if len(list) > 0 {
		s.storePending(req.Host, nil)
		s.Log.Info("actions collected", "remote", r.RemoteAddr, "host", req.Host, "count", len(list))
	}
	if list == nil {
		list = []Action{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"actions": list})
}

// storeResults keeps what a node reports about requests it carried out.
func (s *Server) storeResults(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host    string
		Results []ActionResult
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		apiError(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if _, h := s.authorise(w, r, req.Host); h == nil {
		return
	}
	for i := range req.Results {
		res := req.Results[i]
		res.Host = req.Host // a node speaks for itself only
		s.record(requestEvent(&res))
	}
	if s.Actions == "" || len(req.Results) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	p, err := s.resultsFile(req.Host)
	if err != nil {
		apiError(w, err.Error(), http.StatusBadRequest)
		return
	}
	actionsMu.Lock()
	defer actionsMu.Unlock()
	list := s.results(req.Host)
	for _, res := range req.Results {
		res.Host = req.Host // a node speaks for itself only
		if len(res.Output) > maxOutput {
			res.Output = res.Output[:maxOutput]
		}
		list = append([]ActionResult{res}, list...)
		s.Log.Info("action carried out", "host", req.Host, "kind", res.Kind, "status", res.Status, "exit", res.Exit, "requested_by", res.RequestedBy, "id", res.ID)
	}
	if len(list) > keepResults {
		list = list[:keepResults]
	}
	b, _ := json.MarshalIndent(list, "", "  ")
	if err = os.MkdirAll(s.Actions, 0o750); err == nil {
		if err = os.WriteFile(p+".tmp", b, 0o640); err == nil {
			err = os.Rename(p+".tmp", p)
		}
	}
	if err != nil {
		apiError(w, "storing the results failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listActions returns, for every host, what is waiting and what was
// carried out recently. Command output is included, so this is for the
// operator and exec tokens, not for the read token.
func (s *Server) listActions(w http.ResponseWriter, r *http.Request) {
	if !s.operator(w, r) {
		return
	}
	inv, _, _, err := s.fleet()
	if err != nil {
		apiError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pending, results := []Action{}, []ActionResult{}
	for _, h := range inv.Hosts {
		pending = append(pending, s.pending(h.Name)...)
		results = append(results, s.results(h.Name)...)
	}
	sort.Slice(results, func(a, b int) bool { return results[a].Done > results[b].Done })
	writeJSON(w, http.StatusOK, map[string]any{"pending": pending, "results": results})
}

// ---- node side ----

// Claim collects the requests waiting for this host. A server too old to
// have any answers 404, which simply means none.
func (c *Client) Claim(ctx context.Context) ([]Action, error) {
	b, err := c.post(ctx, "/v1/actions/claim", map[string]string{"host": c.host})
	if err != nil {
		if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "405") {
			return nil, nil
		}
		return nil, err
	}
	var resp struct{ Actions []Action }
	return resp.Actions, json.Unmarshal(b, &resp)
}

// PostResults tells the server what became of collected requests.
func (c *Client) PostResults(ctx context.Context, results []ActionResult) error {
	if len(results) == 0 {
		return nil
	}
	_, err := c.post(ctx, "/v1/actions/results", map[string]any{"host": c.host, "results": results})
	return err
}

// Perform carries out collected requests on this node: rollbacks,
// releases and commands. Requests for a run (apply, plan) are returned
// for the caller to act on, since a run is the caller's business.
func Perform(ctx context.Context, host string, actions []Action) (done []ActionResult, runs []Action) {
	for _, a := range actions {
		res := ActionResult{Action: a, Status: "failed"}
		switch a.Kind {
		case "apply", "plan":
			runs = append(runs, a)
			continue
		case "command":
			res.Output, res.Exit, res.Detail = runCommand(ctx, a.Command, a.Timeout)
			if res.Exit == 0 && res.Detail == "" {
				res.Status = "done"
			}
		case "rollback", "release":
			results, err := engine.Rollback(core.NewCtx(ctx, nil, false),
				engine.RollbackRequest{Key: state.Key(a.Manifest, host), IDs: []string{a.Resource}, Release: a.Kind == "release"})
			switch {
			case err != nil:
				res.Detail = err.Error()
			case len(results) == 1:
				res.Status, res.Detail = results[0].Status, results[0].Error
				if len(results[0].Refreshed) > 0 {
					res.Detail = strings.TrimSpace(res.Detail + " Refreshed: " + strings.Join(results[0].Refreshed, ", ") + ".")
				}
			}
		default:
			res.Detail = "this node does not know the request kind " + a.Kind + "; it may need a newer fleetreeve"
		}
		res.Done = time.Now().UTC().Format(time.RFC3339)
		done = append(done, res)
	}
	return done, runs
}

// runCommand runs a shell command and returns its output (both streams,
// cut off at a limit), exit status, and a note when it did not finish.
func runCommand(ctx context.Context, command string, timeout int) (output string, exit int, note string) {
	if timeout <= 0 || timeout > maxTimeout {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.WaitDelay = 2 * time.Second // do not hang on children that keep the output open
	out := &capped{limit: maxOutput}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	output = out.buf.String()
	if out.dropped > 0 {
		output += fmt.Sprintf("\n[output cut off: %d more bytes]", out.dropped)
	}
	var ee *exec.ExitError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return output, -1, fmt.Sprintf("stopped after %d seconds: it did not finish in time", timeout)
	case err == nil:
		return output, 0, ""
	case errors.As(err, &ee):
		return output, ee.ExitCode(), ""
	}
	return output, -1, err.Error()
}

// capped keeps the first limit bytes written to it.
type capped struct {
	buf     bytes.Buffer
	limit   int
	dropped int
}

func (c *capped) Write(p []byte) (int, error) {
	room := c.limit - c.buf.Len()
	if room >= len(p) {
		c.buf.Write(p)
	} else {
		if room > 0 {
			c.buf.Write(p[:room])
		}
		c.dropped += len(p) - max(room, 0)
	}
	return len(p), nil
}

// Restorable lists what this node could roll back for a manifest.
func Restorable(ctx context.Context, manifest, host string) []engine.RollbackResult {
	list, _ := engine.Rollback(core.NewCtx(ctx, nil, false), engine.RollbackRequest{Key: state.Key(manifest, host)})
	return list
}

// ---- operator side ----

// QueueAction asks the server to pass a request on to a host.
func (c *Client) QueueAction(ctx context.Context, token string, a Action) (*Action, error) {
	b, err := c.do(ctx, http.MethodPost, "/v1/hosts/"+url.PathEscape(a.Host)+"/actions", token, a)
	if err != nil {
		return nil, err
	}
	out := &Action{}
	return out, json.Unmarshal(b, out)
}

// CancelAction withdraws a waiting request.
func (c *Client) CancelAction(ctx context.Context, token, host, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/v1/hosts/"+url.PathEscape(host)+"/actions/"+url.PathEscape(id), token, nil)
	return err
}
