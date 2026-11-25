// Package compile turns manifests, inventory data and facts into a
// per-host catalog. Everything here runs on the controller: by the time a
// catalog leaves, every variable, template and condition has been resolved
// and every resource has been validated.
package compile

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/nxtw/fleetreeve/internal/core"
	"github.com/nxtw/fleetreeve/internal/state"
)

// Input is everything needed to compile the catalog of one host.
type Input struct {
	Manifest  string
	Host      *Host
	Inventory *Inventory
	Facts     map[string]any
	Extra     map[string]any // command-line variables, highest precedence
	Prune     bool
}

// StateName is the name under which nodes keep state for a manifest: its
// `name:` key, or else its file name without the extension.
func StateName(manifest string) (string, error) {
	abs, err := filepath.Abs(manifest)
	if err != nil {
		return "", err
	}
	m, err := loadManifest(abs)
	if err != nil {
		return "", err
	}
	name := m.Name
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(manifest), filepath.Ext(manifest))
	}
	if !state.ValidName(name) {
		return "", fmt.Errorf("%s: %q is not a usable state name; set `name:` in the manifest (letters, digits, . _ -)", manifest, name)
	}
	return name, nil
}

type compiler struct {
	in         Input
	invVars    map[string]any
	invData    map[string]any
	mods       *moduleSet
	composites map[string]*composite    // uses of composite types, by their own ID
	used       map[string]*core.TypeDef // script types the catalog needs
	roles      map[string]bool          // module roles already included, with their values
	depth      int
	tree       *dataTree      // nil without a data layer
	dataVars   map[string]any // this host's data
	dataLevels []DataLevel
	res        []*core.Resource
	late       []lateRef // before/subscribe, resolved once all resources exist
	stack      []string
}

type lateRef struct{ rel, from, to, origin string }

// Compile builds and validates the catalog for one host.
func Compile(in Input) (*core.Catalog, error) {
	// Facts and -e values arrive as JSON, where every number is a float.
	// Whole numbers become ints so `gt .facts.cpu.count 4` compares cleanly.
	in.Facts, _ = wholeNumbers(in.Facts).(map[string]any)
	in.Extra, _ = wholeNumbers(in.Extra).(map[string]any)
	c, _, err := newCompiler(in)
	if err != nil {
		return nil, err
	}
	if err := c.manifest(in.Manifest, nil, nil); err != nil {
		return nil, err
	}
	// References to a use of a composite type mean everything it is made of.
	for _, r := range c.res {
		r.Require, r.Notify = c.resolveRefs(r.Require), c.resolveRefs(r.Notify)
	}
	byID := map[string]*core.Resource{}
	for _, r := range c.res {
		if prev, dup := byID[r.ID()]; dup {
			return nil, fmt.Errorf("%s: duplicate resource %s (already declared at %s)", r.Origin, r.ID(), prev.Origin)
		}
		byID[r.ID()] = r
	}
	for _, r := range c.res {
		t, _ := core.Lookup(r.Type)
		if ct, ok := t.(core.Conflicter); ok {
			if err := ct.Conflicts(r, func(id string) *core.Resource { return byID[id] }); err != nil {
				return nil, fmt.Errorf("%s: %s: %v", r.Origin, r.ID(), err)
			}
		}
	}
	// A directory that is purged has to know what in it is managed: the
	// paths of every resource in the catalog that lie in it.
	for _, r := range c.res {
		if r.Type != "file" || !r.Bool("purge") {
			continue
		}
		dir := filepath.Clean(r.Str("path")) + string(filepath.Separator)
		keep := []string{}
		for _, o := range c.res {
			for _, attr := range []string{"path", "creates"} {
				if p, ok := o.Attrs[attr].(string); ok && o != r && strings.HasPrefix(filepath.Clean(p), dir) {
					keep = append(keep, filepath.Clean(p))
				}
			}
		}
		sort.Strings(keep)
		r.Attrs["keep"] = keep
	}
	// "A before B" is "B requires A"; "A subscribes to B" is "B notifies A".
	for _, l := range c.late {
		for _, to := range c.members(l.to) {
			target, ok := byID[to]
			if !ok {
				ids := make([]string, 0, len(byID))
				for id := range byID {
					ids = append(ids, id)
				}
				return nil, fmt.Errorf("%s: %s: %s references %s, which is not declared%s", l.origin, l.from, l.rel, l.to, core.Suggest(l.to, ids))
			}
			for _, from := range c.members(l.from) {
				if l.rel == "before" {
					target.Require = append(target.Require, from)
				} else {
					target.Notify = append(target.Notify, from)
				}
			}
		}
	}
	name, err := StateName(in.Manifest)
	if err != nil {
		return nil, err
	}
	cat := &core.Catalog{Version: core.ProtocolVersion, Host: in.Host.Name, Name: name, Prune: in.Prune, Facts: in.Facts, Resources: c.res}
	if len(c.used) > 0 {
		cat.Types = c.used
	}
	if _, err := cat.Graph(); err != nil {
		return nil, err
	}
	return cat, nil
}

