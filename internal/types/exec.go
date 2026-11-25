package types

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/nxtw/fleetreeve/internal/core"
)

func init() { core.Register(execType{}) }

type execType struct{}

func (execType) Name() string { return "exec" }
func (execType) Doc() string {
	return "Runs a shell command. Without a guard (creates, unless, onlyif, refreshonly) it runs on every apply, so always give it one."
}

func (execType) Schema() core.Schema {
	return core.Schema{
		"command":     {Kind: core.String, NameVar: true, Doc: "Command line, run with /bin/sh -c (defaults to the title)."},
		"creates":     {Kind: core.String, Doc: "Skip when this path exists."},
		"unless":      {Kind: core.String, Doc: "Skip when this command succeeds. Must be read-only: it also runs during plan."},
		"onlyif":      {Kind: core.String, Doc: "Run only when this command succeeds. Must be read-only: it also runs during plan."},
		"refreshonly": {Kind: core.Bool, Doc: "Run only when notified by another resource."},
		"cwd":         {Kind: core.String, Doc: "Working directory."},
		"env":         {Kind: core.Map, Doc: "Extra environment variables."},
		"timeout":     {Kind: core.Int, Default: 300, Doc: "Seconds before the command is killed."},
	}
}

func (execType) sh(ctx *core.Ctx, r *core.Resource, script string) core.CmdResult {
	timeout, _ := r.Int("timeout")
	c, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	cmd := core.Cmd{Name: "/bin/sh", Args: []string{"-c", script}, Dir: r.Str("cwd")}
	env := r.Map("env")
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cmd.Env = append(cmd.Env, k+"="+env[k])
	}
	res := ctx.Runner.Run(c, cmd)
	if c.Err() == context.DeadlineExceeded {
		res.Err = fmt.Errorf("timed out after %ds", timeout)
	}
	return res
}

func (t execType) Check(ctx *core.Ctx, r *core.Resource) ([]core.Change, error) {
	if r.Bool("refreshonly") {
		return nil, nil
	}
	if c := r.Str("creates"); c != "" {
		if _, err := os.Lstat(c); err == nil {
			return nil, nil
		}
	}
	if u := r.Str("unless"); u != "" {
		if res := t.sh(ctx, r, u); res.Err != nil {
			return nil, res.Error("unless")
		} else if res.Code == 0 {
			return nil, nil
		}
	}
	if o := r.Str("onlyif"); o != "" {
		if res := t.sh(ctx, r, o); res.Err != nil {
			return nil, res.Error("onlyif")
		} else if res.Code != 0 {
			return nil, nil
		}
	}
	return []core.Change{{Prop: "run", From: "pending", To: "executed"}}, nil
}

func (t execType) Apply(ctx *core.Ctx, r *core.Resource, _ []core.Change) error {
	if res := t.sh(ctx, r, r.Str("command")); !res.OK() {
		return res.Error("command")
	}
	return nil
}

func (t execType) Refresh(ctx *core.Ctx, r *core.Resource, applied []core.Change) (bool, error) {
	if len(applied) > 0 {
		return false, nil // already ran in this pass
	}
	return true, t.Apply(ctx, r, nil)
}
