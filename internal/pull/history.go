package pull

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nxtw/fleetreeve/internal/engine"
)

// The history is the server's log of everything hosts did: one event for
// every run a host reported (also the uneventful ones) and for every
// request it carried out. Reports only ever hold the latest run; this is
// where one scrolls back.
//
// Events are appended to one file per day (UTC), <dir>/2026-11-21.jsonl,
// a JSON document per line, so the log can be read, grepped, rotated and
// shipped with ordinary tools. Files older than HistoryDays are removed.

// Event is one line of the history.
type Event struct {
	Time string `json:"time"`
	Host string `json:"host"`
	Kind string `json:"kind"` // "run" or "request"

	// A run.
	Manifest string          `json:"manifest,omitempty"`
	Source   string          `json:"source,omitempty"` // pull or push
	Mode     string          `json:"mode,omitempty"`   // apply or plan
	State    string          `json:"state,omitempty"`  // ok, changed, drift, failed or error
	Summary  *engine.Summary `json:"summary,omitempty"`
	Cached   bool            `json:"cached,omitempty"`
	Error    string          `json:"error,omitempty"`
	// Changes are the resources that were not simply in sync. Diffs of
	// file contents are left out: they can hold secrets and are large.
	Changes []EventChange `json:"changes,omitempty"`

	// A request carried out (apply, plan, rollback, release, command).
	// The text and output of a command are not kept here: the history is
	// readable with the read token.
	Request     string `json:"request,omitempty"`
	Resource    string `json:"resource,omitempty"`
	RequestedBy string `json:"requested_by,omitempty"`
	Status      string `json:"status,omitempty"`
	Detail      string `json:"detail,omitempty"`
	Exit        *int   `json:"exit,omitempty"`
}

