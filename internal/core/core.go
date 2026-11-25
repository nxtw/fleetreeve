// Package core holds the resource model shared by the compiler (controller
// side) and the engine (node side): resources, type schemas, the catalog and
// its dependency graph.
package core

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// ProtocolVersion guards the controller <-> agent catalog format.
const ProtocolVersion = 3

// Resource is one unit of desired state, e.g. file[/etc/motd].
type Resource struct {
	Type    string         `json:"type"`
	Title   string         `json:"title"`
	Attrs   map[string]any `json:"attrs,omitempty"`
	Require []string       `json:"require,omitempty"` // IDs that must be applied first
	Notify  []string       `json:"notify,omitempty"`  // IDs to refresh when this changes
	Origin  string         `json:"origin,omitempty"`  // manifest file:line
	// Rollback asks for this resource's change to be undone when something
	// it notifies fails in the same run. Only for types that are Undoers.
	Rollback bool `json:"rollback,omitempty"`
}

// ID is the canonical reference, "type[title]".
func (r *Resource) ID() string { return r.Type + "[" + r.Title + "]" }

func (r *Resource) Has(k string) bool { _, ok := r.Attrs[k]; return ok }

func (r *Resource) Str(k string) string {
	s, _ := r.Attrs[k].(string)
	return s
}

func (r *Resource) Bool(k string) bool {
	b, _ := r.Attrs[k].(bool)
	return b
}

func (r *Resource) Int(k string) (int, bool) {
	i, ok := r.Attrs[k].(int)
	return i, ok
}

func (r *Resource) List(k string) []string {
	l, _ := r.Attrs[k].([]string)
	return l
}

func (r *Resource) Map(k string) map[string]string {
	m, _ := r.Attrs[k].(map[string]string)
	return m
}

// ParseRef parses and canonicalises a "type[title]" reference.
func ParseRef(s string) (string, error) {
	s = strings.TrimSpace(s)
	i := strings.IndexByte(s, '[')
	if i <= 0 || !strings.HasSuffix(s, "]") || len(s) < i+3 {
		return "", fmt.Errorf("invalid resource reference %q (expected type[title])", s)
	}
	return strings.ToLower(s[:i]) + s[i:], nil
}

// Change is one property that differs between current and desired state.
type Change struct {
	Prop string `json:"prop"`
	From string `json:"from"`
	To   string `json:"to"`
	Diff string `json:"diff,omitempty"`
}

// Kind is the data type of an attribute.
type Kind int

const (
	String Kind = iota
	Bool
	Int
	List
	Map
)

func (k Kind) String() string {
	return [...]string{"string", "bool", "int", "list", "map"}[k]
}

// Attr describes one attribute of a resource type.
type Attr struct {
	Kind     Kind
	Required bool
	NameVar  bool // defaults to the resource title
	Default  any
	Enum     []string
	Internal bool // produced by the compiler, not user-facing
	Doc      string
}

type Schema map[string]Attr

// Type is a resource type. Check reports what differs from the desired
// state without touching the system; Apply makes those changes.
type Type interface {
	Name() string
	Doc() string
	Schema() Schema
	Check(ctx *Ctx, r *Resource) ([]Change, error)
	Apply(ctx *Ctx, r *Resource, changes []Change) error
}

// Refresher is implemented by types that react to notifications (restart a
// service, re-run a command). applied holds the changes made to the resource
// itself in this run. It returns whether a refresh was actually performed.
type Refresher interface {
	Refresh(ctx *Ctx, r *Resource, applied []Change) (bool, error)
}

// Validator adds cross-attribute validation on top of the schema.
type Validator interface {
	Validate(r *Resource) error
}

// AutoRequirer returns IDs this resource implicitly depends on. Only IDs
// present in the catalog become edges, and explicit ordering always wins.
type AutoRequirer interface {
	AutoRequire(r *Resource) []string
}

// Remover is implemented by types whose resources can be undone. Absent
// returns the attributes of a declaration that removes r; it is recorded in
// the node's state and applied when r is dropped from the manifest.
type Remover interface {
	Absent(r *Resource) map[string]any
}

// Snapshot is what a resource looked like at one moment, in a form that
// survives being stored as JSON.
type Snapshot map[string]any

// Undoer is implemented by types that can put a resource back the way it
// was: Snapshot records its state before a change, Restore returns to it.
// It is what makes `rollback: true` and `fleetreeve rollback` possible.
type Undoer interface {
	Snapshot(ctx *Ctx, r *Resource) (Snapshot, error)
	Restore(ctx *Ctx, r *Resource, s Snapshot) error
}

