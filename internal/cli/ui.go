package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nxtw/fleetreeve/internal/compile"
	"github.com/nxtw/fleetreeve/internal/engine"
)

const (
	red    = "31"
	green  = "32"
	yellow = "33"
	cyan   = "36"
	dim    = "2"
	bold   = "1"
)

type ui struct {
	o     *options
	color bool
	mu    sync.Mutex // serialises writes to stdout
}

func newUI(o *options) *ui {
	st, err := os.Stdout.Stat()
	tty := err == nil && st.Mode()&os.ModeCharDevice != 0
	return &ui{o: o, color: tty && !o.noColor && os.Getenv("NO_COLOR") == ""}
}

func (u *ui) c(code, s string) string {
	if !u.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// hostOut prints the progress of one host, either live or, when several
// hosts run at once, as a single block when the host is done.
type hostOut struct {
	host string // set in JSON mode, where events are logged instead of printed
	u    *ui
	w    io.Writer
	buf  *bytes.Buffer
}

func (u *ui) host(h *compile.Host, buffered bool) *hostOut {
	if jsonLogs {
		return &hostOut{u: u, host: h.Name}
	}
	out := &hostOut{u: u, w: os.Stdout}
	if buffered {
		out.buf = &bytes.Buffer{}
		out.w = out.buf
	}
	title := h.Name
	if h.Address != h.Name {
		title += " (" + h.Address + ")"
	}
	fmt.Fprintln(out.w, u.c(bold, "▸ "+title))
	return out
}

// logResult writes one resource outcome as a log record. Unchanged
// resources are logged only with -v. Content diffs are never logged:
// they can hold secrets, and logs travel further than terminals do.
func (o *hostOut) logResult(r engine.Result) {
	level := slog.LevelInfo
	switch r.Status {
	case engine.OK:
		if !r.Refresh && r.Orphan == "" && !r.Held && !o.u.o.verbose {
			return
		}
	case engine.Failed, engine.RolledBack:
		level = slog.LevelError
	case engine.Skipped:
		level = slog.LevelWarn
	}
	if r.Held {
		level = slog.LevelWarn
	}
	attrs := []any{"host", o.host, "resource", r.ID, "status", string(r.Status)}
	if len(r.Changes) > 0 {
		changes := make([]map[string]string, len(r.Changes))
		for i, c := range r.Changes {
			changes[i] = map[string]string{"property": c.Prop, "from": c.From, "to": c.To}
		}
		attrs = append(attrs, "changes", changes)
	}
	if r.Refresh {
		attrs = append(attrs, "refresh", true)
	}
	if r.Drifted {
		attrs = append(attrs, "drifted", true)
	}
	if r.Orphan != "" {
		attrs = append(attrs, "orphan", r.Orphan)
	}
	if r.Held {
		attrs = append(attrs, "held", true)
	}
	if r.Error != "" {
		attrs = append(attrs, "error", r.Error)
	}
	if r.Origin != "" {
		attrs = append(attrs, "origin", r.Origin)
	}
	runLog.Log(context.Background(), level, "resource", attrs...)
}

func (o *hostOut) result(r engine.Result) {
	if jsonLogs {
		o.logResult(r)
		return
	}
	u := o.u
	var mark, note string
	switch r.Status {
	case engine.OK:
		if !r.Refresh && !u.o.verbose && r.Orphan == "" && !r.Held {
			return
		}
		mark = u.c(dim, "  ·")
	case engine.Changed:
		mark = u.c(green, "  ✔")
	case engine.Pending:
		mark = u.c(yellow, "  ~")
	case engine.Failed:
		mark, note = u.c(red, "  ✘"), u.c(red, "failed: ")+r.Error
	case engine.Skipped:
		mark, note = u.c(dim, "  ⊘"), u.c(dim, "skipped: "+r.Error)
	case engine.RolledBack:
		mark, note = u.c(red, "  ↩"), u.c(red, "rolled back: ")+r.Error
	}
	if r.Held {
		mark, note = u.c(yellow, "  ⏸"), u.c(yellow, "held after a manual rollback; not changed (fleetreeve rollback -release to resume)")
	}
	if r.Orphan != "" && r.Status != engine.Failed {
		mark = u.c(red, "  -")
		if r.Orphan == "forget" {
			mark = u.c(dim, "  -")
		}
	}
	if r.Status != engine.Failed && r.Status != engine.Skipped && r.Status != engine.RolledBack && !r.Held {
		var parts []string
		switch r.Orphan {
		case "remove":
			parts = append(parts, "no longer in the manifest")
		case "forget":
			parts = append(parts, u.c(dim, "no longer in the manifest; left as it is and no longer tracked"))
		}
		for _, c := range r.Changes {
			parts = append(parts, fmt.Sprintf("%s: %s → %s", c.Prop, c.From, c.To))
		}
		if r.Refresh {
			parts = append(parts, u.c(cyan, "↻ refresh"))
		}
		note = strings.Join(parts, ", ")
		if r.Drifted {
			note += u.c(yellow, "  (changed outside fleetreeve)")
		}
	}
	fmt.Fprintf(o.w, "%s %s  %s\n", mark, r.ID, note)
	if u.o.noDiff || r.Status == engine.Failed {
		return
	}
	for _, c := range r.Changes {
		for _, l := range strings.Split(strings.TrimRight(c.Diff, "\n"), "\n") {
			if l == "" {
				continue
			}
			col := dim
			switch l[0] {
			case '+':
				col = green
			case '-':
				col = red
			case '@':
				col = cyan
			}
			fmt.Fprintln(o.w, "      "+u.c(col, l))
		}
	}
}

func (o *hostOut) done(rep *hostReport, plan bool) {
	if jsonLogs {
		s := rep.Summary
		mode := "apply"
		if plan {
			mode = "plan"
		}
		attrs := []any{"host", o.host, "mode", mode, "resources", s.Total, "ok", s.OK, "changed", s.Changed, "pending", s.Pending,
			"refreshed", s.Refreshed, "failed", s.Failed, "skipped", s.Skipped, "forgotten", s.Forgotten,
			"rolled_back", s.RolledBack, "held", s.Held, "duration_ms", s.Millis}
		level := slog.LevelInfo
		if rep.Error != "" {
			attrs = append(attrs, "error", rep.Error)
		}
		if rep.Error != "" || s.Failed > 0 {
			level = slog.LevelError
		}
		runLog.Log(context.Background(), level, "run finished", attrs...)
		return
	}
	u := o.u
	if rep.Error != "" {
		fmt.Fprintf(o.w, "  %s %s\n", u.c(red, "✘ error:"), rep.Error)
	} else {
		fmt.Fprintf(o.w, "  %s\n", u.c(dim, summarize(rep.Summary, plan)))
	}
	if o.buf != nil {
		u.mu.Lock()
		os.Stdout.Write(o.buf.Bytes())
		u.mu.Unlock()
	}
}

func summarize(s engine.Summary, plan bool) string {
	parts := []string{fmt.Sprintf("%d resources", s.Total)}
	add := func(n int, label string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, label))
		}
	}
	add(s.Changed, "changed")
	add(s.Pending, "to change")
	if plan {
		add(s.Refreshed, "to refresh")
	} else {
		add(s.Refreshed, "refreshed")
	}
	add(s.Forgotten, "no longer tracked")
	add(s.RolledBack, "rolled back")
	add(s.Held, "held")
	add(s.Failed, "failed")
	add(s.Skipped, "skipped")
	if !s.Drift() && s.Failed+s.Skipped == 0 {
		parts = append(parts, "in sync")
	}
	return strings.Join(parts, " · ") + fmt.Sprintf("  (%s)", (time.Duration(s.Millis)*time.Millisecond).Round(10*time.Millisecond))
}

func (u *ui) recap(reports []hostReport, plan bool) {
	if jsonLogs {
		return // each host already logged its "run finished" record
	}
	width := 0
	for _, r := range reports {
		width = max(width, len(r.Host))
	}
	fmt.Println(u.c(bold, "\nRecap"))
	for _, r := range reports {
		state := u.c(green, "ok     ")
		switch {
		case r.Error != "":
			state = u.c(red, "error  ")
		case r.Summary.Failed > 0:
			state = u.c(red, "failed ")
		case r.Summary.Drift() && plan:
			state = u.c(yellow, "drift  ")
		case r.Summary.Drift():
			state = u.c(green, "changed")
		}
		detail := r.Error
		if detail == "" {
			detail = summarize(r.Summary, plan)
		}
		fmt.Printf("  %-*s  %s  %s\n", width, r.Host, state, detail)
	}
}
