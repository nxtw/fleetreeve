package compile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nxtw/fleetreeve/internal/core"
	_ "github.com/nxtw/fleetreeve/internal/types"
)

// write creates files under a temp dir and returns the dir.
func write(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// writeInto adds or replaces one file below dir.
func writeInto(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func compileHost(t *testing.T, dir, host string, extra map[string]any) (*core.Catalog, error) {
	t.Helper()
	inv := Localhost()
	if _, err := os.Stat(filepath.Join(dir, "inventory.yaml")); err == nil {
		if inv, err = LoadInventory(filepath.Join(dir, "inventory.yaml")); err != nil {
			t.Fatal(err)
		}
	}
	hosts, err := inv.Select(host)
	if err != nil {
		t.Fatal(err)
	}
	facts := map[string]any{"os": map[string]any{"family": "debian"}, "hostname": "box", "cpu": map[string]any{"count": float64(8)}}
	return Compile(Input{Manifest: filepath.Join(dir, "site.yaml"), Host: hosts[0], Inventory: inv, Facts: facts, Extra: extra})
}

func find(t *testing.T, cat *core.Catalog, id string) *core.Resource {
	t.Helper()
	for _, r := range cat.Resources {
		if r.ID() == id {
			return r
		}
	}
	var ids []string
	for _, r := range cat.Resources {
		ids = append(ids, r.ID())
	}
	t.Fatalf("%s not in catalog: %v", id, ids)
	return nil
}

func TestCompile(t *testing.T) {
	dir := write(t, map[string]string{
		"inventory.yaml": `
vars: {port: 1, colour: inv}
groups:
  web: {hosts: [web1, web2], vars: {port: 2}}
  db: {hosts: [db1]}
hosts:
  web2: {vars: {port: 3}}
`,
		"site.yaml": `
vars: {colour: default, pkgs: [a, b], greeting: "hi {{ .colour }}"}
include:
  - manifest: role.yaml
    when: contains "web" .host.groups
    vars: {name: "{{ .host.name }}-site"}
resources:
  - package: "{{ .item }}"
    each: "{{ .pkgs }}"
  - package: [c, d]
    when: eq .facts.os.family "redhat"
  - package: e
    when: and (gt .facts.cpu.count 4) (eq .port 99)   # JSON numbers compare as ints
  - file: /etc/motd
    content: "{{ .greeting }} port={{ .port }}\n"
    mode: 0644
    subscribe: package[a]
    before: exec[/bin/true]
  - exec: /bin/true
    refreshonly: true
`,
		"role.yaml": `
vars: {name: unset, port: 80}
resources:
  - file: /etc/app.conf
    template: app.tmpl
    notify: exec[/bin/true]
`,
		"app.tmpl": "name={{ .name }} port={{ .port }} groups={{ .host.groups | join \",\" }}\n",
	})

	cat, err := compileHost(t, dir, "web2", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Host var beats group var beats inventory var beats manifest default;
	// include vars are rendered in the including manifest.
	if got := find(t, cat, "file[/etc/app.conf]").Str("content"); got != "name=web2-site port=3 groups=web\n" {
		t.Errorf("template: %q", got)
	}
	motd := find(t, cat, "file[/etc/motd]")
	if got := motd.Str("content"); got != "hi inv port=3\n" {
		t.Errorf("motd: %q", got)
	}
	if motd.Str("mode") != "0644" {
		t.Errorf("unquoted mode became %q", motd.Str("mode"))
	}
	find(t, cat, "package[a]")
	find(t, cat, "package[b]")
	if got := find(t, cat, "package[a]").Notify; len(got) != 1 || got[0] != "file[/etc/motd]" {
		t.Errorf("subscribe not turned into notify: %v", got)
	}
	if got := find(t, cat, "exec[/bin/true]").Require; len(got) != 1 || got[0] != "file[/etc/motd]" {
		t.Errorf("before not turned into require: %v", got)
	}
	if len(cat.Resources) != 5 {
		t.Errorf("want 5 resources (when: false must drop package c, d), got %d", len(cat.Resources))
	}

	// db1 is not in web: the role is not included, and -e wins over all.
	cat, err = compileHost(t, dir, "db1", map[string]any{"port": float64(99)})
	if err != nil {
		t.Fatal(err)
	}
	if got := find(t, cat, "file[/etc/motd]").Str("content"); got != "hi inv port=99\n" {
		t.Errorf("motd: %q", got)
	}
	find(t, cat, "package[e]")
	if len(cat.Resources) != 5 {
		t.Errorf("want 5 resources, got %d", len(cat.Resources))
	}
}

func TestCompileErrors(t *testing.T) {
	cases := map[string]struct{ manifest, want string }{
		"unknown type":     {"resources:\n  - flie: /x\n", `did you mean "file"`},
		"unknown attr":     {"resources:\n  - file: /x\n    contnet: a\n", `did you mean "content"`},
		"bad enum":         {"resources:\n  - file: /x\n    ensure: present\n", "is not one of"},
		"bad type":         {"resources:\n  - service: x\n    enabled: maybe\n", "expected bool"},
		"undefined var":    {"resources:\n  - file: /x\n    content: '{{ .nope }}'\n", "nope"},
		"relative path":    {"resources:\n  - file: x\n", "must be absolute"},
		"missing ref":      {"resources:\n  - file: /x\n    require: package[nginx]\n", "not declared"},
		"missing before":   {"resources:\n  - file: /x\n    before: package[nginx]\n", "not declared"},
		"duplicate":        {"resources:\n  - file: /x\n  - file: /x\n", "duplicate resource"},
		"cycle":            {"resources:\n  - file: /x\n    require: file[/y]\n  - file: /y\n    require: file[/x]\n", "dependency cycle"},
		"exclusive":        {"resources:\n  - file: /x\n    content: a\n    source: b\n", "mutually exclusive"},
		"line conflict":    {"resources:\n  - file: /x\n    content: a\n  - line: l\n    path: /x\n    line: b\n", "already manages"},
		"unknown top key":  {"resource: []\n", "unknown top-level key"},
		"missing template": {"resources:\n  - file: /x\n    template: nope.tmpl\n", "no such file"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := write(t, map[string]string{"site.yaml": c.manifest})
			_, err := compileHost(t, dir, "", nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestInventoryRejectsStrayKeys(t *testing.T) {
	for name, inv := range map[string]string{
		"host":  "hosts:\n  web1: {workers: 8}\n",
		"group": "groups:\n  web: {hosts: [web1], workers: 8}\n",
	} {
		dir := write(t, map[string]string{"inventory.yaml": inv})
		_, err := LoadInventory(filepath.Join(dir, "inventory.yaml"))
		if err == nil || !strings.Contains(err.Error(), "vars: {workers: ...}") {
			t.Errorf("%s: want a hint to use vars, got %v", name, err)
		}
	}
}

func TestRollbackMetaparameter(t *testing.T) {
	dir := write(t, map[string]string{"site.yaml": `
vars: {careful: true}
resources:
  - file: /etc/app.conf
    content: x
    rollback: true
  - file: /etc/other.conf
    content: x
    rollback: "{{ .careful }}"
  - file: /etc/plain.conf
    content: x
`})
	cat, err := compileHost(t, dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !find(t, cat, "file[/etc/app.conf]").Rollback || !find(t, cat, "file[/etc/other.conf]").Rollback || find(t, cat, "file[/etc/plain.conf]").Rollback {
		t.Error("rollback flags")
	}
	// Only for types that can be put back.
	dir = write(t, map[string]string{"site.yaml": "resources:\n  - exec: /bin/true\n    rollback: true\n"})
	if _, err := compileHost(t, dir, "", nil); err == nil || !strings.Contains(err.Error(), "rollback is not possible for exec") {
		t.Errorf("rollback on exec: %v", err)
	}
}

func TestOrder(t *testing.T) {
	// Declared "backwards": the graph must reorder, and auto-require must
	// put the directory before the file inside it.
	dir := write(t, map[string]string{"site.yaml": `
resources:
  - service: app
    ensure: running
    require: file[/etc/app/app.conf]
  - file: /etc/app/app.conf
    content: x
    notify: service[app]
  - file: /etc/app
    ensure: directory
  - package: app
    before: file[/etc/app]
`})
	cat, err := compileHost(t, dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	g, err := cat.Graph()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range g.Order {
		got = append(got, r.ID())
	}
	want := "package[app] file[/etc/app] file[/etc/app/app.conf] service[app]"
	if strings.Join(got, " ") != want {
		t.Errorf("order:\n got %v\nwant %s", got, want)
	}
}

// host_key_check is inherited from the defaults and can be set per host.
func TestInventoryHostKeyCheck(t *testing.T) {
	dir := write(t, map[string]string{"inventory.yaml": "defaults: {host_key_check: false}\nhosts:\n  a: {}\n  b: {host_key_check: true}\n"})
	inv, err := LoadInventory(filepath.Join(dir, "inventory.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	hosts, _ := inv.Select("")
	skip := map[string]bool{}
	for _, h := range hosts {
		skip[h.Name] = h.SkipHostKey()
	}
	if !skip["a"] || skip["b"] {
		t.Errorf("skip host key: %v", skip)
	}
}

// A purged directory is told which paths in it the catalog manages.
func TestPurgeKnowsWhatIsManaged(t *testing.T) {
	dir := write(t, map[string]string{"site.yaml": `
resources:
  - file: /etc/app/conf.d
    ensure: directory
    purge: true
    ignore: ["*.bak"]
  - file: /etc/app/conf.d/a.conf
    content: a
  - file: /etc/app/conf.d/sub/b.conf
    content: b
  - line: tune c
    path: /etc/app/conf.d/c.conf
    line: x=1
    create: true
  - exec: make d
    command: touch /etc/app/conf.d/d.conf
    creates: /etc/app/conf.d/d.conf
  - file: /etc/app/conf.d.old/e.conf
    content: e
  - file: /etc/app/other
    ensure: directory
`})
	cat, err := compileHost(t, dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(find(t, cat, "file[/etc/app/conf.d]").List("keep"), " ")
	if want := "/etc/app/conf.d/a.conf /etc/app/conf.d/c.conf /etc/app/conf.d/d.conf /etc/app/conf.d/sub/b.conf"; got != want {
		t.Errorf("keep:\n got %s\nwant %s", got, want)
	}
	if find(t, cat, "file[/etc/app/other]").Has("keep") {
		t.Error("a directory that is not purged got a keep list")
	}
	// keep is the compiler's: whatever a manifest puts there is replaced.
	writeInto(t, dir, "site.yaml", "resources:\n  - file: /etc/app/conf.d\n    ensure: directory\n    purge: true\n    keep: [/etc/app/conf.d/smuggled]\n")
	if cat, err = compileHost(t, dir, "", nil); err != nil || len(find(t, cat, "file[/etc/app/conf.d]").List("keep")) != 0 {
		t.Errorf("a manifest could set keep: %v", err)
	}
}
