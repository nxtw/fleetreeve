package compile

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// The data layer keeps data out of code. Manifests say what to manage;
// the values they use live in a tree of YAML files, and each host gets the
// values of the files that apply to it, the most specific file winning:
//
//	data/
//	  common.yaml               every host
//	  os/redhat.yaml            hosts of that OS family
//	  groups/web.yaml           hosts in that inventory group
//	  hosts/web2.yaml           that one host
//
// That is the default hierarchy, used when a `data` directory sits next to
// the manifest. A manifest can define its own with the top-level `data:` key.

// DataConfig is the `data:` key of the root manifest.
type DataConfig struct {
	Dir string
	// Hierarchy lists the levels, most specific first. Each is a path below
	// Dir, without extension, and may use {{ .host.name }}, {{ .facts... }},
	// inventory variables, and {{ .group }} - which makes the level apply
	// once for every group the host is in.
	Hierarchy []string
	// Merge names keys that are combined across levels instead of the most
	// specific level replacing the others: "deep" merges maps key by key,
	// "unique" concatenates lists and drops duplicates.
	Merge map[string]string
}

var defaultHierarchy = []string{
	"hosts/{{ .host.name }}",
	"groups/{{ .group }}",
	"os/{{ .facts.os.family }}",
	"common",
}

// DataLevel is one file of the hierarchy as it resolved for a host.
type DataLevel struct {
	File   string         // path below the data directory
	Status string         // "used", "no such file", or why the level was skipped
	Vars   map[string]any // its content when used
}

func parseDataConfig(n *yaml.Node) (*DataConfig, error) {
	v, err := toAny(n)
	if err != nil {
		return nil, err
	}
	c := &DataConfig{}
	switch x := v.(type) {
	case string:
		c.Dir = x
	case map[string]any:
		for k, e := range x {
			switch k {
			case "dir":
				c.Dir = fmt.Sprint(e)
			case "hierarchy":
				list, ok := e.([]any)
				if !ok || len(list) == 0 {
					return nil, fmt.Errorf("data: hierarchy must be a non-empty list, most specific level first")
				}
				for _, l := range list {
					c.Hierarchy = append(c.Hierarchy, fmt.Sprint(l))
				}
			case "merge":
				m, ok := e.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("data: merge must map keys to deep or unique")
				}
				c.Merge = map[string]string{}
				for key, how := range m {
					if how != "deep" && how != "unique" {
						return nil, fmt.Errorf("data: merge: %s: %v is not deep or unique", key, how)
					}
					c.Merge[key] = how.(string)
				}
			default:
				return nil, fmt.Errorf("data: unknown key %q (expected dir, hierarchy, merge)", k)
			}
		}
	default:
		return nil, fmt.Errorf("data must be a directory name or a mapping with dir, hierarchy and merge")
	}
	if c.Dir == "" {
		c.Dir = "data"
	}
	return c, nil
}

// dataTree resolves the data layer for hosts. Files are read once per
// compilation, however many hosts ask for them.
type dataTree struct {
	conf  *DataConfig
	dir   string // absolute
	files map[string]map[string]any
	errs  map[string]error
}

// newDataTree returns the tree configured by the root manifest, or nil
// when there is none: no `data:` key and no data directory beside it.
func newDataTree(root *Manifest, rootPath string) (*dataTree, error) {
	conf := root.Data
	base := filepath.Dir(rootPath)
	if conf == nil {
		if st, err := os.Stat(filepath.Join(base, "data")); err != nil || !st.IsDir() {
			return nil, nil
		}
		conf = &DataConfig{Dir: "data"}
	}
	if len(conf.Hierarchy) == 0 {
		conf = &DataConfig{Dir: conf.Dir, Hierarchy: defaultHierarchy, Merge: conf.Merge}
	}
	dir := conf.Dir
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(base, dir)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("%s: data directory %s does not exist", rootPath, dir)
	}
	return &dataTree{conf: conf, dir: dir, files: map[string]map[string]any{}, errs: map[string]error{}}, nil
}

func (t *dataTree) read(rel string) (map[string]any, error) {
	if v, ok := t.files[rel]; ok {
		return v, t.errs[rel]
	}
	var vars map[string]any
	var err error
	for _, ext := range []string{".yaml", ".yml"} {
		var b []byte
		if b, err = os.ReadFile(filepath.Join(t.dir, rel+ext)); err == nil {
			var doc yaml.Node
			if err = yaml.Unmarshal(b, &doc); err == nil && doc.Kind != 0 {
				var v any
				if v, err = toAny(doc.Content[0]); err == nil {
					var isMap bool
					if vars, isMap = v.(map[string]any); !isMap && v != nil {
						err = fmt.Errorf("must be a mapping of variables")
					}
				}
			}
			if vars == nil && err == nil {
				vars = map[string]any{} // an empty file
			}
			if err != nil {
				err = fmt.Errorf("%s: %v", filepath.Join(t.conf.Dir, rel+ext), err)
			}
			break
		}
		if !os.IsNotExist(err) {
			break
		}
		err = nil
	}
	t.files[rel], t.errs[rel] = vars, err
	return vars, err
}

