package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nxtw/fleetreeve/internal/core"
	_ "github.com/nxtw/fleetreeve/internal/types"
)

func res(typ, title string, attrs map[string]any, require, notify []string) *core.Resource {
	return &core.Resource{Type: typ, Title: title, Attrs: attrs, Require: require, Notify: notify}
}

func runCat(t *testing.T, plan bool, rs ...*core.Resource) (map[string]Result, Summary) {
	t.Helper()
	cat := &core.Catalog{Version: core.ProtocolVersion, Resources: rs}
	got := map[string]Result{}
	sum, err := Run(core.NewCtx(context.Background(), nil, plan), cat, func(r Result) { got[r.ID] = r })
	if err != nil {
		t.Fatal(err)
	}
	return got, sum
}

func TestConvergeAndRefresh(t *testing.T) {
	dir := t.TempDir()
	conf, marker := filepath.Join(dir, "sub", "app.conf"), filepath.Join(dir, "restarts")
	catalog := func(content string) []*core.Resource {
		return []*core.Resource{
			res("exec", "restart", map[string]any{"command": "echo x >> " + marker, "refreshonly": true}, nil, nil),
			res("file", conf, map[string]any{"content": content, "mode": "0600"}, nil, []string{"exec[restart]"}),
			res("file", filepath.Dir(conf), map[string]any{"ensure": "directory"}, nil, nil),
		}
	}
	restarts := func() int { b, _ := os.ReadFile(marker); return len(b) / 2 }

	// Plan changes nothing.
	got, sum := runCat(t, true, catalog("v1\n")...)
	if sum.Pending != 2 || !got["exec[restart]"].Refresh {
		t.Errorf("plan: %+v", sum)
	}
	if _, err := os.Stat(filepath.Dir(conf)); err == nil {
		t.Fatal("plan modified the system")
	}

	// Apply: directory first (auto-require), file written, handler runs once.
	_, sum = runCat(t, false, catalog("v1\n")...)
	if sum.Changed != 2 || sum.Refreshed != 1 || sum.Failed != 0 {
		t.Errorf("apply: %+v", sum)
	}
	if b, _ := os.ReadFile(conf); string(b) != "v1\n" {
		t.Errorf("content %q", b)
	}
	if st, _ := os.Stat(conf); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}

	// Second apply is a no-op and does not fire the handler.
	_, sum = runCat(t, false, catalog("v1\n")...)
	if sum.Drift() || restarts() != 1 {
		t.Errorf("not idempotent: %+v, restarts=%d", sum, restarts())
	}

	// Drift in content and mode is corrected and the handler fires again.
	os.WriteFile(conf, []byte("tampered\n"), 0o644)
	os.Chmod(conf, 0o644)
	got, sum = runCat(t, false, catalog("v1\n")...)
	if len(got["file["+conf+"]"].Changes) != 2 || restarts() != 2 {
		t.Errorf("drift: %+v restarts=%d", got["file["+conf+"]"], restarts())
	}
}

func TestFailureIsolation(t *testing.T) {
	dir := t.TempDir()
	a, b, c := filepath.Join(dir, "a"), filepath.Join(dir, "b"), filepath.Join(dir, "c")
	got, sum := runCat(t, false,
		res("exec", "boom", map[string]any{"command": "echo nope >&2; exit 3"}, nil, nil),
		res("file", a, map[string]any{"content": "a"}, []string{"exec[boom]"}, nil),
		res("file", b, map[string]any{"content": "b"}, []string{"file[" + a + "]"}, nil),
		res("file", c, map[string]any{"content": "c"}, nil, nil),
	)
	if sum.Failed != 1 || sum.Skipped != 2 || sum.Changed != 1 {
		t.Errorf("summary: %+v", sum)
	}
	if got["exec[boom]"].Error != "command: exit status 3: nope" {
		t.Errorf("error: %q", got["exec[boom]"].Error)
	}
	if _, err := os.Stat(c); err != nil {
		t.Error("independent resource was not applied")
	}
	if _, err := os.Stat(a); err == nil {
		t.Error("dependent resource was applied after a failure")
	}
}

func TestLineAndLink(t *testing.T) {
	dir := t.TempDir()
	f, link := filepath.Join(dir, "sshd_config"), filepath.Join(dir, "link")
	os.WriteFile(f, []byte("Port 22\n#PasswordAuthentication yes\nX11 no\n"), 0o640)
	rs := func() []*core.Resource {
		return []*core.Resource{
			res("line", "pw", map[string]any{"path": f, "line": "PasswordAuthentication no", "match": `^#?PasswordAuthentication`}, nil, nil),
			res("line", "x11", map[string]any{"path": f, "line": "X11 no", "ensure": "absent"}, nil, nil),
			res("file", link, map[string]any{"ensure": "link", "target": f}, nil, nil),
		}
	}
	_, sum := runCat(t, false, rs()...)
	if sum.Changed != 3 {
		t.Errorf("summary: %+v", sum)
	}
	if b, _ := os.ReadFile(link); string(b) != "Port 22\nPasswordAuthentication no\n" {
		t.Errorf("content %q", b)
	}
	if st, _ := os.Stat(f); st.Mode().Perm() != 0o640 {
		t.Errorf("mode not preserved: %v", st.Mode())
	}
	if _, sum = runCat(t, false, rs()...); sum.Drift() {
		t.Errorf("not idempotent: %+v", sum)
	}
}
