package types

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nxtw/fleetreeve/internal/core"
	"github.com/nxtw/fleetreeve/internal/state"
)

// scriptType is a resource type implemented by a module's script.
//
// The script is run as `<script> check` and `<script> apply` (and
// `<script> refresh`, if the type says it reacts to notifications). It
// gets the resource two ways, so that both a shell one-liner and a real
// program are comfortable:
//
//	environment  FLEETREEVE_OP, FLEETREEVE_TITLE, FLEETREEVE_PLAN (1 during a dry run),
//	             and FLEETREEVE_PARAM_<NAME> for every parameter (lists one
//	             item per line, maps as JSON)
//	stdin        {"op": ..., "title": ..., "params": {...}, "plan": bool}
//
// check prints what differs from the desired state, and nothing when the
// resource is as it should be. Either one change per line,
//
//	value: 0 -> 1
//
// (any other line is taken as a description of a change), or JSON:
// {"changes": [{"prop": "value", "from": "0", "to": "1"}]}. check must not
// change anything: it also runs during a dry run. apply makes the changes.
// Any operation fails the resource by exiting non-zero; what it wrote to
// stderr is the error message.
type scriptType struct {
	def    *core.TypeDef
	schema core.Schema
}

// NewScript builds a resource type from a module's script type.
func NewScript(def *core.TypeDef) (core.Type, error) {
	schema, err := core.ParamSchema(def.Params)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", def.Name, err)
	}
	t := scriptType{def: def, schema: schema}
	if def.Refresh {
		return refreshingScript{t}, nil
	}
	return t, nil
}

func (t scriptType) Name() string        { return t.def.Name }
func (t scriptType) Doc() string         { return t.def.Doc }
func (t scriptType) Schema() core.Schema { return t.schema }

const scriptTimeout = 5 * time.Minute

// file puts the script on disk, once per version, under the node's state
// directory - which, unlike /tmp, is not mounted noexec on hardened hosts.
func (t scriptType) file() (string, error) {
	if t.def.Script == "" {
		return "", fmt.Errorf("the type %s arrived without its script", t.def.Name)
	}
	dir := filepath.Join(state.Dir(), "scripts")
	path := filepath.Join(dir, fmt.Sprintf("%x", sha256.Sum256([]byte(t.def.Script)))[:24])
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(t.def.Script), 0o700); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

func (t scriptType) run(ctx *core.Ctx, r *core.Resource, op string) (core.CmdResult, error) {
	path, err := t.file()
	if err != nil {
		return core.CmdResult{}, err
	}
	params := map[string]any{}
	env := []string{"FLEETREEVE_OP=" + op, "FLEETREEVE_TITLE=" + r.Title, "FLEETREEVE_TYPE=" + t.def.Name}
	if ctx.Plan {
		env = append(env, "FLEETREEVE_PLAN=1")
	}
	for _, name := range core.ParamNames(t.schema) {
		v, set := r.Attrs[name]
		if !set {
			continue
		}
		params[name] = v
		var text string
		switch x := v.(type) {
		case []string:
			text = strings.Join(x, "\n")
		case map[string]string:
			b, _ := json.Marshal(x)
			text = string(b)
		default:
			text = fmt.Sprint(x)
		}
		env = append(env, "FLEETREEVE_PARAM_"+strings.ToUpper(regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(name, "_"))+"="+text)
	}
	stdin, _ := json.Marshal(map[string]any{"op": op, "title": r.Title, "params": params, "plan": ctx.Plan})
	c, cancel := context.WithTimeout(ctx, scriptTimeout)
	defer cancel()
	cmd := core.Cmd{Name: path, Args: []string{op}, Env: env, Stdin: string(stdin)}
	// Without a #! line the script is taken to be shell.
	if !strings.HasPrefix(t.def.Script, "#!") {
		cmd.Name, cmd.Args = "/bin/sh", []string{path, op}
	}
	res := ctx.Runner.Run(c, cmd)
	if c.Err() == context.DeadlineExceeded {
		res.Err = fmt.Errorf("stopped after %s", scriptTimeout)
	}
	return res, nil
}

var changeLine = regexp.MustCompile(`^([^:\s][^:]*):\s*(.*?)\s*->\s*(.*)$`)

func (t scriptType) Check(ctx *core.Ctx, r *core.Resource) ([]core.Change, error) {
	res, err := t.run(ctx, r, "check")
	if err != nil {
		return nil, err
	}
	if !res.OK() {
		return nil, res.Error("check")
	}
	out := strings.TrimSpace(res.Stdout)
	if out == "" {
		return nil, nil
	}
	if strings.HasPrefix(out, "{") {
		var parsed struct {
			Changes []core.Change `json:"changes"`
		}
		if err := json.Unmarshal([]byte(out), &parsed); err != nil {
			return nil, fmt.Errorf("check printed something that is neither change lines nor valid JSON: %v", err)
		}
		return parsed.Changes, nil
	}
	var changes []core.Change
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if m := changeLine.FindStringSubmatch(line); m != nil {
			changes = append(changes, core.Change{Prop: strings.TrimSpace(m[1]), From: m[2], To: m[3]})
		} else {
			changes = append(changes, core.Change{Prop: "state", From: "", To: line})
		}
	}
	sort.SliceStable(changes, func(a, b int) bool { return changes[a].Prop < changes[b].Prop })
	return changes, nil
}

func (t scriptType) Apply(ctx *core.Ctx, r *core.Resource, _ []core.Change) error {
	res, err := t.run(ctx, r, "apply")
	if err != nil {
		return err
	}
	if !res.OK() {
		return res.Error("apply")
	}
	return nil
}

// refreshingScript is a script type that reacts to notifications.
type refreshingScript struct{ scriptType }

func (t refreshingScript) Refresh(ctx *core.Ctx, r *core.Resource, _ []core.Change) (bool, error) {
	res, err := t.run(ctx, r, "refresh")
	if err != nil {
		return false, err
	}
	if !res.OK() {
		return false, res.Error("refresh")
	}
	return true, nil
}
