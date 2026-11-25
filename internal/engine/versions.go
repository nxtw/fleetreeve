package engine

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/nxtw/fleetreeve/internal/core"
	"github.com/nxtw/fleetreeve/internal/state"
)

// Versions. Whenever fleetreeve changes a resource whose type can be undone, it
// first keeps a copy of what was there. Those copies are what `fleetreeve
// rollback` restores. They live next to the node's state, readable by root
// only (a kept file is a full copy of its old content), and the newest few
// per resource are retained.

const keepVersions = 5

// Version is the state of a resource before one change.
type Version struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Title    string         `json:"title"`
	Attrs    map[string]any `json:"attrs"` // enough to address the resource; no desired content
	Time     string         `json:"time"`  // when the change was made
	Changes  []core.Change  `json:"changes"`
	Snapshot core.Snapshot  `json:"snapshot"`
	// Refresh lists what the resource notified, so that restoring a
	// configuration file can also restart the service that reads it.
	Refresh []*core.Resource `json:"refresh,omitempty"`
}

func versionsRoot(key string) string { return filepath.Join(state.Dir(), "versions", key) }

func versionDir(key, id string) string {
	return filepath.Join(versionsRoot(key), fmt.Sprintf("%x", sha1.Sum([]byte(id))))
}

// identity strips what a version must not carry along: the desired content
// of the change that replaced it.
func identity(r *core.Resource) map[string]any {
	out := map[string]any{}
	for k, v := range r.Attrs {
		switch k {
		case "content", "content_b64", "source", "template":
		default:
			out[k] = v
		}
	}
	return out
}

