# fleetreeve

Declarative, agentless configuration management: Puppet's model, Ansible's
logistics, in one static binary.

```sh
fleetreeve plan  -i inventory.yaml site.yaml    # what would change, with diffs
fleetreeve apply -i inventory.yaml site.yaml    # make it so
```

You describe the state hosts should be in - files, packages, services,
users - as YAML. fleetreeve works out what differs on each host, shows it, and
changes exactly that, over plain SSH. Nothing has to be installed on the
hosts first.

```yaml
# site.yaml
name: site
resources:
  - package: nginx
  - file: /etc/nginx/conf.d/site.conf
    template: templates/site.conf.tmpl
    validate: nginx -t
    notify: service[nginx]
  - service: nginx
    ensure: running
    enabled: true
```

## What it takes from each

| From Puppet | From Ansible |
| --- | --- |
| **Resources, not tasks.** You declare state (`file`, `package`, `service`…); every type knows how to check and converge it, so runs are idempotent by construction. | **Agentless.** Only SSH is needed. No daemon, no open port, no PKI, nothing to install on nodes. |
| **A dependency graph.** `require` / `before` / `notify` / `subscribe`, plus automatic edges (a file after its directory and owner). | **Readable order.** Where the graph leaves a choice, resources run in the order written. No surprises, no ordering lottery. |
| **Compile, then apply.** Each host gets a *catalog* that is fully resolved and validated — unknown attributes, bad references, cycles and duplicate resources are errors before any host is touched. | **YAML and plain data.** No DSL to learn; variables layered from inventory, groups and hosts. |
| **A trustworthy dry run.** `plan` computes real diffs against the node and exits 2 on drift, so it works as a drift detector in CI or cron. | **Push when you want it.** Run from a laptop or pipeline against any subset of hosts, in parallel; ad-hoc commands with `fleetreeve run`. |
| **Failure isolation.** A failed resource skips only what depends on it; the rest of the host still converges. | **Simple inventory** with groups, `-l` limits and `-e` overrides. |
| **Continuous enforcement** when you ask for it: `apply -every 30m`, or nodes that pull on their own (see *Pull mode*). | **Pre-flight `validate`** on config files before they replace the live one. |

And what it leaves behind: Puppet's server, agents, certificates and custom
language; Ansible's imperative task lists, `changed_when` bookkeeping,
handlers-as-a-separate-concept, and per-task SSH round trips (fleetreeve makes a
handful of SSH calls per host, however many resources there are).

## Quick start

```sh
make build agents          # bin/fleetreeve + node binaries for linux/darwin, amd64/arm64
bin/fleetreeve plan  examples/demo.yaml     # runs on this machine, no root needed
bin/fleetreeve apply examples/demo.yaml
bin/fleetreeve apply examples/demo.yaml     # second run: "in sync"
```

[`examples/`](examples/README.md) has a runnable manifest for every feature,
a complete multi-host project (`examples/fleet`) and a pull-mode walkthrough
you can run on one machine (`examples/pull/local-demo.sh`).

## Install

Download a build from the [releases page](https://github.com/nxtw/fleetreeve/releases):
a single binary for Linux and macOS, RPM and `.deb` packages, and container
images. For example, on Linux:

```sh
curl -fsSLO https://github.com/nxtw/fleetreeve/releases/latest/download/fleetreeve_linux_amd64.tar.gz
tar xzf fleetreeve_linux_amd64.tar.gz && sudo install fleetreeve /usr/local/bin/
fleetreeve version
```

Or build it yourself; it needs only Go:

```sh
git clone https://github.com/nxtw/fleetreeve && cd fleetreeve
make build agents          # bin/fleetreeve, and the builds for hosts of every platform
```

[Getting started](docs/getting-started.md) has every way of installing and
the first steps after it.

## Quick start

```sh
fleetreeve plan  examples/demo.yaml     # runs on this machine, no root needed
fleetreeve apply examples/demo.yaml
fleetreeve apply examples/demo.yaml     # second run: "in sync"
```

[`examples/`](examples/README.md) has a runnable manifest for every feature,
a complete multi-host project (`examples/fleet`) and a pull-mode walkthrough
you can run on one machine (`examples/pull/local-demo.sh`).

A whole server - with web console, its own user and key, and optionally
HashiCorp Vault as certificate authority - is one command on a fresh
machine:

```sh
sudo fleetreeve setup
```

## Documentation

| | |
| --- | --- |
| [Getting started](docs/getting-started.md) | installing, a first manifest, a first host, a first server |
| [Manifests](docs/manifests.md) | resources, ordering, variables, templates, conditions, purging |
| [Data](docs/data.md) | keeping values out of code: layered data per host, group and OS |
| [Inventory](docs/inventory.md) | which hosts exist and how they are reached |
| [Modules](docs/modules.md) | extending fleetreeve with roles and resource types; [developing your own](docs/module-development.md) |
| [State and rollback](docs/state-and-rollback.md) | what fleetreeve remembers, removing what left the manifest, undoing a change |
| [Commands, settings and logging](docs/commands.md) | every command, the settings file, log formats |
| [Pull mode](docs/pull-mode.md) | hosts that fetch their own configuration from a server |
| [Setting up a server](docs/server-setup.md) | `fleetreeve setup`, as services or containers; the server's own identity |
| [Certificates for hosts](docs/certificates.md) | HashiCorp Vault as certificate authority, also for the web interfaces |
| [The web console](docs/console.md) | what it shows, accounts and roles, running and rolling back from it |
| [Server API](docs/api.md) | the HTTP API, with an [OpenAPI document](api/openapi.yaml) |
| [Containers](docs/containers.md) and [packages](docs/packages.md) | other ways to run and install it |
| [How it works](docs/how-it-works.md) | a remote run step by step, the code layout, current limits |

`fleetreeve types` documents every resource type from the command line,
including those of installed modules.

## Status

fleetreeve is young: version 0.x, used by its authors, with interfaces that may
still change between minor versions. The [changelog](CHANGELOG.md) says
what did. [Current limits](docs/how-it-works.md#current-limits) lists what
it does not do yet.

## Contributing and security

Bug reports, ideas and patches are welcome: see
[CONTRIBUTING.md](CONTRIBUTING.md). Please report security problems
privately, as [SECURITY.md](SECURITY.md) describes.

## Licence

[Apache License 2.0](LICENSE).
