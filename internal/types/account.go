package types

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/nxtw/fleetreeve/internal/core"
)

func init() {
	core.Register(userType{})
	core.Register(groupType{})
}

// getent returns the colon-separated fields of a passwd/group entry looked
// up by name or numeric id, or nil when there is none.
func getent(ctx *core.Ctx, db, key string) []string {
	if ctx.Has("getent") {
		res := ctx.Cmd("getent", db, key)
		if !res.OK() {
			return nil
		}
		return strings.Split(strings.TrimSpace(strings.SplitN(res.Stdout, "\n", 2)[0]), ":")
	}
	b, _ := os.ReadFile("/etc/" + db)
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Split(l, ":")
		if len(f) >= 3 && (f[0] == key || f[2] == key) {
			return f
		}
	}
	return nil
}

func run(ctx *core.Ctx, what string, argv ...string) error {
	if res := ctx.Cmd(argv[0], argv[1:]...); !res.OK() {
		return res.Error(what)
	}
	return nil
}

// ---- group ----

type groupType struct{}

func (groupType) Name() string { return "group" }
func (groupType) Doc() string  { return "Manages a local group (Linux)." }

func (groupType) Schema() core.Schema {
	return core.Schema{
		"name":   {Kind: core.String, NameVar: true, Doc: "Group name (defaults to the title)."},
		"ensure": {Kind: core.String, Default: "present", Enum: []string{"present", "absent"}},
		"gid":    {Kind: core.Int, Doc: "Numeric group id."},
		"system": {Kind: core.Bool, Doc: "Create as a system group."},
	}
}

func (groupType) Absent(r *core.Resource) map[string]any {
	return map[string]any{"name": r.Str("name"), "ensure": "absent"}
}

func (groupType) Check(ctx *core.Ctx, r *core.Resource) ([]core.Change, error) {
	ent := getent(ctx, "group", r.Str("name"))
	if r.Str("ensure") == "absent" {
		if ent == nil {
			return nil, nil
		}
		return []core.Change{{Prop: "ensure", From: "present", To: "absent"}}, nil
	}
	if ent == nil {
		return []core.Change{{Prop: "ensure", From: "absent", To: "present"}}, nil
	}
	if gid, ok := r.Int("gid"); ok && len(ent) > 2 && ent[2] != strconv.Itoa(gid) {
		return []core.Change{{Prop: "gid", From: ent[2], To: strconv.Itoa(gid)}}, nil
	}
	return nil, nil
}

func (groupType) Apply(ctx *core.Ctx, r *core.Resource, changes []core.Change) error {
	name := r.Str("name")
	gid, hasGid := r.Int("gid")
	shadow := ctx.Has("groupadd")
	switch c := changes[0]; {
	case c.Prop == "ensure" && c.To == "absent":
		if shadow {
			return run(ctx, "groupdel", "groupdel", name)
		}
		return run(ctx, "delgroup", "delgroup", name)
	case c.Prop == "ensure":
		argv := []string{"groupadd"}
		sys := "-r"
		if !shadow {
			argv, sys = []string{"addgroup"}, "-S"
		}
		if hasGid {
			argv = append(argv, "-g", strconv.Itoa(gid))
		}
		if r.Bool("system") {
			argv = append(argv, sys)
		}
		return run(ctx, argv[0], append(argv, name)...)
	default:
		return run(ctx, "groupmod", "groupmod", "-g", strconv.Itoa(gid), name)
	}
}

// ---- user ----

type userType struct{}

func (userType) Name() string { return "user" }
func (userType) Doc() string  { return "Manages a local user account (Linux)." }

func (userType) Schema() core.Schema {
	return core.Schema{
		"name":        {Kind: core.String, NameVar: true, Doc: "Login name (defaults to the title)."},
		"ensure":      {Kind: core.String, Default: "present", Enum: []string{"present", "absent"}},
		"uid":         {Kind: core.Int, Doc: "Numeric user id."},
		"group":       {Kind: core.String, Doc: "Primary group (name or gid)."},
		"groups":      {Kind: core.List, Doc: "Supplementary groups; the list is authoritative."},
		"shell":       {Kind: core.String, Doc: "Login shell."},
		"home":        {Kind: core.String, Doc: "Home directory."},
		"comment":     {Kind: core.String, Doc: "Full name / GECOS field."},
		"system":      {Kind: core.Bool, Doc: "Create as a system account."},
		"create_home": {Kind: core.Bool, Default: true, Doc: "Create the home directory with the account."},
	}
}

func (userType) Absent(r *core.Resource) map[string]any {
	return map[string]any{"name": r.Str("name"), "ensure": "absent"}
}