// EventChange is what happened to one resource in a run.
type EventChange struct {
	ID      string   `json:"id"`
	Status  string   `json:"status"`
	Props   []string `json:"props,omitempty"` // "mode: 0644 -> 0600"
	Refresh bool     `json:"refresh,omitempty"`
	Drifted bool     `json:"drifted,omitempty"`
	Orphan  string   `json:"orphan,omitempty"`
	Held    bool     `json:"held,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// Eventful reports whether the event is more than a run that found
// everything in order.
func (e *Event) Eventful() bool { return e.Kind != "run" || e.State != "ok" }

const (
	maxEventChanges   = 200
	maxEventText      = 300
	defaultHistoryMax = 100
	historyMax        = 1000
)

var historyMu sync.Mutex // one server process owns the history files

func clip(s string) string {
	if len(s) > maxEventText {
		return s[:maxEventText] + "…"
	}
	return s
}

// runEvent condenses a report into an event.
func runEvent(rep *Report) Event {
	t, _ := time.Parse(time.RFC3339, rep.Time)
	sum := rep.Summary
	ev := Event{Time: rep.Time, Host: rep.Host, Kind: "run", Manifest: rep.Manifest, Source: rep.Source, Mode: rep.Mode,
		State: rep.Entry(t).State, Summary: &sum, Cached: rep.Cached, Error: clip(rep.Error)}
	for _, r := range rep.Results {
		if r.Status == engine.OK && !r.Refresh && !r.Held && r.Orphan == "" {
			continue
		}
		if len(ev.Changes) == maxEventChanges {
			break
		}
		c := EventChange{ID: r.ID, Status: string(r.Status), Refresh: r.Refresh, Drifted: r.Drifted, Orphan: r.Orphan, Held: r.Held, Error: clip(r.Error)}
		for _, ch := range r.Changes {
			c.Props = append(c.Props, clip(fmt.Sprintf("%s: %s -> %s", ch.Prop, ch.From, ch.To)))
		}
		ev.Changes = append(ev.Changes, c)
	}
	return ev
}

// requestEvent condenses the result of a request into an event.
func requestEvent(res *ActionResult) Event {
	ev := Event{Time: res.Done, Host: res.Host, Kind: "request", Manifest: res.Manifest, Request: res.Kind, Resource: res.Resource,
		RequestedBy: res.RequestedBy, Status: res.Status, Detail: clip(res.Detail)}
	if res.Kind == "command" {
		exit := res.Exit
		ev.Exit = &exit
	}
	if _, err := time.Parse(time.RFC3339, ev.Time); err != nil {
		ev.Time = time.Now().UTC().Format(time.RFC3339)
	}
	return ev
}

// record appends events to the history. It never fails a request: a
// history that cannot be written is logged and otherwise ignored.
func (s *Server) record(events ...Event) {
	if s.History == "" || len(events) == 0 {
		return
	}
	historyMu.Lock()
	defer historyMu.Unlock()
	err := os.MkdirAll(s.History, 0o750)
	for _, ev := range events {
		if err != nil {
			break
		}
		day := time.Now().UTC().Format("2006-01-02")
		if t, perr := time.Parse(time.RFC3339, ev.Time); perr == nil {
			day = t.UTC().Format("2006-01-02")
		}
		p := filepath.Join(s.History, day+".jsonl")
		_, statErr := os.Stat(p)
		var f *os.File
		if f, err = os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640); err != nil {
			break
		}
		b, _ := json.Marshal(ev)
		_, err = f.Write(append(b, '\n'))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if statErr != nil { // a new day: a good moment to drop old ones
			s.pruneHistory()
		}
	}
	if err != nil {
		s.Log.Error("writing the history failed", "error", err.Error())
	}
}

// historyDays lists the days there is history for, newest first.
func (s *Server) historyDays() []string {
	files, _ := filepath.Glob(filepath.Join(s.History, "????-??-??.jsonl"))
	days := make([]string, 0, len(files))
	for _, f := range files {
		days = append(days, strings.TrimSuffix(filepath.Base(f), ".jsonl"))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(days)))
	return days
}

func (s *Server) pruneHistory() {
	if s.HistoryDays <= 0 {
		return
	}
	oldest := time.Now().UTC().AddDate(0, 0, -s.HistoryDays).Format("2006-01-02")
	for _, day := range s.historyDays() {
		if day < oldest {
			os.Remove(filepath.Join(s.History, day+".jsonl"))
		}
	}
}

// HistoryPage is an answer of GET /v1/history: events, newest first, and
// where to continue for older ones ("" at the beginning of the history).
type HistoryPage struct {
	Events []Event `json:"events"`
	Next   string  `json:"next"`
}

// ReadHistory returns up to limit events older than the cursor (""
// starts at the newest), newest first. host and eventful narrow it down.
// A cursor is "<day>:<line>": the events before that line of that day.
func ReadHistory(dir, host string, eventful bool, cursor string, limit int) (HistoryPage, error) {
	page := HistoryPage{Events: []Event{}}
	fromDay, fromLine := "", -1
	if cursor != "" {
		day, line, ok := strings.Cut(cursor, ":")
		n, err := strconv.Atoi(line)
		if _, derr := time.Parse("2006-01-02", day); !ok || err != nil || derr != nil || n < 0 {
			return page, fmt.Errorf("invalid cursor %q", cursor)
		}
		fromDay, fromLine = day, n
	}
	s := &Server{History: dir}
	for _, day := range s.historyDays() {
		if fromDay != "" && day > fromDay {
			continue
		}
		var lines [][]byte
		f, err := os.Open(filepath.Join(dir, day+".jsonl"))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for sc.Scan() {
			lines = append(lines, append([]byte{}, sc.Bytes()...))
		}
		f.Close()
		end := len(lines)
		if day == fromDay && fromLine < end {
			end = fromLine
		}
		for i := end - 1; i >= 0; i-- {
			var ev Event
			if json.Unmarshal(lines[i], &ev) != nil || (host != "" && ev.Host != host) || (eventful && !ev.Eventful()) {
				continue
			}
			page.Events = append(page.Events, ev)
			if len(page.Events) == limit {
				page.Next = fmt.Sprintf("%s:%d", day, i)
				return page, nil
			}
		}
	}
	return page, nil
}

// apiHistory serves the history: GET /v1/history?host=&only=changes&before=<cursor>&limit=.
func (s *Server) apiHistory(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, false) {
		return
	}
	if s.History == "" {
		apiError(w, "this server keeps no history (start it with -history <dir>)", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	limit := defaultHistoryMax
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			apiError(w, "limit must be a positive number", http.StatusBadRequest)
			return
		}
		limit = min(n, historyMax)
	}
	only := q.Get("only")
	if only != "" && only != "changes" {
		apiError(w, `only must be "changes" or left out`, http.StatusBadRequest)
		return
	}
	historyMu.Lock()
	page, err := ReadHistory(s.History, q.Get("host"), only == "changes", q.Get("before"), limit)
	historyMu.Unlock()
	if err != nil {
		apiError(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, page)
}
