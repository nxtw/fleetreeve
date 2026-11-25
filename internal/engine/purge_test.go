package engine

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/nxtw/fleetreeve/internal/core"
	"github.com/nxtw/fleetreeve/internal/state"
)

func tree(t *testing.T, dir string) string {
	t.Helper()
	var out []string
	filepath.Walk(dir, func(p string, st os.FileInfo, _ error) error {
		if rel, _ := filepath.Rel(dir, p); rel != "." {
			if st.IsDir() {
				rel += "/"
			}
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return strings.Join(out, " ")
}

// A purged directory holds only what the catalog manages.
func TestPurge(t *testing.T) {
	t.Setenv("FLEETREEVE_STATE_DIR", t.TempDir())
	dir := filepath.Join(t.TempDir(), "conf.d")
	put := func(rel, body string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("managed.conf", "old")
	put("stray.conf", "stray")
	put("backup.rpmsave", "kept by a pattern")
	put("sub/managed-below.conf", "x")
	put("sub/stray-below.conf", "left: no recurse")
	put("tree/a", "a")
	put("tree/deep/b", "b")
	os.Symlink("/nonexistent", filepath.Join(dir, "dangling"))

	id := "file[" + dir + "]"
	run := func(plan bool, extra map[string]any) (Result, Summary) {
		t.Helper()
		attrs := map[string]any{"ensure": "directory", "purge": true, "ignore": []any{"*.rpmsave"},
			"keep": []any{filepath.Join(dir, "managed.conf"), filepath.Join(dir, "sub", "managed-below.conf")}}
		for k, v := range extra {
			attrs[k] = v
		}
		got, sum, err := runNamed(t, plan, false,
			res("file", dir, attrs, nil, nil),
			res("file", filepath.Join(dir, "managed.conf"), map[string]any{"content": "new"}, nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		return got[id], sum
	}

	// A dry run says exactly what would go, and removes nothing.
	before := tree(t, dir)
	r, _ := run(true, nil)
	if len(r.Changes) != 1 || r.Changes[0].Prop != "purge" || r.Changes[0].From != "2 unmanaged (dangling, stray.conf)" || r.Changes[0].Diff != "-dangling\n-stray.conf\n" {
		t.Fatalf("plan: %+v", r.Changes)
	}
	if tree(t, dir) != before {
		t.Fatal("the dry run removed something")
	}
	// Apply: stray files and links go; managed files, what a pattern
	// protects, and directories (without force) stay - also the stray
	// file in a kept subdirectory, without recurse.
	if r, _ = run(false, nil); r.Status != Changed {
		t.Fatalf("apply: %+v", r)
	}
	if got, want := tree(t, dir), "backup.rpmsave managed.conf sub/ sub/managed-below.conf sub/stray-below.conf tree/ tree/a tree/deep/ tree/deep/b"; got != want {
		t.Errorf("after purge:\n got %s\nwant %s", got, want)
	}
	if _, sum := run(false, nil); sum.Drift() {
		t.Errorf("not in sync after a purge: %+v", sum)
	}
	// recurse purges inside kept subdirectories; force removes whole trees.
	if r, _ = run(false, map[string]any{"recurse": true, "force": true}); !strings.Contains(r.Changes[0].From, "sub/stray-below.conf") || !strings.Contains(r.Changes[0].From, "tree/") {
		t.Fatalf("recurse and force: %+v", r.Changes)
	}
	if got, want := tree(t, dir), "backup.rpmsave managed.conf sub/ sub/managed-below.conf"; got != want {
		t.Errorf("after recurse and force:\n got %s\nwant %s", got, want)
	}
	// A rollback puts back what the last purge removed, trees included.
	back, err := Rollback(core.NewCtx(t.Context(), nil, false), RollbackRequest{Key: state.Key("test", ""), IDs: []string{id}})
	if err != nil || len(back) != 1 || back[0].Error != "" {
		t.Fatalf("rollback: %+v %v", back, err)
	}
	if got, want := tree(t, dir), "backup.rpmsave managed.conf sub/ sub/managed-below.conf sub/stray-below.conf tree/ tree/a tree/deep/ tree/deep/b"; got != want {
		t.Errorf("after rollback:\n got %s\nwant %s", got, want)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "tree", "deep", "b")); string(b) != "b" {
		t.Errorf("restored content: %q", b)
	}
}

// Purge is refused where it makes no sense or would wreck the system.
func TestPurgeValidation(t *testing.T) {
	for want, r := range map[string]map[string]any{
		"needs ensure: directory":   {"path": "/srv/app/x", "purge": true},
		"would remove the system":   {"path": "/etc", "ensure": "directory", "purge": true},
		"only means something with": {"path": "/srv/app", "ensure": "directory", "ignore": []any{"*.bak"}},
		"is not a valid pattern":    {"path": "/srv/app", "ensure": "directory", "purge": true, "ignore": []any{"[x"}},
	} {
		_, _, err := runNamed(t, true, false, res("file", "t", r, nil, nil))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error with %q, got %v", want, err)
		}
	}
}
