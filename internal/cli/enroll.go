package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nxtw/fleetreeve/internal/compile"
	"github.com/nxtw/fleetreeve/internal/core"
	"github.com/nxtw/fleetreeve/internal/facts"
	"github.com/nxtw/fleetreeve/internal/pull"
	"github.com/nxtw/fleetreeve/internal/remote"
)

// Where enrolment puts things on a node.
const (
	enrollBin   = "/usr/local/bin/fleetreeve"
	enrollDir   = "/etc/fleetreeve"
	enrollToken = enrollDir + "/token"
	enrollCA    = enrollDir + "/ca.pem"
	enrollUnit  = "/etc/systemd/system/fleetreeve-pull.service"
	enrollSvc   = "fleetreeve-pull"
	enrollState = "fleetreeve-pull" // state name, separate from any manifest's
	enrollConf  = enrollDir + "/pull.conf"
	enrollRPM   = "/var/cache/fleetreeve/fleetreeve.rpm" // the package enroll -rpm installed
)

// findRPM picks the fleetreeve client package for a node's architecture. path
// is an .rpm file, or a directory such as dist/ that holds one per
// architecture; the server package is never picked.
func findRPM(path, goarch string) (string, error) {
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[goarch]
	if arch == "" {
		return "", fmt.Errorf("no RPM architecture for %s", goarch)
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		if !strings.HasSuffix(path, "."+arch+".rpm") {
			return "", fmt.Errorf("%s is not a package for this node's architecture (%s); pass the directory holding all of them instead", filepath.Base(path), arch)
		}
		return path, nil
	}
	matches, _ := filepath.Glob(filepath.Join(path, "fleetreeve-[0-9]*."+arch+".rpm"))
	if len(matches) == 0 {
		return "", fmt.Errorf("no fleetreeve-<version>.%s.rpm in %s (build it with `make rpm`)", arch, path)
	}
	// Several versions side by side: take the most recently built.
	sort.Slice(matches, func(a, b int) bool {
		sa, _ := os.Stat(matches[a])
		sb, _ := os.Stat(matches[b])
		return sa.ModTime().After(sb.ModTime())
	})
	return matches[0], nil
}

// isPackaged reports whether the node has fleetreeve installed from a system
// package. The package then owns the binary and the service unit, and
// updates arrive through the package manager: enrolment only configures
// and starts the service.
func isPackaged(f map[string]any) bool {
	c, _ := f["fleetreeve"].(map[string]any)
	p, _ := c["packaged"].(bool)
	return p
}

// enrollSpec is what a node needs to start pulling.
type enrollSpec struct {
	Server, Host, Token string
	CA                  []byte // PEM, optional
	Binary              []byte // fleetreeve for the node's platform
	Every               time.Duration
	Insecure            bool
	NewCert             bool   // drop the node's certificate and key, so it gets new ones
	Packaged            bool   // fleetreeve comes from a system package on this node
	RPM                 []byte // package to install first (implies Packaged)
}

