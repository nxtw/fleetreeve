package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nxtw/fleetreeve/internal/core"
)

// A resource type implemented by a script that came with the catalog.
func TestScriptType(t *testing.T) {
	t.Setenv("FLEETREEVE_STATE_DIR", t.TempDir())
	dir := t.TempDir()
	target, log := filepath.Join(dir, "setting"), filepath.Join(dir, "refreshes")
	script := `#!/bin/sh
# A setting kept in a file: title is the file, value its content.
set -eu
file=$FLEETREEVE_TITLE
want=$FLEETREEVE_PARAM_VALUE
have=$(cat "$file" 2>/dev/null || echo "(unset)")
case $1 in
check)
    [ "$have" = "$want" ] || echo "value: $have -> $want"
    [ "${FLEETREEVE_PARAM_STRICT:-false}" = false ] || [ -n "${FLEETREEVE_PARAM_OWNER_LIST:-}" ] || { echo "strict needs owners" >&2; exit 5; }
    ;;
apply)   printf %s "$want" > "$file" ;;
refresh) echo "$FLEETREEVE_OP $FLEETREEVE_TYPE" >> ` + log + ` ;;
esac
`
	types := map[string]*core.TypeDef{"acme.kv.setting": {
		Params:  map[string]core.ParamDef{"value": {Required: true}, "strict": {Type: "bool"}, "owner_list": {Type: "list"}},
		Script:  script,
		Refresh: true,
	}}
	catalog := func(value string, attrs map[string]any) *core.Catalog {
		a := map[string]any{"value": value}
		for k, v := range attrs {
			a[k] = v
		}
		trigger := res("file", filepath.Join(dir, "trigger"), map[string]any{"content": value}, nil, []string{"acme.kv.setting[" + target + "]"})
		return &core.Catalog{Version: core.ProtocolVersion, Name: "test", Types: types,
			Resources: []*core.Resource{trigger, res("acme.kv.setting", target, a, nil, nil)}}
	}
	run := func(plan bool, cat *core.Catalog) (map[string]Result, Summary) {
		t.Helper()
		got := map[string]Result{}
		sum, err := Run(core.NewCtx(t.Context(), nil, plan), cat, func(r Result) { got[r.ID] = r })
		if err != nil {
			t.Fatal(err)
		}
		return got, sum
	}
	id := "acme.kv.setting[" + target + "]"

	// A dry run reports the change the script found, and changes nothing.
	got, _ := run(true, catalog("one", nil))
	if c := got[id].Changes; got[id].Status != Pending || len(c) != 1 || c[0].Prop != "value" || c[0].From != "(unset)" || c[0].To != "one" {
		t.Fatalf("plan: %+v", got[id])
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("the dry run applied the change")
	}
	// Apply; then in sync; then a notification refreshes it.
	if got, _ = run(false, catalog("one", nil)); got[id].Status != Changed {
		t.Fatalf("apply: %+v", got[id])
	}
	if b, _ := os.ReadFile(target); string(b) != "one" {
		t.Errorf("content: %q", b)
	}
	if _, sum := run(false, catalog("one", nil)); sum.Drift() {
		t.Errorf("not idempotent: %+v", sum)
	}
	run(false, catalog("two", nil))
	if b, _ := os.ReadFile(log); !strings.Contains(string(b), "refresh acme.kv.setting") {
		t.Errorf("no refresh: %q", b)
	}
	// A script that exits non-zero fails the resource with what it said.
	got, sum := run(false, catalog("two", map[string]any{"strict": true}))
	if sum.Failed != 1 || !strings.Contains(got[id].Error, "strict needs owners") {
		t.Errorf("failing check: %+v", got[id])
	}
	// Lists reach the script one item per line.
	if got, _ = run(false, catalog("two", map[string]any{"strict": true, "owner_list": []any{"a", "b"}})); got[id].Status == Failed {
		t.Errorf("list parameter: %+v", got[id])
	}
	// Unknown parameters are refused on the node too.
	if _, err := Run(core.NewCtx(t.Context(), nil, true), catalog("x", map[string]any{"nope": 1}), func(Result) {}); err == nil || !strings.Contains(err.Error(), "unknown attribute") {
		t.Errorf("unknown parameter on the node: %v", err)
	}
}
