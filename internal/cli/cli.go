// Package cli implements the fleetreeve command line.
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nxtw/fleetreeve/internal/compile"
	"github.com/nxtw/fleetreeve/internal/core"
	"github.com/nxtw/fleetreeve/internal/engine"
	"github.com/nxtw/fleetreeve/internal/facts"
	"github.com/nxtw/fleetreeve/internal/pull"
	"github.com/nxtw/fleetreeve/internal/remote"
	"github.com/nxtw/fleetreeve/internal/state"
	_ "github.com/nxtw/fleetreeve/internal/types"
)

// Version can be set at build time: -ldflags "-X fleetreeve/internal/cli.Version=1.2.3".
var Version = "0.1.0"

const usage = `fleetreeve - declarative, agentless configuration management

Usage:
  fleetreeve plan     [options] <manifest>   Show what would change (exit 2 if anything would)
  fleetreeve apply    [options] <manifest>   Converge hosts onto the manifest
  fleetreeve validate [options] <manifest>   Compile and check without contacting any host
  fleetreeve graph    [options] <manifest>   Print the dependency graph in DOT format
  fleetreeve lookup   [options] <manifest> [variable...]
                                        Show each host's variables and where they come from
  fleetreeve facts    [options]              Print the facts of each host as JSON
  fleetreeve state    [options] <manifest>   Show what fleetreeve manages on each host
  fleetreeve rollback [options] <manifest> [resource...]
                                        Restore resources to how they were before fleetreeve last
                                        changed them, and hold them there; without a resource,
                                        list what can be restored. -release ends the hold,
                                        -plan only shows what would be restored
  fleetreeve state rm [options] <manifest> <resource>...
                                        Stop tracking resources, leaving them in place
  fleetreeve run      [options] -- <cmd>     Run an ad-hoc shell command on each host
  fleetreeve types    [type]                 List resource types, or describe one
  fleetreeve module   add|install|verify|list|init
                                        Extend fleetreeve with modules: roles and resource types

Pull mode (optional; nodes fetch their own catalog instead of being pushed to):
  fleetreeve serve  -i <inv> -secret-file <f> [options] <manifest>
                                        Serve catalogs to nodes and collect their reports
  fleetreeve token  -secret-file <f> <host>...   Print the token a node needs to pull
  fleetreeve token  -secret-file <f> -ttl <dur>  Print a join token for provisioning new nodes
  fleetreeve install -server <url> (-join <token> | -token-file <f>) [options]
                                        On a node, as root: install fleetreeve and the pull service
  fleetreeve enroll [host...] -i <inv> -server <url> -secret-file <f> [options]
                                        Over SSH: install fleetreeve, the node's token and a
                                        systemd service that pulls
  fleetreeve unenroll [host...] [options]    Undo that: stop the service, remove the node's token and
                                        certificate, withdraw it from the server (enroll -remove)
  fleetreeve pull   -server <url> -token-file <f> [options]
                                        On a node: fetch its catalog, apply, report
  fleetreeve status -reports <dir> [-i <inv>]    Last report of every node (on the server)
  fleetreeve setup [-yes] [-container] [-vault-deploy]
                                            Set up a fleetreeve server on this machine by answering a few
                                            questions (with sudo; -yes takes every default; -container
                                            runs it in podman containers, -vault-deploy adds Vault)
  fleetreeve upgrade [-version <v>] [-from <dir>] [-with-hosts] [-plan] [-yes]
                                            Upgrade fleetreeve on this machine to the newest release (with
                                            sudo): fetch it, verify its checksums, install it the way this
                                            one was installed, and on a server restart the server and the
                                            console on it. -with-hosts upgrades the hosts too; -from takes
                                            the release from a directory instead of from GitHub
  fleetreeve vault setup|check|ca|certs|revoke   HashiCorp Vault as certificate authority for hosts:
                                            prepare Vault, check it, list and revoke certificates
  fleetreeve vault deploy|unseal                 Run Vault on this machine, in a container (deploy: with sudo)
  fleetreeve vault webcert                       Certificates from Vault for the web console and Vault's own
                                            interface, renewed by a daily timer (with sudo)
  fleetreeve identity init|show|key              The server's own user and SSH key pair, so that
                                            nothing depends on personal accounts (init: with sudo)
  fleetreeve status -server <url> -secret-file <f>   The same, from anywhere, through the API
  fleetreeve token  -secret-file <f> -api read|operator|exec|admin   Print a token for the server's HTTP API
  fleetreeve dashboard -server <url> [options]   Web console for a fleetreeve server;
                                        -demo shows generated data instead
  fleetreeve user add|role|rm|list [name] -users <file> [-role viewer|operator|admin]
                                        Accounts for the web console
  fleetreeve version

Options:
  -i <file>      Inventory file (default: manage this machine)
  -l <names>     Limit to these hosts or groups (comma-separated)
  -e key=value   Set a variable, overriding everything else (repeatable)
  -forks <n>     Hosts to work on in parallel (default 10)
  -report <file> Write a JSON report of the run
  -run-as <user> Run commands that connect to hosts as this user of this
                 machine (through sudo) - the server's own identity, see
                 'fleetreeve identity'. Normally set once in .fleetreeve.yaml
  -bootstrap-user <login>  enroll: first log in to the host as this existing
                 account, once, to create the run-as user there with the
                 server's key and sudo rights
  -no-host-key-check  Connect over SSH without verifying host keys (also per host
                 or for all in the inventory: host_key_check: false). Gives up
                 SSH's protection against connecting to the wrong machine
  -no-report     plan/apply: enrolled hosts report every run to their server by
                 themselves; this keeps the run to yourself
  -report-to <url>  plan/apply: have this controller send each host's report to
                 a fleetreeve server instead, also for hosts that are not enrolled
                 (needs -secret-file; -ca for a private CA)
  -every <dur>   apply: keep enforcing at this interval, e.g. 30m
  -no-prune      Keep resources that were removed from the manifest
  -v             Also list resources that are already in the desired state
  -no-diff       Do not print content diffs
  -no-color      Plain output
  -facts <file>  validate, graph, lookup: use the facts in this file (saved with
                 'fleetreeve facts') instead of this machine's
  -log-format <f>  text or json (default: json for serve, text otherwise;
                 $FLEETREEVE_LOG_FORMAT overrides the default)

Pull mode options:
  serve:  -listen <addr> (default :8140)  -tls-cert <file> -tls-key <file>
          -reports <dir>  -secret-file <file>  (plus -i, -e, -no-prune)
          -history <dir> (default: next to -reports)  -history-days <n> (default 90; 0 keeps everything)
          -vault-addr <url> -vault-token-file <file> [-vault-pki pki] [-vault-role fleetreeve]
                         use HashiCorp Vault as certificate authority for hosts: each host
                         gets a certificate signed by it and shows that instead of its token
                         ($VAULT_ADDR, $VAULT_TOKEN, $VAULT_CACERT, $VAULT_NAMESPACE also work;
                         -vault-ca <file> verifies Vault itself; -cert-ttl 72h; -certs <dir>)
          -require-cert  a host's token gets it nothing but its first certificate
          -hosts <dir>   accept hosts registered by 'fleetreeve enroll', kept here
          -actions <dir> accept rollback requests for nodes (from the console), queued here
  enroll: -server <url>  -secret-file <file>  -ca <file>  -every <dur> (default 30m)
          -plan  -remove  -insecure  -skip-check  (plus -i, -l, -forks)
          -new-cert      the host was rebuilt or lost its key: have it get a new certificate
          -no-register   do not add the host to the server's host registry
          -rpm <file|dir>  install the fleetreeve RPM on the node first (a directory
          such as dist/ supplies the right architecture per node)
          (enroll and install first check that the node can reach the server;
          -skip-check turns that off, e.g. when the server is not up yet)
  install: -server <url>  -join <token> | -token-file <file>  -host <name>
          -ca <file>  -every <dur> (default 30m)  -plan  -remove  -insecure
  pull:   -self-update   replace this binary when the server offers another build
  pull:   -server <url>  -token-file <file> (or $FLEETREEVE_TOKEN)  -host <name>
          -ca <file>  -every <dur>  -plan  -insecure  (plus -v, -no-diff)
          -poll <dur>|off   with -every: how often to ask the server for requests
          between runs, such as "run now" from the console (default 1m)

Defaults for any option can be kept in a settings file: .fleetreeve.yaml in the
project directory, or ~/.config/fleetreeve/config.yaml. See the README.

Exit status: 0 success, 1 failure, 2 plan found changes.
`

