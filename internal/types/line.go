package types

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nxtw/fleetreeve/internal/core"
)

func init() { core.Register(lineType{}) }

type lineType struct{}

func (lineType) Name() string { return "line" }
func (lineType) Doc() string {
	return "Ensures a single line is present in (or absent from) a file that is not managed as a whole."
}

func (lineType) Schema() core.Schema {
	return core.Schema{
		"path":   {Kind: core.String, Required: true, Doc: "File to edit."},
		"line":   {Kind: core.String, Doc: "The exact line. Required unless ensure: absent with match."},
		"match":  {Kind: core.String, Doc: "Regular expression; the last matching line is replaced (present) or all matching lines are removed (absent)."},
		"ensure": {Kind: core.String, Default: "present", Enum: []string{"present", "absent"}},
		"create": {Kind: core.Bool, Doc: "Create the file if it does not exist."},
	}
}

func (lineType) Validate(r *core.Resource) error {
	if !filepath.IsAbs(r.Str("path")) {
		return fmt.Errorf("path %q must be absolute", r.Str("path"))
	}
	if strings.Contains(r.Str("line"), "\n") {
		return fmt.Errorf("line must not contain a newline")
	}
	if !r.Has("line") && (r.Str("ensure") == "present" || !r.Has("match")) {
		return fmt.Errorf("line is required")
	}
	if m := r.Str("match"); m != "" {
		if _, err := regexp.Compile(m); err != nil {
			return fmt.Errorf("match: %v", err)
		}
	}
	return nil
}

func (lineType) AutoRequire(r *core.Resource) []string {
	return []string{"file[" + r.Str("path") + "]"}
}

// A line inside a file whose whole content is managed elsewhere would be
// removed and re-added on every run.
func (lineType) Conflicts(r *core.Resource, lookup func(string) *core.Resource) error {
	if f := lookup("file[" + r.Str("path") + "]"); f != nil && (f.Has("content") || f.Has("content_b64")) {
		return fmt.Errorf("%s (%s) already manages the full content of %s; put the line in that content instead", f.ID(), f.Origin, r.Str("path"))
	}
	return nil
}

// A line is undone by putting the whole file back.
func (lineType) Snapshot(_ *core.Ctx, r *core.Resource) (core.Snapshot, error) {
	return snapshotPath(r.Str("path"))
}

func (lineType) Restore(ctx *core.Ctx, r *core.Resource, s core.Snapshot) error {
	return restorePath(ctx, r.Str("path"), s)
}

// edit returns the current and desired content of the file.
func (lineType) edit(ctx *core.Ctx, r *core.Resource) (old, new string, err error) {
	b, err := os.ReadFile(r.Str("path"))
	if err != nil {
		// In a dry run the file may be created by an earlier resource.
		if !os.IsNotExist(err) || !(r.Bool("create") || ctx.Plan) {
			return "", "", err
		}
	}
	old = string(b)
	lines := strings.Split(strings.TrimSuffix(old, "\n"), "\n")
	if old == "" {
		lines = nil
	}
	want := r.Str("line")
	var re *regexp.Regexp
	if m := r.Str("match"); m != "" {
		re = regexp.MustCompile(m)
	}
	if r.Str("ensure") == "absent" {
		kept := lines[:0:0]
		for _, l := range lines {
			if (r.Has("line") && l == want) || (re != nil && re.MatchString(l)) {
				continue
			}
			kept = append(kept, l)
		}
		lines = kept
	} else {
		exact, last := false, -1
		for i, l := range lines {
			if l == want {
				exact = true
			}
			if re != nil && re.MatchString(l) {
				last = i
			}
		}
		switch {
		case exact:
		case last >= 0:
			lines[last] = want
		default:
			lines = append(lines, want)
		}
	}
	if len(lines) > 0 {
		new = strings.Join(lines, "\n") + "\n"
	}
	if strings.TrimSuffix(old, "\n") == strings.TrimSuffix(new, "\n") {
		new = old // don't rewrite a file only to fix its final newline
	}
	return old, new, nil
}

func (t lineType) Check(ctx *core.Ctx, r *core.Resource) ([]core.Change, error) {
	old, new, err := t.edit(ctx, r)
	if err != nil || old == new {
		return nil, err
	}
	to := r.Str("ensure")
	from := "absent"
	if to == "absent" {
		from = "present"
	}
	return []core.Change{{Prop: "line", From: from, To: to, Diff: core.Diff(old, new)}}, nil
}

func (t lineType) Apply(ctx *core.Ctx, r *core.Resource, _ []core.Change) error {
	_, new, err := t.edit(ctx, r)
	if err != nil {
		return err
	}
	return atomicWrite(ctx, r.Str("path"), []byte(new), "", 0, false)
}
