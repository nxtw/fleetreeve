package cli

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/nxtw/fleetreeve/internal/compile"
	"github.com/nxtw/fleetreeve/internal/core"
	"github.com/nxtw/fleetreeve/internal/engine"
	"github.com/nxtw/fleetreeve/internal/facts"
	"github.com/nxtw/fleetreeve/internal/pull"
	"github.com/nxtw/fleetreeve/internal/remote"
	"github.com/nxtw/fleetreeve/internal/state"
)

// serve runs the pull-mode server.
func serve(ctx context.Context, o *options) error {
	manifest, err := o.manifest()
	if err != nil {
		return err
	}
	if o.inventory == "" {
		return fmt.Errorf("serve needs an inventory (-i): only hosts listed in it may pull")
	}
	if _, err := compile.LoadInventory(o.inventory); err != nil {
		return err
	}
	if _, err := compile.StateName(manifest); err != nil {
		return err
	}
	if o.secretFile == "" {
		return fmt.Errorf("serve needs -secret-file (created on first use)")
	}
	secret, created, err := pull.LoadSecret(o.secretFile, true)
	if err != nil {
		return err
	}
	if created {
		opLog.Info("generated a new server secret", "file", o.secretFile)
	}
	if (o.tlsCert == "") != (o.tlsKey == "") {
		return fmt.Errorf("-tls-cert and -tls-key must be given together")
	}
	// The log of every run sits next to the reports unless told otherwise.
	if o.historyDir == "" && o.reports != "" {
		o.historyDir = filepath.Join(filepath.Dir(filepath.Clean(o.reports)), "history")
	}
	// Optional: HashiCorp Vault as the certificate authority for hosts.
	var vault *pull.Vault
	if o.vaultAddr == "" {
		o.vaultAddr = os.Getenv("VAULT_ADDR")
	}
	if o.vaultAddr != "" {
		token := os.Getenv("VAULT_TOKEN")
		if o.vaultTokenFile != "" {
			b, err := os.ReadFile(o.vaultTokenFile)
			if err != nil {
				return err
			}
			token = strings.TrimSpace(string(b))
		}
		if o.vaultCA == "" {
			o.vaultCA = os.Getenv("VAULT_CACERT")
		}
		if vault, err = pull.NewVault(o.vaultAddr, token, o.vaultPKI, o.vaultRole, os.Getenv("VAULT_NAMESPACE"), o.certTTL, o.vaultCA); err != nil {
			return err
		}
		if o.tlsCert == "" {
			return fmt.Errorf("certificates for hosts need the server itself to use TLS: give -tls-cert and -tls-key")
		}
		if err := vault.Check(ctx); err != nil {
			if strings.Contains(err.Error(), "token is not accepted") {
				err = fmt.Errorf("%v\n       A token runs out when the server has not renewed it for its period, as when this machine was off for long. With Vault deployed on this machine, `sudo systemctl restart fleetreeve-vault` makes a new one; otherwise `fleetreeve vault setup` does (see docs/certificates.md)", err)
			}
			return err
		}
		if o.certsDir == "" && o.reports != "" {
			o.certsDir = filepath.Join(filepath.Dir(filepath.Clean(o.reports)), "certs")
		}
		if o.certsDir == "" {
			return fmt.Errorf("certificates for hosts need a directory to keep track of them: -certs <dir> (or -reports, next to which it is put)")
		}
	} else if o.requireCert {
		return fmt.Errorf("-require-cert needs a certificate authority: -vault-addr")
	}
	packages := o.packages
	if packages == "" {
		if st, err := os.Stat(hostPackages); err == nil && st.IsDir() {
			packages = hostPackages
		}
	}
	s := &pull.Server{Packages: packages, CA: vault, Certs: o.certsDir, RequireCert: o.requireCert, History: o.historyDir, HistoryDays: o.historyDays, Manifest: manifest, Inventory: o.inventory, Extra: o.extra, Prune: !o.noPrune, Secret: secret, Reports: o.reports, Hosts: o.hostsDir, Actions: o.actionsDir, Version: Version, Log: opLog}
	ln, err := net.Listen("tcp", o.listen)
	if err != nil {
		return err
	}
	// The HTTP library's own messages (failed TLS handshakes and the like)
	// go through the same logger, so the log stays in one format.
	srv := &http.Server{ErrorLog: slog.NewLogLogger(opLog.Handler(), slog.LevelWarn), Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: time.Minute, WriteTimeout: 2 * time.Minute}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	if vault != nil {
		srv.TLSConfig = &tls.Config{}
		if err := s.ClientAuth(ctx, srv.TLSConfig); err != nil {
			return err
		}
		opLog.Info("hosts get certificates from Vault", "vault", o.vaultAddr, "pki", vault.Mount, "role", vault.Role, "required", o.requireCert)
		// A periodic token lives as long as it is renewed.
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Hour):
					if err := vault.RenewSelf(ctx); err != nil && ctx.Err() == nil {
						opLog.Warn("renewing the Vault token failed", "error", err.Error())
					}
				}
			}
		}()
	}
	if o.tlsCert != "" {
		opLog.Info("serving", "manifest", manifest, "url", "https://"+ln.Addr().String())
		err = srv.ServeTLS(ln, o.tlsCert, o.tlsKey)
	} else {
		opLog.Info("serving", "manifest", manifest, "url", "http://"+ln.Addr().String())
		opLog.Warn("no -tls-cert/-tls-key: node tokens and catalogs are sent in the clear; only do this behind a TLS proxy or on localhost")
		err = srv.Serve(ln)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// token prints the pull tokens of hosts.
func token(o *options) error {
	if o.api != "" {
		if o.api != "read" && o.api != "operator" && o.api != "exec" && o.api != "admin" {
			return fmt.Errorf("-api must be read, operator, exec or admin")
		}
		if o.secretFile == "" {
			return fmt.Errorf("usage: fleetreeve token -secret-file <file> -api read|admin")
		}
		secret, _, err := pull.LoadSecret(o.secretFile, false)
		if err != nil {
			return err
		}
		fmt.Println(pull.APIToken(secret, o.api))
		return nil
	}
	if o.secretFile == "" || (len(o.args) == 0) == (o.ttl == 0) {
		return fmt.Errorf("usage: fleetreeve token -secret-file <file> <host>...     (a node's token)\n       fleetreeve token -secret-file <file> -ttl 24h [-server <url>]   (a join token)\n       fleetreeve token -secret-file <file> -api read|admin            (a token for the server's API)")
	}
	secret, _, err := pull.LoadSecret(o.secretFile, false)
	if err != nil {
		return err
	}
	if o.ttl != 0 {
		tok := pull.JoinToken(secret, time.Now().Add(o.ttl))
		if o.server == "" {
			fmt.Println(tok)
			return nil
		}
		server := strings.TrimRight(o.server, "/")
		fmt.Printf("# Valid for %s. Run on each new node (as root):\ncurl -fsSL %s/install.sh | sh -s -- -server %s -join %s\n", o.ttl, server, server, tok)
		return nil
	}
	for _, h := range o.args {
		if len(o.args) == 1 {
			fmt.Println(pull.Token(secret, h))
		} else {
			fmt.Printf("%s %s\n", h, pull.Token(secret, h))
		}
	}
	return nil
}

// pullCmd is the node side of pull mode: fetch this node's catalog, apply
// it, report back; once, or repeatedly with -every.
func pullCmd(ctx context.Context, o *options) (int, error) {
	// Flags win; the environment supplies defaults. The packaged systemd
	// unit configures the client this way, through /etc/fleetreeve/pull.conf.
	env := func(dst *string, name string) {
		if *dst == "" {
			*dst = os.Getenv(name)
		}
	}
	// Run by hand on an enrolled node (`sudo fleetreeve pull -plan`), with no
	// server given: use the node's own settings, for a single run. The
	// service's interval is deliberately not taken over.
	// The same when a server is known only from a settings file (the
	// directory happens to have a .fleetreeve.yaml for other commands) but no
	// token came with it: the node's own settings belong together, so
	// they are used as a whole.
	noToken := o.tokenFile == "" && os.Getenv("FLEETREEVE_TOKEN") == "" && os.Getenv("FLEETREEVE_TOKEN_FILE") == ""
	if (o.server == "" || noToken) && os.Getenv("FLEETREEVE_SERVER") == "" {
		conf, err := readEnvFile(nodeConfPath())
		switch {
		case err == nil:
			for key, dst := range map[string]*string{"FLEETREEVE_SERVER": &o.server, "FLEETREEVE_HOST": &o.host, "FLEETREEVE_CA": &o.ca, "FLEETREEVE_TOKEN_FILE": &o.tokenFile} {
				if *dst == "" || (noToken && conf[key] != "") {
					*dst = conf[key]
				}
			}
			o.insecure = o.insecure || conf["FLEETREEVE_INSECURE"] == "1"
		case os.IsPermission(err):
			return 1, fmt.Errorf("%s is readable by root only: run this with sudo", nodeConf)
		}
	}
	env(&o.server, "FLEETREEVE_SERVER")
	env(&o.host, "FLEETREEVE_HOST")
	env(&o.ca, "FLEETREEVE_CA")
	env(&o.tokenFile, "FLEETREEVE_TOKEN_FILE")
	if v := os.Getenv("FLEETREEVE_EVERY"); v != "" && o.every == 0 {
		d, err := time.ParseDuration(v)
		if err != nil {
			return 1, fmt.Errorf("FLEETREEVE_EVERY: %v", err)
		}
		o.every = d
	}
	if v := os.Getenv("FLEETREEVE_INSECURE"); v == "1" || v == "true" {
		o.insecure = true
	}
	if o.server == "" {
		return 1, fmt.Errorf("pull needs -server <url> (on an enrolled node it is read from %s)", nodeConf)
	}
	tok := os.Getenv("FLEETREEVE_TOKEN")
	if o.tokenFile != "" {
		b, err := os.ReadFile(o.tokenFile)
		if err != nil {
			return 1, err
		}
		tok = strings.TrimSpace(string(b))
	}
	if tok == "" {
		return 1, fmt.Errorf("pull needs the node token: -token-file <file> or $FLEETREEVE_TOKEN")
	}
	host := o.host
	if host == "" {
		host, _ = os.Hostname()
		host = strings.SplitN(host, ".", 2)[0]
	}
	client, err := pull.NewClient(o.server, host, tok, o.ca, o.insecure)
	if err != nil {
		return 1, err
	}
	nodeCertDir = filepath.Dir(o.tokenFile)
	if o.tokenFile == "" {
		nodeCertDir = enrollDir
	}
	// Before anything else is asked of the server: the host's certificate,
	// if the server works with them.
	if err := ensureNodeCert(ctx, client, host, nodeCertDir); err != nil {
		notef(slog.LevelWarn, "%v", err)
	}
	// How often to ask the server for requests (a run or command asked
	// for in the console) between scheduled runs. A cheap call; it is what
	// makes "run now" start within about a minute instead of at the next
	// check-in.
	poll := time.Minute
	switch v := o.poll; {
	case v == "":
		v = os.Getenv("FLEETREEVE_POLL")
		fallthrough
	default:
		if v == "off" {
			poll = 0
		} else if v != "" {
			if poll, err = time.ParseDuration(v); err != nil || poll < 5*time.Second {
				return 1, fmt.Errorf("the poll interval must be a duration of at least 5s, or off (got %q)", v)
			}
		}
	}
	if o.every == 0 {
		poll = 0
	}

	// serve collects the requests waiting for this node and carries out
	// those that are not runs: rollbacks, releases and commands. It
	// returns their results (already sent to the server) and the runs
	// that were asked for.
	serve := func() (done []pull.ActionResult, runs []pull.Action) {
		actions, err := client.Claim(ctx)
		if err != nil {
			// An unreachable server, or one that refuses this node, is
			// reported by the run itself; saying it twice helps nobody.
			if !errors.Is(err, pull.ErrUnavailable) && !strings.Contains(err.Error(), "server refused") {
				notef(slog.LevelWarn, "could not collect requests from the server: %v", err)
			}
			return nil, nil
		}
		done, runs = pull.Perform(ctx, host, actions)
		for _, a := range done {
			level := slog.LevelInfo
			if a.Status == "failed" {
				level = slog.LevelError
			}
			what := a.Kind + " of " + a.Resource
			if a.Kind == "command" {
				what = fmt.Sprintf("command %q (exit %d)", a.Command, a.Exit)
			}
			notef(level, "%s requested by %s: %s %s", what, orUnknown(a.RequestedBy), a.Status, a.Detail)
		}
		if err := client.PostResults(ctx, done); err != nil {
			notef(slog.LevelWarn, "could not report on requests carried out: %v", err)
		}
		return done, runs
	}
	// run does one configuration run and answers the requests it settles.
	run := func(plan bool, pre []pull.ActionResult, asked []pull.Action) (int, error) {
		if o.selfUpdate {
			if err := selfUpdate(ctx, client); err != nil {
				notef(slog.LevelWarn, "self-update: %v", err)
			}
		}
		// A host with fleetreeve from a package follows its server too.
		if err := packageUpdate(ctx, client); err != nil {
			notef(slog.LevelWarn, "upgrading the package: %v", err)
		}
		rep, code, err := pullOnce(ctx, o, client, host, plan, int(poll.Seconds()), pre)
		var answers []pull.ActionResult
		for _, a := range asked {
			res := pull.ActionResult{Action: a, Status: "done", Done: time.Now().UTC().Format(time.RFC3339)}
			switch {
			case err != nil:
				res.Status, res.Detail = "failed", err.Error()
			case rep.Failed():
				res.Status, res.Detail = "failed", summarize(rep.Summary, plan)
			default:
				res.Detail = summarize(rep.Summary, plan)
			}
			notef(slog.LevelInfo, "%s requested by %s: %s", map[bool]string{true: "dry run", false: "run"}[plan], orUnknown(a.RequestedBy), res.Status)
			answers = append(answers, res)
		}
		if perr := client.PostResults(ctx, answers); perr != nil {
			notef(slog.LevelWarn, "could not report on requests carried out: %v", perr)
		}
		return code, err
	}
	// split sorts requested runs by kind.
	split := func(runs []pull.Action) (applies, plans []pull.Action) {
		for _, a := range runs {
			if a.Kind == "plan" {
				plans = append(plans, a)
			} else {
				applies = append(applies, a)
			}
		}
		return
	}

	for {
		// The scheduled run. Requests waiting are dealt with first, so a
		// hold from a rollback already applies to it. A dry run by hand
		// (`fleetreeve pull -plan`) leaves them for the real thing.
		var done []pull.ActionResult
		var runs []pull.Action
		if !(o.plan && o.every == 0) {
			done, runs = serve()
		}
		applies, plans := split(runs)
		var pre []pull.ActionResult
		for _, d := range done {
			if d.Kind == "rollback" || d.Kind == "release" {
				pre = append(pre, d)
			}
		}
		asked := applies
		if o.plan {
			asked = plans
		}
		code, err := run(o.plan, pre, asked)
		if o.plan && len(applies) > 0 {
			run(false, nil, applies)
		} else if !o.plan && len(plans) > 0 {
			run(true, nil, plans)
		}
		if o.every == 0 {
			return code, err
		}
		if err != nil {
			notef(slog.LevelError, "%v", err) // keep enforcing; the next run may succeed
		}
		// Up to 10% splay, so a fleet does not check in all at once.
		next := time.Now().Add(o.every + time.Duration(rand.Int64N(int64(o.every)/10+1)))
		// Until then, look for requests now and again.
		for time.Now().Before(next) {
			wait := time.Until(next)
			if poll > 0 && poll < wait {
				wait = poll
			}
			select {
			case <-ctx.Done():
				return code, nil
			case <-time.After(wait):
			}
			if poll == 0 || !time.Now().Before(next) {
				break
			}
			done, runs := serve()
			applies, plans := split(runs)
			var pre []pull.ActionResult
			for _, d := range done {
				if d.Kind == "rollback" || d.Kind == "release" {
					pre = append(pre, d)
				}
			}
			switch {
			case len(applies) > 0:
				run(false, pre, applies)
			case len(pre) > 0:
				run(false, pre, nil) // so the server sees the hold and what can now be restored
			}
			if len(plans) > 0 {
				run(true, nil, plans)
			}
		}
	}
}

// pullOnce does one run: fetch the catalog, apply (or plan) it, report.
// pre are requests already carried out that belong in this report.
// nodeCertDir is where this host keeps its certificate and key: next to
// its token.
var nodeCertDir = enrollDir

// nodeCertFiles are the host's certificate and private key.
func nodeCertFiles(dir string) (cert, key string) {
	cert, key = filepath.Join(dir, "node.crt"), filepath.Join(dir, "node.key")
	if v := os.Getenv("FLEETREEVE_CERT"); v != "" {
		cert = v
	}
	if v := os.Getenv("FLEETREEVE_KEY"); v != "" {
		key = v
	}
	return cert, key
}

// ensureNodeCert sees to it that the host has a certificate when its
// server hands them out, and that it is not about to expire: it is
// renewed, with a new key, once two thirds of its life are over. The
// private key is made here and never leaves the host.
func ensureNodeCert(ctx context.Context, client *pull.Client, host, dir string) error {
	certFile, keyFile := nodeCertFiles(dir)
	have, err := tls.LoadX509KeyPair(certFile, keyFile)
	usable := false
	if err == nil {
		if leaf, perr := x509.ParseCertificate(have.Certificate[0]); perr == nil {
			life := leaf.NotAfter.Sub(leaf.NotBefore)
			usable = time.Now().Before(leaf.NotAfter)
			if usable {
				client.WithCert(certFile, keyFile)
			}
			if time.Now().Before(leaf.NotBefore.Add(life * 2 / 3)) {
				return nil // good for a while yet
			}
		}
	}
	health, herr := client.Health(ctx)
	if herr != nil || health["certificates"] == nil {
		return nil // unreachable right now, or it hands out none
	}
	key, csr, err := pull.NewKeyAndCSR(host)
	if err != nil {
		return err
	}
	certPEM, err := client.Cert(ctx, csr)
	if err != nil {
		return fmt.Errorf("getting a certificate: %v", err)
	}
	// The pair is replaced key first; nothing reads it in between.
	for _, f := range []struct {
		path string
		data []byte
		mode os.FileMode
	}{{keyFile, key, 0o600}, {certFile, []byte(certPEM), 0o644}} {
		if err := os.WriteFile(f.path+".new", f.data, f.mode); err != nil {
			return err
		}
		if err := os.Rename(f.path+".new", f.path); err != nil {
			return err
		}
	}
	client.WithCert(certFile, keyFile)
	what := "obtained"
	if usable {
		what = "renewed"
	}
	notef(slog.LevelInfo, "certificate %s for %s (%s)", what, host, certFile)
	return nil
}

func pullOnce(ctx context.Context, o *options, client *pull.Client, host string, plan bool, pollSeconds int, pre []pull.ActionResult) (*pull.Report, int, error) {
	if err := ensureNodeCert(ctx, client, host, nodeCertDir); err != nil {
		notef(slog.LevelWarn, "%v", err)
	}
	rep := &pull.Report{Host: host, Mode: "apply", Version: Version, Interval: int(o.every.Seconds()), Poll: pollSeconds, Actions: pre}
	if plan {
		rep.Mode = "plan"
	}
	// The last catalog is kept so the node keeps enforcing its
	// configuration while the server is unreachable.
	cache := filepath.Join(state.Dir(), state.Key("pull-catalog", host)+".json")
	client.CertRefused() // only what the next request says counts
	cat, err := client.Catalog(ctx, facts.Collect())
	if client.CertRefused() {
		// The server no longer counts this host's certificate (it was
		// revoked, or the host was enrolled again): drop it and ask anew.
		certFile, keyFile := nodeCertFiles(nodeCertDir)
		os.Remove(certFile)
		os.Remove(keyFile)
		if cerr := ensureNodeCert(ctx, client, host, nodeCertDir); cerr != nil {
			notef(slog.LevelWarn, "%v", cerr)
		} else if err != nil {
			cat, err = client.Catalog(ctx, facts.Collect())
		}
	}
	switch {
	case err == nil:
		if b, merr := json.Marshal(cat); merr == nil && os.MkdirAll(filepath.Dir(cache), 0o700) == nil {
			os.WriteFile(cache, b, 0o600)
		}
	case errors.Is(err, pull.ErrUnavailable):
		b, rerr := os.ReadFile(cache)
		if rerr != nil {
			return rep, 1, fmt.Errorf("%v (and no cached catalog to fall back on)", err)
		}
		cat = &core.Catalog{}
		if jerr := json.Unmarshal(b, cat); jerr != nil {
			return rep, 1, fmt.Errorf("%v (and the cached catalog is unreadable: %v)", err, jerr)
		}
		st, _ := os.Stat(cache)
		notef(slog.LevelWarn, "%v", err)
		notef(slog.LevelWarn, "enforcing the cached catalog from %s", st.ModTime().Format(time.RFC3339))
		rep.Cached = true
	default:
		return rep, 1, err
	}

	rep.Manifest, rep.Source = cat.Name, "pull"
	out := newUI(o).host(&compile.Host{Name: host, Address: host}, false)
	// The pull client sends its own, fuller, report below.
	sum, _, err := remote.New(&compile.Host{Connection: "local"}).Run(ctx, cat, remote.RunOptions{Plan: plan, NoReport: true}, func(r engine.Result) {
		out.result(r)
		// Diffs can contain configuration secrets; they stay on the node.
		for i := range r.Changes {
			r.Changes[i].Diff = ""
		}
		rep.Results = append(rep.Results, r)
	})
	rep.Summary = sum
	if err != nil {
		rep.Error = err.Error()
	}
	out.done(&hostReport{Host: host, Error: rep.Error, Summary: sum}, plan)
	rep.Restorable = pull.Restorable(ctx, cat.Name, host)
	if rerr := client.Report(ctx, rep); rerr != nil {
		notef(slog.LevelWarn, "could not send report: %v", rerr)
	}
	switch {
	case rep.Failed():
		return rep, 1, nil
	case plan && sum.Drift():
		return rep, 2, nil
	}
	return rep, 0, nil
}

func orUnknown(s string) string {
	if s == "" {
		return "an operator"
	}
	return s
}

// nodeConf holds an enrolled node's pull settings (written by enroll and
// install; read by the fleetreeve-pull service as its environment).
const nodeConf = "/etc/fleetreeve/pull.conf"

// nodeConfPath is nodeConf, or what $FLEETREEVE_NODE_CONF says ("none": this
// machine is to be treated as not enrolled).
func nodeConfPath() string {
	if p := os.Getenv("FLEETREEVE_NODE_CONF"); p != "" {
		return p
	}
	return nodeConf
}

func init() { remote.ReportFunc = reportRun }

// reportRun tells the server this node is enrolled with about a run that
// did not come from it: `fleetreeve apply` or `fleetreeve plan` from a controller,
// or by hand on the node. It uses the node's own settings and token, so a
// controller needs neither the server's address nor its secret. It
// returns nil on a node that is not enrolled.
func reportRun(ctx context.Context, cat *core.Catalog, plan bool, sum engine.Summary, results []engine.Result, runErr error) *remote.Reported {
	if cat.Name == "" || cat.Name == enrollState || nodeConfPath() == "none" {
		return nil
	}
	conf, err := readEnvFile(nodeConfPath())
	if os.IsNotExist(err) || (err == nil && (conf["FLEETREEVE_SERVER"] == "" || conf["FLEETREEVE_HOST"] == "")) {
		return nil // not enrolled
	}
	out := &remote.Reported{Server: conf["FLEETREEVE_SERVER"]}
	if err != nil {
		// Enrolled, but its settings are root's: the run was not root's.
		out.Server, out.Error = "its server", fmt.Sprintf("%s is readable by root only (the run was not done as root)", nodeConfPath())
		return out
	}
	token, err := os.ReadFile(conf["FLEETREEVE_TOKEN_FILE"])
	if err != nil {
		out.Error = err.Error()
		return out
	}
	client, err := pull.NewClient(conf["FLEETREEVE_SERVER"], conf["FLEETREEVE_HOST"], strings.TrimSpace(string(token)), conf["FLEETREEVE_CA"], conf["FLEETREEVE_INSECURE"] == "1")
	if err != nil {
		out.Error = err.Error()
		return out
	}
	// A host that has a certificate shows it.
	if certFile, keyFile := nodeCertFiles(filepath.Dir(conf["FLEETREEVE_TOKEN_FILE"])); isRegular(certFile) && isRegular(keyFile) {
		client.WithCert(certFile, keyFile)
	}
	rep := &pull.Report{Host: conf["FLEETREEVE_HOST"], Manifest: cat.Name, Source: "push", Mode: "apply", Version: Version, Summary: sum}
	if plan {
		rep.Mode = "plan"
	}
	if runErr != nil {
		rep.Error = runErr.Error()
	}
	for _, r := range results {
		// Diffs can contain configuration secrets; they stay on the node.
		r.Changes = append([]core.Change(nil), r.Changes...)
		for i := range r.Changes {
			r.Changes[i].Diff = ""
		}
		rep.Results = append(rep.Results, r)
	}
	if err := client.Report(ctx, rep); err != nil {
		out.Error = err.Error()
	}
	return out
}

// readEnvFile parses KEY=VALUE lines as systemd reads an EnvironmentFile.
func readEnvFile(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		key, val, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(val[1 : len(val)-1])
		}
		out[strings.TrimSpace(key)] = val
	}
	return out, nil
}

