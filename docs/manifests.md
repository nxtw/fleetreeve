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

**Conditions** (`when:`) decide per host whether a resource or an include
applies: `when: facts.os.family == "redhat"`. See
[Conditions](#conditions) below.

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

## Conditions

`when:` on a resource, or on an `include`, says for which hosts it
applies. Where the condition is false the resource is not in that host's
catalog at all. A condition is written the way it is read, with variables
named as they are - no braces, no leading dot:

```yaml
resources:
  - package: policycoreutils-python-utils
    when: facts.os.family == "redhat"                     # by operating system

  - service: nginx
    when: host.groups has "web"                           # by inventory group

  - file: /etc/motd
    content: "Test system\n"
    when: env != "production" and not quiet               # by your own variables

  - file: "/etc/sudoers.d/{{ .item.name }}"
    each: "{{ .users }}"
    when: item.admin                                      # per item of a loop
    content: "{{ .item.name }} ALL=(ALL) ALL\n"

include:
  - module: fleetreeve.docker
    when: host.groups has "docker" and facts.os.family != "darwin"
```

### What a condition can say

| | | |
| --- | --- | --- |
| a variable | `monitoring` | true unless it is `false`, `0`, empty (text, list or map), or one of the texts `false`, `no`, `off`, `0` |
| equal, not equal | `env == "production"`, `env != "production"` | |
| smaller, larger | `port < 1024`, `port <= 1024`, `uid > 0`, `uid >= 1000` | |
| in a list | `host.groups has "web"`, `"web" in host.groups`, `"web" not in host.groups` | the first two mean the same |
| part of a text | `host.name has "db"`, `"db" in host.name` | |
| not | `not quiet` | |
| and | `a and b` | both |
| or | `a or b` | at least one |
| brackets | `a and (b or c)` | |

**Values:** a variable (`env`), a field of one (`facts.os.family`,
`item.name`), text in double or single quotes (`"web"`, `'web'`), a number
(`8080`), `true`, `false`.

**What is available** is what templates have, by the same names: your
variables, `facts` (see `fleetreeve facts`), `host` (`name`, `address`,
`groups`), `inventory`, and inside `each`, `item`.

**Without brackets**, `not` is applied first, then `and`, then `or`:
`not a and b or c` is `((not a) and b) or c`. Comparisons come before all
three, so `not uid > 0` is `not (uid > 0)`. When in doubt, add brackets.

**Keys that are not plain names**, and items by position, go in square
brackets: `limits["max-size"] == "10m"`, `servers[0] == "a"`.

**Functions** are those of the [templates](#manifests), written with
brackets: `len(keys) > 1`, `defined("item.shell")`,
`hasPrefix("web", host.name)`, `lower(env) == "production"`.

### Things to know

- **A variable that does not exist is an error**, as everywhere in
  fleetreeve, so that a typing mistake does not silently switch
  something off. For a variable that may be missing, ask first; what
  follows `and` is not looked at when the first part is false:

  ```yaml
  when: defined("backup_target") and backup_target != ""
  ```

- **Compare like with like.** `port == 8080` compares numbers and
  `port == "8080"` texts; comparing a number with a text is an error
  that says so. What a variable is follows from how it is written in
  your data: `port: 8080` is a number, `port: "8080"` a text.
- **Text that means no is no.** A variable holding the text `no`,
  `false`, `off` or `0` counts as false, alone and next to `and`, `or`
  and `not`. This helps with values that arrive as text, from `-e` on the
  command line for instance.
- **`has` reads well and needs no extra quotes.** YAML requires a value
  that begins with a quote to be quoted as a whole, so the other
  spelling becomes `when: '"web" in host.groups'`.
- **Slips are explained.** `=` where `==` was meant, `&&`, `||` and `!`
  each get a message that says what to write.
- A condition is not a place for `{{ }}`. If you do write a template
  there, it is rendered and its output judged by the first row of the
  table above.

### The older form

Up to version 0.1.3 a condition was a template expression, with the
operator first and a dot before every variable. That form keeps working
and means the same; you do not have to change anything. Side by side:

| Older | Now |
| --- | --- |
| `eq .facts.os.family "redhat"` | `facts.os.family == "redhat"` |
| `ne .env "production"` | `env != "production"` |
| `contains "web" .host.groups` | `host.groups has "web"` |
| `and .docker_repo (eq .facts.os.family "redhat")` | `docker_repo and facts.os.family == "redhat"` |
| `or (eq .a 1) (eq .b 2)` | `a == 1 or b == 2` |
| `and (eq .ensure "present") (not .sudo)` | `ensure == "present" and not sudo` |
| `gt .uid 0` | `uid > 0` |
| `.item.admin` | `item.admin` |
| `defined "item.shell"` | `defined("item.shell")` |

fleetreeve tells the two apart by the dot in front of a variable, which
only the older form has. One condition is in one form or the other, not a
mixture. With an older fleetreeve on a server, keep to the older form:
versions before 0.1.4 do not read the new one.

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
