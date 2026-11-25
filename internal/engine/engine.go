// Package engine converges a node onto a catalog.
package engine

import (
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/nxtw/fleetreeve/internal/core"
	"github.com/nxtw/fleetreeve/internal/state"
	"github.com/nxtw/fleetreeve/internal/types"
)

type Status string

const (
	OK      Status = "ok"      // already in the desired state
	Changed Status = "changed" // brought into the desired state
	Pending Status = "pending" // plan: differs from the desired state
	Failed  Status = "failed"
	Skipped Status = "skipped" // a dependency failed
	// RolledBack follows an earlier "changed" for the same resource: the
	// change was undone because something it notified failed.
	RolledBack Status = "rolledback"
)

// Result is the outcome of one resource.
type Result struct {
	ID      string        `json:"id"`
	Status  Status        `json:"status"`
	Changes []core.Change `json:"changes,omitempty"`
	Refresh bool          `json:"refresh,omitempty"` // refreshed (or, in plan, would be)
	// Drifted: the manifest is unchanged since the last apply, so the node
	// was changed by something other than fleetreeve.
	Drifted bool `json:"drifted,omitempty"`
	// Orphan is set for resources in the state that left the manifest:
	// "remove" (fleetreeve created it, so it is undone) or "forget" (it existed
	// before fleetreeve, so it is left in place and no longer tracked).
	Orphan string `json:"orphan,omitempty"`
	// Held: left alone because it was rolled back by hand and not yet
	// released (fleetreeve rollback -release).
	Held   bool   `json:"held,omitempty"`
	Error  string `json:"error,omitempty"`
	Origin string `json:"origin,omitempty"`
	// Since is when fleetreeve last changed the resource on this host (empty:
	// it never had to), and Created whether fleetreeve brought it into being.
	// Both come from the state, so they describe what is applied, not
	// just this run.
	Since   string `json:"since,omitempty"`
	Created bool   `json:"created,omitempty"`
	Millis  int64  `json:"ms"`
}

// applied copies what the state knows about the resource into the result.
func (r *Result) applied(e *state.Entry) {
	if e != nil {
		r.Since, r.Created = e.Changed, e.Created
	}
}