func (userType) AutoRequire(r *core.Resource) []string {
	var refs []string
	for _, g := range append([]string{r.Str("group")}, r.List("groups")...) {
		if g != "" {
			refs = append(refs, "group["+g+"]")
		}
	}
	return refs
}

func (userType) Check(ctx *core.Ctx, r *core.Resource) ([]core.Change, error) {
	name := r.Str("name")
	ent := getent(ctx, "passwd", name)
	if r.Str("ensure") == "absent" {
		if ent == nil {
			return nil, nil
		}
		return []core.Change{{Prop: "ensure", From: "present", To: "absent"}}, nil
	}
	if ent == nil {
		return []core.Change{{Prop: "ensure", From: "absent", To: "present"}}, nil
	}
	if len(ent) < 7 {
		return nil, fmt.Errorf("malformed passwd entry for %s", name)
	}
	var changes []core.Change
	diff := func(prop, cur, want string) {
		if cur != want {
			changes = append(changes, core.Change{Prop: prop, From: cur, To: want})
		}
	}
	if uid, ok := r.Int("uid"); ok {
		diff("uid", ent[2], strconv.Itoa(uid))
	}
	if g := r.Str("group"); g != "" {
		cur := ent[3]
		if _, err := strconv.Atoi(g); err != nil { // compare by name
			if ge := getent(ctx, "group", ent[3]); ge != nil {
				cur = ge[0]
			}
		}
		diff("group", cur, g)
	}
	for prop, i := range map[string]int{"comment": 4, "home": 5, "shell": 6} {
		if r.Has(prop) {
			diff(prop, ent[i], r.Str(prop))
		}
	}
	if r.Has("groups") {
		want := append([]string(nil), r.List("groups")...)
		sort.Strings(want)
		var cur []string
		primary := strings.TrimSpace(ctx.Cmd("id", "-gn", name).Stdout)
		for _, g := range strings.Fields(ctx.Cmd("id", "-Gn", name).Stdout) {
			if g != primary {
				cur = append(cur, g)
			}
		}
		sort.Strings(cur)
		diff("groups", strings.Join(cur, ","), strings.Join(want, ","))
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Prop < changes[j].Prop })
	return changes, nil
}

func (userType) Apply(ctx *core.Ctx, r *core.Resource, changes []core.Change) error {
	name := r.Str("name")
	shadow := ctx.Has("useradd")
	if c := changes[0]; c.Prop == "ensure" {
		if c.To == "absent" {
			if shadow {
				return run(ctx, "userdel", "userdel", name)
			}
			return run(ctx, "deluser", "deluser", name)
		}
		if !shadow {
			return busyboxAddUser(ctx, r)
		}
		argv := []string{"useradd"}
		if uid, ok := r.Int("uid"); ok {
			argv = append(argv, "-u", strconv.Itoa(uid))
		}
		for flag, attr := range map[string]string{"-g": "group", "-s": "shell", "-d": "home", "-c": "comment"} {
			if r.Has(attr) {
				argv = append(argv, flag, r.Str(attr))
			}
		}
		if r.Has("groups") {
			argv = append(argv, "-G", strings.Join(r.List("groups"), ","))
		}
		if r.Bool("system") {
			argv = append(argv, "-r")
		}
		argv = append(argv, map[bool]string{true: "-m", false: "-M"}[r.Bool("create_home")])
		return run(ctx, "useradd", append(argv, name)...)
	}
	if !shadow {
		return fmt.Errorf("modifying an existing user needs usermod (install the shadow package)")
	}
	flags := map[string]string{"uid": "-u", "group": "-g", "shell": "-s", "home": "-d", "comment": "-c", "groups": "-G"}
	argv := []string{"usermod"}
	for _, c := range changes {
		argv = append(argv, flags[c.Prop], c.To)
	}
	return run(ctx, "usermod", append(argv, name)...)
}

func busyboxAddUser(ctx *core.Ctx, r *core.Resource) error {
	argv := []string{"adduser", "-D"}
	if uid, ok := r.Int("uid"); ok {
		argv = append(argv, "-u", strconv.Itoa(uid))
	}
	for flag, attr := range map[string]string{"-G": "group", "-s": "shell", "-h": "home", "-g": "comment"} {
		if r.Has(attr) {
			argv = append(argv, flag, r.Str(attr))
		}
	}
	if r.Bool("system") {
		argv = append(argv, "-S")
	}
	if !r.Bool("create_home") {
		argv = append(argv, "-H")
	}
	name := r.Str("name")
	if err := run(ctx, "adduser", append(argv, name)...); err != nil {
		return err
	}
	for _, g := range r.List("groups") {
		if err := run(ctx, "addgroup", "addgroup", name, g); err != nil {
			return err
		}
	}
	return nil
}