// enroll sets nodes up for pull mode over SSH. It is an ordinary fleetreeve run
// of a built-in catalog, so it is idempotent, supports -plan, and
// re-running it upgrades the binary or rotates the token.
func enroll(ctx context.Context, o *options) (int, error) {
	if o.inventory == "" {
		return 1, fmt.Errorf("enroll needs an inventory (-i) of hosts reachable over SSH")
	}
	// Hosts may simply be named: `fleetreeve enroll web1 web2`.
	if len(o.args) > 0 {
		if o.limit != "" {
			return 1, fmt.Errorf("name the hosts either as arguments or with -l, not both")
		}
		o.limit = strings.Join(o.args, ",")
	}
	_, hosts, err := o.hosts()
	if err != nil {
		return 1, err
	}
	build := func(h *compile.Host, f map[string]any) (*core.Catalog, error) {
		// Taking a node out of pull mode also withdraws its registration,
		// which revokes its access (unless the server's inventory lists it).
		if o.server != "" && o.secretFile != "" && !o.noRegister && !o.plan {
			secret, _, err := pull.LoadSecret(o.secretFile, false)
			if err != nil {
				return nil, err
			}
			if c, err := pull.NewClient(o.server, h.Name, "", o.ca, o.insecure); err == nil {
				if err := c.UnregisterHost(ctx, pull.AdminToken(secret), h.Name); err != nil {
					notef(slog.LevelWarn, "%s: could not remove the host from the server's registry: %v", h.Name, err)
				}
			}
		}
		return unenrollCatalog(isPackaged(f)), nil
	}
	if !o.remove {
		if o.server == "" || o.secretFile == "" {
			return 1, fmt.Errorf("usage: fleetreeve enroll [host...] -i <inventory> -server <url> -secret-file <file> [-ca file] [-rpm file|dir] [-every 30m] [-plan]\n" +
				"       (put the settings that never change in .fleetreeve.yaml and this becomes: fleetreeve enroll <host>)")
		}
		if o.config != "" && !jsonLogs {
			fmt.Printf("Using %s: server %s\n", o.config, o.server)
		}
		// Validates the URL with the rules the node will apply.
		if _, err := pull.NewClient(o.server, "", "", o.ca, o.insecure); err != nil {
			return 1, err
		}
		secret, _, err := pull.LoadSecret(o.secretFile, false)
		if err != nil {
			return 1, err
		}
		var ca []byte
		if o.ca != "" {
			if ca, err = os.ReadFile(o.ca); err != nil {
				return 1, err
			}
		}
		every := o.every
		if every == 0 {
			every = 30 * time.Minute
		}
		build = func(h *compile.Host, f map[string]any) (*core.Catalog, error) {
			if err := enrollable(f); err != nil {
				return nil, err
			}
			// A node pointed at an address it cannot reach keeps running
			// from its cached catalog and silently stops reporting, so
			// refuse before changing anything.
			if !o.skipCheck {
				if err := remote.New(h).Probe(ctx, remote.ProbeRequest{Server: o.server, CA: ca, Insecure: o.insecure}); err != nil {
					return nil, fmt.Errorf("this node cannot reach the server at %s, so nothing was changed: %v (check the address; -skip-check enrols anyway)", o.server, err)
				}
			}
			// Make the host known to the server, with its groups and its
			// own variables from this inventory, so nobody has to edit
			// the server's inventory by hand. Done before the node is
			// touched: a node enrolled against a server that refuses it
			// would only fail on every pull.
			if !o.noRegister && !o.plan {
				if err := register(ctx, o, h, secret); err != nil {
					return nil, err
				}
			}
			spec := enrollSpec{
				Server: o.server, Host: h.Name, Token: pull.Token(secret, h.Name),
				CA: ca, Every: every, Insecure: o.insecure, Packaged: isPackaged(f), NewCert: o.newCert,
			}
			if o.rpm != "" {
				if pm := fmt.Sprint(f["package_manager"]); pm != "dnf" && pm != "yum" && pm != "zypper" {
					return nil, fmt.Errorf("-rpm needs an RPM-based host, but this one uses %q", pm)
				}
				file, err := findRPM(o.rpm, fmt.Sprint(f["arch"]))
				if err != nil {
					return nil, err
				}
				if spec.RPM, err = os.ReadFile(file); err != nil {
					return nil, err
				}
				spec.Packaged = true
			}
			if !spec.Packaged {
				kernel, _ := f["kernel"].(map[string]any)
				bin, err := remote.FindAgent(fmt.Sprint(kernel["name"]), fmt.Sprint(f["arch"]))
				if err != nil {
					return nil, err
				}
				if spec.Binary, err = os.ReadFile(bin); err != nil {
					return nil, err
				}
			}
			return enrollCatalog(spec), nil
		}
	}
	o.every = 0 // the interval is for the node's service, not for this run
	return convergeOnce(ctx, o, o.plan, "enroll", hosts, func(h *compile.Host, f map[string]any) (*core.Catalog, error) {
		cat, err := build(h, f)
		if err == nil {
			cat.Host, cat.Facts = h.Name, f
		}
		return cat, err
	})
}

