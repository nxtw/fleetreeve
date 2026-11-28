# Developing modules

This guide is for people who write fleetreeve modules. Using a module that
someone else wrote takes three lines and is covered in the
[Modules](modules.md); everything about making one is here.

- [What a module is, and when to write one](#what-a-module-is-and-when-to-write-one)
- [Anatomy of a module](#anatomy-of-a-module)
- [Starting a module](#starting-a-module)
- [`module.yaml`](#moduleyaml)
- [Roles](#roles)
- [Variables: who sees what](#variables-who-sees-what)
- [Resource types: two kinds](#resource-types-two-kinds)
- [Parameters](#parameters)
- [Composite types](#composite-types)
- [Script types](#script-types)
- [A worked example](#a-worked-example)
- [Developing and testing](#developing-and-testing)
- [Publishing, versions and the lock file](#publishing-versions-and-the-lock-file)
- [Modules on a fleetreeve server](#modules-on-a-fleetreeve-server)
- [Security](#security)
- [Limits](#limits)
- [Checklist before you publish](#checklist-before-you-publish)

## What a module is, and when to write one

A module is a directory that bundles everything fleetreeve needs to manage one
subject - nginx, a firewall, a CI runner, your own application - so that a
project can use it without copying manifests around. It can contain:

- **roles**: ordinary manifests that a project includes;
- **resource types**: new kinds of resource a project can declare, next to
  the built-in `file`, `package`, `service` and so on;
- the **templates and files** those roles and types install;
- **default values** for the variables they read.

Modules extend fleetreeve without changing fleetreeve. Nothing is compiled, nothing
is installed on the hosts, and there is no plugin interface to learn: a
module is YAML, plus scripts in whatever language you like when you need
real logic.

Write a module when:

- the same set of resources is wanted in more than one project, or by more
  than one team;
- a group of resources belongs together and is repeated with different
  values - a virtual host, a database, a tenant - so that it deserves to be
  **one resource with parameters** instead of five resources and a loop;
- something has state that the built-in types cannot see - a firewall port,
  a setting inside an application, an entry in an API - and you are tempted
  to write an `exec` with an `unless`. A script type gives that thing real
  state: it shows up in a plan as `closed → open`, not as "would run a
  command".

Do not write a module for something used once in one project. A role in
the project's own `roles/` directory is simpler, and can be turned into a
module later without changing how it reads.

## Anatomy of a module

```
modules/
  acme.nginx/              the directory is named exactly like the module
    module.yaml            name, version, description, default variables
    README.md              for the people who use it
    roles/
      main.yaml            included with   - module: acme.nginx
      tls.yaml             included with   - {module: acme.nginx, role: tls}
    types/
      vhost.yaml           a type, used as   - acme.nginx.vhost: <title>
      port.yaml            another one
      port.sh              the program behind a script type
    templates/             templates the roles and types render
    files/                 files they copy
```

Only `module.yaml` is required. A module may have only roles, only types,
or both.

**Names.**

- A module is called `namespace.name`, in lower case: letters, digits and
  underscores, each part starting with a letter. `acme.nginx`,
  `fleetreeve.gitlab_runner`. The namespace is yours - a company, a team, a
  person - and exists so that two people can both publish an `nginx`
  module.
- The directory must have the same name as the module in it. fleetreeve refuses
  a module whose directory and `name:` disagree, because every reference
  to it goes by name.
- A role's name is its file name without `.yaml`.
- A type's name is its file name without `.yaml`: a lower-case word
  (letters, digits, underscores, starting with a letter). Its full name,
  the one manifests use, is `<module>.<type>`: `acme.nginx.vhost`.

**Where modules are found.** For a manifest, fleetreeve looks in this order and
takes the first module it finds under a given name:

1. `modules/` next to the manifest - the project's own, committed with it;
2. each directory in `$FLEETREEVE_MODULES` (separated by `:`);
3. `~/.fleetreeve/modules`;
4. `/usr/share/fleetreeve/modules`.

So a project can always override a system-wide module by having its own
copy.

## Starting a module

```sh
cd my-project
fleetreeve module init acme.app
```

creates `modules/acme.app/` with a `module.yaml`, a `README.md`, an empty
main role and one example type, all of them valid, so you can look at the
result immediately:

```
$ fleetreeve module list
acme.app 0.1.0  What this module manages, in one line.
  in modules/acme.app
  role  main                         include:  - module: acme.app
  type  acme.app.thing               composite; One thing.

$ fleetreeve types acme.app.thing
acme.app.thing
  One thing.
  From a module; made of other resources. Defined in modules/acme.app/types/thing.yaml

Parameters:
  content  (string, default "")
      What to put in it.
```

From there you edit files. There is no build step and nothing to
register: the next `fleetreeve` command reads the module as it is on disk.

## `module.yaml`

```yaml
name: acme.app                 # required; must match the directory
version: 1.2.0                 # shown by `fleetreeve module list` and `module add`
description: The app server, its instances and its settings.

# Defaults for the variables the module's roles and types read.
vars:
  app_user: app
  app_root: /opt/app
  app_config: "{{ .app_root }}/config"      # may refer to other variables
```

| Key | |
| --- | --- |
| `name` | Required. `namespace.name`. |
| `version` | Free text; use [semantic versions](https://semver.org) and matching git tags. fleetreeve shows it but does not compare versions - which version a project gets is decided by the git tag or commit it was added at. |
| `description` | One line, shown in listings. |
| `vars` | Default values, see [Variables](#variables-who-sees-what). |

Other keys are ignored, so you can keep your own metadata (`license`,
`maintainer`, `source`) in the same file.

## Roles

A role is a manifest, with exactly the abilities of any other manifest:
`vars`, `include`, `resources`, templates, `when`, `each`, relationships.
What makes it a role is only where it lives and how it is included:

```yaml
# In a project
include:
  - module: acme.app                         # roles/main.yaml
  - {module: acme.app, role: tls}            # roles/tls.yaml
  - module: acme.app                         # with a condition and its own values
    role: worker
    when: host.groups has "workers"
    vars: {worker_count: 4}
```

A role named `main` is what `- module: X` includes; every other role is
included by name. Asking for a role that does not exist is an error that
lists the ones that do.

**Write roles that are driven by data.** A role should say *what* is
managed and leave *the values* to the project:

```yaml
# modules/acme.app/roles/main.yaml
resources:
  - user: "{{ .app_user }}"
    system: true

  - file: "{{ .app_root }}"
    ensure: directory
    owner: "{{ .app_user }}"

  # One instance per entry in the project's data. Adding an entry to the
  # data adds an instance; nothing here changes.
  - acme.app.instance: "{{ .item.name }}"
    each: "{{ .app_instances }}"
    port: "{{ .item.port }}"
```

```yaml
# In the project: data/groups/app.yaml
app_instances:
  - {name: blue, port: 8080}
  - {name: green, port: 8081}
```

**Paths are relative to the file they are written in.** A role in
`roles/main.yaml` refers to the module's templates as
`../templates/app.conf.tmpl` and to its files as `../files/logo.png`. This
holds wherever the module is installed, which is why a module can be moved
or vendored without edits.

**Splitting a module into roles.** Put what everybody needs in `main`, and
what is optional in roles of their own (`tls`, `backup`, `monitoring`). A
role may include another role of the same module:

```yaml
# roles/tls.yaml
include:
  - module: acme.app          # make sure the base is there
resources:
  - file: /etc/app/tls.conf
    template: ../templates/tls.conf.tmpl
```

A role is included **once** however often it is asked for with the same
values - by the project, by other roles, or both - so a role can always
include what it builds on without anyone having to keep track. Including
it again with *different* `vars:` is a second inclusion, and its resources
then clash with the first (fleetreeve reports a resource declared twice, with
both places): a role that is meant to be used several times with
different values should be a [type](#resource-types-two-kinds) instead.

**Refuse what you do not support.** A role that only works on one family
of systems should say so instead of half-working elsewhere:

```yaml
resources:
  - exec: acme.app supports the Red Hat family only
    command: 'echo "this host is {{ .facts.os.pretty }}" >&2; exit 1'
    unless: "false"
    when: facts.os.family != "redhat"

  - package: app-server
    when: facts.os.family == "redhat"
```

**Insist on what you cannot do without.** The `required` template function
turns a missing value into a compile error with your own message, before
any host is touched:

```yaml
  - file: /etc/app/license
    content: '{{ required "acme.app needs app_license: put it in your data" .app_license }}'
```

## Variables: who sees what

This is the part of module writing that surprises people, so it is spelled
out.

A module has two places for default values, and they differ in reach:

| Where | Seen by | Use it for |
| --- | --- | --- |
| `vars:` in `module.yaml` | every role **and every type** of the module | values the whole module relies on |
| `vars:` at the top of a role | that role, and manifests it includes | values only that role needs |

What can be seen from where:

- **A role** sees the module's `vars`, its own `vars`, and everything the
  project provides: its manifests' variables, the inventory, the data
  layer, facts (`.facts`), the host (`.host`), `-e` values.
- **A type** sees the module's `vars`, everything in scope *at the place
  where the type is used* (the project's variables, a surrounding `each`'s
  `.item`), its own parameters, and `.title`. It does **not** see the
  `vars:` of the module's roles: a type can be used by a project that
  never includes the role. If a type needs a value, it belongs in
  `module.yaml`, or should be a parameter.
- **The project** does not see a module's variables at all. If a project
  needs the same value, it sets the variable itself - which also overrides
  the module's default.

Precedence, lowest first - a later one wins:

1. the module's `vars` (`module.yaml`)
2. the role's own `vars`
3. variables of the manifests that include it
4. the inventory: all hosts, then groups, then the host
5. the data layer
6. `vars:` given on the `include`
7. `-e` on the command line
8. for a type: its parameters and `title`, which nothing overrides

So a project overrides any module default simply by setting the variable
anywhere it sets its other values - usually in `data/`.

**Naming.** Variables are global to a compilation, so prefix yours with
the module's subject: `nginx_user`, not `user`. A project that uses two
modules should never have to wonder whose `port` it is setting.

**Document them.** The variables a module reads are its interface. List
each one in the README with its default and what it does; `fleetreeve` cannot
list them for you, since any template may read any variable.

## Resource types: two kinds

A type is defined by one YAML file in `types/`. What the file contains
decides its kind:

| | Composite type | Script type |
| --- | --- | --- |
| The file has | `resources:` | `script:` |
| It is | a parameterised bundle of other resources | a program that checks and applies one thing |
| It runs | nowhere: it is expanded while compiling | on the host, by the agent |
| A plan shows | the resources it is made of | the type itself, with the changes its script reports |
| Use it for | things that *are* files, packages, services and so on, repeated with different values | things with state the built-in types cannot see |
| Needs on the host | nothing | the script's interpreter |
| Rollback, removal when dropped | as for the resources it is made of | no (see [Limits](#limits)) |

Prefer a composite type whenever the thing can be expressed with existing
resources. It costs nothing at run time, it is fully visible in a plan,
and it inherits state tracking, removal and rollback from the resources it
is made of. Reach for a script type when you would otherwise write an
`exec` guarded by `unless`.

A type cannot be both; a file with `script` and `resources` is an error,
as is one with neither.

Both kinds are used the same way, and take the same metaparameters as
every resource:

```yaml
resources:
  - acme.app.instance: blue            # <module>.<type>: <title>
    port: 8080
    when: host.groups has "app"
    require: package[app-server]
    notify: service[haproxy]

  - acme.app.instance: [green, red]    # several titles, same parameters
    port: 8081

  - acme.app.instance: "{{ .item.name }}"
    each: "{{ .app_instances }}"
    port: "{{ .item.port }}"
```

## Parameters

Both kinds of type declare their parameters the same way:

```yaml
params:
  port:
    type: int
    required: true
    description: The port the instance listens on.
  features:
    type: list
    default: []
    description: Features to switch on.
  debug: {type: bool, default: false}
  owner: {default: app}                 # type defaults to string
  limits: {type: map}                   # optional, no default
```

| Key | |
| --- | --- |
| `type` | `string` (the default), `int`, `bool`, `list`, `map`. |
| `required` | `true`: declaring the resource without it is a compile error. |
| `default` | Used when the parameter is not given. Must fit the type. |
| `description` | Shown by `fleetreeve types <type>`. Write one for every parameter: it is the type's documentation. |

How values are treated:

- A value is converted to the parameter's type, and refused when it cannot
  be: `port: many` for an `int` is a compile error that names the
  resource, the parameter and the place.
- Conversions are the forgiving kind: `"8080"` is accepted for an `int`,
  `"true"` for a `bool`. A template that yields a single value keeps its
  type - `port: "{{ .item.port }}"` passes a number, not text.
- A `list` holds strings; a `map` maps strings to strings. For anything
  richer, pass JSON in a string parameter, or split the type.
- An unknown parameter is a compile error, with a suggestion when the name
  is close to a real one (`unknown parameter "prot" (did you mean
  "port"?)`).
- A parameter that is neither given nor has a default is simply absent:
  templates in a composite type test it with `defined "name"`; a script
  does not get its environment variable.

Names you cannot use for a parameter: `title` (it is always there) and the
metaparameters `require`, `before`, `notify`, `subscribe`, `when`, `each`,
`rollback`.

**Design advice.**

- The **title** should be the thing's natural name - the one a person
  would use for it, and unique on a host: a virtual host's name, a port
  with its protocol, a file's path.
- Make required only what truly has no sensible default. Every required
  parameter is something each user must look up.
- Give a type that can take things away an `ensure` parameter
  (`present`/`absent`, `open`/`closed`), as the built-in types have. fleetreeve
  cannot undo a script, so removal has to be something the type does.
- Do not use a parameter for what is the same on every use; that is a
  variable in `module.yaml`.

## Composite types

```yaml
# types/instance.yaml
# Used as:   - acme.app.instance: blue
#                port: 8080
description: One instance of the app - directory, configuration and service.

params:
  port: {type: int, required: true, description: The port to listen on.}
  features: {type: list, default: [], description: Features to switch on.}

resources:
  - file: "{{ .app_root }}/{{ .title }}"
    ensure: directory
    owner: "{{ .app_user }}"

  - file: "{{ .app_root }}/{{ .title }}/app.conf"
    template: ../templates/instance.conf.tmpl
    notify: service[app@{{ .title }}]
    rollback: true

  - file: "{{ .app_root }}/{{ .title }}/features/{{ .item }}"
    each: "{{ .features }}"
    content: "on\n"

  - file: "{{ .app_root }}/{{ .title }}/debug"
    when: features has "debug"
    content: "on\n"

  - service: "app@{{ .title }}"
    ensure: running
    enabled: true
```

**What is available inside.** `.title`, every parameter by its name, the
module's `vars`, and whatever was in scope where the type is used - see
[Variables](#variables-who-sees-what). Templates rendered by the type
(`template:`) see the same.

**It is expanded while compiling.** By the time a host is involved the
type is gone: the catalog holds the files, packages and services it is
made of, and that is what `plan` shows, what the state records and what
reports list. In the console they are grouped under the module's name.
Their "declared in" points into the type's file.

**Titles must differ per use.** Every resource in a catalog is unique by
type and title. If two uses of your type both declare
`file: /etc/app/app.conf`, the second is an error. Put `{{ .title }}` (or
a parameter that identifies the use) in the title of every resource the
type declares. A resource that all uses share - a package, a reload
command - does not belong in the type; put it in a role and let the type
refer to it.

**Relationships on a use apply to everything it is made of.**

```yaml
  - acme.app.instance: blue
    port: 8080
    require: package[app-server]      # every resource of blue requires the package
    notify: service[haproxy]          # a change to any of them notifies haproxy
```

**A reference to a use means everything it is made of.**

```yaml
  - exec: warm the cache
    command: /usr/local/bin/warm
    refreshonly: true
    subscribe: acme.app.instance[blue]   # refreshed when anything of blue changes
    require: acme.app.instance[blue]     # ordered after all of blue
```

Inside the type, resources refer to each other as they would anywhere:
`notify: service[app@{{ .title }}]`.

**Order.** Resources are applied in the order the type declares them,
unless relationships say otherwise - as in any manifest.

**Conditions and loops.** `when` and `each` work inside a type, and on a
use of it. A use with a false `when` declares nothing. Inside the type,
`.item` is the type's own loop variable where it uses `each`; outside
such a loop, the `.item` of a surrounding `each` (the one on the use) is
visible.

**Rollback.** `rollback: true` inside the type applies to that resource.
`rollback: true` on a use applies to every resource of it whose type can
be rolled back.

**Types can use types.** A composite type may declare resources of other
module types, composite or script, of its own module or another. Nesting
stops at 20 levels, which catches two types that use each other.

**Removal.** When a use disappears from the manifest, its resources leave
the catalog like any others: those fleetreeve created are removed (unless
pruning is off), those it only adopted are left and no longer tracked.

## Script types

A script type is a program that knows how to look at one thing on a host
and how to make it right.

```yaml
# types/port.yaml
# Used as:   - acme.firewall.port: 8080/tcp
description: A port in the firewall, open or closed.
params:
  ensure: {default: open, description: open or closed.}
  zone: {description: The zone; the default zone when omitted.}
script: port.sh           # the program, next to this file
refresh: false            # true: it also wants to be told when notified
```

| Key | |
| --- | --- |
| `description` | One line. |
| `params` | See [Parameters](#parameters). |
| `script` | The program's file, relative to the definition. One file. |
| `refresh` | `true` if the script handles the `refresh` operation. |

### How it travels and runs

When a manifest uses the type, the compiler puts the script's text into
the catalog of every host that needs it. On the host, the agent writes it
to `<state directory>/scripts/<hash of the text>` (by default under
`/var/lib/fleetreeve/state/`), readable and executable by root only, and runs
it from there. This has consequences worth knowing:

- **Nothing is installed on hosts for a module.** It works the same over
  SSH and in pull mode, and a new host needs no preparation.
- **A host always runs the version the catalog was compiled with.** There
  is no way for a host to have an old copy of your script: change the
  script and the next catalog carries the new one.
- **The script is one file.** It cannot import a sibling file from the
  module - the sibling is not sent. Keep helpers in the same file.
- **It runs as the agent does** - root, on managed hosts - with the
  agent's environment. Do not rely on the working directory.
- **Its interpreter must exist on the host.** `/bin/sh` always does. For
  Python, Perl or anything else, start with a `#!` line and make sure the
  role installs the interpreter before the type is used (a `require` on
  the package). A script without a `#!` line is run with `/bin/sh`.
- The state directory is used rather than `/tmp` because hardened hosts
  mount `/tmp` so that nothing in it can be executed.

### The operations

The script is called with one argument:

| Called as | When | It must |
| --- | --- | --- |
| `script check` | every run, and every dry run | print what differs; change nothing |
| `script apply` | when `check` reported differences, and it is not a dry run | make the changes |
| `script refresh` | when something that notifies the resource changed (only with `refresh: true`) | react: reload, re-read, rebuild |

Each operation has five minutes; after that it is stopped and the
resource fails.

### What the script is given

The same information arrives twice, so that both a three-line shell script
and a real program are comfortable.

**In the environment:**

| Variable | |
| --- | --- |
| `FLEETREEVE_OP` | `check`, `apply` or `refresh` - the same as the argument |
| `FLEETREEVE_TITLE` | the resource's title |
| `FLEETREEVE_TYPE` | the type's full name, e.g. `acme.firewall.port` |
| `FLEETREEVE_PLAN` | `1` during a dry run, unset otherwise |
| `FLEETREEVE_PARAM_<NAME>` | one per parameter that has a value, the name in upper case with anything but letters and digits turned into `_` |

How values look in the environment:

| Type | Example | In the environment |
| --- | --- | --- |
| string | `text: hello` | `FLEETREEVE_PARAM_TEXT=hello` |
| int | `n: 7` | `FLEETREEVE_PARAM_N=7` |
| bool | `flag: true` | `FLEETREEVE_PARAM_FLAG=true` (or `false`) |
| list | `items: [a, "b c"]` | `FLEETREEVE_PARAM_ITEMS` holds one item per line |
| map | `pairs: {k: v, x: 1}` | `FLEETREEVE_PARAM_PAIRS={"k":"v","x":"1"}` (JSON) |

A parameter without a value and without a default has no variable at all:
in shell, write `${FLEETREEVE_PARAM_ZONE:-}`, never bare `$FLEETREEVE_PARAM_ZONE`
under `set -u`.

**On standard input**, as one JSON document, with the values in their real
types:

```json
{"op": "apply",
 "title": "8080/tcp",
 "plan": false,
 "params": {"ensure": "open", "flag": true, "n": 7, "items": ["a", "b c"], "pairs": {"k": "v", "x": "1"}}}
```

Use the environment for simple scripts and standard input when you have
lists, maps or values with line breaks. A script that does not read its
standard input can ignore it.

### What `check` prints

`check` reports the difference between how things are and how they were
declared. If there is none, it prints **nothing** and exits 0 - that is
what "in sync" means.

Otherwise it prints the differences, in one of two forms.

*Lines*, one per property that differs:

```
running: closed -> open
permanent: closed -> open
```

Each line is `property: from -> to`. They appear in a plan and in reports
exactly so (`running: closed → open`). A line that is not of this form is
still a difference - it is shown as a description (`state: → <the line>`)
- so `echo "the index needs rebuilding"` is a legitimate check result.

*JSON*, when the output starts with `{`:

```json
{"changes": [{"prop": "value", "from": "8080", "to": "9090"},
             {"prop": "owner", "from": "root", "to": "app"}]}
```

`{"changes": []}` also means in sync. Output that starts with `{` but is
not valid JSON fails the resource, so do not start a plain line with a
brace.

The rules for `check`, which everything else depends on:

- **It must change nothing.** It runs on every dry run, including the ones
  people do precisely because they want nothing to happen, and the ones
  the console runs on request. A check that "fixes things up on the way"
  makes `fleetreeve plan` a lie.
- **It must be honest about being in sync.** After a successful `apply`,
  the next `check` must print nothing. If it keeps reporting a difference,
  the host is reported as changed on every run for ever, and people stop
  reading reports.
- **It should be cheap.** It runs on every host on every run.
- **It should cope with what is not there yet.** On a first run, during a
  dry run, the package your type depends on may not be installed. "The
  thing does not exist" is a difference to report, not an error: test for
  the command before calling it, and report `absent -> present`.

### What `apply` does

`apply` makes the host match the declaration. It is only called when
`check` found something, but it is not told what: it should look for
itself and do whatever is needed, so that running it twice is harmless.
Anything it prints is ignored unless it fails.

### Failing

Any operation fails the resource by exiting with a non-zero status.
Whatever it wrote to standard error becomes the error message that people
see in the output, the report and the console:

```
✘ acme.firewall.port[9100/tcp]  failed: check: exit status 2: ensure must be open or closed, not bogus
```

So write errors for the person who will read them, and write them to
standard error. A failed resource does not stop the run: resources that
depend on it are skipped, everything else carries on.

Validate what you are given at the start of the script - for every
operation, since `check` is what a dry run executes - and fail with a
clear message rather than doing something odd with a bad value.

### Refresh

With `refresh: true`, a resource of your type can be the target of
`notify` (or use `subscribe`), and the script is then called with
`refresh` after the things it depends on changed. Use it for "re-read
your configuration". If the resource itself was just changed by `apply`,
`refresh` is still called when it was notified; make it harmless.

Without `refresh: true`, notifying the resource has no effect.

### A complete script type in shell

```sh
#!/bin/sh
# acme.firewall.port: the title is <port>/<protocol>, e.g. 8080/tcp.
set -eu
port=$FLEETREEVE_TITLE
want=${FLEETREEVE_PARAM_ENSURE:-open}
zone=${FLEETREEVE_PARAM_ZONE:+--zone=$FLEETREEVE_PARAM_ZONE}

# Refuse what makes no sense, for every operation.
case $want in open|closed) ;; *) echo "ensure must be open or closed, not $want" >&2; exit 2 ;; esac
case $port in */tcp|*/udp) ;; *) echo "the title must be <port>/<protocol>, like 8080/tcp; got $port" >&2; exit 2 ;; esac

# Before firewalld is installed or started, every port counts as closed.
is_open() { firewall-cmd $1 $zone --query-port="$port" >/dev/null 2>&1; }
state() { if is_open "$1"; then echo open; else echo closed; fi; }

case $1 in
check)
    [ "$(state --permanent)" = "$want" ] || echo "permanent: $(state --permanent) -> $want"
    [ "$(state '')" = "$want" ] || echo "running: $(state '') -> $want"
    ;;
apply)
    verb=--add-port
    [ "$want" = open ] || verb=--remove-port
    [ "$(state --permanent)" = "$want" ] || firewall-cmd --permanent $zone $verb="$port" >/dev/null
    [ "$(state '')" = "$want" ] || firewall-cmd $zone $verb="$port" >/dev/null
    ;;
*)
    echo "unknown operation: $1" >&2
    exit 2
    ;;
esac
```

This is the `fleetreeve.firewalld.port` type that ships in
[`examples/fleet/modules`](../examples/fleet/modules/fleetreeve.firewalld).

### The same idea in Python

```python
#!/usr/bin/env python3
"""acme.app.setting: one key in a JSON settings file."""
import json, os, sys

req = json.load(sys.stdin)                 # {"op", "title", "params", "plan"}
p = req["params"]

try:
    with open(p["file"]) as f:
        doc = json.load(f)
except FileNotFoundError:
    doc = {}
except ValueError as e:
    sys.exit(f"{p['file']} is not valid JSON: {e}")      # to stderr, exit status 1

want = {"value": p["value"], "tags": p.get("tags", [])}
have = doc.get(p["key"])

if req["op"] == "check":
    changes = []
    if have is None:
        changes.append({"prop": "key", "from": "absent", "to": "present"})
    else:
        for field in want:
            if have.get(field) != want[field]:
                changes.append({"prop": field, "from": json.dumps(have.get(field)), "to": json.dumps(want[field])})
    print(json.dumps({"changes": changes}))

elif req["op"] == "apply":
    doc[p["key"]] = want
    tmp = p["file"] + ".tmp"                # write next to it, then move: never half a file
    with open(tmp, "w") as f:
        json.dump(doc, f, indent=2, sort_keys=True)
    os.replace(tmp, p["file"])

elif req["op"] == "refresh":
    pass                                    # tell the app to re-read its settings
```

with

```yaml
# types/setting.yaml
description: One key in a JSON settings file.
params:
  file: {required: true}
  key: {required: true}
  value: {required: true}
  tags: {type: list, default: []}
script: setting.py
refresh: true
```

### Secrets in script types

Parameters travel in the catalog, and reach the script in its environment
and on its standard input. Treat them accordingly:

- Never print a secret, in any operation - not in a change line either.
  Report `token: (another) -> (the one given)`, not the values.
- Do not pass a secret on a command line (`curl -H "Token: $t"`): other
  users on the host can see command lines. Feed it on standard input or
  through a file only root can read (`curl --config -`).
- If your script keeps something it obtained (a token an API returned),
  keep it under a directory only root can read, and say in the README
  where.
- The catalog is cached on the host in pull mode. It is readable by root
  only, but it is there.

## A worked example

A small but complete module: an application with a settings file, where
each instance is one resource.

```
modules/acme.app/
  module.yaml
  roles/main.yaml
  types/instance.yaml       composite: uses the script type below
  types/setting.yaml        script
  types/setting.py          (the Python script shown above)
```

```yaml
# module.yaml
name: acme.app
version: 0.1.0
description: An application with instances and settings.
vars:
  app_root: /opt/app
```

```yaml
# roles/main.yaml
resources:
  - file: "{{ .app_root }}"
    ensure: directory
```

(A real module would also make sure of the script's interpreter here -
`- package: python3` - and have the settings `require` it.)

```yaml
# types/instance.yaml
description: One instance of the app - directory, settings and a debug marker.
params:
  port: {type: int, required: true}
  features: {type: list, default: []}
resources:
  - file: "{{ .app_root }}/{{ .title }}"
    ensure: directory

  - acme.app.setting: "{{ .title }} port"           # a script type inside a composite type
    file: "{{ .app_root }}/{{ .title }}/settings.json"
    key: port
    value: "{{ .port }}"

  - acme.app.setting: "{{ .title }} feature {{ .item }}"
    each: "{{ .features }}"
    file: "{{ .app_root }}/{{ .title }}/settings.json"
    key: "feature.{{ .item }}"
    value: "on"
    tags: [feature]

  - file: "{{ .app_root }}/{{ .title }}/debug"
    when: features has "debug"
    content: "on\n"
```

A project uses it like this:

```yaml
# site.yaml
include:
  - module: acme.app

resources:
  - acme.app.instance: blue
    port: 8080
    features: [search, debug]
    require: file[{{ .app_root }}]

  - file: "{{ .app_root }}/release"
    content: "{{ .release }}"
    notify: acme.app.instance[blue]     # reaches the settings inside blue
```

(The project sets `app_root` itself if it wants to use the variable in its
own manifest, as here: a module's variables are not visible outside it.)

A dry run shows one use of the type as the seven resources it is:

```
$ fleetreeve plan site.yaml -e release=1
  ~ file[/opt/app]  ensure: absent → directory
  ~ file[/opt/app/release]  ensure: absent → file
  ~ file[/opt/app/blue]  ensure: absent → directory
  ~ acme.app.setting[blue port]  key: absent → present, ↻ refresh
  ~ acme.app.setting[blue feature search]  key: absent → present, ↻ refresh
  ~ acme.app.setting[blue feature debug]  key: absent → present, ↻ refresh
  ~ file[/opt/app/blue/debug]  ensure: absent → file
  7 resources · 7 to change · 3 to refresh
```

After `fleetreeve apply`, a second run finds nothing to do. Changing the port
and the release then changes exactly one setting, and the notification
refreshes all three:

```
$ fleetreeve apply site.yaml -e release=2
  ✔ file[/opt/app/release]  content: sha256:6b86b273ff34 → sha256:d4735e3a265e
  ✔ acme.app.setting[blue port]  value: "8080" → "9090", ↻ refresh
  · acme.app.setting[blue feature search]  ↻ refresh
  · acme.app.setting[blue feature debug]  ↻ refresh
  7 resources · 2 changed · 3 refreshed
```

And a settings file that someone broke by hand fails the resources that
need it, with the script's own words:

```
  ✘ acme.app.setting[blue port]  failed: check: exit status 1: /opt/app/blue/settings.json is not valid JSON: Expecting property name enclosed in double quotes: line 1 column 2 (char 1)
```

## Developing and testing

Work in a small project of its own, with your module in its `modules/`
directory and a `site.yaml` that uses it. Every step below works on your
own machine, with no server and no other host.

**See what fleetreeve sees.**

```sh
fleetreeve module list                    # the module, its roles and types - or the error in it
fleetreeve types                          # module types appear after the built-in ones
fleetreeve types acme.app.instance        # one type: its kind, its file, its parameters
```

A mistake in a module - a name that does not match its directory, a type
with neither script nor resources, a parameter of an unknown type, a
script file that is missing - is reported by any of these, with the file.

**Compile without touching anything.**

```sh
fleetreeve validate site.yaml                                # for this machine
fleetreeve validate -i inventory.yaml site.yaml              # for every host of an inventory
fleetreeve validate -i inventory.yaml -facts centos9.facts.json site.yaml
```

`validate` renders every template, expands every composite type and
checks every parameter. `-facts` uses facts saved from a real host
(`fleetreeve facts > centos9.facts.json` on it), so you can compile for a
system you are not sitting at - and see your "Red Hat only" guard work
from a Mac. `fleetreeve graph` shows the order resources will be applied in,
which is the quickest way to check the relationships of a composite type.

**Dry-run and apply on your own machine.** Point the module at a scratch
directory through a variable and run it for real:

```sh
fleetreeve plan site.yaml -e app_root=/tmp/app-test       # runs each script's check
fleetreeve apply site.yaml -e app_root=/tmp/app-test
fleetreeve apply site.yaml -e app_root=/tmp/app-test      # must say: in sync
```

The third command is the most important test a module has. If the second
run is not in sync, a `check` is reporting a difference that `apply` does
not remove.

Set `FLEETREEVE_STATE_DIR` to a scratch directory while developing, so that
your experiments do not mix with the machine's real state and need no
root.

**Run a script by hand.** It is an ordinary program:

```sh
echo '{"op":"check","title":"t","params":{"file":"/tmp/x.json","key":"k","value":"v"},"plan":true}' |
    FLEETREEVE_OP=check FLEETREEVE_TITLE=t FLEETREEVE_PARAM_FILE=/tmp/x.json FLEETREEVE_PARAM_KEY=k FLEETREEVE_PARAM_VALUE=v \
    modules/acme.app/types/setting.py check
```

This is the fastest way to debug one, and it is how you would test it in
your own test suite: call it with `check`, assert on the output; call
`apply`; call `check` again and assert that it prints nothing.

**Test the unhappy paths.** For every type: a missing required parameter,
a parameter of the wrong type, a nonsense value your script should refuse,
the thing not being installed yet, and the thing being broken. Each
should produce a message that tells its reader what to do.

**Test on a real host before you publish.** A throwaway virtual machine
and `fleetreeve apply -i inventory.yaml site.yaml` shows what your own machine
hides: SELinux, a missing interpreter, a different path, a service that
needs a moment to start.

**In CI.** `fleetreeve validate` with saved facts for each system you support,
and `fleetreeve module verify` if the project vendors modules, are cheap and
catch most regressions. Both exit non-zero on failure.

**Reading errors.** An error inside a type names the resource, the type's
file and line, and where the type was used:

```
modules/acme.app/types/instance.yaml:10: file[/opt/app/blue/unit]: template ../templates/unit.tmpl: <.app_user>: map has no entry for key "app_user"
    (in acme.app.instance[blue], declared at site.yaml:7)
```

"map has no entry for key" always means a variable that is not in scope:
see [Variables](#variables-who-sees-what) - most often a type reading a
variable that only a role defines.

## Publishing, versions and the lock file

A module is published by putting it in a git repository. There is no
registry and nothing to upload.

**Layout of the repository.** The simplest is one module per repository,
with `module.yaml` at the top. The repository's own name does not matter;
the module is installed under the name in its `module.yaml`. Several
modules can share a repository in subdirectories; users then say which
with `-path`.

**Versions.** Set `version:` in `module.yaml` and tag the commit with the
same number (`git tag v1.2.0`). Follow semantic versioning: a new major
version when a project has to change something to keep working (a
parameter renamed or removed, a variable renamed, a default that changes
what hosts end up with), a minor one for additions, a patch for fixes.
Write down what changed; people read it before they update.

**What users do.**

```sh
fleetreeve module add https://git.example.com/acme/fleetreeve-app.git -version v1.2.0
fleetreeve module add https://git.example.com/acme/fleetreeve-modules.git -version v3.0.0 -path app
fleetreeve module add ../shared/acme.app              # a directory, copied as it is
```

`add` fetches the module into the project's `modules/` directory, without
its git history, and records two things:

```yaml
# modules.yaml - what the project asked for
modules:
  - name: acme.app
    source: https://git.example.com/acme/fleetreeve-app.git
    version: v1.2.0
```

```yaml
# modules.lock - exactly what it got
modules:
  - name: acme.app
    source: https://git.example.com/acme/fleetreeve-app.git
    version: v1.2.0
    commit: 3f9c2e1…
    sha256: 6a790b40…
```

The module's files are meant to be **committed with the project**, lock
file included. What gets compiled is then what was reviewed, a checkout
needs no network, and a module's repository going away breaks nothing.

```sh
fleetreeve module verify       # is ./modules still exactly what was locked?
fleetreeve module install      # fetch what modules.yaml lists and ./modules lacks
```

- `verify` compares a checksum of each module's files with the lock and
  says which are as locked, which differ, which are missing, and which
  are not in the lock at all (a module of the project's own). It exits
  non-zero when something differs - put it in CI.
- `install` restores a module to exactly the locked content. It fetches
  the locked *commit*, not the tag: if a tag has since been moved to
  other content, `install` still gives what was locked, and if the source
  no longer has that commit, or holds different content for it, it says
  so instead of installing something else.
- **Updating** is `fleetreeve module add` again with the new version. It
  replaces the module and the lock entry; `git diff` then shows exactly
  what changed in the module, which is the review.

Do not edit a vendored module in place: `verify` will (rightly) complain,
and the next update overwrites your change. Override its variables from
your data, or change the module at its source.

**A module of the project's own** simply lives in `modules/` without an
entry in `modules.yaml`. `fleetreeve module init` starts one. `verify` reports
it as "not in modules.lock", which is information, not a failure.

## Modules on a fleetreeve server

Nothing about modules is specific to pull mode, but two facts are worth
knowing when a server compiles the catalogs:

- Modules are read **where catalogs are compiled**. In pull mode that is
  the server: they live next to the server's manifest
  (`/srv/fleetreeve/modules/`), and the server reads them afresh for every
  catalog, so a changed module takes effect at each host's next run with
  no restart. With `fleetreeve apply` from a controller, it is the
  controller's copy that counts.
- **Hosts need nothing.** A composite type never leaves the compiler; a
  script type arrives inside the catalog. Enrolling a host does not
  involve modules, and a host cannot be "missing a module".

The one thing that does have to match is fleetreeve itself: a host's client
must understand the catalog format the server produces. Keep server and
clients on the same release.

## Security

A module is code that runs as root on every host that uses it - its roles
as much as its scripts. That is true of any configuration, but a module
is configuration you did not write.

For people **using** modules:

- Read a module before you add it, as you would any dependency: the
  roles, and every script.
- Pin a version, commit the module and the lock file, and review the diff
  when you update. The lock tells you *that* something changed, not
  whether it is trustworthy.
- Run `fleetreeve module verify` in CI, so that a module edited in place is
  noticed.

For people **writing** them:

- Do not download and execute things at run time. If your role needs a
  repository, configure it with signature checks on, as the system's own
  tools would.
- Treat every parameter as text from someone else: quote it in shell
  (`"$FLEETREEVE_PARAM_NAME"`), never `eval` it, validate it before use.
- Keep secrets out of output and off command lines (see
  [Secrets in script types](#secrets-in-script-types)).
- Say in the README what the module does to a host that is not obvious
  from its name: users it creates, ports it opens, services it enables,
  what it grants.

## Limits

What modules cannot do today, so that you do not go looking:

- **Script types cannot be rolled back.** `rollback: true` on one is a
  compile error: fleetreeve does not know how to undo what a script did.
- **Script types are not removed when they leave the manifest.** A
  resource of a script type that is dropped is simply no longer tracked.
  Give the type an `ensure` parameter, and tell users to set it to the
  removing value before deleting the resource.
- **A script type is one file.** No helper files, no libraries shipped
  with it.
- **The interpreter is your job.** fleetreeve does not install Python because
  a script starts with `#!/usr/bin/env python3`.
- **No dependencies between modules.** A module cannot declare that it
  needs another one, at any version. If yours uses another module's type,
  say so in the README; a project that lacks it gets a clear compile
  error naming the missing module.
- **A module's variables are private to it.** Projects cannot read them,
  and a type cannot read a role's.
- **Modules add roles and types only** - not facts, not template
  functions, not commands.
- **`list` and `map` parameters hold strings.** No nested structures.
- **No registry, no search.** A module is found by knowing its git URL.

## Checklist before you publish

- [ ] The directory, `name:` in `module.yaml` and every type's prefix
      agree.
- [ ] `fleetreeve module list` and `fleetreeve types <type>` show what you expect,
      and every parameter has a description.
- [ ] `fleetreeve validate` passes with facts from each system you claim to
      support, and fails helpfully on one you do not.
- [ ] `fleetreeve apply` twice: the second run is in sync.
- [ ] `fleetreeve plan` changes nothing on the host - try it with the thing
      your type manages absent, present and broken.
- [ ] Every resource a composite type declares has `{{ .title }}` (or an
      identifying parameter) in its title.
- [ ] Every `check` prints nothing when in sync, and reports "not there
      yet" as a difference rather than failing.
- [ ] Errors go to standard error and tell the reader what to do.
- [ ] No secret is printed or passed on a command line.
- [ ] Variables are prefixed with the module's subject, have defaults in
      `module.yaml`, and are listed in the README.
- [ ] The README says what the module manages, shows the three lines it
      takes to use it, lists roles, types and variables, and names the
      systems it supports.
- [ ] `version:` is set, the commit is tagged with it, and the changes
      since the last version are written down.
