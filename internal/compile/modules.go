package compile

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nxtw/fleetreeve/internal/core"
	"github.com/nxtw/fleetreeve/internal/types"
)

// Modules extend fleetreeve without changing it. A module is a directory:
//
//	modules/acme.nginx/
//	  module.yaml        name, version, description
//	  roles/main.yaml    manifests to include:  - module: acme.nginx
//	  roles/tls.yaml                            - {module: acme.nginx, role: tls}
//	  types/vhost.yaml   resource types, used as  - acme.nginx.vhost: www
//	  types/…            and the scripts and templates they need
//	  templates/ files/
//
// A type is either composite - a parameterised bundle of other resources,
// expanded while compiling - or a script: a program that checks and
// applies the resource on the node (see types.NewScript).
//
// Modules are looked for in `modules/` next to the manifest, then in the
// directories of $FLEETREEVE_MODULES, then in ~/.fleetreeve/modules and
// /usr/share/fleetreeve/modules. The first one found under a name wins.

// Module is an installed module.
type Module struct {
	Name        string `yaml:"name"`
	Version     string `yaml:"version"`
	Description string `yaml:"description"`
	// Vars are the module's defaults: variables its roles and types can
	// rely on, overridden by anything the project sets.
	Vars  map[string]any `yaml:"vars"`
	Dir   string         `yaml:"-"`
	Types map[string]*ModuleType
	Roles []string
}

// ModuleType is a resource type defined by a module.
type ModuleType struct {
	Name   string // fully qualified: module.type
	Module *Module
	Doc    string
	Params map[string]core.ParamDef
	Schema core.Schema
	File   string // the definition, for messages and relative paths

	// One of the two:
	Resources []Decl        // composite
	Script    *core.TypeDef // script
}

// Kind says what a type is made of.
func (t *ModuleType) Kind() string {
	if t.Script != nil {
		return "script"
	}
	return "composite"
}