// install is enrolment run on the node itself, for nodes the controller
// cannot reach over SSH: it installs this binary, the node's token and the
// pull service. The token is given directly or obtained with a join token.
func install(ctx context.Context, o *options) (int, error) {
	f := facts.Collect()
	host := o.host
	if host == "" {
		host = fmt.Sprint(f["hostname"])
	}
	cat := unenrollCatalog(isPackaged(f))
	if !o.remove {
		if o.server == "" {
			return 1, fmt.Errorf("usage: fleetreeve install -server <url> (-join <token> | -token-file <file>) [-host name] [-ca file] [-every 30m] [-plan]")
		}
		if err := enrollable(f); err != nil {
			return 1, err
		}
		client, err := pull.NewClient(o.server, host, "", o.ca, o.insecure)
		if err != nil {
			return 1, err
		}
		if !o.skipCheck {
			if err := client.Ping(ctx); err != nil {
				return 1, fmt.Errorf("cannot reach the server at %s, so nothing was changed: %v (check the address; -skip-check installs anyway)", o.server, err)
			}
		}
		tok := os.Getenv("FLEETREEVE_TOKEN")
		switch {
		case o.tokenFile != "":
			b, err := os.ReadFile(o.tokenFile)
			if err != nil {
				return 1, err
			}
			tok = strings.TrimSpace(string(b))
		case o.join != "":
			if tok, err = client.Join(ctx, o.join); err != nil {
				return 1, fmt.Errorf("joining as %q: %v", host, err)
			}
		}
		if tok == "" {
			return 1, fmt.Errorf("install needs -join <token>, -token-file <file> or $FLEETREEVE_TOKEN")
		}
		spec := enrollSpec{Server: o.server, Host: host, Token: tok, Every: o.every, Insecure: o.insecure, Packaged: isPackaged(f)}
		if spec.Every == 0 {
			spec.Every = 30 * time.Minute
		}
		if !spec.Packaged {
			self, err := os.Executable()
			if err != nil {
				return 1, err
			}
			if spec.Binary, err = os.ReadFile(self); err != nil {
				return 1, err
			}
		}
		if o.ca != "" {
			if spec.CA, err = os.ReadFile(o.ca); err != nil {
				return 1, err
			}
		}
		cat = enrollCatalog(spec)
	}
	cat.Host, cat.Facts = host, f
	h := &compile.Host{Name: host, Address: host, Connection: "local"}
	o.every = 0
	return convergeOnce(ctx, o, o.plan, "install", []*compile.Host{h}, func(*compile.Host, map[string]any) (*core.Catalog, error) {
		return cat, nil
	})
}

// register adds a host to the server's registry on behalf of enroll.
func register(ctx context.Context, o *options, h *compile.Host, secret []byte) error {
	c, err := pull.NewClient(o.server, h.Name, "", o.ca, o.insecure)
	if err != nil {
		return err
	}
	res, err := c.RegisterHost(ctx, pull.AdminToken(secret), pull.HostEntry{Name: h.Name, Groups: h.Groups, Vars: h.Vars, NewCert: o.newCert})
	switch {
	case errors.Is(err, pull.ErrNoRegistry):
		notef(slog.LevelWarn, "%s: %v; it must be in the server's inventory file", h.Name, err)
		return nil
	case err != nil:
		return fmt.Errorf("registering the host with the server failed, so nothing was changed: %v (-no-register skips this)", err)
	case res.Status == "registered":
		notef(slog.LevelInfo, "%s: registered with the server", h.Name)
		if len(res.UnknownGroups) > 0 {
			notef(slog.LevelWarn, "%s: the server's inventory does not define the group(s) %s, so the host gets no variables from them there",
				h.Name, strings.Join(res.UnknownGroups, ", "))
		}
	}
	return nil
}

func init() { remote.ProbeFunc = probeServer }

// probeServer runs on the node: can it reach the pull server it is about
// to be pointed at, with the address and CA it will be given?
func probeServer(ctx context.Context, req remote.ProbeRequest) error {
	c, err := pull.NewClientCA(req.Server, "", "", req.CA, req.Insecure)
	if err != nil {
		return err
	}
	return c.Ping(ctx)
}

// enrollable checks that a node can run the pull service.
func enrollable(f map[string]any) error {
	if init := fmt.Sprint(f["init"]); init != "systemd" {
		return fmt.Errorf("enrolment installs a systemd service, but this host runs %s; start `fleetreeve pull -every ...` there with its own service manager", init)
	}
	if u, _ := f["user"].(map[string]any); fmt.Sprint(u["uid"]) != "0" {
		return fmt.Errorf("installing the pull service needs root on the node (run as root, or set `become: true` for the host in the inventory)")
	}
	return nil
}

// certFiles are the node's certificate and key (see ensureNodeCert).
var certFiles = []string{enrollDir + "/node.crt", enrollDir + "/node.key"}

// dropCert removes the node's certificate and key, and has the service
// start over, at which it asks for new ones.
func dropCert(cat *core.Catalog) *core.Catalog {
	for _, p := range certFiles {
		cat.Resources = append(cat.Resources, &core.Resource{Type: "file", Title: p, Origin: "fleetreeve enroll", Notify: []string{"service[" + enrollSvc + "]"}, Attrs: map[string]any{"ensure": "absent"}})
	}
	return cat
}