type Summary struct {
	Total     int `json:"total"`
	OK        int `json:"ok"`
	Changed   int `json:"changed"`
	Pending   int `json:"pending"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
	Refreshed int `json:"refreshed"`
	Forgotten int `json:"forgotten"`
	// RolledBack counts changes undone in this run; Held, resources left
	// alone after a manual rollback.
	RolledBack int   `json:"rolled_back,omitempty"`
	Held       int   `json:"held,omitempty"`
	Millis     int64 `json:"ms"`
}

// Drift reports whether the node differed from its catalog.
func (s Summary) Drift() bool { return s.Changed+s.Pending+s.Refreshed > 0 }

// Run walks the catalog in dependency order. In plan mode nothing is
// modified. A failure never aborts the run: resources depending on the
// failed one are skipped and everything else still converges.
func Run(ctx *core.Ctx, cat *core.Catalog, emit func(Result)) (sum Summary, err error) {
	start := time.Now()
	if cat.Version != core.ProtocolVersion {
		return sum, fmt.Errorf("catalog version %d does not match agent version %d", cat.Version, core.ProtocolVersion)
	}
	// Types that came with the catalog (from modules) join the built-in
	// ones for this run.
	for name, def := range cat.Types {
		def.Name = name
		t, err := types.NewScript(def)
		if err != nil {
			return sum, err
		}
		core.Register(t)
	}
	if err := cat.Normalize(); err != nil {
		return sum, err
	}
	g, err := cat.Graph()
	if err != nil {
		return sum, err
	}
	// State: plan reads it; apply locks it and records the outcome, also
	// when the run fails or is interrupted part-way.
	st := &state.File{Resources: map[string]*state.Entry{}}
	key := state.Key(cat.Name, cat.Host)
	if cat.Name != "" && ctx.Plan {
		if st, err = state.Load(key); err != nil {
			return sum, err
		}
	} else if cat.Name != "" {
		store, err := state.Open(key)
		if err != nil {
			return sum, err
		}
		defer store.Close()
		defer store.Save()
		st = store.File
	}
	count := func(res Result) {
		sum.Total++
		switch res.Status {
		case OK:
			sum.OK++
		case Changed:
			sum.Changed++
		case Pending:
			sum.Pending++
		case Failed:
			sum.Failed++
		case Skipped:
			sum.Skipped++
		}
		if res.Refresh {
			sum.Refreshed++
		}
		emit(res)
	}
	defer func() { sum.Millis = time.Since(start).Milliseconds() }()

	status := map[string]Status{}
	refresh := map[string]bool{}
	index := map[string]*core.Resource{}
	for _, r := range g.Order {
		index[r.ID()] = r
	}
	// For automatic rollback: what each rollback-enabled resource looked
	// like before this run changed it, and for every resource, which of
	// those changes led (through notifications) to it being refreshed.
	type undo struct {
		r       *core.Resource
		snap    core.Snapshot
		prev    *state.Entry
		version string // the kept version of this change
	}
	undoable := map[string]*undo{}
	var undoOrder []string
	causes := map[string][]string{}

	for i, r := range g.Order {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		t0 := time.Now()
		res := Result{ID: r.ID(), Origin: r.Origin}
		res.Status, res.Error = OK, ""
		for _, dep := range g.Deps[res.ID] {
			if s := status[dep]; s == Failed || s == Skipped {
				res.Status, res.Error = Skipped, fmt.Sprintf("dependency %s %s", dep, map[Status]string{Failed: "failed", Skipped: "was skipped"}[s])
				break
			}
		}
		prev, digest := st.Resources[res.ID], state.Digest(r)
		if res.Status != Skipped && prev != nil && prev.Hold != "" {
			// Rolled back by hand: leave it as it is until released.
			res.Held = true
			sum.Held++
			status[res.ID] = OK
			res.applied(prev)
			count(res)
			continue
		}
		var snap core.Snapshot
		if res.Status != Skipped {
			snap = run(ctx, r, &res, refresh[res.ID])
		}
		if res.Status == Changed && snap != nil {
			kept := ""
			if cat.Name != "" {
				v := &Version{ID: res.ID, Type: r.Type, Title: r.Title, Attrs: identity(r), Snapshot: snap,
					Time: time.Now().UTC().Format(time.RFC3339), Changes: stripDiffs(res.Changes), Refresh: refreshTargets(r, index)}
				kept, _ = saveVersion(key, v) // a version that cannot be kept must not fail the run
			}
			if r.Rollback {
				undoable[res.ID] = &undo{r, snap, prev, kept}
				undoOrder = append(undoOrder, res.ID)
			}
		}
		if res.Status == Changed || res.Status == Pending || res.Refresh {
			for _, n := range r.Notify {
				refresh[n] = true
				for _, c := range append(append([]string{}, causes[res.ID]...), res.ID) {
					if undoable[c] != nil && !contains(causes[n], c) {
						causes[n] = append(causes[n], c)
					}
				}
			}
		}
		status[res.ID] = res.Status
		res.Millis = time.Since(t0).Milliseconds()
		res.Drifted = len(res.Changes) > 0 && prev != nil && prev.Digest == digest
		if !ctx.Plan && (res.Status == OK || res.Status == Changed) {
			st.Resources[res.ID] = record(r, res, prev, digest, i)
		}
		res.applied(st.Resources[res.ID])
		count(res)

		// Something failed that was set off by rollback-enabled changes:
		// undo those changes, newest first, and refresh what they notified
		// once more so it runs on the restored state again.
		if res.Status != Failed || ctx.Plan {
			continue
		}
		for j := len(undoOrder) - 1; j >= 0; j-- {
			id := undoOrder[j]
			u := undoable[id]
			if u == nil || !contains(causes[res.ID], id) {
				continue
			}
			delete(undoable, id)
			back := Result{ID: id, Status: RolledBack, Origin: u.r.Origin}
			t, _ := core.Lookup(u.r.Type)
			msg := fmt.Sprintf("undone because %s failed", res.ID)
			if err := t.(core.Undoer).Restore(ctx, u.r, u.snap); err != nil {
				back.Status = Failed
				msg = fmt.Sprintf("%s failed, and undoing this change failed too: %v", res.ID, err)
			} else {
				for _, n := range refreshTargets(u.r, index) {
					if _, seen := status[n.ID()]; !seen {
						continue // not reached yet; it will see the restored state anyway
					}
					nt, _ := core.Lookup(n.Type)
					if _, err := nt.(core.Refresher).Refresh(ctx, n, nil); err != nil {
						msg += fmt.Sprintf("; %s still fails on the restored state: %v", n.ID(), err)
					} else {
						msg += fmt.Sprintf("; %s refreshed on the restored state", n.ID())
					}
				}
			}
			back.Error = msg
			if u.version != "" && back.Status == RolledBack {
				os.Remove(u.version) // the change it preceded no longer exists
			}
			// It is not in its desired state: forget that it was applied,
			// so the next run tries again and its dependents are skipped.
			if u.prev != nil {
				st.Resources[id] = u.prev
			} else {
				delete(st.Resources, id)
			}
			status[id] = Failed
			sum.Changed--
			if back.Status == RolledBack {
				sum.RolledBack++
			} else {
				sum.Failed++
			}
			emit(back)
		}
	}

	if !cat.Prune {
		return sum, nil
	}
	// Whatever is left in the state but no longer in the catalog was
	// dropped from the manifest. Undo in reverse order of creation, so
	// files go before the directory that holds them.
	var orphans []string
	for id := range st.Resources {
		if _, declared := status[id]; !declared {
			orphans = append(orphans, id)
		}
	}
	sort.Slice(orphans, func(a, b int) bool {
		ea, eb := st.Resources[orphans[a]], st.Resources[orphans[b]]
		if ea.Order != eb.Order {
			return ea.Order > eb.Order
		}
		return orphans[a] > orphans[b]
	})
	for _, id := range orphans {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		e := st.Resources[id]
		res := Result{ID: id, Status: OK, Orphan: "forget"}
		if e.Created && e.Absent != nil {
			res.Orphan = "remove"
			r := &core.Resource{Type: e.Type, Title: e.Title, Attrs: e.Absent}
			if err := core.Normalize(r); err != nil {
				res.Status, res.Error = Failed, err.Error()
			} else {
				run(ctx, r, &res, false)
			}
		}
		switch {
		case res.Orphan == "forget":
			sum.Forgotten++
			emit(res)
		case res.Status == OK: // already gone
		default:
			count(res)
		}
		if !ctx.Plan && res.Status != Failed {
			delete(st.Resources, id)
			if cat.Name != "" {
				os.RemoveAll(versionDir(key, id)) // nothing left to roll back to
			}
		}
	}
	return sum, nil
}

// record builds the state entry of a resource that is now in its desired
// state.
func record(r *core.Resource, res Result, prev *state.Entry, digest string, order int) *state.Entry {
	e := &state.Entry{Type: r.Type, Title: r.Title, Digest: digest, Order: order}
	if prev != nil {
		e.Created, e.Changed = prev.Created, prev.Changed
	}
	if res.Status == Changed {
		e.Changed = time.Now().UTC().Format(time.RFC3339)
		for _, c := range res.Changes {
			if c.Prop == "ensure" && c.From == "absent" {
				e.Created = true
			}
		}
	}
	if r.Str("ensure") == "absent" {
		e.Created = false // nothing to undo once the manifest says absent
	}
	t, _ := core.Lookup(r.Type)
	if rm, ok := t.(core.Remover); ok && e.Created {
		e.Absent = rm.Absent(r)
	}
	return e
}

// run checks one resource and, unless planning, applies and refreshes it.
// It returns what the resource looked like before it was changed, when
// its type can tell (nil otherwise, or when nothing changed).
func run(ctx *core.Ctx, r *core.Resource, res *Result, notified bool) (before core.Snapshot) {
	t, _ := core.Lookup(r.Type)
	fail := func(err error) { res.Status, res.Error = Failed, err.Error() }
	changes, err := t.Check(ctx, r)
	if err != nil {
		fail(err)
		return nil
	}
	res.Changes = changes
	switch {
	case len(changes) == 0:
	case ctx.Plan:
		res.Status = Pending
	default:
		if u, ok := t.(core.Undoer); ok {
			if before, err = u.Snapshot(ctx, r); err != nil {
				if r.Rollback {
					// Asked to be undoable: do not change what cannot be put back.
					fail(fmt.Errorf("not changed, because its current state could not be kept for rollback: %v", err))
					return nil
				}
				before = nil
			}
		}
		if err := t.Apply(ctx, r, changes); err != nil {
			if r.Rollback && before != nil {
				// A change that failed half-way is undone as well.
				if rerr := t.(core.Undoer).Restore(ctx, r, before); rerr != nil {
					err = fmt.Errorf("%v (and undoing it failed: %v)", err, rerr)
				} else {
					err = fmt.Errorf("%v (undone)", err)
				}
			}
			fail(err)
			return nil
		}
		res.Status = Changed
	}
	rf, ok := t.(core.Refresher)
	if !notified || !ok {
		return before
	}
	if ctx.Plan {
		res.Refresh = true
		return before
	}
	done, err := rf.Refresh(ctx, r, changes)
	if err != nil {
		fail(fmt.Errorf("refresh: %v", err))
		// The resource itself did change; whether that is undone is
		// decided by the caller, like any other failed refresh.
		return nil
	}
	res.Refresh = done
	return before
}

// refreshTargets returns the resources r notifies, directly or through
// further notifications, that react to being refreshed - in catalog order.
func refreshTargets(r *core.Resource, index map[string]*core.Resource) []*core.Resource {
	var out []*core.Resource
	seen := map[string]bool{r.ID(): true}
	queue := append([]string{}, r.Notify...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		n := index[id]
		if n == nil || seen[id] {
			continue
		}
		seen[id] = true
		if t, _ := core.Lookup(n.Type); t != nil {
			if _, ok := t.(core.Refresher); ok {
				out = append(out, n)
			}
		}
		queue = append(queue, n.Notify...)
	}
	return out
}

func stripDiffs(changes []core.Change) []core.Change {
	out := make([]core.Change, len(changes))
	for i, c := range changes {
		c.Diff = ""
		out[i] = c
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
