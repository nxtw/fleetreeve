# Manifests

```yaml
name: site                         # state name; defaults to the file name

vars:                              # defaults; lowest precedence
  http_port: 80

include:
  - roles/base.yaml
  - manifest: roles/web.yaml       # a manifest is a reusable role
    when: contains "web" .host.groups
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
    when: eq .facts.os.family "redhat"
```

**Resource types:** `file`, `line`, `package`, `service`, `exec`, `user`,
`group`, plus those of installed [modules](modules.md). Run
`fleetreeve types <name>` for the attributes of each.

**Metaparameters** (any resource): `require`, `before`, `notify`,
`subscribe`, `when`, `each`, `rollback`. References are written `type[title]`.

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
