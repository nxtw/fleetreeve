package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/nxtw/fleetreeve/internal/compile"
	"github.com/nxtw/fleetreeve/internal/remote"
)

// lookup shows the variables a host ends up with and where each comes
// from: which data file, which part of the inventory, the manifest's
// defaults or the command line. With keys, it explains just those, listing
// every place that sets them.
func lookup(ctx context.Context, o *options) (int, error) {
	if len(o.args) == 0 {
		return 1, fmt.Errorf("usage: fleetreeve lookup [-i inventory] [-l hosts] [-offline | -facts file] <manifest> [variable...]")
	}
	manifest, keys := o.args[0], o.args[1:]
	inv, hosts, err := o.hosts()
	if err != nil {
		return 1, err
	}
	ui := newUI(o)
	var mu sync.Mutex
	code := 0
	each(hosts, o.forks, func(_ int, h *compile.Host) {
		// Facts decide which data applies (os/<family>.yaml), so they come
		// from the host itself unless -offline asks for this machine's.
		var f map[string]any
		var err error
		if o.offline || o.factsFile != "" {
			f, err = o.standInFacts()
		} else if f, err = remote.New(h).Facts(ctx); err != nil {
			err = fmt.Errorf("%v (use -offline to look up with this machine's facts instead)", err)
		}
		var ex *compile.Explanation
		if err == nil {
			ex, err = compile.Explain(compile.Input{Manifest: manifest, Host: h, Inventory: inv, Facts: f, Extra: o.extra})
		}
		mu.Lock()
		defer mu.Unlock()
		fmt.Println(ui.c(bold, "▸ "+h.Name))
		if err != nil {
			fmt.Printf("  %s %v\n", ui.c(red, "✘ error:"), err)
			code = 1
			return
		}
		if ex.DataDir == "" {
			fmt.Println(ui.c(dim, "  No data layer: there is no data directory next to the manifest."))
		} else if len(keys) == 0 {
			fmt.Printf("  Data in %s/, most specific first:\n", ex.DataDir)
			for _, l := range ex.Levels {
				status := l.Status
				if status != "used" {
					status = ui.c(dim, status)
				}
				fmt.Printf("    %-34s %s\n", l.File, status)
			}
			fmt.Println()
		}
		show := ex.Keys()
		if len(keys) > 0 {
			show = keys
		}
		width := 0
		for _, k := range show {
			width = max(width, len(k))
		}
		for _, k := range show {
			src := ex.Sources[k]
			if len(src) == 0 {
				fmt.Printf("  %s %s\n", ui.c(red, k), "is not set for this host")
				code = 1
				continue
			}
			from := src[len(src)-1].Source
			if how := ex.Merge[k]; how != "" {
				var files []string
				for _, s := range src {
					if strings.HasPrefix(s.Source, ex.DataDir+"/") {
						files = append(files, s.Source)
					}
				}
				if len(files) > 1 && strings.HasPrefix(from, ex.DataDir+"/") {
					from = fmt.Sprintf("%s merge of %s", how, strings.Join(files, ", "))
				}
			}
			fmt.Printf("  %-*s = %s  %s\n", width, k, compact(ex.Vars[k]), ui.c(dim, "← "+from))
			if len(keys) > 0 { // explaining: every place that sets it, winner last
				for i, s := range src {
					note := "overridden"
					if ex.Merge[k] != "" && strings.HasPrefix(s.Source, ex.DataDir+"/") && strings.HasPrefix(from, ex.Merge[k]+" merge") {
						note = "merged"
					} else if i == len(src)-1 {
						note = "wins"
					}
					fmt.Printf("      %-40s %s  %s\n", s.Source, compact(s.Value), ui.c(dim, note))
				}
			}
		}
	})
	return code, nil
}

func compact(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	if s := string(b); len(s) > 100 {
		return s[:97] + "..."
	}
	return string(b)
}
