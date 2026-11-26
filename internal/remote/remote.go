// Package remote runs fleetreeve's node-side work on a host: in-process for
// local hosts, or over SSH by pushing a copy of the fleetreeve binary and
// talking to it on stdin/stdout. Nothing is installed permanently and no
// daemon or open port is needed on the node.
package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nxtw/fleetreeve/internal/compile"
	"github.com/nxtw/fleetreeve/internal/core"
	"github.com/nxtw/fleetreeve/internal/engine"
	"github.com/nxtw/fleetreeve/internal/facts"
	"github.com/nxtw/fleetreeve/internal/state"
)

// Executor does node-side work for one host.
type Executor interface {
	Facts(ctx context.Context) (map[string]any, error)
	// Run plans or applies a catalog. Unless told not to, an enrolled
	// node then reports the run to its server itself; what became of
	// that is returned (nil: the node is not enrolled, or was told not to).
	Run(ctx context.Context, cat *core.Catalog, opts RunOptions, emit func(engine.Result)) (engine.Summary, *Reported, error)
	// Command runs a shell command and returns its combined output.
	Command(ctx context.Context, script string) (string, int, error)
	// State returns the node's state for a manifest name, after dropping
	// the entries listed in forget.
	State(ctx context.Context, name string, forget []string) (*state.File, error)
	// Probe runs fn-side check "can this node reach url?" on the node and
	// returns its error, if any. req is passed to the node as JSON.
	Probe(ctx context.Context, req ProbeRequest) (ProbeResult, error)
	// Rollback restores resources to an earlier kept version, releases
	// them, or lists what the node has kept.
	Rollback(ctx context.Context, req engine.RollbackRequest) ([]engine.RollbackResult, error)
}

// RunOptions say how to run a catalog.
type RunOptions struct {
	Plan bool
	// NoReport: the node must not report the run to its server - the
	// caller does that itself, or it was asked not to.
	NoReport bool
}

// Reported is what became of a node's own report of a run.
type Reported struct {
	Server string `json:"server,omitempty"`
	Error  string `json:"error,omitempty"`
}

// ReportFunc reports a run to the server the node is enrolled with, and
// returns nil when it is not enrolled. It is set by the cli package,
// which knows the node's settings.
var ReportFunc func(ctx context.Context, cat *core.Catalog, plan bool, sum engine.Summary, results []engine.Result, runErr error) *Reported

// RunAndReport runs a catalog on this machine and has it reported. It is
// what both the local executor and the agent do.
func RunAndReport(ctx context.Context, cat *core.Catalog, opts RunOptions, emit func(engine.Result)) (engine.Summary, *Reported, error) {
	var results []engine.Result
	sum, err := engine.Run(core.NewCtx(ctx, cat.Facts, opts.Plan), cat, func(r engine.Result) {
		results = append(results, r)
		emit(r)
	})
	if opts.NoReport || ReportFunc == nil {
		return sum, nil, err
	}
	// The report is sent even when the run was interrupted.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return sum, ReportFunc(rctx, cat, opts.Plan, sum, results, err), err
}

// ProbeRequest asks a node whether it can reach a pull server.
type ProbeRequest struct {
	Server   string `json:"server"`
	CA       []byte `json:"ca,omitempty"`
	Insecure bool   `json:"insecure,omitempty"`
}

// ProbeResult is what a node found besides "the server can be reached".
type ProbeResult struct {
	// CertRefused: the node holds a certificate that this server does not
	// accept, as after a move from another server.
	CertRefused bool `json:"cert_refused,omitempty"`
}

// ProbeFunc performs a probe on the node. It is set by the pull package's
// user (the cli), which keeps this package free of a dependency on it.
var ProbeFunc func(ctx context.Context, req ProbeRequest) (ProbeResult, error)

func (local) Rollback(ctx context.Context, req engine.RollbackRequest) ([]engine.RollbackResult, error) {
	return engine.Rollback(core.NewCtx(ctx, nil, false), req)
}