// levels resolves the hierarchy for one host, most specific first. ctx is
// what level names may refer to: inventory variables, .host and .facts.
func (t *dataTree) levels(h *Host, ctx map[string]any) ([]DataLevel, error) {
	var out []DataLevel
	for _, tmpl := range t.conf.Hierarchy {
		scopes := []map[string]any{ctx}
		if strings.Contains(tmpl, ".group") {
			scopes = nil
			// The inventory lets later groups win, so here too.
			for i := len(h.Groups) - 1; i >= 0; i-- {
				scopes = append(scopes, merge(ctx, map[string]any{"group": h.Groups[i]}))
			}
		}
		for _, scope := range scopes {
			rel, err := render(tmpl, scope)
			if err != nil {
				// A level that needs something this host does not have (a
				// fact while compiling offline, an optional variable)
				// simply does not apply.
				out = append(out, DataLevel{File: tmpl, Status: "skipped: " + err.Error()})
				continue
			}
			rel = strings.TrimSuffix(strings.TrimSuffix(rel, ".yaml"), ".yml")
			if rel == "" || strings.Contains(rel, "..") || filepath.IsAbs(rel) {
				out = append(out, DataLevel{File: tmpl, Status: fmt.Sprintf("skipped: %q is not a path inside the data directory", rel)})
				continue
			}
			vars, err := t.read(rel)
			if err != nil {
				return nil, err
			}
			lvl := DataLevel{File: rel + ".yaml", Status: "no such file"}
			if vars != nil {
				lvl.Status, lvl.Vars = "used", vars
			}
			out = append(out, lvl)
		}
	}
	return out, nil
}

// resolve merges a host's levels, least specific first, into its data.
func (t *dataTree) resolve(levels []DataLevel) map[string]any {
	out := map[string]any{}
	for i := len(levels) - 1; i >= 0; i-- {
		for k, v := range levels[i].Vars {
			prev, had := out[k]
			switch {
			case had && t.conf.Merge[k] == "deep":
				out[k] = deepMerge(prev, v)
			case had && t.conf.Merge[k] == "unique":
				out[k] = uniqueMerge(prev, v)
			default:
				out[k] = v
			}
		}
	}
	return out
}

// forHost returns the data of a host. facts may be nil (another host, or
// an offline compile): levels that depend on facts are then skipped.
func (t *dataTree) forHost(inv *Inventory, h *Host, facts map[string]any) (map[string]any, []DataLevel, error) {
	invVars := inv.HostVars(h)
	ctx := merge(invVars, map[string]any{"host": HostData(h, invVars)})
	if facts != nil {
		ctx["facts"] = facts
	}
	levels, err := t.levels(h, ctx)
	if err != nil {
		return nil, nil, err
	}
	return t.resolve(levels), levels, nil
}

func deepMerge(base, over any) any {
	bm, ok1 := base.(map[string]any)
	om, ok2 := over.(map[string]any)
	if !ok1 || !ok2 {
		return over
	}
	out := merge(bm)
	for k, v := range om {
		if prev, ok := out[k]; ok {
			out[k] = deepMerge(prev, v)
		} else {
			out[k] = v
		}
	}
	return out
}

func uniqueMerge(base, over any) any {
	bl, ok1 := base.([]any)
	ol, ok2 := over.([]any)
	if !ok1 || !ok2 {
		return over
	}
	out := append([]any{}, bl...)
	for _, v := range ol {
		dup := false
		for _, have := range out {
			dup = dup || reflect.DeepEqual(have, v)
		}
		if !dup {
			out = append(out, v)
		}
	}
	return out
}

// ---- explanation ----

// VarSource is one place that sets a variable.
type VarSource struct {
	Source string
	Value  any
}

// Explanation shows how a host's variables come about.
type Explanation struct {
	DataDir string      // "" when the manifest has no data layer
	Levels  []DataLevel // most specific first
	Merge   map[string]string
	Vars    map[string]any         // final values, as the root manifest sees them
	Sources map[string][]VarSource // per variable, lowest precedence first
}

// Explain resolves the variables of a host without compiling its catalog,
// and records where each value comes from.
func Explain(in Input) (*Explanation, error) {
	in.Facts, _ = wholeNumbers(in.Facts).(map[string]any)
	in.Extra, _ = wholeNumbers(in.Extra).(map[string]any)
	c, root, err := newCompiler(in)
	if err != nil {
		return nil, err
	}
	ex := &Explanation{Sources: map[string][]VarSource{}, Levels: c.dataLevels}
	add := func(source string, vars map[string]any) {
		for k, v := range vars {
			ex.Sources[k] = append(ex.Sources[k], VarSource{source, v})
		}
	}
	add(filepath.Base(in.Manifest)+" (default)", root.Vars)
	for _, l := range in.Inventory.VarLayers(in.Host) {
		add(l.Name, l.Vars)
	}
	if c.tree != nil {
		ex.DataDir, ex.Merge = c.tree.conf.Dir, c.tree.conf.Merge
		for i := len(c.dataLevels) - 1; i >= 0; i-- {
			add(filepath.Join(c.tree.conf.Dir, c.dataLevels[i].File), c.dataLevels[i].Vars)
		}
	}
	add("-e on the command line", in.Extra)
	data, err := c.data(merge(root.Vars, c.invVars, c.dataVars, in.Extra))
	if err != nil {
		return nil, fmt.Errorf("%s: vars: %v", in.Manifest, err)
	}
	ex.Vars = map[string]any{}
	for k := range ex.Sources {
		ex.Vars[k] = data[k]
	}
	return ex, nil
}

// Keys returns the explained variables, sorted.
func (e *Explanation) Keys() []string {
	keys := make([]string, 0, len(e.Vars))
	for k := range e.Vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