type options struct {
	inventory, limit, report string
	extra                    map[string]any
	forks                    int
	every                    time.Duration
	verbose, noDiff, noColor bool
	noPrune                  bool
	logFormat                string

	// pull mode
	listen, secretFile, tlsCert, tlsKey, reports string
	server, host, tokenFile, ca                  string
	plan, insecure, remove                       bool
	selfUpdate                                   bool
	skipCheck                                    bool
	noHostKeyCheck                               bool
	runAs, bootstrapUser                         string
	yes                                          bool
	vaultAddr, vaultTokenFile, vaultPKI          string
	vaultRole, vaultCA, certTTL, certsDir        string
	requireCert, newCert                         bool
	domains                                      string
	vaultImage, webNames                         string
	vaultDeploy, container                       bool
	rpm                                          string
	reportTo                                     string
	noReport                                     bool
	hostsDir                                     string
	actionsDir                                   string
	historyDir                                   string
	historyDays                                  int
	noRegister                                   bool
	api                                          string
	demo                                         bool
	users                                        string
	role                                         string
	poll                                         string
	modVersion                                   string
	modPath                                      string
	offline                                      bool
	factsFile                                    string
	release                                      bool
	config                                       string // settings file in use, if any
	join                                         string
	from                                         string // upgrade: a directory with a release
	withHosts                                    bool   // upgrade: the hosts as well
	ttl                                          time.Duration
	args                                         []string
}