// newCompiler prepares a compilation: the host's inventory variables and
// its data layer, as configured by the root manifest.
func newCompiler(in Input) (*compiler, *Manifest, error) {
	abs, err := filepath.Abs(in.Manifest)
	if err != nil {
		return nil, nil, err
	}
	root, err := loadManifest(abs)
	if err != nil {
		return nil, nil, err
	}
	c := &compiler{in: in, invVars: in.Inventory.HostVars(in.Host), composites: map[string]*composite{}, used: map[string]*core.TypeDef{}, roles: map[string]bool{}}
	if c.mods, err = loadModuleSet(abs); err != nil {
		return nil, nil, err
	}
	if c.tree, err = newDataTree(root, abs); err != nil {
		return nil, nil, err
	}
	var others func(*Host) map[string]any
	if c.tree != nil {
		if c.dataVars, c.dataLevels, err = c.tree.forHost(in.Inventory, in.Host, in.Facts); err != nil {
			return nil, nil, err
		}
		// Templates can read other hosts' data too (.inventory.hosts.X.vars),
		// e.g. a load balancer listing its backends. Their facts are not
		// known here, so levels that depend on facts do not apply to them.
		others = func(h *Host) map[string]any {
			if h == in.Host {
				return c.dataVars
			}
			vars, _, _ := c.tree.forHost(in.Inventory, h, nil)
			return vars
		}
	}
	c.invData = in.Inventory.Data(others)
	return c, root, nil
}

// manifest evaluates one manifest file. Variable precedence, lowest first:
// this manifest's vars, enclosing manifests' vars, inventory (all < group <
// host), the data layer (least to most specific level), include parameters,
// command-line variables.
func (c *compiler) manifest(path string, outerDefaults, params map[string]any) error {
	return c.manifestWith(path, outerDefaults, params, nil)
}

// manifestWith is manifest with a layer of defaults below the manifest's
// own: those of the module it belongs to.
func (c *compiler) manifestWith(path string, outerDefaults, params, base map[string]any) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for _, s := range c.stack {
		if s == abs {
			return fmt.Errorf("include cycle: %s includes itself", path)
		}
	}
	c.stack = append(c.stack, abs)
	defer func() { c.stack = c.stack[:len(c.stack)-1] }()

	m, err := loadManifest(abs)
	if err != nil {
		return err
	}
	m = &Manifest{Path: path, Vars: m.Vars, Includes: m.Includes, Resources: m.Resources}
	defaults := merge(base, m.Vars, outerDefaults)
	vars := merge(defaults, c.invVars, c.dataVars, params, c.in.Extra)
	data, err := c.data(vars)
	if err != nil {
		return fmt.Errorf("%s: vars: %v", path, err)
	}

	for _, inc := range m.Includes {
		origin := fmt.Sprintf("%s:%d", path, inc.Line)
		if inc.When != "" {
			ok, err := evalWhen(inc.When, data)
			if err != nil {
				return fmt.Errorf("%s: when: %v", origin, err)
			}
			if !ok {
				continue
			}
		}
		target, err := render(inc.Manifest, data)
		if err != nil {
			return fmt.Errorf("%s: %v", origin, err)
		}
		var base map[string]any
		if inc.Module != "" {
			// A role of a module, found on the module path. The module's
			// own defaults sit below everything else.
			var mod *Module
			if target, mod, err = c.mods.role(inc.Module, inc.Role); err != nil {
				return fmt.Errorf("%s: %v", origin, err)
			}
			base = mod.Vars
		} else {
			target = filepath.Join(filepath.Dir(path), target)
		}
		// Include parameters are evaluated in the including manifest.
		incVars, err := renderAny(inc.Vars, data)
		if err != nil {
			return fmt.Errorf("%s: vars: %v", origin, err)
		}
		iv, _ := incVars.(map[string]any)
		if inc.Module != "" {
			// A role of a module is included once however often it is
			// asked for with the same values: roles include the roles
			// they build on, and so does the project.
			key, _ := json.Marshal([]any{target, merge(params, iv)})
			if c.roles[string(key)] {
				continue
			}
			c.roles[string(key)] = true
		}
		if err := c.manifestWith(target, defaults, merge(params, iv), base); err != nil {
			return err
		}
	}
	for i := range m.Resources {
		if err := c.decl(m, &m.Resources[i], data); err != nil {
			return err
		}
	}
	return nil
}

