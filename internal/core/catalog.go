package core

import (
	"container/heap"
	"fmt"
	"strings"
)

// Catalog is the fully compiled desired state of one host: no variables, no
// templates, no conditionals left. It is what gets shipped to the node.
type Catalog struct {
	Version   int            `json:"version"`
	Host      string         `json:"host"`
	Name      string         `json:"name,omitempty"`  // state name; empty runs stateless
	Prune     bool           `json:"prune,omitempty"` // remove resources dropped from the manifest
	Facts     map[string]any `json:"facts,omitempty"`
	Resources []*Resource    `json:"resources"`
	// Types holds the script types from modules that the resources use,
	// with their scripts: nodes need nothing installed to run them.
	Types map[string]*TypeDef `json:"types,omitempty"`
}

// Normalize validates every resource in the catalog.
func (c *Catalog) Normalize() error {
	for _, r := range c.Resources {
		if err := Normalize(r); err != nil {
			return fmt.Errorf("%s: %v", r.ID(), err)
		}
	}
	return nil
}

// Edge kinds, used for graph output.
const (
	EdgeRequire = "require"
	EdgeNotify  = "notify"
	EdgeAuto    = "auto"
)

type Edge struct{ From, To, Kind string }

// Graph is the resolved dependency graph of a catalog.
type Graph struct {
	Order []*Resource         // dependency order, declaration order as tie-break
	Deps  map[string][]string // ID -> IDs it depends on
	Edges []Edge
}

// Graph resolves references and returns the resources in an order that
// satisfies every dependency. Where the graph leaves a choice, declaration
// order is kept, so runs are deterministic and read top to bottom.
func (c *Catalog) Graph() (*Graph, error) {
	idx := make(map[string]int, len(c.Resources))
	ids := make([]string, len(c.Resources))
	for i, r := range c.Resources {
		id := r.ID()
		if j, dup := idx[id]; dup {
			return nil, fmt.Errorf("duplicate resource %s (declared at %s and %s)", id, c.Resources[j].Origin, r.Origin)
		}
		idx[id], ids[i] = i, id
	}
	g := &Graph{Deps: map[string][]string{}}
	add := func(from, to, kind string) {
		if !contains(g.Deps[to], from) {
			g.Deps[to] = append(g.Deps[to], from)
			g.Edges = append(g.Edges, Edge{from, to, kind})
		}
	}
	check := func(r *Resource, rel, ref string) error {
		if _, ok := idx[ref]; !ok {
			return fmt.Errorf("%s (%s): %s references %s, which is not declared%s", r.ID(), r.Origin, rel, ref, Suggest(ref, ids))
		}
		if ref == r.ID() {
			return fmt.Errorf("%s (%s): %s references itself", r.ID(), r.Origin, rel)
		}
		return nil
	}
	for _, r := range c.Resources {
		for _, ref := range r.Require {
			if err := check(r, "require", ref); err != nil {
				return nil, err
			}
			add(ref, r.ID(), EdgeRequire)
		}
		for _, ref := range r.Notify {
			if err := check(r, "notify", ref); err != nil {
				return nil, err
			}
			add(r.ID(), ref, EdgeNotify)
		}
	}
	// Implicit edges never override explicit ordering: skip any that would
	// close a cycle.
	for _, r := range c.Resources {
		t, _ := Lookup(r.Type)
		ar, ok := t.(AutoRequirer)
		if !ok {
			continue
		}
		for _, ref := range ar.AutoRequire(r) {
			if _, exists := idx[ref]; !exists || ref == r.ID() || g.dependsOn(ref, r.ID()) {
				continue
			}
			add(ref, r.ID(), EdgeAuto)
		}
	}

	// Kahn's algorithm with a min-heap on declaration index.
	indeg := make([]int, len(ids))
	dependents := make(map[string][]int)
	for id, deps := range g.Deps {
		indeg[idx[id]] = len(deps)
		for _, d := range deps {
			dependents[d] = append(dependents[d], idx[id])
		}
	}
	ready := &intHeap{}
	for i, d := range indeg {
		if d == 0 {
			heap.Push(ready, i)
		}
	}
	for ready.Len() > 0 {
		i := heap.Pop(ready).(int)
		g.Order = append(g.Order, c.Resources[i])
		for _, j := range dependents[ids[i]] {
			if indeg[j]--; indeg[j] == 0 {
				heap.Push(ready, j)
			}
		}
	}
	if len(g.Order) != len(ids) {
		return nil, fmt.Errorf("dependency cycle: %s", strings.Join(g.findCycle(ids, indeg), " -> "))
	}
	return g, nil
}

// dependsOn reports whether a (transitively) depends on b.
func (g *Graph) dependsOn(a, b string) bool {
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(n string) bool {
		if n == b {
			return true
		}
		if seen[n] {
			return false
		}
		seen[n] = true
		for _, d := range g.Deps[n] {
			if walk(d) {
				return true
			}
		}
		return false
	}
	return walk(a)
}

func (g *Graph) findCycle(ids []string, indeg []int) []string {
	stuck := map[string]bool{}
	start := ""
	for i, d := range indeg {
		if d > 0 {
			stuck[ids[i]] = true
			if start == "" {
				start = ids[i]
			}
		}
	}
	// Every stuck node has a stuck dependency, so following them must loop.
	path, pos := []string{}, map[string]int{}
	for n := start; ; {
		if p, ok := pos[n]; ok {
			cyc := append(path[p:], n)
			// Deps point backwards; reverse so the arrow reads "then".
			for l, r := 0, len(cyc)-1; l < r; l, r = l+1, r-1 {
				cyc[l], cyc[r] = cyc[r], cyc[l]
			}
			return cyc
		}
		pos[n] = len(path)
		path = append(path, n)
		for _, d := range g.Deps[n] {
			if stuck[d] {
				n = d
				break
			}
		}
	}
}

type intHeap []int

func (h intHeap) Len() int           { return len(h) }
func (h intHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h intHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *intHeap) Push(x any)        { *h = append(*h, x.(int)) }
func (h *intHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}