type extraFlag struct{ m map[string]any }

func (e extraFlag) String() string { return "" }
func (e extraFlag) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("expected key=value")
	}
	// Values are parsed as JSON when possible, so -e port=8080 is a number
	// and -e 'pkgs=["a","b"]' a list; anything else is a string.
	var parsed any
	if json.Unmarshal([]byte(v), &parsed) == nil {
		e.m[k] = parsed
	} else {
		e.m[k] = v
	}
	return nil
}

func parse(cmd string, args []string) (*options, error) {
	o := &options{extra: map[string]any{}}
	fs := flag.NewFlagSet("fleetreeve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.inventory, "i", "", "")
	fs.StringVar(&o.limit, "l", "", "")
	fs.StringVar(&o.report, "report", "", "")
	fs.Var(extraFlag{o.extra}, "e", "")
	fs.IntVar(&o.forks, "forks", 10, "")
	fs.DurationVar(&o.every, "every", 0, "")
	fs.BoolVar(&o.verbose, "v", false, "")
	fs.BoolVar(&o.noDiff, "no-diff", false, "")
	fs.BoolVar(&o.noColor, "no-color", false, "")
	fs.BoolVar(&o.noPrune, "no-prune", false, "")
	fs.StringVar(&o.logFormat, "log-format", "", "")
	fs.StringVar(&o.listen, "listen", ":8140", "")
	fs.StringVar(&o.secretFile, "secret-file", "", "")
	fs.StringVar(&o.tlsCert, "tls-cert", "", "")
	fs.StringVar(&o.tlsKey, "tls-key", "", "")
	fs.StringVar(&o.reports, "reports", "", "")
	fs.StringVar(&o.server, "server", "", "")
	fs.StringVar(&o.host, "host", "", "")
	fs.StringVar(&o.tokenFile, "token-file", "", "")
	fs.StringVar(&o.ca, "ca", "", "")
	fs.BoolVar(&o.plan, "plan", false, "")
	fs.BoolVar(&o.insecure, "insecure", false, "")
	fs.BoolVar(&o.remove, "remove", false, "")
	fs.BoolVar(&o.skipCheck, "skip-check", false, "")
	fs.BoolVar(&o.noHostKeyCheck, "no-host-key-check", false, "")
	fs.StringVar(&o.runAs, "run-as", "", "")
	fs.BoolVar(&o.yes, "yes", false, "")
	fs.StringVar(&o.vaultAddr, "vault-addr", "", "")
	fs.StringVar(&o.vaultTokenFile, "vault-token-file", "", "")
	fs.StringVar(&o.vaultPKI, "vault-pki", "", "")
	fs.StringVar(&o.vaultRole, "vault-role", "", "")
	fs.StringVar(&o.vaultCA, "vault-ca", "", "")
	fs.StringVar(&o.certTTL, "cert-ttl", "", "")
	fs.StringVar(&o.certsDir, "certs", "", "")
	fs.BoolVar(&o.requireCert, "require-cert", false, "")
	fs.BoolVar(&o.newCert, "new-cert", false, "")
	fs.StringVar(&o.domains, "domains", "", "")
	fs.StringVar(&o.vaultImage, "vault-image", "", "")
	fs.StringVar(&o.webNames, "web-names", "", "")
	fs.BoolVar(&o.vaultDeploy, "vault-deploy", false, "")
	fs.BoolVar(&o.container, "container", false, "")
	fs.StringVar(&o.bootstrapUser, "bootstrap-user", "", "")
	fs.StringVar(&o.rpm, "rpm", "", "")
	fs.StringVar(&o.reportTo, "report-to", "", "")
	fs.BoolVar(&o.noReport, "no-report", false, "")
	fs.StringVar(&o.hostsDir, "hosts", "", "")
	fs.StringVar(&o.actionsDir, "actions", "", "")
	fs.StringVar(&o.historyDir, "history", "", "")
	fs.IntVar(&o.historyDays, "history-days", 90, "")
	fs.BoolVar(&o.noRegister, "no-register", false, "")
	fs.StringVar(&o.api, "api", "", "")
	fs.BoolVar(&o.demo, "demo", false, "")
	fs.StringVar(&o.users, "users", "", "")
	fs.StringVar(&o.role, "role", "", "")
	fs.StringVar(&o.poll, "poll", "", "")
	fs.StringVar(&o.modVersion, "version", "", "")
	fs.StringVar(&o.modPath, "path", "", "")
	fs.BoolVar(&o.offline, "offline", false, "")
	fs.StringVar(&o.factsFile, "facts", "", "")
	fs.BoolVar(&o.release, "release", false, "")
	fs.BoolVar(&o.selfUpdate, "self-update", false, "")
	fs.StringVar(&o.from, "from", "", "")
	fs.BoolVar(&o.withHosts, "with-hosts", false, "")
	fs.StringVar(&o.join, "join", "", "")
	fs.DurationVar(&o.ttl, "ttl", 0, "")
	// Defaults from the settings file come first; flags override them.
	var err error
	if o.config, err = applyConfig(fs, cmd); err != nil {
		return nil, err
	}
	// Everything after "--" is positional; before it, flags and positional
	// arguments may be mixed freely.
	var tail []string
	for i, a := range args {
		if a == "--" {
			args, tail = args[:i], args[i+1:]
			break
		}
	}
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) > 0 {
			o.args, args = append(o.args, args[0]), args[1:]
		}
	}
	o.args = append(o.args, tail...)
	if o.forks < 1 {
		o.forks = 1
	}
	return o, nil
}