// data builds the template context: variables at the top level, plus the
// reserved keys facts, host and inventory. Variables may reference each
// other, so they are expanded until they stop changing.
func (c *compiler) data(vars map[string]any) (map[string]any, error) {
	reserved := map[string]any{
		"facts":     c.in.Facts,
		"host":      HostData(c.in.Host, merge(c.invVars, c.dataVars)),
		"inventory": c.invData,
	}
	for pass := 0; ; pass++ {
		data := merge(vars, reserved)
		next, err := renderAny(vars, data)
		if err != nil {
			return nil, err
		}
		if reflect.DeepEqual(next, vars) {
			return data, nil
		}
		if pass == 8 {
			return nil, fmt.Errorf("variables reference each other in a loop")
		}
		vars = next.(map[string]any)
	}
}

func (c *compiler) decl(m *Manifest, d *Decl, data map[string]any) error {
	origin := fmt.Sprintf("%s:%d", m.Path, d.Line)
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s: %s: %s", origin, d.Type, fmt.Sprintf(format, args...))
	}
	// A composite type from a module is expanded here; every other type
	// (built in, or a module's script type) is known to the registry.
	comp := c.mods.types[d.Type]
	if comp != nil && comp.Script != nil {
		comp = nil
	}
	if _, ok := core.Lookup(d.Type); !ok && comp == nil {
		hint := core.Suggest(d.Type, append(core.TypeNames(), c.mods.typeNames()...))
		if hint == "" && strings.Count(d.Type, ".") == 2 {
			hint = fmt.Sprintf(" (it looks like a type of the module %s, which is not installed)", d.Type[:strings.LastIndex(d.Type, ".")])
		}
		return fail("unknown resource type%s", hint)
	}
	items := []any{nil}
	if each, ok := d.Meta["each"]; ok {
		v, err := renderAny(each, data)
		if err != nil {
			return fail("each: %v", err)
		}
		if items, err = toList(v); err != nil {
			return fail("each: %v", err)
		}
	}
	for _, item := range items {
		scope := data
		if item != nil {
			scope = merge(data, map[string]any{"item": item})
		}
		if w, ok := d.Meta["when"]; ok {
			keep, err := evalWhen(fmt.Sprint(w), scope)
			if err != nil {
				return fail("when: %v", err)
			}
			if !keep {
				continue
			}
		}
		attrs, err := renderAny(d.Attrs, scope)
		if err != nil {
			return fail("%v", err)
		}
		rels := map[string][]string{}
		for _, rel := range []string{"require", "before", "notify", "subscribe"} {
			v, err := renderAny(d.Meta[rel], scope)
			if err != nil {
				return fail("%s: %v", rel, err)
			}
			if s, ok := v.(string); ok {
				v = []any{s}
			}
			list, _ := v.([]any)
			for _, e := range list {
				ref, err := core.ParseRef(fmt.Sprint(e))
				if err != nil {
					return fail("%s: %v", rel, err)
				}
				rels[rel] = append(rels[rel], ref)
			}
		}
		for _, rawTitle := range d.Titles {
			title, err := render(rawTitle, scope)
			if err != nil {
				return fail("title: %v", err)
			}
			if comp != nil {
				if err := c.expand(comp, title, attrs.(map[string]any), rels, d.Meta["rollback"], scope, origin); err != nil {
					return err
				}
				continue
			}
			r := &core.Resource{
				Type: d.Type, Title: title, Origin: origin,
				Attrs:   merge(attrs.(map[string]any)), // copy: one map per resource
				Require: append([]string(nil), rels["require"]...),
				Notify:  append([]string(nil), rels["notify"]...),
			}
			if err := core.Normalize(r); err != nil {
				return fmt.Errorf("%s: %s: %v", origin, r.ID(), err)
			}
			t, _ := core.Lookup(r.Type)
			if rb, set := d.Meta["rollback"]; set {
				v, err := renderAny(rb, scope)
				if err != nil {
					return fail("rollback: %v", err)
				}
				if r.Rollback = truthy(v); r.Rollback {
					if _, can := t.(core.Undoer); !can {
						return fmt.Errorf("%s: %s: rollback is not possible for %s resources (it is for file, line, package and service)", origin, r.ID(), r.Type)
					}
				}
			}
			if tc, ok := t.(core.Compiler); ok {
				if err := tc.Compile(r, compileCtx{filepath.Dir(m.Path), scope}); err != nil {
					return fmt.Errorf("%s: %s: %v", origin, r.ID(), err)
				}
			}
			for _, rel := range []string{"before", "subscribe"} {
				for _, ref := range rels[rel] {
					c.late = append(c.late, lateRef{rel, r.ID(), ref, origin})
				}
			}
			if mt := c.mods.types[r.Type]; mt != nil && mt.Script != nil {
				c.used[r.Type] = mt.Script // its script goes along in the catalog
			}
			c.res = append(c.res, r)
		}
	}
	return nil
}

