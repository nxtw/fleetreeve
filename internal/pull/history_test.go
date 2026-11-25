package pull

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nxtw/fleetreeve/internal/core"
	"github.com/nxtw/fleetreeve/internal/engine"
)

// Every run and every request ends up in the history, which can be paged
// back through, narrowed to a host or to what was not simply in sync.
func TestHistory(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "inv.yaml"), []byte("hosts:\n  web1: {}\n  web2: {}\n"), 0o644)
	secret := []byte(strings.Repeat("s", 32))
	s := &Server{Inventory: filepath.Join(dir, "inv.yaml"), Secret: secret, Reports: filepath.Join(dir, "reports"),
		Actions: filepath.Join(dir, "actions"), History: filepath.Join(dir, "history"), HistoryDays: 30, Log: slog.New(slog.DiscardHandler)}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	send := func(method, path, token string, body any) (int, []byte) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, ts.URL+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out bytes.Buffer
		out.ReadFrom(resp.Body)
		return resp.StatusCode, out.Bytes()
	}
	report := func(host string, changed bool) {
		t.Helper()
		rep := Report{Host: host, Manifest: "site", Source: "pull", Mode: "apply", Interval: 300, Summary: engine.Summary{Total: 3, OK: 3}}
		rep.Results = []engine.Result{{ID: "package[a]", Status: engine.OK}}
		if changed {
			rep.Summary = engine.Summary{Total: 3, OK: 2, Changed: 1}
			rep.Results = append(rep.Results, engine.Result{ID: "file[/etc/motd]", Status: engine.Changed,
				Changes: []core.Change{{Prop: "content", From: "sha256:aa", To: "sha256:bb", Diff: "-the old secret\n+the new secret\n"}}})
		}
		if code, b := send("POST", "/v1/report", Token(secret, host), rep); code != http.StatusNoContent {
			t.Fatalf("report: %d %s", code, b)
		}
	}
	for i := 0; i < 5; i++ {
		report("web1", i == 2)
		report("web2", false)
	}
	done := time.Now().UTC().Format(time.RFC3339)
	results := map[string]any{"host": "web1", "results": []ActionResult{{Action: Action{ID: "x1", Host: "web1", Kind: "command", Command: "cat /etc/shadow", RequestedBy: "ann (console)"},
		Status: "done", Output: "root:$6$secret", Done: done}}}
	if code, b := send("POST", "/v1/actions/results", Token(secret, "web1"), results); code != http.StatusNoContent {
		t.Fatalf("results: %d %s", code, b)
	}

	read := APIToken(secret, "read")
	page := func(query string) HistoryPage {
		t.Helper()
		code, b := send("GET", "/v1/history"+query, read, nil)
		if code != http.StatusOK {
			t.Fatalf("history%s: %d %s", query, code, b)
		}
		var p HistoryPage
		if err := json.Unmarshal(b, &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Everything, newest first: the request, then the runs.
	all := page("")
	if len(all.Events) != 11 || all.Next != "" || all.Events[0].Kind != "request" || all.Events[1].Host != "web2" || all.Events[10].Host != "web1" {
		t.Fatalf("all: %d events, next %q: %+v", len(all.Events), all.Next, all.Events)
	}
	// Paging back gives every event once, in the same order.
	var paged []Event
	for cursor, n := "", 0; ; n++ {
		q := "?limit=4"
		if cursor != "" {
			q += "&before=" + cursor
		}
		p := page(q)
		paged = append(paged, p.Events...)
		if cursor = p.Next; cursor == "" || n > 5 {
			break
		}
	}
	a, _ := json.Marshal(paged)
	b, _ := json.Marshal(all.Events)
	if string(a) != string(b) {
		t.Errorf("paging: %d events instead of %d", len(paged), len(all.Events))
	}
	// Narrowed down.
	if p := page("?host=web2"); len(p.Events) != 5 {
		t.Errorf("one host: %d", len(p.Events))
	}
	changes := page("?only=changes")
	if len(changes.Events) != 2 || changes.Events[1].State != "changed" {
		t.Fatalf("only changes: %+v", changes.Events)
	}
	// A run says what changed, without the diff; a request, who asked,
	// without the command or its output.
	run := changes.Events[1]
	if len(run.Changes) != 1 || run.Changes[0].ID != "file[/etc/motd]" || run.Changes[0].Props[0] != "content: sha256:aa -> sha256:bb" || run.Summary.Changed != 1 {
		t.Errorf("run event: %+v", run)
	}
	if req := changes.Events[0]; req.Request != "command" || req.RequestedBy != "ann (console)" || req.Exit == nil {
		t.Errorf("request event: %+v", req)
	}
	files, _ := filepath.Glob(filepath.Join(s.History, "*.jsonl"))
	raw, _ := os.ReadFile(files[0])
	if len(files) != 1 || bytes.Contains(raw, []byte("secret")) || bytes.Contains(raw, []byte("shadow")) {
		t.Errorf("the history holds what it should not: %s", raw)
	}
	// Bad questions, and no access without a token.
	for _, q := range []string{"?before=nonsense", "?limit=0", "?only=everything"} {
		if code, _ := send("GET", "/v1/history"+q, read, nil); code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, code)
		}
	}
	if code, _ := send("GET", "/v1/history", Token(secret, "web1"), nil); code != http.StatusUnauthorized {
		t.Errorf("node token: %d", code)
	}
	// Old days are dropped when a new day begins.
	old := filepath.Join(s.History, "2001-01-01.jsonl")
	os.WriteFile(old, []byte("{}\n"), 0o640)
	s.pruneHistory()
	if _, err := os.Stat(old); err == nil {
		t.Error("an old day was kept")
	}
}