// Main runs the command line and returns the process exit status.
// unalias rewrites commands that are another command with an option:
// `unenroll web1` is `enroll -remove web1`. Everything that applies to the
// real command (its section in .fleetreeve.yaml, run-as) then applies.
func unalias(args []string) []string {
	if len(args) > 0 && args[0] == "unenroll" {
		return append([]string{"enroll", "-remove"}, args[1:]...)
	}
	return args
}

func Main(args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(usage)
		return 0
	}
	args = unalias(args)
	cmd := args[0]
	if cmd == "agent" {
		return agent(args[1:])
	}
	o, err := parse(cmd, args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleetreeve: %v\n\n%s", err, usage)
		return 1
	}
	if o.logFormat == "" {
		o.logFormat = os.Getenv("FLEETREEVE_LOG_FORMAT")
	}
	if o.logFormat == "" {
		o.logFormat = "text"
		if cmd == "serve" || cmd == "dashboard" {
			o.logFormat = "json"
		}
	}
	if err := setLogFormat(o.logFormat); err != nil {
		fmt.Fprintf(os.Stderr, "fleetreeve: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// With an identity of its own configured (run-as), the command is
	// handed over to that user.
	if code, done := switchUser(ctx, cmd, o, args[1:]); done {
		return code
	}
	code := 0
	switch cmd {
	case "plan", "apply":
		code, err = converge(ctx, o, cmd == "plan")
	case "validate":
		err = validate(o)
	case "graph":
		err = graph(o)
	case "lookup":
		code, err = lookup(ctx, o)
	case "rollback":
		code, err = rollback(ctx, o)
	case "facts":
		err = showFacts(ctx, o)
	case "run":
		code, err = adhoc(ctx, o)
	case "state":
		code, err = showState(ctx, o)
	case "serve":
		err = serve(ctx, o)
	case "token":
		err = token(o)
	case "pull":
		code, err = pullCmd(ctx, o)
	case "enroll":
		code, err = enroll(ctx, o)
	case "install":
		code, err = install(ctx, o)
	case "status":
		code, err = status(ctx, o)
	case "dashboard":
		err = dashboardCmd(ctx, o)
	case "user":
		err = user(o)
	case "module":
		err = module(o)
	case "identity":
		err = identity(o)
	case "setup":
		err = setup(ctx, o)
	case "upgrade":
		err = upgrade(ctx, o)
	case "vault":
		err = vaultCmd(ctx, o)
	case "types":
		err = describeTypes(o)
	case "version":
		fmt.Println("fleetreeve", Version)
	default:
		err = fmt.Errorf("unknown command %q%s", cmd, core.Suggest(cmd, []string{"plan", "apply", "validate", "graph", "lookup", "rollback", "facts", "state", "run", "types", "serve", "token", "enroll", "unenroll", "install", "pull", "status", "dashboard", "user", "module", "identity", "setup", "upgrade", "vault", "version"}))
	}
	if err != nil {
		notef(slog.LevelError, "%v", err)
		return 1
	}
	return code
}

// agent is the node side of a remote run. It is started over SSH by the
// controller and speaks JSON on stdin/stdout.
func agent(args []string) int {
	enc := json.NewEncoder(os.Stdout)
	if len(args) > 0 && args[0] == "facts" {
		enc.Encode(facts.Collect())
		return 0
	}
	if len(args) == 1 && args[0] == "rollback" {
		var req engine.RollbackRequest
		resp := map[string]any{}
		if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
			resp["error"] = "reading rollback request: " + err.Error()
		} else if results, err := engine.Rollback(core.NewCtx(context.Background(), nil, false), req); err != nil {
			resp["error"] = err.Error()
		} else {
			resp["results"] = results
		}
		enc.Encode(resp)
		return 0
	}
	if len(args) == 1 && args[0] == "probe" {
		var req remote.ProbeRequest
		resp := map[string]any{}
		if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
			resp["error"] = "reading probe request: " + err.Error()
		} else if res, err := probeServer(context.Background(), req); err != nil {
			resp["error"] = err.Error()
		} else if res.CertRefused {
			resp["cert_refused"] = true
		}
		enc.Encode(resp)
		return 0
	}
	if len(args) == 2 && args[0] == "state" {
		var forget []string
		json.NewDecoder(os.Stdin).Decode(&forget) // empty stdin: just show
		f, err := remote.EditState(args[1], forget)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		enc.Encode(f)
		return 0
	}
	if len(args) == 0 || args[0] != "run" {
		fmt.Fprintln(os.Stderr, "fleetreeve agent is started by the controller; see `fleetreeve help`")
		return 1
	}
	opts := remote.RunOptions{}
	for _, a := range args[1:] {
		opts.Plan = opts.Plan || a == "--plan"
		opts.NoReport = opts.NoReport || a == "--no-report"
	}
	var cat core.Catalog
	if err := json.NewDecoder(os.Stdin).Decode(&cat); err != nil {
		enc.Encode(remote.Message{Error: "reading catalog: " + err.Error()})
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	sum, reported, err := remote.RunAndReport(ctx, &cat, opts, func(r engine.Result) {
		enc.Encode(remote.Message{Result: &r})
	})
	if reported != nil {
		enc.Encode(remote.Message{Reported: reported})
	}
	if err != nil {
		enc.Encode(remote.Message{Error: err.Error()})
		return 1
	}
	enc.Encode(remote.Message{Summary: &sum})
	return 0
}