func (s *ssh) Rollback(ctx context.Context, req engine.RollbackRequest) ([]engine.RollbackResult, error) {
	if err := s.bootstrap(ctx); err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(req)
	var out bytes.Buffer
	if _, err := s.exec(ctx, s.agentCmd("rollback"), bytes.NewReader(payload), &out); err != nil {
		return nil, err
	}
	var resp struct {
		Results []engine.RollbackResult
		Error   string
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("unexpected agent output: %.200s", out.String())
	}
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	return resp.Results, nil
}

func (local) Probe(ctx context.Context, req ProbeRequest) (ProbeResult, error) {
	return ProbeFunc(ctx, req)
}

func (s *ssh) Probe(ctx context.Context, req ProbeRequest) (ProbeResult, error) {
	var resp struct {
		ProbeResult
		Error string
	}
	if err := s.bootstrap(ctx); err != nil {
		return resp.ProbeResult, err
	}
	payload, _ := json.Marshal(req)
	var out bytes.Buffer
	if _, err := s.exec(ctx, s.agentCmd("probe"), bytes.NewReader(payload), &out); err != nil {
		return resp.ProbeResult, err
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		return resp.ProbeResult, fmt.Errorf("unexpected agent output: %.200s", out.String())
	}
	if resp.Error != "" {
		return resp.ProbeResult, errors.New(resp.Error)
	}
	return resp.ProbeResult, nil
}

// EditState is the node side of State.
func EditState(name string, forget []string) (*state.File, error) {
	if len(forget) == 0 {
		return state.Load(name)
	}
	s, err := state.Open(name)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	for _, id := range forget {
		if _, ok := s.Resources[id]; !ok {
			return nil, fmt.Errorf("%s is not in state %q", id, name)
		}
		delete(s.Resources, id)
	}
	return s.File, s.Save()
}

func New(h *compile.Host) Executor {
	if h.Connection == "local" {
		return local{}
	}
	return &ssh{h: h}
}

