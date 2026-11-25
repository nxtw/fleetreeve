# Data: keeping values out of code

Manifests say *what* to manage. The values they use - ports, package
lists, account names, anything that differs between hosts - can live in a
separate tree of YAML files, so that changing a value never means editing a
manifest, and a role works unchanged on every host. Put a `data` directory
next to your manifest:

```
site.yaml
roles/…
data/
  common.yaml           every host
  os/redhat.yaml        hosts of that OS family (.facts.os.family)
  groups/web.yaml       hosts in that inventory group
  hosts/web2.yaml       that one host
```

```yaml
# data/common.yaml          # data/os/redhat.yaml        # data/hosts/web2.yaml
http_port: 80               chrony_service: chronyd      http_port: 9090
chrony_service: chrony
```

```yaml
# roles/base.yaml - no values, no conditionals on the OS
- service: chrony
  name: "{{ .chrony_service }}"
```

Each host gets the files that apply to it, and for every key the **most
specific file wins**: `hosts/` over `groups/` over `os/` over `common`. A
host in several groups takes the later group first, as in the inventory.
Data files may use templates, facts and other variables, like any value.

`fleetreeve lookup` answers "what does this host get, and why":

```
$ fleetreeve lookup -i inventory.yaml -l web2 site.yaml http_port
▸ web2
  http_port = 9090  ← data/hosts/web2.yaml
      data/common.yaml          80    overridden
      data/groups/web.yaml      8080  overridden
      data/hosts/web2.yaml      9090  wins
```

Without a variable name it lists every variable of the host with its
source, and which data files applied. It asks the host for its facts, since
they decide which files apply; `-offline` uses this machine's instead, and
`-facts <file>` those saved earlier with `fleetreeve facts` - which also lets
`validate` and `graph` check a Linux fleet from a laptop.

The hierarchy and how keys combine can be set in the root manifest:

```yaml
data:
  dir: data
  hierarchy:                         # most specific first
    - "hosts/{{ .host.name }}"
    - "groups/{{ .group }}"          # once per group of the host
    - "env/{{ .env }}"               # a level chosen by an inventory variable
    - "os/{{ .facts.os.family }}"
    - common
  merge:
    open_ports: unique               # lists add up across levels, without duplicates
    sysctl: deep                     # maps merge key by key
```

A level that refers to something a host does not have is skipped for that
host. By default a key is replaced as a whole; `merge` is for the keys that
should accumulate, such as the firewall ports each group contributes.

Where the data layer sits among the other sources, lowest precedence first:
manifest `vars` (defaults) → inventory `vars` (all, groups, host) → **data**
(least to most specific file) → `include` `vars` → `-e`. With a data layer,
the inventory is best kept to what it is for: which hosts exist, how to
reach them, and their groups.

Templates can read other hosts' data through
`.inventory.hosts.<name>.vars`, as the fleet example's load balancer does
for its backends. Levels that depend on facts do not apply there: another
host's facts are not known while compiling this one.

[`examples/fleet`](../examples/fleet) is built this way: its roles contain no
values at all.
