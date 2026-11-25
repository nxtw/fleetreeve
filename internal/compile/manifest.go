package compile

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Manifest is a parsed, not yet evaluated, manifest file.
type Manifest struct {
	Path      string
	Name      string      // state name; defaults to the file name
	Data      *DataConfig // the data layer; only the root manifest's counts
	Vars      map[string]any
	Includes  []Include
	Resources []Decl
}

type Include struct {
	Manifest string
	Module   string // a role of a module instead of a file
	Role     string // which one; "main" when empty
	Vars     map[string]any
	When     string
	Line     int
}

// Decl is one resource declaration, possibly expanding to several resources
// (list title, each).
type Decl struct {
	Type   string
	Titles []string
	Attrs  map[string]any
	Meta   map[string]any // require, before, notify, subscribe, when, each, rollback
	Line   int
}

var metaKeys = map[string]bool{"require": true, "before": true, "notify": true, "subscribe": true, "when": true, "each": true, "rollback": true}

// manifestCache avoids re-parsing a manifest for every host. Entries are
// keyed on modification time and size, so a long-running server picks up
// edits.
var manifestCache sync.Map // path -> cached

type cached struct {
	m     *Manifest
	mtime time.Time
	size  int64
}

func loadManifest(path string) (*Manifest, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if c, ok := manifestCache.Load(path); ok {
		if c := c.(cached); c.mtime.Equal(st.ModTime()) && c.size == st.Size() {
			return c.m, nil
		}
	}
	m, err := parseManifest(path)
	if err != nil {
		return nil, err
	}
	manifestCache.Store(path, cached{m, st.ModTime(), st.Size()})
	return m, nil
}

func parseManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := &Manifest{Path: path}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	if doc.Kind == 0 {
		return m, nil
	}
	root := doc.Content[0]
	fail := func(n *yaml.Node, format string, args ...any) error {
		return fmt.Errorf("%s:%d: %s", path, n.Line, fmt.Sprintf(format, args...))
	}
	if root.Kind != yaml.MappingNode {
		return nil, fail(root, "manifest must be a mapping with vars, include and resources")
	}
	for i := 0; i < len(root.Content); i += 2 {
		key, val := root.Content[i], root.Content[i+1]
		switch key.Value {
		case "name":
			m.Name = val.Value
		case "data":
			conf, err := parseDataConfig(val)
			if err != nil {
				return nil, fail(val, "%v", err)
			}
			m.Data = conf
		case "vars":
			v, err := toAny(val)
			if err != nil {
				return nil, fail(val, "%v", err)
			}
			vars, ok := v.(map[string]any)
			if !ok && v != nil {
				return nil, fail(val, "vars must be a mapping")
			}
			m.Vars = vars
		case "include":
			if val.Kind != yaml.SequenceNode {
				return nil, fail(val, "include must be a list")
			}
			for _, n := range val.Content {
				inc := Include{Line: n.Line}
				v, err := toAny(n)
				if err != nil {
					return nil, fail(n, "%v", err)
				}
				switch x := v.(type) {
				case string:
					inc.Manifest = x
				case map[string]any:
					for k, e := range x {
						ok := true
						switch k {
						case "manifest":
							inc.Manifest, ok = e.(string)
						case "module":
							inc.Module, ok = e.(string)
						case "role":
							inc.Role, ok = e.(string)
						case "when":
							inc.When = fmt.Sprint(e)
						case "vars":
							inc.Vars, ok = e.(map[string]any)
						default:
							return nil, fail(n, "unknown include key %q (expected manifest or module, role, vars, when)", k)
						}
						if !ok {
							return nil, fail(n, "include: invalid value for %q", k)
						}
					}
				}
				if (inc.Manifest == "") == (inc.Module == "") {
					return nil, fail(n, "include needs either a manifest path or a module")
				}
				if inc.Role != "" && inc.Module == "" {
					return nil, fail(n, "role is only for including from a module")
				}
				m.Includes = append(m.Includes, inc)
			}
		case "resources":
			if val.Kind != yaml.SequenceNode {
				return nil, fail(val, "resources must be a list")
			}
			for _, n := range val.Content {
				d, err := parseDecl(n)
				if err != nil {
					return nil, fail(n, "%v", err)
				}
				m.Resources = append(m.Resources, *d)
			}
		default:
			return nil, fail(key, "unknown top-level key %q (expected name, data, vars, include, resources)", key.Value)
		}
	}
	return m, nil
}

// parseDecl reads a resource declaration. The first key is `type: title`;
// the remaining keys are attributes and metaparameters.
func parseDecl(n *yaml.Node) (*Decl, error) {
	if n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n.Kind != yaml.MappingNode || len(n.Content) < 2 {
		return nil, fmt.Errorf("a resource must be a mapping starting with `type: title`")
	}
	d := &Decl{Type: strings.ToLower(n.Content[0].Value), Line: n.Line, Attrs: map[string]any{}, Meta: map[string]any{}}
	title, err := toAny(n.Content[1])
	if err != nil {
		return nil, err
	}
	switch t := title.(type) {
	case []any:
		for _, e := range t {
			d.Titles = append(d.Titles, fmt.Sprint(e))
		}
	case nil, map[string]any:
		return nil, fmt.Errorf("%s: the first key of a resource must be `type: title`", d.Type)
	default:
		d.Titles = []string{fmt.Sprint(t)}
	}
	if len(d.Titles) == 0 {
		return nil, fmt.Errorf("%s: empty title list", d.Type)
	}
	for i := 2; i < len(n.Content); i += 2 {
		k := n.Content[i].Value
		v, err := toAny(n.Content[i+1])
		if err != nil {
			return nil, err
		}
		if metaKeys[k] {
			d.Meta[k] = v
		} else {
			d.Attrs[k] = v
		}
	}
	return d, nil
}

// toAny converts a YAML node to plain Go values. Unlike a normal decode it
// keeps zero-prefixed numbers such as 0644 as strings, so file modes survive
// without quoting.
func toAny(n *yaml.Node) (any, error) {
	switch n.Kind {
	case yaml.AliasNode:
		return toAny(n.Alias)
	case yaml.MappingNode:
		m := make(map[string]any, len(n.Content)/2)
		for i := 0; i < len(n.Content); i += 2 {
			v, err := toAny(n.Content[i+1])
			if err != nil {
				return nil, err
			}
			m[n.Content[i].Value] = v
		}
		return m, nil
	case yaml.SequenceNode:
		l := make([]any, len(n.Content))
		for i, c := range n.Content {
			v, err := toAny(c)
			if err != nil {
				return nil, err
			}
			l[i] = v
		}
		return l, nil
	}
	if n.ShortTag() == "!!int" && len(n.Value) > 1 && n.Value[0] == '0' && strings.Trim(n.Value, "0123456789") == "" {
		return n.Value, nil
	}
	var v any
	err := n.Decode(&v)
	return v, err
}