// expand turns one use of a composite type into the resources it is made
// of. Its parameters (and `title`) are variables to those resources.
// Relationships declared on the use apply to all of them, and other
// resources may refer to it by its own name; see resolveComposites.
func (c *compiler) expand(t *ModuleType, title string, attrs map[string]any, rels map[string][]string, rollback any, scope map[string]any, origin string) error {
	id := t.Name + "[" + title + "]"
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s: %s: %s", origin, id, fmt.Sprintf(format, args...))
	}
	if prev, dup := c.composites[id]; dup {
		return fail("declared twice (also at %s)", prev.origin)
	}
	if c.depth++; c.depth > 20 {
		return fail("types are nested more than 20 deep; do two of them use each other?")
	}
	defer func() { c.depth-- }()

	params := map[string]any{"title": title}
	for name, v := range attrs {
		a, ok := t.Schema[name]
		if !ok {
			return fail("unknown parameter %q%s", name, core.Suggest(name, core.ParamNames(t.Schema)))
		}
		if v == nil {
			continue
		}
		cv, err := core.Coerce(a.Kind, v)
		if err != nil {
			return fail("parameter %q: %v", name, err)
		}
		params[name] = generic(cv)
	}
	for name, a := range t.Schema {
		if _, set := params[name]; set {
			continue
		}
		switch {
		case a.Default != nil:
			params[name] = generic(a.Default)
		case a.Required:
			return fail("missing required parameter %q", name)
		}
	}
	first := len(c.res)
	// The module's defaults, then what is in scope where the type is used,
	// then the parameters.
	modVars, err := renderAny(map[string]any(t.Module.Vars), merge(t.Module.Vars, scope))
	if err != nil {
		return fail("the module's vars: %v", err)
	}
	inner := merge(modVars.(map[string]any), scope, params)
	file := &Manifest{Path: t.File}
	for i := range t.Resources {
		if err := c.decl(file, &t.Resources[i], inner); err != nil {
			return fmt.Errorf("%v\n    (in %s, declared at %s)", err, id, origin)
		}
	}
	comp := &composite{origin: origin}
	undo := false
	if rollback != nil {
		v, err := renderAny(rollback, scope)
		if err != nil {
			return fail("rollback: %v", err)
		}
		undo = truthy(v)
	}
	for _, r := range c.res[first:] {
		comp.members = append(comp.members, r.ID())
		r.Require = append(r.Require, rels["require"]...)
		r.Notify = append(r.Notify, rels["notify"]...)
		if undo {
			if rt, _ := core.Lookup(r.Type); rt != nil {
				if _, can := rt.(core.Undoer); can {
					r.Rollback = true
				}
			}
		}
	}
	for _, rel := range []string{"before", "subscribe"} {
		for _, ref := range rels[rel] {
			c.late = append(c.late, lateRef{rel, id, ref, origin})
		}
	}
	c.composites[id] = comp
	return nil
}

// generic turns a coerced parameter back into the plain shapes templates
// and `each` work with.
func generic(v any) any {
	switch x := v.(type) {
	case []string:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = s
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(x))
		for k, s := range x {
			out[k] = s
		}
		return out
	}
	return v
}

// members returns the resources a reference stands for: itself, or - for
// a use of a composite type - everything it expanded into, however deep.
func (c *compiler) members(ref string) []string {
	comp, ok := c.composites[ref]
	if !ok {
		return []string{ref}
	}
	var out []string
	for _, m := range comp.members {
		out = append(out, c.members(m)...)
	}
	return out
}

func (c *compiler) resolveRefs(refs []string) []string {
	var out []string
	for _, ref := range refs {
		for _, m := range c.members(ref) {
			dup := false
			for _, have := range out {
				dup = dup || have == m
			}
			if !dup {
				out = append(out, m)
			}
		}
	}
	return out
}

func wholeNumbers(v any) any {
	switch x := v.(type) {
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return int(x)
		}
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = wholeNumbers(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = wholeNumbers(e)
		}
		return out
	}
	return v
}

type compileCtx struct {
	dir  string
	data map[string]any
}

func (cc compileCtx) ReadFile(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(cc.dir, path)
	}
	return os.ReadFile(path)
}

func (cc compileCtx) Render(text string) (string, error) { return render(text, cc.data) }
