package compile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nxtw/fleetreeve/internal/core"
)

// linuxFacts stands in for a Debian node, so the examples that target
// servers can be compiled (not applied) anywhere.
var linuxFacts = map[string]any{
	"hostname": "node", "fqdn": "node.example.com", "arch": "amd64", "init": "systemd", "package_manager": "apt",
	"os":     map[string]any{"name": "debian", "family": "debian", "version": "12", "codename": "bookworm", "pretty": "Debian GNU/Linux 12"},
	"kernel": map[string]any{"name": "linux", "release": "6.1.0"},
	"cpu":    map[string]any{"count": float64(4)}, "memory": map[string]any{"total_mb": float64(8192)},
	"network": map[string]any{"ip": "10.0.0.1", "interfaces": map[string]any{}},
	"user":    map[string]any{"name": "root", "uid": float64(0), "home": "/root"}, "custom": map[string]any{},
	"selinux": "disabled",
}

// centosFacts stands in for a CentOS Stream node with SELinux enforcing.
var centosFacts = func() map[string]any {
	f := map[string]any{}
	for k, v := range linuxFacts {
		f[k] = v
	}
	f["os"] = map[string]any{"name": "centos", "family": "redhat", "version": "9", "codename": "", "pretty": "CentOS Stream 9"}
	f["package_manager"], f["selinux"] = "dnf", "enforcing"
	return f
}()

// Every shipped example must compile for every host it targets.
func TestExamplesCompile(t *testing.T) {
	root := "../../examples"
	cases := []struct{ manifest, inventory string }{
		{"demo.yaml", ""},
		{"features/files.yaml", ""},
		{"features/lines.yaml", ""},
		{"features/exec.yaml", ""},
		{"features/ordering.yaml", ""},
		{"features/failure.yaml", ""},
		{"features/loops-and-conditions.yaml", ""},
		{"features/includes/site.yaml", ""},
		{"features/variables/site.yaml", "features/variables/inventory.yaml"},
		{"fleet/site.yaml", "fleet/inventory.yaml"},
		{"fleet/site.yaml", "fleet/single-host.yaml"},
		{"fleet/site.yaml", "fleet/server-host.yaml"},
		{"pull/site.yaml", "pull/inventory.yaml"},
	}
	for _, c := range cases {
		t.Run(c.manifest, func(t *testing.T) {
			inv := Localhost()
			if c.inventory != "" {
				var err error
				if inv, err = LoadInventory(filepath.Join(root, c.inventory)); err != nil {
					t.Fatal(err)
				}
			}
			for _, h := range inv.Hosts {
				for family, f := range map[string]map[string]any{"debian": linuxFacts, "centos": centosFacts} {
					if _, err := Compile(Input{Manifest: filepath.Join(root, c.manifest), Host: h, Inventory: inv, Facts: f}); err != nil {
						t.Errorf("%s (%s): %v", h.Name, family, err)
					}
				}
			}
		})
	}
}

