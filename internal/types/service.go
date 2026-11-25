package types

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/nxtw/fleetreeve/internal/core"
)

func init() { core.Register(serviceType{}) }

type serviceType struct{}

func (serviceType) Name() string { return "service" }
func (serviceType) Doc() string {
	return "Manages a service's running state and boot-time enablement (systemd, OpenRC or SysV). Restarts when notified."
}

func (serviceType) Schema() core.Schema {
	return core.Schema{
		"name":     {Kind: core.String, NameVar: true, Doc: "Service name (defaults to the title)."},
		"ensure":   {Kind: core.String, Enum: []string{"running", "stopped"}, Doc: "Desired running state; unmanaged when omitted."},
		"enabled":  {Kind: core.Bool, Doc: "Start at boot; unmanaged when omitted."},
		"reload":   {Kind: core.Bool, Doc: "Reload instead of restart when notified."},
		"provider": {Kind: core.String, Enum: []string{"systemd", "openrc", "sysv"}, Doc: "Init system; detected when omitted."},
	}
}

type svc struct {
	ctx            *core.Ctx
	provider, name string
}

func (serviceType) svc(ctx *core.Ctx, r *core.Resource) (*svc, error) {
	p := r.Str("provider")
	if p == "" {
		if _, err := os.Stat("/run/systemd/system"); err == nil && ctx.Has("systemctl") {
			p = "systemd"
		} else if ctx.Has("rc-service") {
			p = "openrc"
		} else if ctx.Has("service") {
			p = "sysv"
		} else {
			return nil, fmt.Errorf("no supported init system found on this host (systemd, openrc, sysv)")
		}
	}
	return &svc{ctx, p, r.Str("name")}, nil
}

func (s *svc) running() bool {
	switch s.provider {
	case "systemd":
		return s.ctx.Cmd("systemctl", "is-active", "--quiet", s.name).OK()
	case "openrc":
		return s.ctx.Cmd("rc-service", s.name, "status").OK()
	}
	return s.ctx.Cmd("service", s.name, "status").OK()
}

func (s *svc) enabled() bool {
	switch s.provider {
	case "systemd":
		return s.ctx.Cmd("systemctl", "is-enabled", "--quiet", s.name).OK()
	case "openrc":
		out := s.ctx.Cmd("rc-update", "show", "default").Stdout
		return regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(s.name) + `\s*\|`).MatchString(out)
	}
	m, _ := filepath.Glob("/etc/rc[2345].d/S??" + s.name)
	return len(m) > 0
}

func (s *svc) action(a string) error {
	var res core.CmdResult
	switch s.provider {
	case "systemd":
		res = s.ctx.Cmd("systemctl", a, s.name)
	case "openrc":
		res = s.ctx.Cmd("rc-service", s.name, a)
	default:
		res = s.ctx.Cmd("service", s.name, a)
	}
	if !res.OK() {
		return res.Error(a)
	}
	return nil
}

func (s *svc) enable(on bool) error {
	var res core.CmdResult
	switch s.provider {
	case "systemd":
		res = s.ctx.Cmd("systemctl", map[bool]string{true: "enable", false: "disable"}[on], s.name)
	case "openrc":
		res = s.ctx.Cmd("rc-update", map[bool]string{true: "add", false: "del"}[on], s.name, "default")
	default:
		if s.ctx.Has("update-rc.d") {
			res = s.ctx.Cmd("update-rc.d", s.name, map[bool]string{true: "enable", false: "disable"}[on])
		} else {
			res = s.ctx.Cmd("chkconfig", s.name, map[bool]string{true: "on", false: "off"}[on])
		}
	}
	if !res.OK() {
		return res.Error("enable")
	}
	return nil
}

func (t serviceType) Check(ctx *core.Ctx, r *core.Resource) ([]core.Change, error) {
	s, err := t.svc(ctx, r)
	if err != nil {
		return nil, err
	}
	var changes []core.Change
	if want := r.Str("ensure"); want != "" {
		cur := "stopped"
		if s.running() {
			cur = "running"
		}
		if cur != want {
			changes = append(changes, core.Change{Prop: "ensure", From: cur, To: want})
		}
	}
	if r.Has("enabled") {
		if cur, want := s.enabled(), r.Bool("enabled"); cur != want {
			changes = append(changes, core.Change{Prop: "enabled", From: strconv.FormatBool(cur), To: strconv.FormatBool(want)})
		}
	}
	return changes, nil
}

func (t serviceType) Apply(ctx *core.Ctx, r *core.Resource, changes []core.Change) error {
	s, err := t.svc(ctx, r)
	if err != nil {
		return err
	}
	for _, c := range changes {
		switch c.Prop {
		case "enabled":
			err = s.enable(r.Bool("enabled"))
		case "ensure":
			err = s.action(map[string]string{"running": "start", "stopped": "stop"}[c.To])
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Snapshot records whether the service runs and starts at boot.
func (t serviceType) Snapshot(ctx *core.Ctx, r *core.Resource) (core.Snapshot, error) {
	s, err := t.svc(ctx, r)
	if err != nil {
		return nil, err
	}
	return core.Snapshot{"running": s.running(), "enabled": s.enabled()}, nil
}

func (t serviceType) Restore(ctx *core.Ctx, r *core.Resource, snap core.Snapshot) error {
	s, err := t.svc(ctx, r)
	if err != nil {
		return err
	}
	if want, ok := snap["enabled"].(bool); ok && want != s.enabled() {
		if err := s.enable(want); err != nil {
			return err
		}
	}
	if want, ok := snap["running"].(bool); ok && want != s.running() {
		return s.action(map[bool]string{true: "start", false: "stop"}[want])
	}
	return nil
}

func (t serviceType) Refresh(ctx *core.Ctx, r *core.Resource, applied []core.Change) (bool, error) {
	for _, c := range applied {
		if c.Prop == "ensure" {
			return false, nil // just started or stopped; nothing to restart
		}
	}
	s, err := t.svc(ctx, r)
	if err != nil {
		return false, err
	}
	if r.Str("ensure") == "stopped" || !s.running() {
		return false, nil
	}
	if r.Bool("reload") {
		return true, s.action("reload")
	}
	return true, s.action("restart")
}
