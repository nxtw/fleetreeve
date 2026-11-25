package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nxtw/fleetreeve/internal/core"
	"github.com/nxtw/fleetreeve/internal/state"
)

// A config file that a "service" (an exec handler that fails on bad
// content, standing in for a restart) reads. With rollback: true on the
// file, a change the service cannot live with is undone.
func rollbackCatalog(conf, log, content string, rollback bool) []*core.Resource {
	file := res("file", conf, map[string]any{"content": content, "mode": "0640"}, nil, []string{"exec[restart]"})
	file.Rollback = rollback
	restart := res("exec", "restart", map[string]any{
		"command":     "echo \"started with $(cat " + conf + ")\" >> " + log + "; grep -q good " + conf,
		"refreshonly": true}, nil, []string{"exec[health]"})
	health := res("exec", "health", map[string]any{"command": "grep -q good " + conf, "refreshonly": true}, nil, nil)
	after := res("file", conf+".after", map[string]any{"content": "x"}, []string{"file[" + conf + "]"}, nil)
	return []*core.Resource{file, restart, health, after}
}

func TestAutomaticRollback(t *testing.T) {
	t.Setenv("FLEETREEVE_STATE_DIR", t.TempDir())
	dir := t.TempDir()
	conf, log := filepath.Join(dir, "app.conf"), filepath.Join(dir, "restarts.log")
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }

	if _, sum, err := runNamed(t, false, true, rollbackCatalog(conf, log, "good v1\n", true)...); err != nil || sum.Failed != 0 || sum.Changed != 2 {
		t.Fatalf("first apply: %+v %v", sum, err)
	}

	// A bad change: the restart fails, so the file goes back to v1 and the
	// service is started once more on the restored file.
	got, sum, _ := runNamed(t, false, true, rollbackCatalog(conf, log, "bad v2\n", true)...)
	if read(conf) != "good v1\n" {
		t.Fatalf("file not restored: %q", read(conf))
	}
	if st, _ := os.Stat(conf); st.Mode().Perm() != 0o640 {
		t.Errorf("mode after restore: %v", st.Mode())
	}
	back := got["file["+conf+"]"] // the last event for the file
	if back.Status != RolledBack || !strings.Contains(back.Error, "exec[restart] failed") || !strings.Contains(back.Error, "exec[restart] refreshed on the restored state") {
		t.Errorf("rollback event: %+v", back)
	}
	if sum.RolledBack != 1 || sum.Changed != 0 || sum.Failed != 1 {
		t.Errorf("summary: %+v", sum)
	}
	if got["file["+conf+".after]"].Status != Skipped {
		t.Errorf("a resource depending on the rolled-back file was not skipped: %+v", got["file["+conf+".after]"])
	}
	if lines := strings.Split(strings.TrimSpace(read(log)), "\n"); len(lines) != 3 || lines[1] != "started with bad v2" || lines[2] != "started with good v1" {
		t.Errorf("restart log: %q", lines)
	}
	// The state still describes v1, so the next run tries v2 again rather
	// than believing it was applied.
	if got, _, _ = runNamed(t, true, true, rollbackCatalog(conf, log, "bad v2\n", true)...); got["file["+conf+"]"].Status != Pending {
		t.Errorf("after a rollback the change must still be pending: %+v", got["file["+conf+"]"])
	}

	// Without rollback: true the bad file stays, as before.
	_, sum, _ = runNamed(t, false, true, rollbackCatalog(conf, log, "bad v3\n", false)...)
	if read(conf) != "bad v3\n" || sum.RolledBack != 0 || sum.Failed != 1 {
		t.Errorf("without rollback: %q %+v", read(conf), sum)
	}
}

func TestRollbackFurtherDownTheChain(t *testing.T) {
	t.Setenv("FLEETREEVE_STATE_DIR", t.TempDir())
	dir := t.TempDir()
	conf := filepath.Join(dir, "app.conf")
	// The restart succeeds; a health check notified by it does not.
	cat := func(content string) []*core.Resource {
		file := res("file", conf, map[string]any{"content": content}, nil, []string{"exec[restart]"})
		file.Rollback = true
		restart := res("exec", "restart", map[string]any{"command": "true", "refreshonly": true}, nil, []string{"exec[health]"})
		health := res("exec", "health", map[string]any{"command": "grep -q healthy " + conf, "refreshonly": true}, nil, nil)
		return []*core.Resource{file, restart, health}
	}
	runNamed(t, false, true, cat("healthy\n")...)
	got, sum, _ := runNamed(t, false, true, cat("sick\n")...)
	if b, _ := os.ReadFile(conf); string(b) != "healthy\n" || sum.RolledBack != 1 {
		t.Errorf("content %q, summary %+v", b, sum)
	}
	if e := got["file["+conf+"]"].Error; !strings.Contains(e, "exec[health] failed") || !strings.Contains(e, "exec[health] refreshed on the restored state") {
		t.Errorf("event: %q", e)
	}
}

