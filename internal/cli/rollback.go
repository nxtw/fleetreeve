package cli

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nxtw/fleetreeve/internal/compile"
	"github.com/nxtw/fleetreeve/internal/core"
	"github.com/nxtw/fleetreeve/internal/engine"
	"github.com/nxtw/fleetreeve/internal/remote"
	"github.com/nxtw/fleetreeve/internal/state"
)

// rollback restores resources on hosts to the version fleetreeve kept before it
// last changed them. A restored resource is held: runs leave it alone
// until `fleetreeve rollback -release`, because the manifest still asks for the
// state that was rolled back.
func rollback(ctx context.Context, o *options) (int, error) {
	if len(o.args) == 0 {
		return 1, fmt.Errorf("usage: fleetreeve rollback [-i inventory] [-l hosts] <manifest> [resource...] [-plan] [-release]")
	}
	name, err := compile.StateName(o.args[0])
	if err != nil {
		return 1, err
	}
	var ids []string
	for _, ref := range o.args[1:] {
		id, err := core.ParseRef(ref)
		if err != nil {
			return 1, err
		}
		ids = append(ids, id)
	}
	if o.release && len(ids) == 0 {
		return 1, fmt.Errorf("name the resources to release")
	}
	_, hosts, err := o.hosts()
	if err != nil {
		return 1, err
	}
	ui := newUI(o)
	var mu sync.Mutex
	code := 0
	each(hosts, o.forks, func(_ int, h *compile.Host) {
		results, err := remote.New(h).Rollback(ctx, engine.RollbackRequest{Key: state.Key(name, h.Name), IDs: ids, Release: o.release, Plan: o.plan})
		mu.Lock()
		defer mu.Unlock()
		fmt.Println(ui.c(bold, "▸ "+h.Name))
		if err != nil {
			fmt.Printf("  %s %v\n", ui.c(red, "✘ error:"), err)
			code = 1
			return
		}
		if len(results) == 0 {
			fmt.Println(ui.c(dim, "  Nothing is kept for this manifest: fleetreeve has not changed anything here that it can restore."))
		}
		held := false
		for _, r := range results {
			undo := make([]string, len(r.Changes))
			for i, c := range r.Changes { // restoring reverses the change
				undo[i] = fmt.Sprintf("%s: %s → %s", c.Prop, c.To, c.From)
			}
			back := strings.Join(undo, ", ")
			switch r.Status {
			case "kept":
				note := ""
				if r.Held != "" {
					note = ui.c(yellow, "  held since "+r.Held)
				}
				if r.Remaining == 0 {
					fmt.Printf("  %s  no earlier version left%s\n", r.ID, note)
				} else {
					fmt.Printf("  %s  %d kept, last changed %s%s\n", r.ID, r.Remaining, r.Time, note)
				}
			case "would-restore":
				fmt.Printf("  %s %s  would be restored to before the change of %s (%s)\n", ui.c(yellow, "~"), r.ID, r.Time, back)
				code = max(code, 2)
			case "restored":
				fmt.Printf("  %s %s  restored to before the change of %s (%s)\n", ui.c(green, "↩"), r.ID, r.Time, back)
				for _, n := range r.Refreshed {
					fmt.Printf("      %s %s\n", ui.c(cyan, "↻ refreshed"), n)
				}
				if r.Error != "" {
					fmt.Printf("      %s\n", ui.c(red, r.Error))
					code = 1
				}
				held = true
			case "released":
				fmt.Printf("  %s %s  released: the next run brings it back to what the manifest says\n", ui.c(green, "✔"), r.ID)
			default:
				fmt.Printf("  %s %s  %s\n", ui.c(red, "✘"), r.ID, r.Error)
				code = 1
			}
		}
		if held {
			fmt.Println(ui.c(dim, "  Held: fleetreeve leaves these as they are now. Fix the manifest or data, then run the same command with -release."))
		}
	})
	return code, nil
}