// The fleet example on CentOS: data from the inventory ends up in the
// right resources on the right hosts.
func TestFleetOnCentOS(t *testing.T) {
	inv, err := LoadInventory("../../examples/fleet/inventory.yaml")
	if err != nil {
		t.Fatal(err)
	}
	catalog := func(host string, facts map[string]any) *core.Catalog {
		t.Helper()
		h, _ := inv.Host(host)
		cat, err := Compile(Input{Manifest: "../../examples/fleet/site.yaml", Host: h, Inventory: inv, Facts: facts})
		if err != nil {
			t.Fatal(err)
		}
		return cat
	}
	has := func(cat *core.Catalog, id string) bool {
		for _, r := range cat.Resources {
			if r.ID() == id {
				return true
			}
		}
		return false
	}

	// The backend list is generated from the inventory's web group.
	lb := catalog("lb1", centosFacts)
	cfg := find(t, lb, "file[/etc/haproxy/haproxy.cfg]").Str("content")
	for _, want := range []string{"server web1 10.0.1.11:8080 check", "server web2 10.0.1.12:9090 check"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("haproxy.cfg lacks %q:\n%s", want, cfg)
		}
	}
	find(t, lb, "exec[selinux boolean haproxy_connect_any]")
	find(t, lb, "fleetreeve.firewalld.port[80/tcp]")

	// web2 overrides the port; firewall and SELinux follow it.
	web2 := catalog("web2", centosFacts)
	find(t, web2, "fleetreeve.firewalld.port[9090/tcp]")
	find(t, web2, "exec[selinux http port 9090]")
	if got := find(t, web2, "file[/etc/nginx/nginx.conf]").Str("content"); !strings.Contains(got, "user nginx;") {
		t.Errorf("nginx.conf on CentOS:\n%s", got)
	}
	if got := find(t, web2, "service[chrony]").Str("name"); got != "chronyd" {
		t.Errorf("chrony service name on CentOS: %q", got)
	}
	if got := find(t, web2, "service[sshd]").Str("name"); got != "sshd" {
		t.Errorf("ssh service name on CentOS: %q", got)
	}
	// SELinux labels and firewall rules come before the service starts.
	g, err := web2.Graph()
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, r := range g.Order {
		pos[r.ID()] = i
	}
	for _, first := range []string{"exec[selinux http port 9090]", "fleetreeve.firewalld.port[9090/tcp]"} {
		if pos[first] > pos["service[nginx]"] {
			t.Errorf("%s is applied after service[nginx]", first)
		}
	}

	// The port type is a script from a module: it travels in the catalog,
	// so the node needs nothing installed for it.
	if def := web2.Types["fleetreeve.firewalld.port"]; def == nil || !strings.Contains(def.Script, "firewall-cmd") {
		t.Errorf("the catalog lacks the script of fleetreeve.firewalld.port: %v", web2.Types)
	}
	// The vhost type is composite: it is gone from the catalog, replaced
	// by the files it is made of, the rollback flag included.
	if has(web2, "fleetreeve.nginx.vhost[www]") || !find(t, web2, "file[/etc/nginx/conf.d/www.conf]").Rollback {
		t.Error("fleetreeve.nginx.vhost was not expanded into its resources")
	}
	// A web root belongs to root: what serves a site must not be able to
	// rewrite it.
	roots := 0
	for _, r := range web2.Resources {
		if r.Type == "file" && r.Str("ensure") == "directory" && strings.HasPrefix(r.Title, "/usr/share/nginx/html/") {
			if roots++; r.Str("owner") != "root" {
				t.Errorf("%s is owned by %q, want root", r.ID(), r.Str("owner"))
			}
		}
	}
	if roots == 0 {
		t.Error("no web root found in the catalog of web2")
	}
	if got := find(t, web2, "file[/etc/nginx/conf.d/www.conf]").Str("content"); !strings.Contains(got, "listen 9090;") || !strings.Contains(got, "server_name www.example.com;") {
		t.Errorf("vhost:\n%s", got)
	}

	db := catalog("db1", centosFacts)
	if got := find(t, db, "file[/var/lib/pgsql/data/conf.d/90-fleetreeve.conf]").Str("content"); !strings.Contains(got, "shared_buffers = 2048MB") || !strings.Contains(got, "max_connections = 200") {
		t.Errorf("postgres config:\n%s", got)
	}
	find(t, db, "fleetreeve.firewalld.port[5432/tcp]")

	// On Debian the Red Hat roles are left out and names switch.
	deb := catalog("web1", linuxFacts)
	if has(deb, "package[firewalld]") || has(deb, "package[policycoreutils-python-utils]") {
		t.Error("Red Hat roles included on Debian")
	}
	if got := find(t, deb, "file[/etc/nginx/nginx.conf]").Str("content"); !strings.Contains(got, "user www-data;") {
		t.Errorf("nginx.conf on Debian:\n%s", got)
	}
	if _, err := os.Stat("../../examples/README.md"); err != nil {
		t.Error("examples/README.md is missing")
	}
}

// Catalogs are compiled for several hosts at once - a controller does it
// for its hosts, the server for hosts that ask together - and each
// compilation registers the types of the project's modules. That has to
// be safe: run with -race, this fails on any unguarded shared state.
func TestCompileInParallel(t *testing.T) {
	inv, err := LoadInventory("../../examples/fleet/inventory.yaml")
	if err != nil {
		t.Fatal(err)
	}
	hosts, err := inv.Select("")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for round := 0; round < 8; round++ {
		for _, h := range hosts {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := Compile(Input{Manifest: "../../examples/fleet/site.yaml", Host: h, Inventory: inv, Facts: centosFacts}); err != nil {
					errs <- fmt.Errorf("%s: %v", h.Name, err)
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