func (o *options) hosts() (*compile.Inventory, []*compile.Host, error) {
	inv := compile.Localhost()
	if o.inventory != "" {
		var err error
		if inv, err = compile.LoadInventory(o.inventory); err != nil {
			return nil, nil, err
		}
	}
	hosts, err := inv.Select(o.limit)
	if err == nil && len(hosts) == 0 {
		err = fmt.Errorf("no hosts selected")
	}
	if o.noHostKeyCheck {
		off := false
		for _, h := range hosts {
			h.HostKeyCheck = &off
		}
	}
	return inv, hosts, err
}

// standInFacts returns the facts used by commands that do not contact
// hosts (validate, graph, lookup -offline): those of a facts file given
// with -facts - typically saved from a real host with `fleetreeve facts` - or
// else this machine's. They matter wherever facts steer the result: `when`
// conditions and data chosen per OS family.
func (o *options) standInFacts() (map[string]any, error) {
	if o.factsFile == "" {
		return facts.Collect(), nil
	}
	b, err := os.ReadFile(o.factsFile)
	if err != nil {
		return nil, err
	}
	var f map[string]any
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %v", o.factsFile, err)
	}
	// `fleetreeve facts -i inventory` prints {host: facts}; accept one host's worth.
	if _, direct := f["os"]; !direct && len(f) == 1 {
		for _, v := range f {
			if m, ok := v.(map[string]any); ok {
				f = m
			}
		}
	}
	if _, ok := f["os"]; !ok {
		return nil, fmt.Errorf("%s does not look like the output of `fleetreeve facts`", o.factsFile)
	}
	return f, nil
}

func (o *options) manifest() (string, error) {
	if len(o.args) != 1 {
		return "", fmt.Errorf("expected exactly one manifest file")
	}
	return o.args[0], nil
}

// each runs fn for every host, at most forks at a time.
func each(hosts []*compile.Host, forks int, fn func(i int, h *compile.Host)) {
	sem := make(chan struct{}, forks)
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			fn(i, h)
		}()
	}
	wg.Wait()
}

// hostReport is the outcome of one host in a plan or apply.
type hostReport struct {
	Host    string          `json:"host"`
	Error   string          `json:"error,omitempty"`
	Summary engine.Summary  `json:"summary"`
	Results []engine.Result `json:"results"`
	// Reported: what became of the node's own report to its server.
	Reported *remote.Reported `json:"reported,omitempty"`
}

