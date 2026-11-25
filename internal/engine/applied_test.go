package engine

import (
	"path/filepath"
	"testing"
	"time"
)

// Results say what the state knows of each resource: when fleetreeve last
// changed it and whether fleetreeve created it - on every run, not only the
// one that made the change.
func TestResultsDescribeWhatIsApplied(t *testing.T) {
	t.Setenv("FLEETREEVE_STATE_DIR", t.TempDir())
	dir := t.TempDir()
	made, found := filepath.Join(dir, "made"), dir
	rs := func() (a, b Result) {
		got, _, err := runNamed(t, false, false,
			res("file", made, map[string]any{"content": "x"}, nil, nil),
			res("file", found, map[string]any{"ensure": "directory"}, nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		return got["file["+made+"]"], got["file["+found+"]"]
	}
	a, b := rs()
	if !a.Created || a.Since == "" {
		t.Errorf("a file fleetreeve created: %+v", a)
	}
	if b.Created || b.Since != "" {
		t.Errorf("a directory that was already there: %+v", b)
	}
	// A later run that changes nothing still says so.
	again, _ := rs()
	if again.Status != OK || !again.Created || again.Since != a.Since {
		t.Errorf("second run: %+v, first %+v", again, a)
	}
	if _, err := time.Parse(time.RFC3339, again.Since); err != nil {
		t.Error(err)
	}
	// So does a dry run.
	got, _, _ := runNamed(t, true, false, res("file", made, map[string]any{"content": "y"}, nil, nil))
	if p := got["file["+made+"]"]; p.Status != Pending || p.Since != a.Since || !p.Created {
		t.Errorf("dry run: %+v", p)
	}
}