// Message is one line of the agent's stdout protocol.
type Message struct {
	Result   *engine.Result  `json:"result,omitempty"`
	Summary  *engine.Summary `json:"summary,omitempty"`
	Reported *Reported       `json:"reported,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// ---- local ----

type local struct{}

func (local) Facts(context.Context) (map[string]any, error) { return facts.Collect(), nil }

func (local) Run(ctx context.Context, cat *core.Catalog, opts RunOptions, emit func(engine.Result)) (engine.Summary, *Reported, error) {
	return RunAndReport(ctx, cat, opts, emit)
}

func (local) Command(ctx context.Context, script string) (string, int, error) {
	out, err := exec.CommandContext(ctx, "/bin/sh", "-c", script).CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(out), ee.ExitCode(), nil
	}
	return string(out), 0, err
}

func (local) State(_ context.Context, name string, forget []string) (*state.File, error) {
	return EditState(name, forget)
}

// ---- ssh ----

// Login runs a command on a host with the script on its stdin, and
// returns its output. Unlike everything else here it does not insist on
// logging in unattended: ssh may ask for a password or a passphrase on
// the terminal. It is for the first contact with a host, by a person.
func Login(ctx context.Context, h *compile.Host, command, script string) (string, error) {
	s := &ssh{h: h, interactive: true}
	var out bytes.Buffer
	_, err := s.exec(ctx, command, strings.NewReader(script), &out)
	return out.String(), err
}

type ssh struct {
	h     *compile.Host
	once  sync.Once
	agent string // shell expression for the agent binary on the node
	err   error
	// interactive: a person is present, so ssh may ask for a password.
	interactive bool
	trust       sync.Once // host_key_check: false - learn the host's key once per run
}

func (s *ssh) args() []string {
	var a []string
	if !s.interactive {
		a = append(a, "-o", "BatchMode=yes")
	}
	if dir := controlDir(); dir != "" && !s.interactive {
		// Reuse one connection for every step of the run.
		a = append(a, "-o", "ControlMaster=auto", "-o", "ControlPersist=60s", "-o", "ControlPath="+dir+"/%C")
	}
	if s.h.Port != 0 {
		a = append(a, "-p", strconv.Itoa(s.h.Port))
	}
	if s.h.Identity != "" {
		a = append(a, "-i", s.h.Identity)
	}
	if s.h.SkipHostKey() && os.Getenv("FLEETREEVE_SSH") != "" {
		// A stand-in for ssh (tests): just do not verify.
		a = append(a, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null")
	}
	a = append(a, s.h.SSHArgs...)
	target := s.h.Address
	if s.h.User != "" {
		target = s.h.User + "@" + target
	}
	return append(a, target)
}

// controlDirMax is how long the directory for control sockets may be.
// A Unix socket path is limited to 104 bytes on some systems; ssh puts
// "/" and 40 characters (%C) after the directory, and while it sets the
// socket up another 17 for a temporary suffix.
const controlDirMax = 104 - 1 - 40 - 17 - 1

// controlDir is where the control sockets of multiplexed connections
// go: under the cache directory, or - where that makes paths too long
// for a socket - somewhere shorter in the home directory. Without a
// place that fits, connections are simply not shared.
func controlDir() string {
	var candidates []string
	if base, err := os.UserCacheDir(); err == nil {
		candidates = append(candidates, filepath.Join(base, "fleetreeve", "cm"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".fleetreeve", "cm"), filepath.Join(home, ".frcm"))
	}
	return firstControlDir(candidates)
}

func firstControlDir(candidates []string) string {
	for _, dir := range candidates {
		if len(dir) <= controlDirMax && os.MkdirAll(dir, 0o700) == nil {
			return dir
		}
	}
	return ""
}

// exec runs a shell script on the node. A non-zero exit status of the
// script itself is returned as code, not as an error.
// KnownHostsName is the name ssh looks a host's key up under.
func KnownHostsName(h *compile.Host) string {
	for _, a := range h.SSHArgs {
		if v, ok := strings.CutPrefix(a, "HostKeyAlias="); ok {
			return v
		}
	}
	if h.Port != 0 && h.Port != 22 {
		return "[" + h.Address + "]:" + strconv.Itoa(h.Port)
	}
	return h.Address
}

// scanned turns the output of ssh-keyscan into known_hosts lines for name.
func scanned(out, name string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) == 3 && !strings.HasPrefix(f[0], "#") {
			lines = append(lines, name+" "+f[1]+" "+f[2])
		}
	}
	sort.Strings(lines)
	return lines
}

// trustPresentKey is what "do not verify the host key" means: whatever
// key the host presents now is taken for its key and remembered, in place
// of any other this user knew it by. The connection that follows, and
// later ones made with verification on, are checked against it - so a
// rebuilt machine is no obstacle, yet a host enrolled this way is still
// recognised afterwards. Failing to learn the key is not an error here:
// ssh will say what is wrong.
func (s *ssh) trustPresentKey(ctx context.Context) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	name := KnownHostsName(s.h)
	args := []string{"-T", "5"}
	if s.h.Port != 0 {
		args = append(args, "-p", strconv.Itoa(s.h.Port))
	}
	out, err := exec.CommandContext(ctx, "ssh-keyscan", append(args, s.h.Address)...).Output()
	now := scanned(string(out), name)
	if err != nil || len(now) == 0 {
		return
	}
	file := filepath.Join(home, ".ssh", "known_hosts")
	have, _ := exec.CommandContext(ctx, "ssh-keygen", "-F", name, "-f", file).Output()
	known := map[string]bool{}
	for _, l := range strings.Split(string(have), "\n") {
		if f := strings.Fields(l); len(f) >= 3 && !strings.HasPrefix(l, "#") {
			known[f[len(f)-2]+" "+f[len(f)-1]] = true
		}
	}
	same := len(known) > 0
	for _, l := range now {
		if _, key, _ := strings.Cut(l, " "); !known[key] {
			same = false
		}
	}
	if same {
		return
	}
	if len(known) > 0 {
		exec.CommandContext(ctx, "ssh-keygen", "-R", name, "-f", file).Run()
	}
	if os.MkdirAll(filepath.Dir(file), 0o700) != nil {
		return
	}
	if f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintln(f, strings.Join(now, "\n"))
		f.Close()
	}
}

func (s *ssh) exec(ctx context.Context, script string, stdin io.Reader, stdout io.Writer) (int, error) {
	bin := os.Getenv("FLEETREEVE_SSH")
	if bin == "" {
		bin = "ssh"
		if s.h.SkipHostKey() {
			s.trust.Do(func() { s.trustPresentKey(ctx) })
		}
	}
	cmd := exec.CommandContext(ctx, bin, append(s.args(), script)...)
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, &stderr
	err := cmd.Run()
	msg := strings.TrimSpace(stderr.String())
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case ctx.Err() != nil:
		return -1, ctx.Err()
	case errors.As(err, &ee) && ee.ExitCode() == 255:
		return 255, fmt.Errorf("ssh connection failed: %s", msg)
	case errors.As(err, &ee):
		if msg == "" {
			msg = fmt.Sprintf("exit status %d", ee.ExitCode())
		}
		return ee.ExitCode(), errors.New(msg)
	}
	return -1, err
}

// bootstrap makes sure a matching fleetreeve binary exists on the node. It is
// stored under a content hash, so it is uploaded once per version.
func (s *ssh) bootstrap(ctx context.Context) error {
	s.once.Do(func() {
		var out bytes.Buffer
		if _, err := s.exec(ctx, "uname -sm", nil, &out); err != nil {
			s.err = err
			return
		}
		f := strings.Fields(strings.ToLower(out.String()))
		if len(f) != 2 {
			s.err = fmt.Errorf("unexpected platform %q", strings.TrimSpace(out.String()))
			return
		}
		arch := map[string]string{"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[f[1]]
		if arch == "" {
			s.err = fmt.Errorf("unsupported architecture %s", f[1])
			return
		}
		bin, err := FindAgent(f[0], arch)
		if err != nil {
			s.err = err
			return
		}
		data, err := os.ReadFile(bin)
		if err != nil {
			s.err = err
			return
		}
		name := fmt.Sprintf("agent-%x", sha256.Sum256(data))[:22]
		s.agent = `"$HOME/.cache/fleetreeve/` + name + `"`
		if code, _ := s.exec(ctx, "test -x "+s.agent, nil, nil); code == 0 {
			return
		}
		upload := `set -e; d="$HOME/.cache/fleetreeve"; mkdir -p "$d"; cat > "$d/NAME.$$"; chmod 755 "$d/NAME.$$"; mv "$d/NAME.$$" "$d/NAME"; ` +
			`find "$d" -name 'agent-*' ! -name NAME -mtime +7 -exec rm -f {} + 2>/dev/null || true`
		if _, err := s.exec(ctx, strings.ReplaceAll(upload, "NAME", name), bytes.NewReader(data), nil); err != nil {
			s.err = fmt.Errorf("uploading agent: %v", err)
		}
	})
	return s.err
}

// FindAgent locates a fleetreeve binary for the node's platform.
func FindAgent(goos, goarch string) (string, error) {
	name := "fleetreeve-" + goos + "-" + goarch
	self, _ := os.Executable()
	dirs := []string{os.Getenv("FLEETREEVE_AGENT_DIR")}
	if self != "" {
		dirs = append(dirs, filepath.Join(filepath.Dir(self), "agents"), filepath.Dir(self))
	}
	if c, err := os.UserCacheDir(); err == nil {
		dirs = append(dirs, filepath.Join(c, "fleetreeve", "agents"))
	}
	dirs = append(dirs, "/usr/libexec/fleetreeve/agents") // where the fleetreeve-server package puts them
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if p := filepath.Join(d, name); isFile(p) {
			return p, nil
		}
	}
	if goos == runtime.GOOS && goarch == runtime.GOARCH && self != "" {
		return self, nil
	}
	return "", fmt.Errorf("no agent binary for %s/%s: build one with `make agents`, or put %s in $FLEETREEVE_AGENT_DIR", goos, goarch, name)
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func (s *ssh) agentCmd(args string) string {
	cmd := s.agent + " agent " + args
	if s.h.Sudo() {
		cmd = "sudo -n " + cmd
	}
	return cmd
}

func (s *ssh) Facts(ctx context.Context) (map[string]any, error) {
	if err := s.bootstrap(ctx); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if _, err := s.exec(ctx, s.agentCmd("facts"), nil, &out); err != nil {
		return nil, fmt.Errorf("gathering facts: %v", err)
	}
	var f map[string]any
	if err := json.Unmarshal(out.Bytes(), &f); err != nil {
		return nil, fmt.Errorf("gathering facts: unexpected agent output: %.200s", out.String())
	}
	return f, nil
}

func (s *ssh) Run(ctx context.Context, cat *core.Catalog, opts RunOptions, emit func(engine.Result)) (engine.Summary, *Reported, error) {
	var sum engine.Summary
	var reported *Reported
	if err := s.bootstrap(ctx); err != nil {
		return sum, nil, err
	}
	payload, err := json.Marshal(cat)
	if err != nil {
		return sum, nil, err
	}
	args := "run"
	if opts.Plan {
		args += " --plan"
	}
	if opts.NoReport {
		args += " --no-report"
	}
	var agentErr error
	done := false
	lines := &lineWriter{fn: func(line []byte) {
		var m Message
		if err := json.Unmarshal(line, &m); err != nil {
			agentErr = fmt.Errorf("unexpected agent output: %.200s", line)
			return
		}
		switch {
		case m.Result != nil:
			emit(*m.Result)
		case m.Summary != nil:
			sum, done = *m.Summary, true
		case m.Reported != nil:
			reported = m.Reported
		case m.Error != "":
			agentErr = errors.New(m.Error)
		}
	}}
	_, err = s.exec(ctx, s.agentCmd(args), bytes.NewReader(payload), lines)
	lines.flush()
	switch {
	case agentErr != nil:
		return sum, reported, agentErr
	case err != nil:
		return sum, reported, err
	case !done:
		return sum, reported, errors.New("agent exited without reporting a summary")
	}
	return sum, reported, nil
}

func (s *ssh) State(ctx context.Context, name string, forget []string) (*state.File, error) {
	if strings.ContainsAny(name, " '\"$`\\;&|<>") {
		return nil, fmt.Errorf("invalid state name %q", name)
	}
	if err := s.bootstrap(ctx); err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(forget)
	var out bytes.Buffer
	if _, err := s.exec(ctx, s.agentCmd("state "+name), bytes.NewReader(payload), &out); err != nil {
		return nil, err
	}
	f := &state.File{}
	if err := json.Unmarshal(out.Bytes(), f); err != nil {
		return nil, fmt.Errorf("unexpected agent output: %.200s", out.String())
	}
	return f, nil
}

func (s *ssh) Command(ctx context.Context, script string) (string, int, error) {
	if s.h.Sudo() {
		script = "sudo -n /bin/sh -c " + shQuote(script)
	}
	var out bytes.Buffer
	code, err := s.exec(ctx, script+" 2>&1", nil, &out)
	if err != nil && code > 0 && code != 255 {
		err = nil // the command ran and failed; that is the caller's to judge
	}
	return out.String(), code, err
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// lineWriter calls fn for every complete line written to it.
type lineWriter struct {
	buf bytes.Buffer
	fn  func([]byte)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		i := bytes.IndexByte(w.buf.Bytes(), '\n')
		if i < 0 {
			return len(p), nil
		}
		line := append([]byte(nil), w.buf.Next(i + 1)[:i]...)
		if len(bytes.TrimSpace(line)) > 0 {
			w.fn(line)
		}
	}
}

func (w *lineWriter) flush() {
	if rest := bytes.TrimSpace(w.buf.Bytes()); len(rest) > 0 {
		w.fn(rest)
	}
	w.buf.Reset()
}
