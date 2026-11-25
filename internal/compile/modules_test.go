package compile

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nxtw/fleetreeve/internal/core"
)

func moduleFixture(t *testing.T, site string) string {
	return write(t, map[string]string{
		"site.yaml":                            site,
		"modules/acme.app/module.yaml":         "name: acme.app\nversion: 1.2.0\ndescription: An app.\nvars: {app_user: app}\n",
		"modules/acme.app/roles/main.yaml":     "resources:\n  - exec: reload app\n    command: /bin/true\n    refreshonly: true\n",
		"modules/acme.app/roles/extra.yaml":    "resources:\n  - file: /etc/app/extra\n    content: '{{ .flavour }}'\n",
		"modules/acme.app/templates/unit.tmpl": "name={{ .title }} port={{ .port }} user={{ .app_user }} tags={{ .tags | join \",\" }}\n",
		// A composite type: a bundle of resources with parameters.
		"modules/acme.app/types/instance.yaml": `
description: One instance of the app.
params:
  port: {type: int, required: true}
  tags: {type: list, default: [a, b]}
  debug: {type: bool, default: false}
resources:
  - file: "/etc/app/{{ .title }}"
    ensure: directory
  - file: "/etc/app/{{ .title }}/unit"
    template: ../templates/unit.tmpl
    notify: exec[reload app]
  - file: "/etc/app/{{ .title }}/debug"
    when: .debug
    content: "on"
  - acme.app.flag: "{{ .title }}-ready"       # types can use other types
    value: "{{ .port }}"
`,
		// A script type: a program run on the node.
		"modules/acme.app/types/flag.yaml": "description: A flag.\nparams:\n  value: {required: true}\n  mode: {default: plain}\nscript: flag.sh\nrefresh: true\n",
		"modules/acme.app/types/flag.sh":   "#!/bin/sh\necho would-do\n",
	})
}