// saveVersion keeps a version and returns the file it is in.
func saveVersion(key string, v *Version) (string, error) {
	dir := versionDir(key, v.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	name := filepath.Join(dir, fmt.Sprintf("%020d.json", time.Now().UnixNano()))
	if err := os.WriteFile(name, b, 0o600); err != nil {
		return "", err
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	sort.Strings(files)
	for len(files) > keepVersions {
		os.Remove(files[0])
		files = files[1:]
	}
	return name, nil
}

// loadVersions returns a resource's kept versions, newest first, with the
// files that hold them.
func loadVersions(key, id string) ([]Version, []string) {
	files, _ := filepath.Glob(filepath.Join(versionDir(key, id), "*.json"))
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	var out []Version
	var kept []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var v Version
		if json.Unmarshal(b, &v) == nil && v.ID == id {
			out, kept = append(out, v), append(kept, f)
		}
	}
	return out, kept
}

// allVersions returns every resource that has kept versions.
func allVersions(key string) map[string][]Version {
	out := map[string][]Version{}
	dirs, _ := filepath.Glob(filepath.Join(versionsRoot(key), "*"))
	for _, d := range dirs {
		files, _ := filepath.Glob(filepath.Join(d, "*.json"))
		if len(files) == 0 {
			continue
		}
		b, err := os.ReadFile(files[0])
		var v Version
		if err != nil || json.Unmarshal(b, &v) != nil {
			continue
		}
		if vs, _ := loadVersions(key, v.ID); len(vs) > 0 {
			out[v.ID] = vs
		}
	}
	return out
}

// ---- manual rollback ----

// RollbackRequest asks a node to restore resources to their previous
// version, to release them again, or just to list what it has kept.
type RollbackRequest struct {
	Key     string   `json:"key"` // state key: manifest name and host
	IDs     []string `json:"ids"`
	Release bool     `json:"release,omitempty"`
	Plan    bool     `json:"plan,omitempty"`
}

// RollbackResult is the outcome for one resource.
type RollbackResult struct {
	ID string `json:"id"`
	// Status is restored, would-restore, released, kept (when listing) or failed.
	Status    string        `json:"status"`
	Time      string        `json:"time,omitempty"`    // when the undone change was made
	Changes   []core.Change `json:"changes,omitempty"` // that change; restoring reverses it
	Refreshed []string      `json:"refreshed,omitempty"`
	Remaining int           `json:"remaining"`      // older versions still kept
	Held      string        `json:"held,omitempty"` // since when the resource is held
	Error     string        `json:"error,omitempty"`
}

// Rollback restores resources by hand. A restored resource is put on hold:
// later runs leave it alone until it is released, because the manifest
// still asks for the state that was just rolled back and would otherwise
// bring it straight back.
func Rollback(ctx *core.Ctx, req RollbackRequest) ([]RollbackResult, error) {
	if len(req.IDs) == 0 { // list
		st, err := state.Load(req.Key)
		if err != nil {
			return nil, err
		}
		var out []RollbackResult
		for id, vs := range allVersions(req.Key) {
			res := RollbackResult{ID: id, Status: "kept", Time: vs[0].Time, Changes: vs[0].Changes, Remaining: len(vs)}
			if e := st.Resources[id]; e != nil {
				res.Held = e.Hold
			}
			out = append(out, res)
		}
		// Held resources whose versions are used up still need releasing.
		for id, e := range st.Resources {
			if _, listed := allVersions(req.Key)[id]; !listed && e.Hold != "" {
				out = append(out, RollbackResult{ID: id, Status: "kept", Held: e.Hold})
			}
		}
		sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
		return out, nil
	}
	if req.Plan {
		var out []RollbackResult
		for _, id := range req.IDs {
			vs, _ := loadVersions(req.Key, id)
			if len(vs) == 0 {
				out = append(out, RollbackResult{ID: id, Status: "failed", Error: "no earlier version is kept for this resource"})
				continue
			}
			out = append(out, RollbackResult{ID: id, Status: "would-restore", Time: vs[0].Time, Changes: vs[0].Changes, Remaining: len(vs) - 1})
		}
		return out, nil
	}
	store, err := state.Open(req.Key)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	defer store.Save()
	var out []RollbackResult
	for _, id := range req.IDs {
		res := RollbackResult{ID: id}
		entry := store.Resources[id]
		if req.Release {
			if entry == nil || entry.Hold == "" {
				res.Status, res.Error = "failed", "this resource is not on hold"
			} else {
				entry.Hold, res.Status = "", "released"
			}
			out = append(out, res)
			continue
		}
		vs, files := loadVersions(req.Key, id)
		if len(vs) == 0 {
			res.Status, res.Error = "failed", "no earlier version is kept for this resource"
			out = append(out, res)
			continue
		}
		v := vs[0]
		res.Time, res.Changes, res.Remaining = v.Time, v.Changes, len(vs)-1
		r := &core.Resource{Type: v.Type, Title: v.Title, Attrs: v.Attrs}
		t, known := core.Lookup(v.Type)
		u, can := t.(core.Undoer)
		switch err := error(nil); {
		case !known || !can:
			res.Status, res.Error = "failed", "this kind of resource cannot be restored"
		default:
			if err = core.Normalize(r); err == nil {
				err = u.Restore(ctx, r, v.Snapshot)
			}
			if err != nil {
				res.Status, res.Error = "failed", err.Error()
				break
			}
			res.Status = "restored"
			os.Remove(files[0]) // a second rollback goes one version further back
			for _, n := range v.Refresh {
				nt, _ := core.Lookup(n.Type)
				rf, ok := nt.(core.Refresher)
				if !ok || core.Normalize(n) != nil {
					continue
				}
				if _, err := rf.Refresh(ctx, n, nil); err != nil {
					res.Error += fmt.Sprintf("restored, but refreshing %s failed: %v. ", n.ID(), err)
				} else {
					res.Refreshed = append(res.Refreshed, n.ID())
				}
			}
			if entry == nil {
				entry = &state.Entry{Type: v.Type, Title: v.Title}
				store.Resources[id] = entry
			}
			entry.Hold = time.Now().UTC().Format(time.RFC3339)
			entry.Digest = "" // no longer what the manifest last applied
			res.Held = entry.Hold
		}
		out = append(out, res)
	}
	return out, nil
}
