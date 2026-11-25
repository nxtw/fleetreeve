package compile

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Host is a machine to manage and how to reach it.
type Host struct {
	Name       string   `yaml:"-"`
	Address    string   `yaml:"address"`
	User       string   `yaml:"user"`
	Port       int      `yaml:"port"`
	Identity   string   `yaml:"identity"`
	Connection string   `yaml:"connection"` // "ssh" (default) or "local"
	Become     *bool    `yaml:"become"`
	SSHArgs    []string `yaml:"ssh_args"`
	// HostKeyCheck false connects without verifying the host's SSH key:
	// for machines that are rebuilt all the time. It gives up SSH's
	// protection against connecting to the wrong machine.
	HostKeyCheck *bool          `yaml:"host_key_check"`
	Vars         map[string]any `yaml:"vars"`
	Groups       []string       `yaml:"-"`
}

// SkipHostKey reports whether the host's SSH key goes unverified.
func (h *Host) SkipHostKey() bool { return h.HostKeyCheck != nil && !*h.HostKeyCheck }

func (h *Host) Sudo() bool { return h.Become != nil && *h.Become }

type group struct {
	Hosts []string       `yaml:"hosts"`
	Vars  map[string]any `yaml:"vars"`
}

// Inventory lists hosts, groups them, and layers data onto them.
type Inventory struct {
	Vars   map[string]any
	Hosts  []*Host // declaration order
	byName map[string]*Host
	groups map[string]*group
	order  []string // group names, declaration order
}

// Localhost is the implicit inventory used when none is given.
func Localhost() *Inventory {
	h := &Host{Name: "localhost", Address: "localhost", Connection: "local"}
	return &Inventory{Hosts: []*Host{h}, byName: map[string]*Host{h.Name: h}, groups: map[string]*group{}}
}

