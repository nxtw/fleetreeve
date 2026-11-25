package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Ctx is what a resource type gets to work with on the node.
type Ctx struct {
	context.Context
	Facts  map[string]any
	Runner Runner
	Plan   bool // dry run: Check must tolerate state earlier resources would create
}

func NewCtx(ctx context.Context, facts map[string]any, plan bool) *Ctx {
	return &Ctx{Context: ctx, Facts: facts, Runner: OSRunner{}, Plan: plan}
}

// Cmd is a command to run on the node.
type Cmd struct {
	Name  string
	Args  []string
	Env   []string // extra KEY=VALUE entries
	Dir   string
	Stdin string
}

type CmdResult struct {
	Stdout, Stderr string
	Code           int
	Err            error // set when the command could not be started
}

func (r CmdResult) OK() bool { return r.Err == nil && r.Code == 0 }

// Error turns a failed result into an error carrying the command output.
func (r CmdResult) Error(what string) error {
	if r.Err != nil {
		return fmt.Errorf("%s: %v", what, r.Err)
	}
	out := strings.TrimSpace(r.Stderr)
	if out == "" {
		out = strings.TrimSpace(r.Stdout)
	}
	if lines := strings.Split(out, "\n"); len(lines) > 15 {
		out = "...\n" + strings.Join(lines[len(lines)-15:], "\n")
	}
	if out == "" {
		return fmt.Errorf("%s: exit status %d", what, r.Code)
	}
	return fmt.Errorf("%s: exit status %d: %s", what, r.Code, out)
}

// Runner abstracts command execution so types can be tested without a system.
type Runner interface {
	Run(ctx context.Context, c Cmd) CmdResult
	Has(bin string) bool
}

type OSRunner struct{}

func (OSRunner) Has(bin string) bool { _, err := exec.LookPath(bin); return err == nil }

func (OSRunner) Run(ctx context.Context, c Cmd) CmdResult {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr, cmd.Dir = &stdout, &stderr, c.Dir
	if c.Stdin != "" {
		cmd.Stdin = strings.NewReader(c.Stdin)
	}
	if len(c.Env) > 0 {
		cmd.Env = append(os.Environ(), c.Env...)
	}
	err := cmd.Run()
	res := CmdResult{Stdout: stdout.String(), Stderr: stderr.String()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.Code = ee.ExitCode()
		if res.Code < 0 { // killed by signal or context
			res.Err = err
		}
	default:
		res.Err = err
	}
	return res
}

func (c *Ctx) Cmd(name string, args ...string) CmdResult {
	return c.Runner.Run(c, Cmd{Name: name, Args: args})
}

func (c *Ctx) Has(bin string) bool { return c.Runner.Has(bin) }

// Fact looks up a dotted path such as "os.family"; "" when absent.
func (c *Ctx) Fact(path string) string {
	var cur any = c.Facts
	for _, p := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[p]
	}
	if cur == nil {
		return ""
	}
	return fmt.Sprint(cur)
}