func TestManualRollbackHoldAndRelease(t *testing.T) {
	t.Setenv("FLEETREEVE_STATE_DIR", t.TempDir())
	dir := t.TempDir()
	conf, log := filepath.Join(dir, "app.conf"), filepath.Join(dir, "reloads.log")
	cat := func(content string) []*core.Resource {
		return []*core.Resource{
			res("file", conf, map[string]any{"content": content}, nil, []string{"exec[reload]"}),
			res("exec", "reload", map[string]any{"command": "cat " + conf + " >> " + log, "refreshonly": true}, nil, nil),
		}
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	id := "file[" + conf + "]"
	ctx := core.NewCtx(t.Context(), nil, false)
	key := state.Key("test", "")

	for _, v := range []string{"v1\n", "v2\n", "v3\n"} {
		runNamed(t, false, true, cat(v)...)
	}
	// Listing shows what is kept; a dry run changes nothing.
	list, _ := Rollback(ctx, RollbackRequest{Key: key})
	if len(list) != 1 || list[0].ID != id || list[0].Remaining != 3 {
		t.Fatalf("list: %+v", list)
	}
	if out, _ := Rollback(ctx, RollbackRequest{Key: key, IDs: []string{id}, Plan: true}); out[0].Status != "would-restore" || read(conf) != "v3\n" {
		t.Fatalf("dry run: %+v, content %q", out, read(conf))
	}

	// Roll back: v2 is restored, the handler it notifies runs, and the
	// resource is held so the next run does not put v3 straight back.
	out, err := Rollback(ctx, RollbackRequest{Key: key, IDs: []string{id}})
	if err != nil || out[0].Status != "restored" || read(conf) != "v2\n" || len(out[0].Refreshed) != 1 || out[0].Held == "" {
		t.Fatalf("rollback: %+v %v, content %q", out, err, read(conf))
	}
	if !strings.HasSuffix(read(log), "v3\nv2\n") {
		t.Errorf("handler did not run on the restored file: %q", read(log))
	}
	got, sum, _ := runNamed(t, false, true, cat("v3\n")...)
	if !got[id].Held || read(conf) != "v2\n" || sum.Held != 1 || sum.Changed != 0 {
		t.Errorf("held resource was changed: %+v %+v", got[id], sum)
	}
	// A second rollback goes one version further back; then nothing before
	// the file existed... except its absence, which is the first version.
	Rollback(ctx, RollbackRequest{Key: key, IDs: []string{id}})
	if read(conf) != "v1\n" {
		t.Errorf("second rollback: %q", read(conf))
	}
	Rollback(ctx, RollbackRequest{Key: key, IDs: []string{id}})
	if _, err := os.Stat(conf); err == nil {
		t.Error("third rollback should remove the file fleetreeve created")
	}
	if out, _ = Rollback(ctx, RollbackRequest{Key: key, IDs: []string{id}}); out[0].Status != "failed" {
		t.Errorf("rollback beyond the first version: %+v", out)
	}

	// Release: the next run converges on the manifest again.
	if out, _ = Rollback(ctx, RollbackRequest{Key: key, IDs: []string{id}, Release: true}); out[0].Status != "released" {
		t.Fatalf("release: %+v", out)
	}
	if _, sum, _ = runNamed(t, false, true, cat("v3\n")...); read(conf) != "v3\n" || sum.Changed != 1 {
		t.Errorf("after release: %q %+v", read(conf), sum)
	}
	if out, _ = Rollback(ctx, RollbackRequest{Key: key, IDs: []string{"file[/nope]"}}); out[0].Status != "failed" {
		t.Errorf("unknown resource: %+v", out)
	}
	// Kept versions are private: they are full copies of old content.
	root := versionsRoot(key)
	filepath.Walk(root, func(p string, info os.FileInfo, _ error) error {
		if info != nil && info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is readable by others: %v", p, info.Mode())
		}
		return nil
	})
}

func TestVersionRetention(t *testing.T) {
	t.Setenv("FLEETREEVE_STATE_DIR", t.TempDir())
	conf := filepath.Join(t.TempDir(), "f")
	for i := 0; i < keepVersions+4; i++ {
		runNamed(t, false, true, res("file", conf, map[string]any{"content": strings.Repeat("x", i+1)}, nil, nil))
	}
	if vs, _ := loadVersions(state.Key("test", ""), "file["+conf+"]"); len(vs) != keepVersions {
		t.Errorf("kept %d versions, want %d", len(vs), keepVersions)
	}
}
