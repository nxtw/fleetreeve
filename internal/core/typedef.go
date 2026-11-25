package core

import (
	"fmt"
	"sort"
)

// Resource types from modules.
//
// A module can add resource types without touching fleetreeve itself. A script
// type is implemented by an executable in any language: the compiler puts
// the script into the catalog, and the node runs it to check and apply the
// resource. Nothing is installed on nodes for it. (The other kind, a
// composite type, is a parameterised bundle of existing resources and is
// expanded by the compiler; it never reaches a node.)

// ParamDef describes one parameter of a module type.
type ParamDef struct {
	Type     string `json:"type,omitempty" yaml:"type"` // string (default), int, bool, list, map
	Required bool   `json:"required,omitempty" yaml:"required"`
	Default  any    `json:"default,omitempty" yaml:"default"`
	Doc      string `json:"description,omitempty" yaml:"description"`
}

// TypeDef is a script type as it travels in a catalog.
type TypeDef struct {
	Name    string              `json:"name"` // fully qualified: module.type
	Doc     string              `json:"description,omitempty"`
	Params  map[string]ParamDef `json:"params,omitempty"`
	Script  string              `json:"script"`            // the program text
	Refresh bool                `json:"refresh,omitempty"` // it reacts to notifications
}

// KindOf maps a parameter type name to a Kind.
func KindOf(name string) (Kind, error) {
	switch name {
	case "", "string":
		return String, nil
	case "int":
		return Int, nil
	case "bool":
		return Bool, nil
	case "list":
		return List, nil
	case "map":
		return Map, nil
	}
	return String, fmt.Errorf("unknown parameter type %q (string, int, bool, list, map)", name)
}

// ParamSchema turns parameter definitions into a Schema.
func ParamSchema(params map[string]ParamDef) (Schema, error) {
	s := Schema{}
	for name, p := range params {
		kind, err := KindOf(p.Type)
		if err != nil {
			return nil, fmt.Errorf("parameter %s: %v", name, err)
		}
		a := Attr{Kind: kind, Required: p.Required, Doc: p.Doc}
		if p.Default != nil {
			if a.Default, err = Coerce(kind, p.Default); err != nil {
				return nil, fmt.Errorf("parameter %s: default: %v", name, err)
			}
		}
		s[name] = a
	}
	return s, nil
}

// Coerce converts a value to the Go type of a Kind, or explains why not.
func Coerce(k Kind, v any) (any, error) { return coerce(k, v) }

// ParamNames lists the parameters of a schema, sorted.
func ParamNames(s Schema) []string {
	names := make([]string, 0, len(s))
	for n := range s {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
