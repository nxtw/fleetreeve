package types

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nxtw/fleetreeve/internal/core"
)

// Purging: a directory declared with `purge: true` holds only what the
// catalog manages. Anything else in it is removed.
//
//	- file: /etc/nginx/conf.d
//	  ensure: directory
//	  purge: true
//	  ignore: ["*.rpmsave"]
//
// What counts as managed is decided when compiling: the paths of every
// resource in the catalog that lie in the directory (the compiler puts
// them in the internal attribute `keep`). An entry is left alone when it
//
//   - is managed itself, or holds something managed (a subdirectory with a
//     managed file in it);
//   - matches one of the `ignore` patterns;
//   - is a directory, and `force` is not set - removing a whole tree is
//     asked for explicitly, as elsewhere in this type.
//
// Only the directory's own entries are looked at; `recurse: true` also
// purges inside the subdirectories that are kept.

// noPurge are directories whose unmanaged content is the system itself.
var noPurge = map[string]bool{"/": true, "/bin": true, "/boot": true, "/dev": true, "/etc": true, "/home": true, "/lib": true,
	"/lib64": true, "/opt": true, "/proc": true, "/root": true, "/run": true, "/sbin": true, "/srv": true, "/sys": true,
	"/tmp": true, "/usr": true, "/usr/bin": true, "/usr/lib": true, "/usr/lib64": true, "/usr/sbin": true, "/usr/share": true, "/var": true, "/var/lib": true}

func validatePurge(r *core.Resource) error {
	purge := r.Bool("purge")
	for _, k := range []string{"ignore", "recurse"} {
		if r.Has(k) && !purge {
			return fmt.Errorf("%s only means something with purge: true", k)
		}
	}
	if !purge {
		return nil
	}
	if r.Str("ensure") != "directory" {
		return fmt.Errorf("purge is for directories: it needs ensure: directory")
	}
	if p := filepath.Clean(r.Str("path")); noPurge[p] {
		return fmt.Errorf("purge on %s would remove the system itself; purge a directory of your own below it", p)
	}
	for _, pat := range r.List("ignore") {
		if _, err := filepath.Match(pat, "x"); err != nil {
			return fmt.Errorf("ignore: %q is not a valid pattern", pat)
		}
	}
	return nil
}

// unmanaged lists what purge would remove from the directory, as paths,
// sorted. Directories among them are whole trees.
func unmanaged(r *core.Resource) ([]string, error) {
	keep := map[string]bool{}
	for _, p := range r.List("keep") {
		keep[filepath.Clean(p)] = true
	}
	holds := func(dir string) bool { // something managed lies below dir
		for p := range keep {
			if strings.HasPrefix(p, dir+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
	ignore, recurse, force := r.List("ignore"), r.Bool("recurse"), r.Bool("force")
	var out []string
	var walk func(dir string) error
	walk = func(dir string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
	next:
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			for _, pat := range ignore {
				if ok, _ := filepath.Match(pat, e.Name()); ok {
					continue next
				}
			}
			isDir := e.IsDir() // false for a link to a directory: the link is what is in here
			switch {
			case keep[p] || holds(p):
				if isDir && recurse {
					if err := walk(p); err != nil {
						return err
					}
				}
			case isDir && !force:
			default:
				out = append(out, p)
			}
		}
		return nil
	}
	if err := walk(filepath.Clean(r.Str("path"))); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// purgeChange describes what purge would remove, or nil.
func purgeChange(r *core.Resource) (*core.Change, error) {
	if !r.Bool("purge") {
		return nil, nil
	}
	gone, err := unmanaged(r)
	if err != nil || len(gone) == 0 {
		return nil, err
	}
	base := filepath.Clean(r.Str("path"))
	names := make([]string, len(gone))
	var list strings.Builder
	for i, p := range gone {
		names[i], _ = filepath.Rel(base, p)
		if st, err := os.Lstat(p); err == nil && st.IsDir() {
			names[i] += "/"
		}
		list.WriteString("-" + names[i] + "\n")
	}
	short := names
	more := ""
	if len(short) > 8 {
		short, more = short[:8], fmt.Sprintf(" and %d more", len(names)-8)
	}
	return &core.Change{Prop: "purge", From: fmt.Sprintf("%d unmanaged (%s%s)", len(names), strings.Join(short, ", "), more), To: "removed", Diff: list.String()}, nil
}

func purge(r *core.Resource) error {
	gone, err := unmanaged(r)
	if err != nil {
		return err
	}
	for _, p := range gone {
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("purge: %v", err)
		}
	}
	return nil
}

// purgedSnapshot keeps what a purge is about to remove, so that a
// rollback can put it back: every file, link and directory, as paths
// relative to the directory. What is too large to keep is counted, not
// kept.
func purgedSnapshot(r *core.Resource) ([]any, int, error) {
	if !r.Bool("purge") {
		return nil, 0, nil
	}
	if st, err := os.Stat(r.Str("path")); err != nil || !st.IsDir() {
		return nil, 0, nil
	}
	gone, err := unmanaged(r)
	if err != nil {
		return nil, 0, err
	}
	base := filepath.Clean(r.Str("path"))
	var kept []any
	skipped, total := 0, int64(0)
	for _, top := range gone {
		filepath.Walk(top, func(p string, st os.FileInfo, err error) error {
			if err != nil {
				skipped++
				return nil
			}
			if st.Mode().IsRegular() {
				if total += st.Size(); total > maxSnapshot {
					skipped++
					return nil
				}
			}
			s, err := snapshotPath(p)
			if err != nil {
				skipped++
				return nil
			}
			rel, _ := filepath.Rel(base, p)
			kept = append(kept, map[string]any{"path": rel, "was": map[string]any(s)})
			return nil
		})
	}
	return kept, skipped, nil
}

// restorePurged puts back what a purge removed.
func restorePurged(ctx *core.Ctx, base string, list any) error {
	entries, _ := list.([]any)
	for _, e := range entries {
		m, _ := e.(map[string]any)
		rel, _ := m["path"].(string)
		was, _ := m["was"].(map[string]any)
		p := filepath.Join(base, rel)
		if rel == "" || was == nil || !strings.HasPrefix(p, filepath.Clean(base)+string(filepath.Separator)) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := restorePath(ctx, p, core.Snapshot(was)); err != nil {
			return err
		}
	}
	return nil
}