// selfUpdate replaces the running binary with the server's build for this
// platform when they differ, then restarts in place. This keeps installed
// clients on the server's version without anyone visiting the nodes. It
// grants the server nothing new: it already decides what runs on the node.
func selfUpdate(ctx context.Context, client *pull.Client) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return err
	}
	cur, err := os.ReadFile(self)
	if err != nil {
		return err
	}
	platform := runtime.GOOS + "-" + runtime.GOARCH
	want, err := client.AgentSHA(ctx, platform)
	// FLEETREEVE_UPDATED_TO stops a loop should an update not take effect.
	if err != nil || want == fmt.Sprintf("%x", sha256.Sum256(cur)) || want == os.Getenv("FLEETREEVE_UPDATED_TO") {
		return err
	}
	data, err := client.Agent(ctx, platform)
	if err != nil {
		return err
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != want {
		return fmt.Errorf("downloaded build does not match its checksum")
	}
	tmp := self + ".update"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, self); err != nil {
		os.Remove(tmp)
		return err
	}
	notef(slog.LevelInfo, "updated to the server's build (%s), restarting", want[:12])
	os.Setenv("FLEETREEVE_UPDATED_TO", want)
	return syscall.Exec(self, os.Args, os.Environ())
}

// status shows the last run of every host: from a reports directory on
// the server itself (-reports), or from anywhere through the server's API
// (-server with -secret-file, or $FLEETREEVE_API_TOKEN).
func status(ctx context.Context, o *options) (int, error) {
	var st *pull.StatusResponse
	switch {
	case o.reports != "":
		reports, err := pull.LoadReports(o.reports)
		if err != nil {
			return 1, err
		}
		var expected []string
		if o.inventory != "" {
			inv, hosts, err := o.hosts()
			if err != nil {
				return 1, err
			}
			// Hosts registered by `fleetreeve enroll` count as expected, too.
			if o.hostsDir != "" && o.limit == "" {
				if err := pull.MergeHosts(inv, o.hostsDir); err != nil {
					return 1, err
				}
				hosts = inv.Hosts
			}
			for _, h := range hosts {
				expected = append(expected, h.Name)
			}
		}
		st = pull.Status(reports, expected, time.Now())
	case o.server != "":
		token := os.Getenv("FLEETREEVE_API_TOKEN")
		if token == "" && o.secretFile != "" {
			secret, _, err := pull.LoadSecret(o.secretFile, false)
			if err != nil {
				return 1, err
			}
			token = pull.APIToken(secret, "read")
		}
		if token == "" {
			return 1, fmt.Errorf("status -server needs an API token: $FLEETREEVE_API_TOKEN, or -secret-file to derive it")
		}
		client, err := pull.NewClient(o.server, "", "", o.ca, o.insecure)
		if err != nil {
			return 1, err
		}
		if st, err = client.Status(ctx, token); err != nil {
			return 1, err
		}
	default:
		return 1, fmt.Errorf("usage: fleetreeve status -reports <dir> [-i inventory] [-hosts dir]   (on the server)\n       fleetreeve status -server <url> -secret-file <file> [-ca file]        (from anywhere)")
	}

	ui := newUI(o)
	width, mwidth := 4, 1
	for _, e := range st.Reports {
		width = max(width, len(e.Host))
		mwidth = max(mwidth, len(e.Manifest)+len(e.Source)+3)
	}
	for _, m := range st.Missing {
		width = max(width, len(m))
	}
	colour := map[string]string{"ok": green, "changed": green, "drift": yellow, "failed": red, "error": red, "stale": red}
	code := 0
	for _, e := range st.Reports {
		if !e.Healthy() {
			code = 1
		}
		detail := e.Error
		if detail == "" {
			detail = summarize(e.Summary, e.Mode == "plan")
		}
		if e.Cached {
			detail += "  [cached catalog]"
		}
		// A node that missed two check-ins is not "ok", whatever its last
		// report said: it may be down, or unable to reach this server.
		if e.State == "stale" {
			detail = fmt.Sprintf("no report for more than twice its %s interval; last: %s", time.Duration(e.Interval)*time.Second, detail)
		}
		what := e.Manifest
		if what == "" {
			what = "-"
		}
		if e.Source != "" {
			what += " (" + e.Source + ")"
		}
		age := (time.Duration(e.AgeSeconds) * time.Second).String() + " ago"
		fmt.Printf("%-*s  %-*s  %s  %-14s  %s\n", width, e.Host, mwidth, what, ui.c(colour[e.State], fmt.Sprintf("%-7s", e.State)), age, detail)
	}
	for _, m := range st.Missing {
		fmt.Printf("%-*s  %s  never reported\n", width, m, ui.c(yellow, "unknown"))
	}
	return code, nil
}
