package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsFile(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, ".fleetreeve.yaml")
	os.WriteFile(conf, []byte(`
server: https://fleetreeve.example.com:8140
secret-file: keys/server.secret
ca: /abs/ca.pem
forks: 3
enroll:
  inventory: inv.yaml
  rpm: dist
plan:
  no_diff: true
`), 0o644)
	t.Setenv("FLEETREEVE_CONFIG", conf)

	// Top-level settings apply everywhere, a section only to its command;
	// relative paths are taken from the file's directory.
	o, err := parse("enroll", []string{"web1"})
	if err != nil {
		t.Fatal(err)
	}
	if o.server != "https://fleetreeve.example.com:8140" || o.secretFile != filepath.Join(dir, "keys/server.secret") || o.ca != "/abs/ca.pem" ||
		o.inventory != filepath.Join(dir, "inv.yaml") || o.rpm != filepath.Join(dir, "dist") || o.forks != 3 || o.noDiff || o.config != conf {
		t.Errorf("enroll: %+v", o)
	}
	if o, _ = parse("plan", nil); o.inventory != "" || o.rpm != "" || !o.noDiff || o.server == "" {
		t.Errorf("plan: %+v", o)
	}
	// The command line wins.
	if o, _ = parse("enroll", []string{"-server", "https://other:1", "-i", "x.yaml", "-forks", "9"}); o.server != "https://other:1" || o.inventory != "x.yaml" || o.forks != 9 {
		t.Errorf("flags do not override the file: %+v", o)
	}

	// Mistakes are reported, not ignored.
	for body, want := range map[string]string{
		"sever: x\n":             `unknown setting "sever"`,
		"enroll:\n  rmp: dist\n": `unknown setting "rmp"`,
		"forks: many\n":          "forks",
	} {
		os.WriteFile(conf, []byte(body), 0o644)
		if _, err := parse("enroll", nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want error containing %q, got %v", body, want, err)
		}
	}

	// Found by walking up from the working directory; "none" disables it.
	os.WriteFile(conf, []byte("server: https://found:1\n"), 0o644)
	t.Setenv("FLEETREEVE_CONFIG", "")
	sub := filepath.Join(dir, "a", "b")
	os.MkdirAll(sub, 0o755)
	t.Chdir(sub)
	if o, _ = parse("pull", nil); o.server != "https://found:1" {
		t.Errorf("not found from a subdirectory: %+v", o)
	}
	t.Setenv("FLEETREEVE_CONFIG", "none")
	if o, _ = parse("pull", nil); o.server != "" {
		t.Errorf("FLEETREEVE_CONFIG=none ignored: %+v", o)
	}
}