var moduleName = regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$`)
var typeName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// ValidModuleName reports whether a name has the form namespace.name.
func ValidModuleName(name string) bool { return moduleName.MatchString(name) }

// ModulePath lists where modules are looked for, for a manifest.
func ModulePath(manifest string) []string {
	dirs := []string{filepath.Join(filepath.Dir(manifest), "modules")}
	dirs = append(dirs, filepath.SplitList(os.Getenv("FLEETREEVE_MODULES"))...)
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".fleetreeve", "modules"))
	}
	return append(dirs, "/usr/share/fleetreeve/modules")
}

// LoadModules reads every module on the path. Earlier directories win.
func LoadModules(dirs []string) (map[string]*Module, error) {
	mods := map[string]*Module{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // not every directory on the path has to exist
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if _, seen := mods[e.Name()]; seen {
				continue
			}
			mdir := filepath.Join(dir, e.Name())
			if _, err := os.Stat(filepath.Join(mdir, "module.yaml")); err != nil {
				continue
			}
			m, err := LoadModule(mdir)
			if err != nil {
				return nil, err
			}
			mods[m.Name] = m
		}
	}
	return mods, nil
}

// LoadModule reads one module directory.
func LoadModule(dir string) (*Module, error) {
	b, err := os.ReadFile(filepath.Join(dir, "module.yaml"))
	if err != nil {
		return nil, fmt.Errorf("%s is not a module: %v", dir, err)
	}
	m := &Module{Dir: dir, Types: map[string]*ModuleType{}}
	if err := yaml.Unmarshal(b, m); err != nil {
		return nil, fmt.Errorf("%s/module.yaml: %v", dir, err)
	}
	if !ValidModuleName(m.Name) {
		return nil, fmt.Errorf("%s/module.yaml: name %q must have the form namespace.name, in lower case", dir, m.Name)
	}
	if base := filepath.Base(dir); base != m.Name {
		return nil, fmt.Errorf("%s: the directory must be called %s, like the module in it", dir, m.Name)
	}
	roles, _ := filepath.Glob(filepath.Join(dir, "roles", "*.yaml"))
	for _, r := range roles {
		m.Roles = append(m.Roles, strings.TrimSuffix(filepath.Base(r), ".yaml"))
	}
	defs, _ := filepath.Glob(filepath.Join(dir, "types", "*.yaml"))
	for _, f := range defs {
		t, err := loadType(m.Name, f)
		if err != nil {
			return nil, err
		}
		t.Module = m
		m.Types[t.Name] = t
	}
	return m, nil
}

func loadType(module, file string) (*ModuleType, error) {
	short := strings.TrimSuffix(filepath.Base(file), ".yaml")
	if !typeName.MatchString(short) {
		return nil, fmt.Errorf("%s: a type's file name must be a lower-case word", file)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Description string                   `yaml:"description"`
		Params      map[string]core.ParamDef `yaml:"params"`
		Script      string                   `yaml:"script"`
		Refresh     bool                     `yaml:"refresh"`
		Resources   yaml.Node                `yaml:"resources"`
	}
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("%s: %v", file, err)
	}
	t := &ModuleType{Name: module + "." + short, Doc: raw.Description, Params: raw.Params, File: file}
	if t.Schema, err = core.ParamSchema(raw.Params); err != nil {
		return nil, fmt.Errorf("%s: %v", file, err)
	}
	for p := range raw.Params {
		if metaKeys[p] || p == "title" {
			return nil, fmt.Errorf("%s: a parameter cannot be called %q", file, p)
		}
	}
	hasResources := raw.Resources.Kind == yaml.SequenceNode
	switch {
	case raw.Script != "" && hasResources:
		return nil, fmt.Errorf("%s: a type has either a script or resources, not both", file)
	case raw.Script != "":
		program, err := os.ReadFile(filepath.Join(filepath.Dir(file), raw.Script))
		if err != nil {
			return nil, fmt.Errorf("%s: script: %v", file, err)
		}
		t.Script = &core.TypeDef{Name: t.Name, Doc: t.Doc, Params: raw.Params, Script: string(program), Refresh: raw.Refresh}
	case hasResources:
		for _, n := range raw.Resources.Content {
			d, err := parseDecl(n)
			if err != nil {
				return nil, fmt.Errorf("%s:%d: %v", file, n.Line, err)
			}
			t.Resources = append(t.Resources, *d)
		}
	default:
		return nil, fmt.Errorf("%s: a type needs a script, or resources to be made of", file)
	}
	return t, nil
}

// moduleSet is what a compilation knows about modules.
type moduleSet struct {
	mods  map[string]*Module
	types map[string]*ModuleType
}

func loadModuleSet(manifest string) (*moduleSet, error) {
	mods, err := LoadModules(ModulePath(manifest))
	if err != nil {
		return nil, err
	}
	set := &moduleSet{mods: mods, types: map[string]*ModuleType{}}
	for _, m := range mods {
		for name, t := range m.Types {
			set.types[name] = t
			// Script types become ordinary resource types, so that
			// declarations are validated like any other.
			if t.Script != nil {
				st, err := types.NewScript(t.Script)
				if err != nil {
					return nil, fmt.Errorf("%s: %v", t.File, err)
				}
				core.Register(st)
			}
		}
	}
	return set, nil
}

// typeNames lists the module types, for "did you mean".
func (s *moduleSet) typeNames() []string {
	names := make([]string, 0, len(s.types))
	for n := range s.types {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// role returns the manifest of a module's role, and the module.
func (s *moduleSet) role(module, role string) (string, *Module, error) {
	m, ok := s.mods[module]
	if !ok {
		names := make([]string, 0, len(s.mods))
		for n := range s.mods {
			names = append(names, n)
		}
		return "", nil, fmt.Errorf("module %s is not installed%s (fleetreeve module list shows what is)", module, core.Suggest(module, names))
	}
	if role == "" {
		role = "main"
	}
	for _, r := range m.Roles {
		if r == role {
			return filepath.Join(m.Dir, "roles", role+".yaml"), m, nil
		}
	}
	return "", nil, fmt.Errorf("module %s has no role %q (it has: %s)", module, role, strings.Join(m.Roles, ", "))
}

// composite is one use of a composite type: the resources it expanded
// into stand in for it wherever it is referred to.
type composite struct {
	members []string
	origin  string
}