func converge(ctx context.Context, o *options, plan bool) (int, error) {
	manifest, err := o.manifest()
	if err != nil {
		return 1, err
	}
	inv, hosts, err := o.hosts()
	if err != nil {
		return 1, err
	}
	for {
		code, err := convergeOnce(ctx, o, plan, manifest, hosts, func(h *compile.Host, f map[string]any) (*core.Catalog, error) {
			return compile.Compile(compile.Input{Manifest: manifest, Host: h, Inventory: inv, Facts: f, Extra: o.extra, Prune: !o.noPrune})
		})
		if err != nil || o.every == 0 || plan {
			return code, err
		}
		select {
		case <-ctx.Done():
			return code, nil
		case <-time.After(o.every):
		}
	}
}

// convergeOnce gathers facts on every host, builds its catalog with build,
// and plans or applies it.
func convergeOnce(ctx context.Context, o *options, plan bool, manifest string, hosts []*compile.Host, build func(*compile.Host, map[string]any) (*core.Catalog, error)) (int, error) {
	started := time.Now()
	ui := newUI(o)
	reports := make([]hostReport, len(hosts))
	each(hosts, o.forks, func(i int, h *compile.Host) {
		// With several hosts, each host's output is printed as one block.
		out := ui.host(h, len(hosts) > 1)
		rep := &reports[i]
		rep.Host = h.Name
		err := func() error {
			ex := remote.New(h)
			f, err := ex.Facts(ctx)
			if err != nil {
				return err
			}
			cat, err := build(h, f)
			if err != nil {
				return err
			}
			// An enrolled node reports the run to its server itself -
			// unless this controller does (-report-to), or was told not
			// to, or the run is the enrolment.
			opts := remote.RunOptions{Plan: plan, NoReport: o.noReport || o.reportTo != "" || manifest == "enroll" || manifest == "install"}
			rep.Summary, rep.Reported, err = ex.Run(ctx, cat, opts, func(r engine.Result) {
				rep.Results = append(rep.Results, r)
				out.result(r)
			})
			return err
		}()
		if err != nil {
			rep.Error = err.Error()
		}
		out.done(rep, plan)
	})

	code := 0
	for _, r := range reports {
		switch {
		case r.Error != "" || r.Summary.Failed > 0:
			code = 1
		case plan && r.Summary.Drift() && code == 0:
			code = 2
		}
	}
	if len(hosts) > 1 {
		ui.recap(reports, plan)
	}
	reportedBy(reports)
	if o.reportTo != "" && !o.noReport && manifest != "enroll" && manifest != "install" {
		if err := sendReports(ctx, o, manifest, plan, reports); err != nil {
			// The run itself is done; failing to report it must not
			// change its outcome, but it must not go unnoticed either.
			notef(slog.LevelWarn, "reporting to %s: %v", o.reportTo, err)
		}
	}
	if o.report != "" {
		mode := "apply"
		if plan {
			mode = "plan"
		}
		b, _ := json.MarshalIndent(map[string]any{
			"version": Version, "mode": mode, "manifest": manifest,
			"started": started.UTC().Format(time.RFC3339), "seconds": time.Since(started).Seconds(), "hosts": reports,
		}, "", "  ")
		if err := os.WriteFile(o.report, append(b, '\n'), 0o644); err != nil {
			return 1, err
		}
	}
	return code, nil
}

// reportedBy says where the nodes reported their runs, and which could not.
func reportedBy(reports []hostReport) {
	servers, order := map[string]int{}, []string{}
	for _, r := range reports {
		switch {
		case r.Reported == nil:
		case r.Reported.Error != "":
			notef(slog.LevelWarn, "%s could not report this run to %s: %s", r.Host, r.Reported.Server, r.Reported.Error)
		default:
			if servers[r.Reported.Server] == 0 {
				order = append(order, r.Reported.Server)
			}
			servers[r.Reported.Server]++
		}
	}
	for _, s := range order {
		if jsonLogs {
			opLog.Info("reports sent", "server", s, "hosts", servers[s], "by", "the nodes")
		} else {
			fmt.Printf("Reported by %d host(s) to %s\n", servers[s], s)
		}
	}
}

