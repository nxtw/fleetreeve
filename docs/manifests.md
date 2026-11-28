# Manifests

```yaml
name: site                         # state name; defaults to the file name

vars:                              # defaults; lowest precedence
  http_port: 80

include:
  - roles/base.yaml
  - manifest: roles/web.yaml       # a manifest is a reusable role
    when: host.groups has "web"
    vars: {http_port: 8080}

resources:
  - package: nginx                 # first key is always  type: title

  - file: /etc/nginx/conf.d/site.conf
    template: templates/site.conf.tmpl
    mode: 0644
    validate: nginx -t             # must pass before the file is replaced
    require: package[nginx]
    notify: service[nginx]         # restart/reload only when this changes

  - service: nginx
    ensure: running
    enabled: true

  - package: [curl, git, jq]       # a list title declares several resources

  - user: "{{ .item.name }}"       # each: one resource per item
    each: "{{ .admins }}"
    uid: "{{ .item.uid }}"

  - package: chrony
    when: facts.os.family == "redhat"
```

**Resource types:** `file`, `line`, `package`, `service`, `exec`, `user`,
`group`, plus those of installed [modules](modules.md). Run
`fleetreeve types <name>` for the attributes of each.

**Metaparameters** (any resource): `require`, `before`, `notify`,
`subscribe`, `when`, `each`, `rollback`. References are written `type[title]`.

**Conditions** (`when:`) are written the way they are read. Variables are
named as they are, without braces or a leading dot:

```yaml
when: facts.os.family == "redhat"
when: docker_repo and facts.os.family == "redhat"
when: host.groups has "web"
when: not sudo and (env != "production" or uid > 0)
```

| | |
| --- | --- |
| a variable on its own | true unless it is `false`, `0`, empty (text, list or map), or one of the texts `false`, `no`, `off`, `0` |
| `==` `!=` `<` `<=` `>` `>=` | comparing two values |
| `list has item`, `item in list`, `item not in list` | membership; with two texts, whether one is part of the other |
| `not`, `and`, `or` | in that order of precedence: `not a and b or c` is `((not a) and b) or c`. Brackets group otherwise |
| `name.field`, `name["a-key"]`, `list[0]` | a field of a map; a key that is not a plain name; an item by position |
| `"text"`, `'text'`, `12`, `true`, `false` | values |
| `f(a, b)` | a function from the list below: `defined("item.shell")`, `len(keys) > 1` |

A condition is available on every resource and on every `include`. A
variable that does not exist is a compile error, as everywhere; ask with
`defined("name")` first for optional ones. A condition whose text would
start with a quote needs quoting as a whole in YAML, which `has` avoids:
`host.groups has "web"` rather than `'"web" in host.groups'`.

Conditions can also be written as template expressions, which is how they
were written up to version 0.1.3: `and .docker_repo (eq .facts.os.family
"redhat")`. That form keeps working and means the same.

**Templating** is Go `text/template`, in any string and in `template:` files.
Variables are at the top level (`{{ .http_port }}`); also available are
`.facts` (see `fleetreeve facts`), `.host` (`name`, `address`, `groups`),
`.inventory` (`groups`, `hosts`) and, inside `each`, `.item`. A string that
is a single `{{ expression }}` keeps its type, so lists, numbers and booleans
pass through. Referencing an undefined variable is a compile error; use
`{{ get "name" "fallback" }}` for optional ones. Extra functions: `get`,
`defined`, `default`, `contains`, `join`, `split`, `replace`, `lower`,
`upper`, `trim`, `hasPrefix`, `hasSuffix`, `quote`, `indent`, `toJson`,
`b64enc`, `env`, `add`, `sub`, `mul`, `div`, `mod`, `required`, `list`, `dict`.

**Variable precedence**, lowest to highest: manifest `vars` → enclosing
manifests' `vars` → inventory `vars` → group `vars` → host `vars` → the
[data layer](data.md) → `include` `vars` →
`-e key=value`.

## Purging: directories that hold only what is managed

Some directories are read as a whole - `conf.d`, `sudoers.d`, `cron.d`,
a directory of virtual hosts. A file left behind there, or dropped in by
hand, takes effect although nothing declares it. `purge` makes such a
directory hold only what your configuration manages:

```yaml
resources:
  - file: /etc/nginx/conf.d
    ensure: directory
    purge: true
    ignore: ["*.rpmsave", "README"]     # names to leave alone, as patterns
    notify: service[nginx]

  - file: /etc/nginx/conf.d/www.conf    # managed: stays
    template: templates/www.conf.tmpl
```

```
$ fleetreeve plan site.yaml
  ~ file[/etc/nginx/conf.d]  purge: 2 unmanaged (default.conf, old-site.conf) → removed
      -default.conf
      -old-site.conf
```

- **Managed** is whatever a resource of the same catalog has as its path:
  files, links and directories declared with `file`, files edited with
  `line`, what an `exec` says it `creates`. A subdirectory that holds
  something managed is kept too.
- Only the directory's own entries are looked at. `recurse: true` also
  purges inside the subdirectories that are kept.
- Unmanaged **files and links** are removed. Unmanaged **subdirectories**
  are left alone unless you add `force: true`, which removes them with
  everything in them.
- A dry run lists exactly what would go, and removes nothing.
- What a purge removes is kept like any other change: `fleetreeve rollback
  'file[/etc/nginx/conf.d]'` puts it back (up to 32 MB per purge; beyond
  that the files are removed without a copy). Remember that the next run
  purges again unless the resource is on hold - which a manual rollback
  arranges.
- fleetreeve refuses to purge `/`, `/etc`, `/usr`, `/var` and the like
  directly: purge a directory of your own below them.

Compared with removing resources from the manifest (see [State](state-and-rollback.md#state)),
which only undoes what fleetreeve itself created, purging also removes what
fleetreeve never knew about. That is its point, and the reason to use it only
on directories that are yours.