func LoadInventory(path string) (*Inventory, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Vars     map[string]any `yaml:"vars"`
		Defaults Host           `yaml:"defaults"`
		Hosts    yaml.Node      `yaml:"hosts"`
		Groups   yaml.Node      `yaml:"groups"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	inv := &Inventory{Vars: raw.Vars, byName: map[string]*Host{}, groups: map[string]*group{}}
	host := func(name string) *Host {
		h, ok := inv.byName[name]
		if !ok {
			h = &Host{Name: name}
			inv.byName[name] = h
			inv.Hosts = append(inv.Hosts, h)
		}
		return h
	}
	// A mapping node's children alternate key, value; iterating them keeps
	// the order hosts and groups were written in.
	for i := 0; i+1 < len(raw.Hosts.Content); i += 2 {
		name, n := raw.Hosts.Content[i].Value, raw.Hosts.Content[i+1]
		h := host(name)
		if err := knownKeys(n, "address", "user", "port", "identity", "connection", "become", "ssh_args", "host_key_check", "vars"); err != nil {
			return nil, fmt.Errorf("%s:%d: host %s: %v", path, n.Line, name, err)
		}
		if err := n.Decode(h); err != nil {
			return nil, fmt.Errorf("%s: host %s: %v", path, name, err)
		}
		h.Name = name
	}
	for i := 0; i+1 < len(raw.Groups.Content); i += 2 {
		name := raw.Groups.Content[i].Value
		g := &group{}
		if err := knownKeys(raw.Groups.Content[i+1], "hosts", "vars"); err != nil {
			return nil, fmt.Errorf("%s:%d: group %s: %v", path, raw.Groups.Content[i+1].Line, name, err)
		}
		if err := raw.Groups.Content[i+1].Decode(g); err != nil {
			return nil, fmt.Errorf("%s: group %s: %v", path, name, err)
		}
		if _, clash := inv.byName[name]; clash || name == "all" {
			return nil, fmt.Errorf("%s: group name %q clashes with a host or the built-in group \"all\"", path, name)
		}
		inv.groups[name], inv.order = g, append(inv.order, name)
		for _, hn := range g.Hosts {
			h := host(hn)
			h.Groups = append(h.Groups, name)
		}
	}
	d := raw.Defaults
	for _, h := range inv.Hosts {
		if h.Address == "" {
			h.Address = h.Name
		}
		if h.User == "" {
			h.User = d.User
		}
		if h.Port == 0 {
			h.Port = d.Port
		}
		if h.Identity == "" {
			h.Identity = d.Identity
		}
		if h.Connection == "" {
			h.Connection = d.Connection
		}
		if h.Connection == "" {
			h.Connection = "ssh"
		}
		if h.Connection != "ssh" && h.Connection != "local" {
			return nil, fmt.Errorf("%s: host %s: connection must be ssh or local", path, h.Name)
		}
		if h.Become == nil {
			h.Become = d.Become
		}
		if h.SSHArgs == nil {
			h.SSHArgs = d.SSHArgs
		}
		if h.HostKeyCheck == nil {
			h.HostKeyCheck = d.HostKeyCheck
		}
	}
	if len(inv.Hosts) == 0 {
		return nil, fmt.Errorf("%s: inventory defines no hosts", path)
	}
	return inv, nil
}

// Host returns the host with exactly this name.
func (inv *Inventory) Host(name string) (*Host, bool) {
	h, ok := inv.byName[name]
	return h, ok
}

// HasGroup reports whether the inventory defines a group.
func (inv *Inventory) HasGroup(name string) bool { _, ok := inv.groups[name]; return ok }

// AddHost adds a host that is not in the inventory file, with its groups
// and its own variables. Groups the inventory does not define are created
// empty: the host is a member, but gets no variables from them. A host
// that already exists is left as the file has it.
func (inv *Inventory) AddHost(name string, groups []string, vars map[string]any) bool {
	if _, exists := inv.byName[name]; exists {
		return false
	}
	h := &Host{Name: name, Address: name, Connection: "ssh", Vars: vars}
	inv.byName[name] = h
	inv.Hosts = append(inv.Hosts, h)
	for _, g := range groups {
		if _, ok := inv.groups[g]; !ok {
			inv.groups[g] = &group{}
			inv.order = append(inv.order, g)
		}
		inv.groups[g].Hosts = append(inv.groups[g].Hosts, name)
		h.Groups = append(h.Groups, g)
	}
	return true
}

// knownKeys rejects settings that are not part of the inventory format. A
// variable written directly under a host, instead of under its `vars`,
// would otherwise be ignored without a word.
func knownKeys(n *yaml.Node, allowed ...string) error {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i].Value
		ok := false
		for _, a := range allowed {
			ok = ok || a == key
		}
		if !ok {
			return fmt.Errorf("unknown setting %q (expected %s); variables belong under vars, e.g. vars: {%s: ...}", key, strings.Join(allowed, ", "), key)
		}
	}
	return nil
}

// Select returns the hosts matching a comma-separated list of host and
// group names, in inventory order. An empty limit or "all" selects everything.
func (inv *Inventory) Select(limit string) ([]*Host, error) {
	if limit == "" || limit == "all" {
		return inv.Hosts, nil
	}
	want := map[string]bool{}
	for _, name := range strings.Split(limit, ",") {
		name = strings.TrimSpace(name)
		if g, ok := inv.groups[name]; ok {
			for _, h := range g.Hosts {
				want[h] = true
			}
		} else if _, ok := inv.byName[name]; ok {
			want[name] = true
		} else {
			return nil, fmt.Errorf("no host or group named %q in the inventory", name)
		}
	}
	var out []*Host
	for _, h := range inv.Hosts {
		if want[h.Name] {
			out = append(out, h)
		}
	}
	return out, nil
}

// HostVars layers inventory data for a host: all < groups (in declaration
// order) < host.
func (inv *Inventory) HostVars(h *Host) map[string]any {
	layers := []map[string]any{inv.Vars}
	for _, g := range h.Groups {
		layers = append(layers, inv.groups[g].Vars)
	}
	return merge(append(layers, h.Vars)...)
}

// VarLayer is one source of inventory variables for a host.
type VarLayer struct {
	Name string
	Vars map[string]any
}

// VarLayers lists where a host's inventory variables come from, lowest
// precedence first.
func (inv *Inventory) VarLayers(h *Host) []VarLayer {
	layers := []VarLayer{{"inventory vars", inv.Vars}}
	for _, g := range h.Groups {
		layers = append(layers, VarLayer{"inventory group " + g, inv.groups[g].Vars})
	}
	return append(layers, VarLayer{"inventory host " + h.Name, h.Vars})
}

// Data is the view of the inventory exposed to templates as .inventory.
// more, when not nil, supplies further variables of each host (its data
// layer), which take precedence over its inventory variables.
func (inv *Inventory) Data(more func(*Host) map[string]any) map[string]any {
	groups, hosts := map[string]any{}, map[string]any{}
	for _, name := range inv.order {
		l := make([]any, len(inv.groups[name].Hosts))
		for i, h := range inv.groups[name].Hosts {
			l[i] = h
		}
		groups[name] = l
	}
	for _, h := range inv.Hosts {
		vars := inv.HostVars(h)
		if more != nil {
			vars = merge(vars, more(h))
		}
		hosts[h.Name] = HostData(h, vars)
	}
	return map[string]any{"groups": groups, "hosts": hosts}
}

// HostData is the view of a host exposed to templates as .host.
func HostData(h *Host, vars map[string]any) map[string]any {
	groups := make([]any, len(h.Groups))
	for i, g := range h.Groups {
		groups[i] = g
	}
	return map[string]any{"name": h.Name, "address": h.Address, "groups": groups, "vars": vars}
}