func TestModules(t *testing.T) {
	dir := moduleFixture(t, `
vars: {flavour: vanilla}
include:
  - module: acme.app
  - {module: acme.app, role: extra, vars: {flavour: mint}}
resources:
  - acme.app.instance: [blue, green]
    port: 8080
    debug: "{{ eq .flavour \"vanilla\" }}"
    require: package[base]
  - acme.app.instance: red
    port: 9090
    tags: [x]
    before: file[/etc/after]
  - package: base
  - file: /etc/after
    content: x
    subscribe: acme.app.instance[blue]     # a reference to a use of a composite type
  - acme.app.flag: standalone
    value: 1
    require: acme.app.instance[red]
`)
	cat, err := compileHost(t, dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Roles: main, and a named one with its own parameters.
	find(t, cat, "exec[reload app]")
	if got := find(t, cat, "file[/etc/app/extra]").Str("content"); got != "mint" {
		t.Errorf("role parameter: %q", got)
	}
	// Composite types are expanded: parameters, defaults, the title, and
	// variables from the surrounding scope (app_user from the role).
	if got := find(t, cat, "file[/etc/app/blue/unit]").Str("content"); got != "name=blue port=8080 user=app tags=a,b\n" {
		t.Errorf("blue: %q", got)
	}
	if got := find(t, cat, "file[/etc/app/red/unit]").Str("content"); got != "name=red port=9090 user=app tags=x\n" {
		t.Errorf("red: %q", got)
	}
	find(t, cat, "file[/etc/app/green/debug]") // `when` inside the type sees its parameters
	for _, r := range cat.Resources {
		if strings.HasPrefix(r.ID(), "acme.app.instance[") || r.ID() == "file[/etc/app/red/debug]" {
			t.Errorf("%s should not be in the catalog", r.ID())
		}
	}
	// Relationships on a use apply to everything it is made of...
	for _, id := range []string{"file[/etc/app/blue]", "file[/etc/app/blue/unit]", "acme.app.flag[blue-ready]"} {
		if r := find(t, cat, id); len(r.Require) == 0 || r.Require[len(r.Require)-1] != "package[base]" {
			t.Errorf("%s does not require package[base]: %v", id, r.Require)
		}
	}
	// ...and a reference to a use means everything it is made of.
	after := find(t, cat, "file[/etc/after]")
	if len(after.Require) != 3 || after.Require[0] != "file[/etc/app/red]" {
		t.Errorf("before on a composite: %v", after.Require)
	}
	if n := find(t, cat, "file[/etc/app/blue/unit]").Notify; len(n) != 2 || n[1] != "file[/etc/after]" {
		t.Errorf("subscribe to a composite: %v", n)
	}
	if req := find(t, cat, "acme.app.flag[standalone]").Require; len(req) != 3 {
		t.Errorf("require of a composite: %v", req)
	}
	// Script types stay resources, and their script goes along.
	flag := find(t, cat, "acme.app.flag[red-ready]")
	if flag.Str("value") != "9090" || flag.Str("mode") != "plain" {
		t.Errorf("script type params: %v", flag.Attrs)
	}
	def := cat.Types["acme.app.flag"]
	if def == nil || def.Script != "#!/bin/sh\necho would-do\n" || !def.Refresh || len(cat.Types) != 1 {
		t.Errorf("types in the catalog: %+v", cat.Types)
	}
	if _, err := cat.Graph(); err != nil {
		t.Error(err)
	}
}

func TestModuleErrors(t *testing.T) {
	for name, c := range map[string]struct{ site, want string }{
		"module not installed":   {"include:\n  - module: acme.missing\n", "not installed"},
		"no such role":           {"include:\n  - {module: acme.app, role: nope}\n", `has no role "nope" (it has: extra, main)`},
		"role without module":    {"include:\n  - {manifest: x.yaml, role: main}\n", "role is only for including from a module"},
		"unknown parameter":      {"resources:\n  - acme.app.instance: a\n    prot: 1\n", `unknown parameter "prot" (did you mean "port"?)`},
		"missing parameter":      {"resources:\n  - acme.app.instance: a\n", `missing required parameter "port"`},
		"wrong parameter type":   {"resources:\n  - acme.app.instance: a\n    port: many\n", "expected int"},
		"used twice":             {"include:\n  - module: acme.app\nresources:\n  - acme.app.instance: a\n    port: 1\n  - acme.app.instance: a\n    port: 2\n", "declared twice"},
		"type of missing module": {"resources:\n  - other.thing.widget: a\n", "the module other.thing, which is not installed"},
		"misspelt module type":   {"resources:\n  - acme.app.instanse: a\n", `did you mean "acme.app.instance"`},
		"script type parameter":  {"resources:\n  - acme.app.flag: a\n    valeu: 1\n", `did you mean "value"`},
		"rollback on a script":   {"resources:\n  - acme.app.flag: a\n    value: 1\n    rollback: true\n", "rollback is not possible"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := moduleFixture(t, c.site)
			if _, err := compileHost(t, dir, "", nil); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("want error containing %q, got %v", c.want, err)
			}
		})
	}
	// A broken module is reported as such.
	for name, c := range map[string]struct{ file, body, want string }{
		"bad name":             {"modules/acme.app/module.yaml", "name: App\n", "namespace.name"},
		"directory mismatch":   {"modules/acme.app/module.yaml", "name: acme.other\n", "must be called acme.other"},
		"script and resources": {"modules/acme.app/types/both.yaml", "script: flag.sh\nresources: []\n", "not both"},
		"neither":              {"modules/acme.app/types/none.yaml", "description: x\n", "needs a script"},
		"missing script":       {"modules/acme.app/types/gone.yaml", "script: gone.sh\n", "gone.sh"},
		"reserved parameter":   {"modules/acme.app/types/res.yaml", "params: {require: {}}\nscript: flag.sh\n", `cannot be called "require"`},
		"bad parameter type":   {"modules/acme.app/types/typ.yaml", "params: {x: {type: float}}\nscript: flag.sh\n", "unknown parameter type"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := moduleFixture(t, "resources: []\n")
			writeInto(t, dir, c.file, c.body)
			if _, err := compileHost(t, dir, "", nil); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

// A role is included once however often it is asked for with the same
// values, so roles can include the roles they build on.
func TestModuleRoleIncludedOnce(t *testing.T) {
	dir := moduleFixture(t, "vars: {flavour: mint}\ninclude:\n  - module: acme.app\n  - {module: acme.app, role: onmain}\n  - module: acme.app\n")
	writeInto(t, dir, "modules/acme.app/roles/onmain.yaml", "include:\n  - module: acme.app\nresources:\n  - file: /etc/app/on-main\n    content: x\n    notify: exec[reload app]\n")
	cat, err := compileHost(t, dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	find(t, cat, "exec[reload app]")
	find(t, cat, "file[/etc/app/on-main]")
	// With other values it is another inclusion, and its resources clash.
	writeInto(t, dir, "site.yaml", "include:\n  - module: acme.app\n  - {module: acme.app, vars: {x: 1}}\n")
	if _, err := compileHost(t, dir, "", nil); err == nil || !strings.Contains(err.Error(), "duplicate resource") {
		t.Errorf("the same role with other values: %v", err)
	}
}

// A project can override a module's defaults like any other variable.
func TestModuleDefaults(t *testing.T) {
	dir := moduleFixture(t, "vars: {app_user: custom}\ninclude:\n  - module: acme.app\nresources:\n  - acme.app.instance: a\n    port: 1\n")
	cat, err := compileHost(t, dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := find(t, cat, "file[/etc/app/a/unit]").Str("content"); !strings.Contains(got, "user=custom") {
		t.Errorf("a module default was not overridden: %q", got)
	}
	_ = core.ProtocolVersion
	_ = filepath.Join
}

// The GitLab Runner module of the fleet example: its configuration comes
// from data, and a runner without a token is refused while compiling.
func TestGitLabRunnerModule(t *testing.T) {
	mods, err := filepath.Abs("../../examples/fleet/modules")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEETREEVE_MODULES", mods)
	site := func(runners string) string {
		return "vars:\n  gitlab_runner_podman: true\n  gitlab_runner_version: 17.5.0-1\n  gitlab_runners:\n" + runners + "include:\n  - module: fleetreeve.gitlab_runner\n"
	}
	on := func(dir, family, version, pretty string) (*core.Catalog, error) {
		inv := Localhost()
		hosts, _ := inv.Select("")
		facts := map[string]any{"os": map[string]any{"family": family, "version": version, "pretty": pretty}, "hostname": "box"}
		return Compile(Input{Manifest: filepath.Join(dir, "site.yaml"), Host: hosts[0], Inventory: inv, Facts: facts})
	}
	redhat := func(dir string) (*core.Catalog, error) { return on(dir, "redhat", "9.4", "CentOS Stream 9") }
	dir := write(t, map[string]string{"site.yaml": site(`
    - {name: shell, url: "https://gitlab.example.com", token: glrt-one}
    - name: containers
      url: https://gitlab.example.com
      token: glrt-two
      executor: docker
      limit: 2
      docker: {privileged: true}
      extra: "output_limit = 8192\n"
`)})
	cat, err := redhat(dir)
	if err != nil {
		t.Fatal(err)
	}
	conf := find(t, cat, "file[/etc/gitlab-runner/config.toml]")
	if !conf.Bool("sensitive") || conf.Str("mode") != "0600" {
		t.Errorf("the file with the tokens is not protected: %v", conf.Attrs)
	}
	got := conf.Str("content")
	for _, want := range []string{
		"concurrent = 1\n", "name = \"shell\"\n  url = \"https://gitlab.example.com\"\n  token = \"glrt-one\"\n  executor = \"shell\"\n",
		"executor = \"docker\"\n  limit = 2\n  output_limit = 8192\n  [runners.docker]\n    image = \"almalinux:9\"\n    privileged = true\n    volumes = [\"/cache\"]\n    host = \"unix:///run/podman/podman.sock\"\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config.toml lacks %q:\n%s", want, got)
		}
	}
	if repo := find(t, cat, "file[/etc/yum.repos.d/runner_gitlab-runner.repo]").Str("content"); !strings.Contains(repo, "/el/9/$basearch") || !strings.Contains(repo, "gpgcheck=1") {
		t.Errorf("repository:\n%s", repo)
	}
	if name := find(t, cat, "package[gitlab-runner]").Str("name"); name != "gitlab-runner-17.5.0-1" {
		t.Errorf("pinned package: %q", name)
	}
	find(t, cat, "service[podman.socket]")

	// A runner without a token never reaches a host.
	dir = write(t, map[string]string{"site.yaml": site("    - {name: shell, url: \"https://gitlab.example.com\"}\n")})
	if _, err := redhat(dir); err == nil || !strings.Contains(err.Error(), "gitlab_runners[0]: token is required") {
		t.Errorf("missing token: %v", err)
	}
	// Elsewhere it says so, and installs nothing.
	dir = write(t, map[string]string{"site.yaml": site("")})
	cat, err = on(dir, "debian", "12", "Debian 12")
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Resources) != 1 || cat.Resources[0].Type != "exec" {
		t.Errorf("on Debian: %v", cat.Resources)
	}
}

// The same module, with the host registering its runners itself.
func TestGitLabRunnerRegister(t *testing.T) {
	mods, _ := filepath.Abs("../../examples/fleet/modules")
	t.Setenv("FLEETREEVE_MODULES", mods)
	compile := func(vars string) (*core.Catalog, error) {
		dir := write(t, map[string]string{"site.yaml": "vars:\n" + vars + "include:\n  - module: fleetreeve.gitlab_runner\n"})
		inv := Localhost()
		hosts, _ := inv.Select("")
		facts := map[string]any{"os": map[string]any{"family": "redhat", "version": "9", "pretty": "CentOS Stream 9"}, "hostname": "box"}
		return Compile(Input{Manifest: filepath.Join(dir, "site.yaml"), Host: hosts[0], Inventory: inv, Facts: facts})
	}
	cat, err := compile(`  gitlab_runner_podman: true
  gitlab_runner_register:
    - {name: auto, url: "https://gitlab.example.com", access_token: glpat-x, runner_type: group_type, group_id: 42, tags: [a, b], run_untagged: false}
    - {name: given, url: "https://gitlab.example.com", token: glrt-y, executor: docker, limit: 2}
`)
	if err != nil {
		t.Fatal(err)
	}
	// gitlab-runner owns the runners in config.toml; fleetreeve only two lines.
	for _, r := range cat.Resources {
		if r.ID() == "file[/etc/gitlab-runner/config.toml]" {
			t.Error("config.toml is still written as a whole")
		}
	}
	find(t, cat, "line[gitlab-runner concurrent]")
	auto, given := find(t, cat, "fleetreeve.gitlab_runner.runner[auto]"), find(t, cat, "fleetreeve.gitlab_runner.runner[given]")
	if auto.Str("group_id") != "42" || auto.Bool("run_untagged") || fmt.Sprint(auto.Attrs["tags"]) != "[a b]" || auto.Str("executor") != "shell" {
		t.Errorf("auto: %v", auto.Attrs)
	}
	if given.Str("docker_host") != "unix:///run/podman/podman.sock" || fmt.Sprint(given.Attrs["limit"]) != "2" || !given.Bool("run_untagged") {
		t.Errorf("given: %v", given.Attrs)
	}
	if def := cat.Types["fleetreeve.gitlab_runner.runner"]; def == nil || !strings.Contains(def.Script, "gitlab-runner") {
		t.Error("the catalog lacks the registration script")
	}
	if _, err := compile("  gitlab_runner_register:\n    - {name: nourl}\n"); err == nil || !strings.Contains(err.Error(), "every runner needs a url") {
		t.Errorf("missing url: %v", err)
	}
}