// sendReports posts the outcome of a push run to a fleetreeve server, one
// report per host, in the same form the pull client uses. The controller
// authenticates as each host with a token derived from the server secret.
func sendReports(ctx context.Context, o *options, manifest string, plan bool, reports []hostReport) error {
	if o.secretFile == "" {
		return fmt.Errorf("-report-to needs -secret-file (the server's secret)")
	}
	secret, _, err := pull.LoadSecret(o.secretFile, false)
	if err != nil {
		return err
	}
	name, err := compile.StateName(manifest)
	if err != nil {
		return err
	}
	mode := "apply"
	if plan {
		mode = "plan"
	}
	sent, failed := 0, []string{}
	for _, r := range reports {
		client, err := pull.NewClient(o.reportTo, r.Host, pull.Token(secret, r.Host), o.ca, o.insecure)
		if err != nil {
			return err
		}
		rep := &pull.Report{Host: r.Host, Manifest: name, Source: "push", Mode: mode, Version: Version, Error: r.Error, Summary: r.Summary}
		for _, res := range r.Results {
			// Diffs can contain configuration secrets; they are not sent.
			res.Changes = append([]core.Change(nil), res.Changes...)
			for i := range res.Changes {
				res.Changes[i].Diff = ""
			}
			rep.Results = append(rep.Results, res)
		}
		if err := client.Report(ctx, rep); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", r.Host, err))
			continue
		}
		sent++
	}
	if jsonLogs {
		opLog.Info("reports sent", "server", o.reportTo, "hosts", sent)
	} else if sent > 0 {
		fmt.Printf("Reported %d host(s) to %s\n", sent, o.reportTo)
	}
	if len(failed) > 0 {
		return fmt.Errorf("%s", strings.Join(failed, "; "))
	}
	return nil
}

// validate compiles the catalog of every selected host without contacting
// it, using this machine's facts as a stand-in.
func validate(o *options) error {
	manifest, err := o.manifest()
	if err != nil {
		return err
	}
	inv, hosts, err := o.hosts()
	if err != nil {
		return err
	}
	f, err := o.standInFacts()
	if err != nil {
		return err
	}
	for _, h := range hosts {
		cat, err := compile.Compile(compile.Input{Manifest: manifest, Host: h, Inventory: inv, Facts: f, Extra: o.extra})
		if err != nil {
			return fmt.Errorf("%s: %v", h.Name, err)
		}
		fmt.Printf("%s: ok, %d resources\n", h.Name, len(cat.Resources))
	}
	return nil
}

func graph(o *options) error {
	manifest, err := o.manifest()
	if err != nil {
		return err
	}
	inv, hosts, err := o.hosts()
	if err != nil {
		return err
	}
	f, err := o.standInFacts()
	if err != nil {
		return err
	}
	cat, err := compile.Compile(compile.Input{Manifest: manifest, Host: hosts[0], Inventory: inv, Facts: f, Extra: o.extra})
	if err != nil {
		return err
	}
	g, err := cat.Graph()
	if err != nil {
		return err
	}
	fmt.Printf("digraph %q {\n  rankdir=LR;\n  node [shape=box, fontname=\"monospace\"];\n", hosts[0].Name)
	for _, r := range g.Order {
		fmt.Printf("  %q;\n", r.ID())
	}
	style := map[string]string{core.EdgeRequire: "", core.EdgeNotify: ` [style=dashed, label="notify"]`, core.EdgeAuto: ` [color=gray, label="auto"]`}
	for _, e := range g.Edges {
		fmt.Printf("  %q -> %q%s;\n", e.From, e.To, style[e.Kind])
	}
	fmt.Println("}")
	return nil
}

