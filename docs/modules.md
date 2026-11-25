# Modules

fleetreeve is extended with modules, without changing fleetreeve itself. A module is
a directory, named `namespace.name`, that bundles **roles** and **resource
types** around one subject, with the templates and default values they
need:

```yaml
include:
  - module: acme.nginx                   # its main role
  - {module: acme.nginx, role: tls}      # another of its roles

resources:
  - acme.nginx.vhost: www                # one of its types, by its full name
    server_name: www.example.com
    root: /srv/www
```

A type is either *composite* - a parameterised bundle of other resources,
expanded while compiling, so a plan shows the real files and services - or
a *script*: a program in any language that checks and applies the resource
on the host, and travels inside the catalog, so nothing is installed on
hosts for it. Parameters are checked when compiling, like the attributes
of built-in types.

Modules live in `modules/` next to your manifests and are meant to be
committed with them:

```sh
fleetreeve module add https://git.example.com/acme/fleetreeve-nginx.git -version v1.2.0
fleetreeve module list            # installed modules, their roles and types
fleetreeve types acme.nginx.vhost # one type and its parameters
fleetreeve module verify          # is ./modules still what was locked?
fleetreeve module install         # fetch what modules.yaml lists and ./modules lacks
fleetreeve module init acme.myapp # start one of your own
```

`add` records the source and version in `modules.yaml` and the exact
commit and a checksum in `modules.lock`. A module is code that runs as
root on your hosts: read what you add.

[`examples/fleet/modules`](../examples/fleet/modules) has three real ones:
`fleetreeve.nginx` (a composite virtual-host type), `fleetreeve.firewalld` (a script
type for ports) and `fleetreeve.gitlab_runner` (a role driven by data, and a
script type that registers runners).

**Writing modules** has a guide of its own:
[Developing modules](module-development.md) - roles, variables and who sees them,
both kinds of type, the script protocol, testing, publishing, security
and limits.
