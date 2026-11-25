package compile

import (
	"path/filepath"
	"strings"
	"testing"
)

func dataFixture(t *testing.T, manifestHead string) string {
	return write(t, map[string]string{
		"inventory.yaml": `
vars: {env: prod, colour: inventory}
groups:
  web: {hosts: [web1, web2]}
  eu: {hosts: [web2]}
hosts:
  web2: {vars: {shape: host-inventory}}
  db1: {}
`,
		"site.yaml": manifestHead + `
vars: {port: 80, colour: manifest, size: small}
resources:
  - file: /etc/app.conf
    content: "{{ .colour }} {{ .port }} {{ .size }} {{ .service }} {{ .packages | join \",\" }} {{ toJson .limits }}\n"
`,
		"data/common.yaml":     "service: generic\npackages: [curl]\nlimits: {nofile: 1024, nproc: 100}\nport: 8000\n",
		"data/os/debian.yaml":  "service: apache2\npackages: [apt-utils]\n",
		"data/os/redhat.yaml":  "service: httpd\n",
		"data/groups/web.yaml": "port: 8080\npackages: [nginx, curl]\nlimits: {nofile: 65536}\ncolour: web\n",
		"data/groups/eu.yaml":  "colour: eu\n",
		"data/hosts/web2.yaml": "port: 9090\nmode: 0640\n",
		"data/env/prod.yaml":   "size: large\n",
	})
}

func content(t *testing.T, dir, host string, extra map[string]any) string {
	t.Helper()
	cat, err := compileHost(t, dir, host, extra)
	if err != nil {
		t.Fatal(err)
	}
	return find(t, cat, "file[/etc/app.conf]").Str("content")
}

func TestDataLayer(t *testing.T) {
	// Default hierarchy: hosts > groups > os > common. A key is taken from
	// the most specific level that has it.
	dir := dataFixture(t, "")
	for host, want := range map[string]string{
		// db1: only common and os apply. Data beats the manifest default
		// (port) and the inventory (colour is not in its data: inventory).
		"db1": `inventory 8000 small apache2 apt-utils {"nofile":1024,"nproc":100}`,
		// web1: the group overrides port, packages, limits and colour.
		"web1": `web 8080 small apache2 apt-utils {"nofile":1024,"nproc":100}`,
		// web2: the host file wins for port; of its two groups the later
		// one (eu) wins for colour, as in the inventory.
		"web2": `eu 9090 small apache2 apt-utils {"nofile":1024,"nproc":100}`,
	} {
		// os/ is less specific than groups/: on the web hosts the group's
		// packages and limits replace those from os/ and common.
		if host != "db1" {
			want = strings.Replace(want, "apt-utils", "nginx,curl", 1)
			want = strings.Replace(want, `{"nofile":1024,"nproc":100}`, `{"nofile":65536}`, 1)
		}
		if got := content(t, dir, host, nil); got != want+"\n" {
			t.Errorf("%s:\n got %q\nwant %q", host, got, want+"\n")
		}
	}
	// -e still beats everything.
	if got := content(t, dir, "web2", map[string]any{"port": float64(1)}); !strings.HasPrefix(got, "eu 1 ") {
		t.Errorf("-e: %q", got)
	}
}

func TestDataMergeAndCustomHierarchy(t *testing.T) {
	dir := dataFixture(t, `
data:
  hierarchy:
    - "hosts/{{ .host.name }}"
    - "groups/{{ .group }}"
    - "env/{{ .env }}"            # a level chosen by an inventory variable
    - "region/{{ .region }}"      # .region is not defined: the level is skipped
    - "os/{{ .facts.os.family }}"
    - common
  merge:
    packages: unique              # lists add up across levels
    limits: deep                  # maps merge key by key
`)
	want := `eu 9090 large apache2 curl,apt-utils,nginx {"nofile":65536,"nproc":100}` + "\n"
	if got := content(t, dir, "web2", nil); got != want {
		t.Errorf("merge:\n got %q\nwant %q", got, want)
	}

	// Explain: which files applied, and where a value comes from.
	inv, _ := LoadInventory(filepath.Join(dir, "inventory.yaml"))
	h, _ := inv.Host("web2")
	ex, err := Explain(Input{Manifest: filepath.Join(dir, "site.yaml"), Host: h, Inventory: inv,
		Facts: map[string]any{"os": map[string]any{"family": "debian"}}})
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, l := range ex.Levels {
		files = append(files, l.File+"="+strings.SplitN(l.Status, ":", 2)[0])
	}
	wantFiles := "hosts/web2.yaml=used groups/eu.yaml=used groups/web.yaml=used env/prod.yaml=used region/{{ .region }}=skipped os/debian.yaml=used common.yaml=used"
	if strings.Join(files, " ") != wantFiles {
		t.Errorf("levels:\n got %s\nwant %s", strings.Join(files, " "), wantFiles)
	}
	src := ex.Sources["port"]
	if len(src) != 4 || src[0].Source != "site.yaml (default)" || src[3].Source != "data/hosts/web2.yaml" || ex.Vars["port"] != 9090 {
		t.Errorf("port sources: %+v = %v", src, ex.Vars["port"])
	}
	if last := ex.Sources["colour"]; last[len(last)-1].Source != "data/groups/eu.yaml" {
		t.Errorf("colour sources: %+v", last)
	}
	// Unquoted octal survives in data files, as in manifests.
	if ex.Vars["mode"] != "0640" {
		t.Errorf("mode: %v", ex.Vars["mode"])
	}
	// Other hosts' data is visible through .inventory, without their facts.
	c, _, err := newCompiler(Input{Manifest: filepath.Join(dir, "site.yaml"), Host: h, Inventory: inv, Facts: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	web1 := c.invData["hosts"].(map[string]any)["web1"].(map[string]any)["vars"].(map[string]any)
	if web1["port"] != 8080 || web1["service"] != "generic" {
		t.Errorf("web1 seen from web2: %v", web1)
	}
}

func TestDataErrors(t *testing.T) {
	for name, c := range map[string]struct {
		files map[string]string
		want  string
	}{
		"missing dir":   {map[string]string{"site.yaml": "data: nope\nresources: []\n"}, "does not exist"},
		"bad merge":     {map[string]string{"site.yaml": "data: {merge: {a: sideways}}\nresources: []\n", "data/common.yaml": "a: 1\n"}, "not deep or unique"},
		"bad key":       {map[string]string{"site.yaml": "data: {directory: x}\nresources: []\n"}, "unknown key"},
		"not a mapping": {map[string]string{"site.yaml": "resources: []\n", "data/common.yaml": "- a\n- b\n"}, "must be a mapping"},
		"bad yaml":      {map[string]string{"site.yaml": "resources: []\n", "data/common.yaml": "a: [1\n"}, "common.yaml"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := write(t, c.files)
			if _, err := compileHost(t, dir, "", nil); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}