func enrollCatalog(s enrollSpec) *core.Catalog {
	if s.NewCert {
		s.NewCert = false
		return dropCert(enrollCatalog(s))
	}
	if s.Packaged {
		return packagedCatalog(s)
	}
	args := []string{enrollBin, "pull", "-server", s.Server, "-host", s.Host, "-token-file", enrollToken}
	if len(s.CA) > 0 {
		args = append(args, "-ca", enrollCA)
	}
	if s.Insecure {
		args = append(args, "-insecure")
	}
	args = append(args, "-every", s.Every.String(), "-self-update", "-log-format", "json")
	for i, a := range args {
		// systemd: % starts a specifier; quote anything with spaces.
		a = strings.ReplaceAll(a, "%", "%%")
		if strings.ContainsAny(a, " \t\"'\\") {
			a = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(a) + `"`
		}
		args[i] = a
	}
	unit := `# Managed by fleetreeve enroll; changes are overwritten.
[Unit]
Description=fleetreeve pull: fetch and enforce this node's configuration
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=` + strings.Join(args, " ") + `
Restart=on-failure
RestartSec=60

[Install]
WantedBy=multi-user.target
`
	svc := "service[" + enrollSvc + "]"
	res := []*core.Resource{
		{Type: "file", Title: enrollBin, Notify: []string{svc}, Attrs: map[string]any{
			"content_b64": base64.StdEncoding.EncodeToString(s.Binary), "mode": "0755", "owner": "0", "group": "0", "sensitive": true}},
		{Type: "file", Title: enrollDir, Attrs: map[string]any{"ensure": "directory", "mode": "0755"}},
		{Type: "file", Title: enrollToken, Notify: []string{svc}, Attrs: map[string]any{
			"content": s.Token + "\n", "mode": "0600", "owner": "0", "group": "0", "sensitive": true}},
	}
	if len(s.CA) > 0 {
		res = append(res, &core.Resource{Type: "file", Title: enrollCA, Notify: []string{svc}, Attrs: map[string]any{
			"content": string(s.CA), "mode": "0644"}})
	} else {
		res = append(res, &core.Resource{Type: "file", Title: enrollCA, Attrs: map[string]any{"ensure": "absent"}})
	}
	res = append(res,
		&core.Resource{Type: "file", Title: enrollUnit, Notify: []string{"exec[systemctl daemon-reload]", svc}, Attrs: map[string]any{
			"content": unit, "mode": "0644"}},
		&core.Resource{Type: "exec", Title: "systemctl daemon-reload", Attrs: map[string]any{"refreshonly": true}},
		&core.Resource{Type: "service", Title: enrollSvc, Require: []string{"exec[systemctl daemon-reload]"}, Attrs: map[string]any{
			"ensure": "running", "enabled": true, "provider": "systemd"}},
	)
	return finish(res)
}

