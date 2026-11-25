package types

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nxtw/fleetreeve/internal/core"
)

func init() { core.Register(packageType{}) }

type packageType struct{}

// pkgProvider describes one package manager. "%" in argv is the package name.
type pkgProvider struct {
	name, bin              string
	query, install, remove []string
	env                    []string
}

var pkgProviders = []pkgProvider{
	{name: "apt", bin: "apt-get",
		query:   []string{"sh", "-c", `dpkg-query -W -f='${Status}' "$1" 2>/dev/null | grep -q 'ok installed'`, "sh", "%"},
		install: []string{"apt-get", "install", "-y", "%"},
		remove:  []string{"apt-get", "remove", "-y", "%"},
		env:     []string{"DEBIAN_FRONTEND=noninteractive"}},
	{name: "dnf", bin: "dnf",
		query:   []string{"rpm", "-q", "%"},
		install: []string{"dnf", "install", "-y", "%"},
		remove:  []string{"dnf", "remove", "-y", "%"}},
	{name: "yum", bin: "yum",
		query:   []string{"rpm", "-q", "%"},
		install: []string{"yum", "install", "-y", "%"},
		remove:  []string{"yum", "remove", "-y", "%"}},
	{name: "apk", bin: "apk",
		query:   []string{"apk", "info", "-e", "%"},
		install: []string{"apk", "add", "%"},
		remove:  []string{"apk", "del", "%"}},
	{name: "pacman", bin: "pacman",
		query:   []string{"pacman", "-Q", "%"},
		install: []string{"pacman", "-S", "--noconfirm", "--needed", "%"},
		remove:  []string{"pacman", "-R", "--noconfirm", "%"}},
	{name: "zypper", bin: "zypper",
		query:   []string{"rpm", "-q", "%"},
		install: []string{"zypper", "--non-interactive", "install", "%"},
		remove:  []string{"zypper", "--non-interactive", "remove", "%"}},
	{name: "brew", bin: "brew",
		query:   []string{"brew", "list", "--versions", "%"},
		install: []string{"brew", "install", "%"},
		remove:  []string{"brew", "uninstall", "%"}},
}

func (packageType) Name() string { return "package" }
func (packageType) Doc() string {
	return "Installs or removes a package using the system package manager (apt, dnf, yum, apk, pacman, zypper or brew)."
}

func (packageType) Schema() core.Schema {
	names := make([]string, len(pkgProviders))
	for i, p := range pkgProviders {
		names[i] = p.name
	}
	return core.Schema{
		"name":     {Kind: core.String, NameVar: true, Doc: "Package name (defaults to the title)."},
		"ensure":   {Kind: core.String, Default: "present", Enum: []string{"present", "absent"}},
		"provider": {Kind: core.String, Enum: names, Doc: "Package manager; detected when omitted."},
	}
}

func (packageType) Absent(r *core.Resource) map[string]any {
	a := map[string]any{"name": r.Str("name"), "ensure": "absent"}
	if r.Has("provider") {
		a["provider"] = r.Str("provider")
	}
	return a
}

func (packageType) provider(ctx *core.Ctx, r *core.Resource) (*pkgProvider, error) {
	want := r.Str("provider")
	for i, p := range pkgProviders {
		if (want == "" || want == p.name) && ctx.Has(p.bin) {
			return &pkgProviders[i], nil
		}
	}
	if want != "" {
		return nil, fmt.Errorf("package provider %q is not available on this host", want)
	}
	return nil, fmt.Errorf("no supported package manager found on this host")
}

func (p *pkgProvider) run(ctx *core.Ctx, argv []string, pkg string) core.CmdResult {
	args := make([]string, len(argv)-1)
	for i, a := range argv[1:] {
		if a == "%" {
			a = pkg
		}
		args[i] = a
	}
	return ctx.Runner.Run(ctx, core.Cmd{Name: argv[0], Args: args, Env: p.env})
}

func (t packageType) Check(ctx *core.Ctx, r *core.Resource) ([]core.Change, error) {
	p, err := t.provider(ctx, r)
	if err != nil {
		return nil, err
	}
	res := p.run(ctx, p.query, r.Str("name"))
	if res.Err != nil {
		return nil, res.Error("query")
	}
	cur := "absent"
	if res.Code == 0 {
		cur = "present"
	}
	if cur == r.Str("ensure") {
		return nil, nil
	}
	return []core.Change{{Prop: "ensure", From: cur, To: r.Str("ensure")}}, nil
}

// Snapshot records whether the package is installed. A rollback installs
// or removes it again; it does not return to an earlier version.
func (t packageType) Snapshot(ctx *core.Ctx, r *core.Resource) (core.Snapshot, error) {
	p, err := t.provider(ctx, r)
	if err != nil {
		return nil, err
	}
	res := p.run(ctx, p.query, r.Str("name"))
	if res.Err != nil {
		return nil, res.Error("query")
	}
	return core.Snapshot{"present": res.Code == 0}, nil
}

func (t packageType) Restore(ctx *core.Ctx, r *core.Resource, s core.Snapshot) error {
	p, err := t.provider(ctx, r)
	if err != nil {
		return err
	}
	want, _ := s["present"].(bool)
	if have := p.run(ctx, p.query, r.Str("name")).Code == 0; have == want {
		return nil
	}
	argv := p.remove
	if want {
		argv = p.install
	}
	if res := p.run(ctx, argv, r.Str("name")); !res.OK() {
		return res.Error(p.name + " rollback")
	}
	return nil
}

var aptUpdate sync.Once

func (t packageType) Apply(ctx *core.Ctx, r *core.Resource, _ []core.Change) error {
	p, err := t.provider(ctx, r)
	if err != nil {
		return err
	}
	if r.Str("ensure") == "absent" {
		if res := p.run(ctx, p.remove, r.Str("name")); !res.OK() {
			return res.Error(p.name + " remove")
		}
		return nil
	}
	if p.name == "apt" {
		// Refresh the package index once per run if it is missing or stale.
		aptUpdate.Do(func() {
			lists, _ := filepath.Glob("/var/lib/apt/lists/*Packages*")
			st, err := os.Stat("/var/cache/apt/pkgcache.bin")
			if len(lists) == 0 || err != nil || time.Since(st.ModTime()) > 24*time.Hour {
				p.run(ctx, []string{"apt-get", "update", "-q"}, "")
			}
		})
	}
	if res := p.run(ctx, p.install, r.Str("name")); !res.OK() {
		return res.Error(p.name + " install")
	}
	return nil
}