// Conflicter lets a type reject catalogs in which it would fight another
// resource over the same state. lookup returns nil for undeclared IDs.
type Conflicter interface {
	Conflicts(r *Resource, lookup func(id string) *Resource) error
}

// CompileCtx gives a type access to controller-side files and templating.
type CompileCtx interface {
	ReadFile(path string) ([]byte, error) // relative to the declaring manifest
	Render(text string) (string, error)
}

// Compiler is implemented by types that resolve controller-side inputs
// (source files, templates) into the catalog.
type Compiler interface {
	Compile(r *Resource, cc CompileCtx) error
}

var registry = map[string]Type{}

func Register(t Type) { registry[t.Name()] = t }

func Lookup(name string) (Type, bool) { t, ok := registry[name]; return t, ok }

func TypeNames() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Normalize validates a resource against its type schema and coerces
// attribute values to their canonical Go types. It is idempotent, so the
// agent re-runs it on catalogs received as JSON.
func Normalize(r *Resource) error {
	t, ok := Lookup(r.Type)
	if !ok {
		return fmt.Errorf("unknown resource type %q%s", r.Type, Suggest(r.Type, TypeNames()))
	}
	schema := t.Schema()
	if r.Attrs == nil {
		r.Attrs = map[string]any{}
	}
	for k, v := range r.Attrs {
		a, ok := schema[k]
		if !ok {
			names := make([]string, 0, len(schema))
			for n, sa := range schema {
				if !sa.Internal {
					names = append(names, n)
				}
			}
			return fmt.Errorf("unknown attribute %q%s", k, Suggest(k, names))
		}
		if v == nil {
			delete(r.Attrs, k)
			continue
		}
		cv, err := coerce(a.Kind, v)
		if err != nil {
			return fmt.Errorf("attribute %q: %v", k, err)
		}
		if s, isStr := cv.(string); isStr && len(a.Enum) > 0 && !contains(a.Enum, s) {
			return fmt.Errorf("attribute %q: %q is not one of %s", k, s, strings.Join(a.Enum, ", "))
		}
		r.Attrs[k] = cv
	}
	for k, a := range schema {
		if r.Has(k) {
			continue
		}
		switch {
		case a.NameVar:
			r.Attrs[k] = r.Title
		case a.Default != nil:
			r.Attrs[k] = a.Default
		case a.Required:
			return fmt.Errorf("missing required attribute %q", k)
		}
	}
	if v, ok := t.(Validator); ok {
		return v.Validate(r)
	}
	return nil
}

func coerce(k Kind, v any) (any, error) {
	switch k {
	case String:
		switch x := v.(type) {
		case string:
			return x, nil
		case bool:
			return strconv.FormatBool(x), nil
		case int:
			return strconv.Itoa(x), nil
		case int64:
			return strconv.FormatInt(x, 10), nil
		case uint64:
			return strconv.FormatUint(x, 10), nil
		case float64:
			return strconv.FormatFloat(x, 'f', -1, 64), nil
		}
	case Bool:
		switch x := v.(type) {
		case bool:
			return x, nil
		case string:
			switch strings.ToLower(x) {
			case "true", "yes", "on":
				return true, nil
			case "false", "no", "off":
				return false, nil
			}
		}
	case Int:
		switch x := v.(type) {
		case int:
			return x, nil
		case int64:
			return int(x), nil
		case float64:
			if x == math.Trunc(x) {
				return int(x), nil
			}
		case string:
			if i, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
				return i, nil
			}
		}
	case List:
		switch x := v.(type) {
		case []string:
			return x, nil
		case string:
			return []string{x}, nil
		case []any:
			out := make([]string, 0, len(x))
			for _, e := range x {
				s, err := coerce(String, e)
				if err != nil {
					return nil, err
				}
				out = append(out, s.(string))
			}
			return out, nil
		}
	case Map:
		switch x := v.(type) {
		case map[string]string:
			return x, nil
		case map[string]any:
			out := make(map[string]string, len(x))
			for mk, e := range x {
				s, err := coerce(String, e)
				if err != nil {
					return nil, err
				}
				out[mk] = s.(string)
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("expected %s, got %T (%v)", k, v, v)
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Suggest returns ` (did you mean "x"?)` for the closest candidate, or "".
func Suggest(s string, candidates []string) string {
	best, bestD := "", len(s)/2+2
	for _, c := range candidates {
		if d := levenshtein(s, c); d < bestD {
			best, bestD = c, d
		}
	}
	if best == "" {
		return ""
	}
	return fmt.Sprintf(" (did you mean %q?)", best)
}

func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