// packagedCatalog configures the service shipped by the fleetreeve package:
// the token, the CA and /etc/fleetreeve/pull.conf, which the unit reads as its
// environment. The binary and the unit belong to the package.
func packagedCatalog(s enrollSpec) *core.Catalog {
	quote := func(v string) string { // systemd EnvironmentFile quoting
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
	}
	conf := "# Written by fleetreeve (enroll/install); read by fleetreeve-pull.service.\n" +
		"FLEETREEVE_SERVER=" + quote(s.Server) + "\n" +
		"FLEETREEVE_HOST=" + quote(s.Host) + "\n" +
		"FLEETREEVE_TOKEN_FILE=" + enrollToken + "\n" +
		"FLEETREEVE_EVERY=" + s.Every.String() + "\n"
	if len(s.CA) > 0 {
		conf += "FLEETREEVE_CA=" + enrollCA + "\n"
	}
	if s.Insecure {
		conf += "FLEETREEVE_INSECURE=1\n"
	}
	svc := []string{"service[" + enrollSvc + "]"}
	reload := "exec[systemctl daemon-reload]"
	var res []*core.Resource
	needs := []string{reload} // what must be done before the service starts
	if len(s.RPM) > 0 {
		// Upload the package and install it. `rpm -U --replacepkgs
		// --oldpackage` covers a first install, an upgrade, a rebuild of
		// the same version and a downgrade alike. It runs when the package
		// is missing, and again whenever the uploaded file changes.
		install := "exec[install fleetreeve package]"
		needs = append(needs, install)
		res = append(res,
			&core.Resource{Type: "file", Title: filepath.Dir(enrollRPM), Attrs: map[string]any{"ensure": "directory", "mode": "0755"}},
			&core.Resource{Type: "file", Title: enrollRPM, Notify: []string{install}, Attrs: map[string]any{
				"content_b64": base64.StdEncoding.EncodeToString(s.RPM), "mode": "0644", "sensitive": true}},
			&core.Resource{Type: "exec", Title: "install fleetreeve package", Notify: svc, Attrs: map[string]any{
				"command": "rpm -U --replacepkgs --oldpackage " + enrollRPM, "unless": "rpm -q fleetreeve"}},
		)
	}
	res = append(res,
		// Leftovers of an earlier enrolment without the package: its unit
		// in /etc would shadow the packaged one.
		&core.Resource{Type: "file", Title: enrollUnit, Notify: []string{reload, svc[0]}, Attrs: map[string]any{"ensure": "absent"}},
		&core.Resource{Type: "file", Title: enrollBin, Attrs: map[string]any{"ensure": "absent"}},
		&core.Resource{Type: "exec", Title: "systemctl daemon-reload", Attrs: map[string]any{"refreshonly": true}},
	)
	res = append(res, []*core.Resource{
		{Type: "file", Title: enrollDir, Attrs: map[string]any{"ensure": "directory", "mode": "0755"}},
		{Type: "file", Title: enrollToken, Notify: svc, Attrs: map[string]any{
			"content": s.Token + "\n", "mode": "0600", "owner": "0", "group": "0", "sensitive": true}},
		{Type: "file", Title: enrollConf, Notify: svc, Attrs: map[string]any{"content": conf, "mode": "0600"}},
	}...)
	if len(s.CA) > 0 {
		res = append(res, &core.Resource{Type: "file", Title: enrollCA, Notify: svc, Attrs: map[string]any{
			"content": string(s.CA), "mode": "0644"}})
	} else {
		res = append(res, &core.Resource{Type: "file", Title: enrollCA, Attrs: map[string]any{"ensure": "absent"}})
	}
	res = append(res, &core.Resource{Type: "service", Title: enrollSvc, Require: needs, Attrs: map[string]any{
		"ensure": "running", "enabled": true, "provider": "systemd"}})
	return finish(res)
}

// unenrollCatalog stops the service and removes what enrolment installed.
// The node's configuration and its state are left as they are. On a
// packaged node the binary, unit and pull.conf stay: they are the package's.
func unenrollCatalog(packaged bool) *core.Catalog {
	if packaged {
		svc := []string{"service[" + enrollSvc + "]"}
		return finish([]*core.Resource{
			{Type: "service", Title: enrollSvc, Attrs: map[string]any{"ensure": "stopped", "enabled": false, "provider": "systemd"}},
			{Type: "file", Title: enrollToken, Require: svc, Attrs: map[string]any{"ensure": "absent"}},
			{Type: "file", Title: enrollCA, Require: svc, Attrs: map[string]any{"ensure": "absent"}},
			{Type: "file", Title: certFiles[0], Require: svc, Attrs: map[string]any{"ensure": "absent"}},
			{Type: "file", Title: certFiles[1], Require: svc, Attrs: map[string]any{"ensure": "absent"}},
		})
	}
	svc := []string{"service[" + enrollSvc + "]"}
	absent := func(path string, notify ...string) *core.Resource {
		return &core.Resource{Type: "file", Title: path, Require: svc, Notify: notify, Attrs: map[string]any{"ensure": "absent"}}
	}
	return finish([]*core.Resource{
		{Type: "service", Title: enrollSvc, Attrs: map[string]any{"ensure": "stopped", "enabled": false, "provider": "systemd"}},
		absent(enrollUnit, "exec[systemctl daemon-reload]"),
		{Type: "exec", Title: "systemctl daemon-reload", Attrs: map[string]any{"refreshonly": true}},
		absent(enrollToken),
		absent(enrollCA),
		absent(certFiles[0]),
		absent(certFiles[1]),
		absent(enrollBin),
	})
}

func finish(res []*core.Resource) *core.Catalog {
	for _, r := range res {
		r.Origin = "fleetreeve enroll"
	}
	return &core.Catalog{Version: core.ProtocolVersion, Name: enrollState, Resources: res}
}