func showFacts(ctx context.Context, o *options) error {
	_, hosts, err := o.hosts()
	if err != nil {
		return err
	}
	all := make([]any, len(hosts))
	each(hosts, o.forks, func(i int, h *compile.Host) {
		f, err := remote.New(h).Facts(ctx)
		if err != nil {
			all[i] = map[string]any{"error": err.Error()}
		} else {
			all[i] = f
		}
	})
	var out any = all[0]
	if o.inventory != "" {
		m := map[string]any{}
		for i, h := range hosts {
			m[h.Name] = all[i]
		}
		out = m
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	return nil
}

// showState prints, or with "rm" edits, the state nodes keep for a manifest.
func showState(ctx context.Context, o *options) (int, error) {
	var forget []string
	if len(o.args) > 0 && o.args[0] == "rm" {
		if len(o.args) < 3 {
			return 1, fmt.Errorf("usage: fleetreeve state rm [options] <manifest> <resource>...")
		}
		for _, ref := range o.args[2:] {
			id, err := core.ParseRef(ref)
			if err != nil {
				return 1, err
			}
			forget = append(forget, id)
		}
		o.args = o.args[1:2]
	}
	manifest, err := o.manifest()
	if err != nil {
		return 1, err
	}
	name, err := compile.StateName(manifest)
	if err != nil {
		return 1, err
	}
	_, hosts, err := o.hosts()
	if err != nil {
		return 1, err
	}
	ui := newUI(o)
	var mu sync.Mutex
	code := 0
	each(hosts, o.forks, func(_ int, h *compile.Host) {
		f, err := remote.New(h).State(ctx, state.Key(name, h.Name), forget)
		mu.Lock()
		defer mu.Unlock()
		fmt.Println(ui.c(bold, "▸ "+h.Name))
		if err != nil {
			fmt.Printf("  %s %v\n", ui.c(red, "✘ error:"), err)
			code = 1
			return
		}
		ids := make([]string, 0, len(f.Resources))
		for id := range f.Resources {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(a, b int) bool { return f.Resources[ids[a]].Order < f.Resources[ids[b]].Order })
		for _, id := range ids {
			e := f.Resources[id]
			origin := ui.c(dim, "tracked")
			if e.Created {
				origin = ui.c(green, "created")
			}
			changed := ""
			if e.Changed != "" {
				changed = ui.c(dim, "  last changed "+e.Changed)
			}
			fmt.Printf("  %s  %s%s\n", origin, id, changed)
		}
		updated := "never applied"
		if f.Serial > 0 {
			updated = fmt.Sprintf("serial %d, updated %s", f.Serial, f.Updated)
		}
		fmt.Printf("  %s\n", ui.c(dim, fmt.Sprintf("state %q: %d resources, %s", name, len(ids), updated)))
	})
	return code, nil
}

func adhoc(ctx context.Context, o *options) (int, error) {
	if len(o.args) == 0 {
		return 1, fmt.Errorf("usage: fleetreeve run [options] -- <command>")
	}
	_, hosts, err := o.hosts()
	if err != nil {
		return 1, err
	}
	script := strings.Join(o.args, " ")
	ui := newUI(o)
	var mu sync.Mutex
	code := 0
	each(hosts, o.forks, func(_ int, h *compile.Host) {
		out, rc, err := remote.New(h).Command(ctx, script)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case err != nil:
			fmt.Printf("%s %s\n", ui.c(red, "✘ "+h.Name), err)
			code = 1
		case rc != 0:
			fmt.Printf("%s (exit %d)\n", ui.c(red, "✘ "+h.Name), rc)
			code = 1
		default:
			fmt.Println(ui.c(green, "✔ "+h.Name))
		}
		if out = strings.TrimRight(out, "\n"); out != "" {
			fmt.Println("  " + strings.ReplaceAll(out, "\n", "\n  "))
		}
	})
	return code, nil
}

func describeTypes(o *options) error {
	// Types from the modules installed for the project in this directory.
	mods, err := compile.LoadModules(compile.ModulePath("site.yaml"))
	if err != nil {
		return err
	}
	fromModules := map[string]*compile.ModuleType{}
	var moduleNames []string
	for _, m := range mods {
		for name, t := range m.Types {
			fromModules[name] = t
			moduleNames = append(moduleNames, name)
		}
	}
	sort.Strings(moduleNames)
	if len(o.args) == 0 {
		for _, n := range core.TypeNames() {
			t, _ := core.Lookup(n)
			fmt.Printf("%-9s %s\n", n, t.Doc())
		}
		if len(moduleNames) > 0 {
			fmt.Println("\nFrom modules:")
			for _, n := range moduleNames {
				fmt.Printf("%-28s %s\n", n, fromModules[n].Doc)
			}
		}
		fmt.Println("\nEvery resource also accepts: require, before, notify, subscribe, when, each, rollback.")
		return nil
	}
	if mt, ok := fromModules[o.args[0]]; ok {
		made := "a script that runs on the host"
		if mt.Kind() == "composite" {
			made = "other resources"
		}
		fmt.Printf("%s\n  %s\n  From a module; made of %s. Defined in %s\n\nParameters:\n", mt.Name, mt.Doc, made, mt.File)
		for _, n := range core.ParamNames(mt.Schema) {
			a := mt.Schema[n]
			meta := a.Kind.String()
			if a.Default != nil {
				meta += fmt.Sprintf(", default %v", a.Default)
			}
			if a.Required {
				meta += ", required"
			}
			fmt.Printf("  %-12s (%s)\n", n, meta)
			if a.Doc != "" {
				fmt.Printf("      %s\n", a.Doc)
			}
		}
		return nil
	}
	t, ok := core.Lookup(o.args[0])
	if !ok {
		return fmt.Errorf("unknown resource type %q%s", o.args[0], core.Suggest(o.args[0], append(core.TypeNames(), moduleNames...)))
	}
	fmt.Printf("%s\n  %s\n\nAttributes:\n", t.Name(), t.Doc())
	schema := t.Schema()
	names := make([]string, 0, len(schema))
	for n, a := range schema {
		if !a.Internal {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		a := schema[n]
		meta := a.Kind.String()
		if len(a.Enum) > 0 {
			meta = strings.Join(a.Enum, "|")
		}
		if a.Default != nil {
			meta += fmt.Sprintf(", default %v", a.Default)
		}
		if a.Required {
			meta += ", required"
		}
		fmt.Printf("  %-12s (%s)\n", n, meta)
		if a.Doc != "" {
			fmt.Printf("      %s\n", a.Doc)
		}
	}
	return nil
}
