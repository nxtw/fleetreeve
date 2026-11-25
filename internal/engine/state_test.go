package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nxtw/fleetreeve/internal/core"
	"github.com/nxtw/fleetreeve/internal/state"
)

func runNamed(t *testing.T, plan, prune bool, rs ...*core.Resource) (map[string]Result, Summary, error) {
	t.Helper()
	cat := &core.Catalog{Version: core.ProtocolVersion, Name: "test", Prune: prune, Resources: rs}
	got := map[string]Result{}
	sum, err := Run(core.NewCtx(t.Context(), nil, plan), cat, func(r Result) { got[r.ID] = r })
	return got, sum, err
}

func TestStatePruneAndDrift(t *testing.T) {
	t.Setenv("FLEETREEVE_STATE_DIR", t.TempDir())
	dir := t.TempDir()
	sub, mine, theirs := filepath.Join(dir, "sub"), filepath.Join(dir, "sub", "mine"), filepath.Join(dir, "theirs")
	os.WriteFile(theirs, []byte("old"), 0o644) // exists before fleetreeve manages it
	file := func(path, content string) *core.Resource {
		return res("file", path, map[string]any{"content": content}, nil, nil)
	}
	all := func() []*core.Resource {
		return []*core.Resource{res("file", sub, map[string]any{"ensure": "directory"}, nil, nil), file(mine, "a"), file(theirs, "b")}
	}
	exists := func(p string) bool { _, err := os.Lstat(p); return err == nil }

	if _, sum, err := runNamed(t, false, true, all()...); err != nil || sum.Changed != 3 {
		t.Fatalf("apply: %+v %v", sum, err)
	}
	st, _ := state.Load("test")
	if !st.Resources["file["+mine+"]"].Created || st.Resources["file["+theirs+"]"].Created || st.Serial != 1 {
		t.Fatalf("state: %+v", st.Resources)
	}
	if b, _ := os.ReadFile(filepath.Join(state.Dir(), "test.json")); strings.Contains(string(b), `"content"`) {
		t.Error("state stores attribute values")
	}

	// Same manifest, node changed: drift. Changed manifest: not drift.
	os.WriteFile(mine, []byte("tampered"), 0o644)
	got, _, _ := runNamed(t, true, true, all()...)
	if !got["file["+mine+"]"].Drifted {
		t.Error("out-of-band change not flagged as drift")
	}
	got, _, _ = runNamed(t, true, true, res("file", sub, map[string]any{"ensure": "directory"}, nil, nil), file(mine, "new"), file(theirs, "b"))
	if got["file["+mine+"]"].Drifted {
		t.Error("manifest change flagged as drift")
	}

	// Dropping everything: plan reports it and changes nothing...
	got, sum, _ := runNamed(t, true, true)
	if sum.Pending != 2 || sum.Forgotten != 1 || got["file["+theirs+"]"].Orphan != "forget" || !exists(mine) {
		t.Errorf("plan prune: %+v %+v", sum, got)
	}
	// ...-no-prune leaves both node and state alone...
	if _, sum, _ = runNamed(t, false, false); sum.Total != 0 || !exists(mine) {
		t.Errorf("no-prune: %+v", sum)
	}
	// ...and apply removes what fleetreeve created (file before its directory)
	// while leaving what it merely adopted.
	if _, sum, _ = runNamed(t, false, true); sum.Changed != 2 || sum.Failed != 0 {
		t.Errorf("prune: %+v", sum)
	}
	if exists(sub) || !exists(theirs) {
		t.Errorf("after prune: sub=%v theirs=%v", exists(sub), exists(theirs))
	}
	if st, _ = state.Load("test"); len(st.Resources) != 0 {
		t.Errorf("state not emptied: %+v", st.Resources)
	}
}

func TestStateLock(t *testing.T) {
	t.Setenv("FLEETREEVE_STATE_DIR", t.TempDir())
	held, err := state.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runNamed(t, false, true); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Errorf("apply under a held lock: %v", err)
	}
	if _, _, err := runNamed(t, true, true); err != nil {
		t.Errorf("plan must not need the lock: %v", err)
	}
	held.Close()
	if _, _, err := runNamed(t, false, true); err != nil {
		t.Errorf("apply after release: %v", err)
	}
}
