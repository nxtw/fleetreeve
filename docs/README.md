# fleetreeve documentation

Start with [Getting started](getting-started.md). After that the pages
stand on their own; read what you need.

## Describing hosts

| | |
| --- | --- |
| [Manifests](manifests.md) | resources, ordering, variables, templates, conditions, purging unmanaged files |
| [Data](data.md) | keeping values out of code: layered data per host, group and operating system |
| [Inventory](inventory.md) | which hosts exist, their groups, and how they are reached over SSH |
| [Modules](modules.md) | using modules: roles and resource types that others wrote |
| [Developing modules](module-development.md) | writing your own, from a first role to script types |

## Running it

| | |
| --- | --- |
| [Commands, settings and logging](commands.md) | every command, the `.fleetreeve.yaml` settings file, log formats |
| [State and rollback](state-and-rollback.md) | what fleetreeve remembers per host, pruning, undoing a change |
| [How it works](how-it-works.md) | a remote run step by step, the code layout, current limits |

## A server, for hosts that pull

| | |
| --- | --- |
| [Pull mode](pull-mode.md) | hosts that fetch their own configuration; enrolment, tokens, reports, history |
| [Setting up a server](server-setup.md) | `sudo fleetreeve setup`, as services or as containers; the server's own user and key; upgrading with `sudo fleetreeve upgrade` |
| [Certificates for hosts](certificates.md) | HashiCorp Vault as certificate authority; certificates for the web interfaces |
| [The web console](console.md) | what it shows, accounts and roles, applying and rolling back from a browser |
| [Server API](api.md) | the HTTP API; the full description is [`api/openapi.yaml`](../api/openapi.yaml) |

## Installing and packaging

| | |
| --- | --- |
| [Packages](packages.md) | the RPM and `.deb` packages, and what they do on install |
| [Containers](containers.md) | the server and console images, and running them yourself |

## Examples

[`examples/`](../examples/README.md) has a runnable manifest for every
feature, a complete multi-host project and a pull-mode demo for one
machine.
